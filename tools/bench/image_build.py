#!/usr/bin/env python3
"""Measure comparable local container-image builds.

The runner deliberately builds from throw-away, allow-listed source contexts.  It
never modifies the checkout and never prunes the engine.  Result images are
retained by default so a caller can inspect or smoke-test them after the run;
use ``--cleanup-images`` to remove only this run's uniquely tagged images.

Example::

    python tools/bench/image_build.py \
      --engine podman \
      --baseline-dockerfile .artifacts/image-review-baseline/Dockerfile \
      --baseline-dockerignore .artifacts/image-review-baseline/.dockerignore \
      --output .artifacts/image-build-report.json

The report's ``inspect.size_bytes`` is the engine-reported image-inspect
``Size`` field.  It is not a sum of history display sizes.  When Podman can
export an OCI directory, ``oci_export.layer_compressed_blob_bytes`` is reported
separately as the sum of exported compressed layer blobs.
"""

from __future__ import annotations

import argparse
import datetime as _datetime
import hashlib
import json
import os
import platform
import re
import shlex
import shutil
import signal
import subprocess
import sys
import tempfile
import time
import uuid
from pathlib import Path
from typing import Any, Iterable, Sequence


DEFAULT_ROOT_FILES = (
    "go.mod",
    "go.sum",
    "go.work",
    "go.work.sum",
    "package.json",
    "package-lock.json",
    "npm-shrinkwrap.json",
    "pnpm-lock.yaml",
    "yarn.lock",
    "bun.lock",
    "bun.lockb",
    "tsconfig.json",
    "vite.config.ts",
    "vite.config.js",
    "Makefile",
    "LICENSE",
    "COPYING",
    "VERSION",
)
DEFAULT_SOURCE_DIRS = (
    "cmd",
    "internal",
    "web",
    "tools/schema",
    "tools/verify",
)
EXCLUDED_COMPONENTS = {
    ".git",
    ".artifacts",
    "node_modules",
    ".venv",
    "venv",
    "__pycache__",
    ".pytest_cache",
    "coverage",
    "dist",
    "build",
    "tmp",
    "data",
    "charts",
    "private",
}
EXCLUDED_PREFIXES = (".env",)
EXCLUDED_BASENAMES = {
    ".npmrc",
    ".netrc",
    ".pypirc",
    ".git-credentials",
    "credentials.json",
    "service-account.json",
}
EXCLUDED_SUFFIXES = (".pem", ".key", ".p12", ".pfx", ".jks", ".secret")
GENERATED_ASSET_PREFIX = Path("internal") / "console" / "assets"
GENERATED_WEB_PREFIX = Path("web") / "src" / "generated"
GENERATED_EMPTY_DIRS = (GENERATED_WEB_PREFIX,)

SCENARIOS = (
    {"id": "cold", "label": "cold build", "mutation": None, "no_cache": True},
    {
        "id": "no-change-warm",
        "label": "no-change warm build",
        "mutation": None,
        "no_cache": False,
    },
    {
        "id": "backend-only-comment",
        "label": "backend-only comment change",
        "mutation": "backend",
        "no_cache": False,
    },
    {
        "id": "frontend-only-css-change",
        "label": "frontend-only CSS probe (unused custom property)",
        "mutation": "frontend_css",
        "no_cache": False,
    },
    {
        "id": "verification-tool-only-comment",
        "label": "verification-tool-only comment change",
        "mutation": "verification_tool",
        "no_cache": False,
    },
)

MUTATION_MARKERS = {
    "backend": "// image-build measurement: backend-only comment",
    "frontend_css": ":root { --hoorific-image-build-probe: 1; }",
    "verification_tool": "// image-build measurement: verification-tool-only comment",
}


def utc_now() -> str:
    return _datetime.datetime.now(_datetime.timezone.utc).isoformat()


def positive_float(value: str) -> float:
    try:
        parsed = float(value)
    except ValueError as exc:
        raise argparse.ArgumentTypeError("must be a positive number") from exc
    if parsed <= 0:
        raise argparse.ArgumentTypeError("must be a positive number")
    return parsed


def sha256_bytes(data: bytes) -> str:
    return hashlib.sha256(data).hexdigest()


def sha256_file(path: Path) -> str:
    digest = hashlib.sha256()
    with path.open("rb") as stream:
        for chunk in iter(lambda: stream.read(1024 * 1024), b""):
            digest.update(chunk)
    return digest.hexdigest()


def _relative_or_none(path: Path, root: Path) -> Path | None:
    try:
        return path.resolve().relative_to(root.resolve())
    except ValueError:
        return None


def is_excluded(relative: Path) -> bool:
    parts = relative.parts
    if not parts:
        return False
    if any(part in EXCLUDED_COMPONENTS for part in parts):
        return True
    if any(part.startswith(prefix) for part in parts for prefix in EXCLUDED_PREFIXES):
        return True
    name = relative.name
    if name in EXCLUDED_BASENAMES:
        return True
    if name.lower().endswith(EXCLUDED_SUFFIXES):
        return True
    generated = GENERATED_ASSET_PREFIX
    if relative == generated or generated in relative.parents:
        return True
    generated_web = GENERATED_WEB_PREFIX
    return relative == generated_web or generated_web in relative.parents


def iter_regular_files(root: Path) -> Iterable[tuple[Path, Path]]:
    """Yield (absolute path, relative path), skipping symlinks and excluded data."""
    root = root.resolve()
    if not root.exists():
        return
    for current, directories, filenames in os.walk(root, topdown=True, followlinks=False):
        current_path = Path(current)
        kept_directories: list[str] = []
        for directory in sorted(directories):
            path = current_path / directory
            relative = path.relative_to(root)
            if path.is_symlink() or is_excluded(relative):
                continue
            kept_directories.append(directory)
        directories[:] = kept_directories
        for filename in sorted(filenames):
            path = current_path / filename
            relative = path.relative_to(root)
            if path.is_symlink() or is_excluded(relative) or not path.is_file():
                continue
            yield path, relative


def _copy_entry(source_root: Path, relative: Path, destination: Path, included: set[str]) -> None:
    if is_excluded(relative):
        return
    source = source_root / relative
    if source.is_symlink() or not source.exists():
        return
    if source.is_dir():
        for source_file, _ in iter_regular_files(source):
            file_relative = source_file.relative_to(source_root)
            destination_file = destination / file_relative
            destination_file.parent.mkdir(parents=True, exist_ok=True)
            shutil.copy2(source_file, destination_file)
            included.add(file_relative.as_posix())
    elif source.is_file():
        target = destination / relative
        target.parent.mkdir(parents=True, exist_ok=True)
        shutil.copy2(source, target)
        included.add(relative.as_posix())


def dockerfile_copy_sources(dockerfile: str) -> list[str]:
    """Find explicit COPY source paths without treating COPY . as an allow-list."""
    logical_lines: list[str] = []
    pending = ""
    for raw_line in dockerfile.splitlines():
        line = raw_line.rstrip()
        if pending:
            pending += line.lstrip()
        else:
            pending = line
        if pending.endswith("\\"):
            pending = pending[:-1]
            continue
        logical_lines.append(pending)
        pending = ""
    if pending:
        logical_lines.append(pending)

    sources: list[str] = []
    for line in logical_lines:
        match = re.match(r"^\s*COPY\s+(.+)$", line, flags=re.IGNORECASE)
        if not match:
            continue
        try:
            tokens = shlex.split(match.group(1), posix=True)
        except ValueError:
            continue
        tokens = [token for token in tokens if not token.startswith("--")]
        if len(tokens) < 2:
            continue
        for token in tokens[:-1]:
            normalized = token.replace("\\", "/")
            if not normalized or normalized in (".", "./"):
                continue
            if normalized.startswith("/") or "$" in normalized:
                continue
            sources.append(normalized.lstrip("./"))
    return sources


def copy_source_context(
    source_root: Path,
    destination: Path,
    dockerfiles: Sequence[str],
) -> list[str]:
    """Copy only known source roots/manifests and safe explicit COPY inputs."""
    source_root = source_root.resolve()
    destination.mkdir(parents=True, exist_ok=True)
    included: set[str] = set()

    for relative_name in DEFAULT_ROOT_FILES:
        _copy_entry(source_root, Path(relative_name), destination, included)
    for directory_name in DEFAULT_SOURCE_DIRS:
        _copy_entry(source_root, Path(directory_name), destination, included)

    for dockerfile in dockerfiles:
        for source_name in dockerfile_copy_sources(dockerfile):
            relative = Path(source_name)
            if relative.is_absolute() or _relative_or_none(source_root / relative, source_root) is None:
                continue
            _copy_entry(source_root, relative, destination, included)

    return sorted(included)


def tree_hash(root: Path, *, exclude_context_files: bool = False) -> str:
    digest = hashlib.sha256()
    for path, relative in iter_regular_files(root):
        if exclude_context_files and relative.as_posix() in {"Dockerfile", ".dockerignore"}:
            continue
        digest.update(relative.as_posix().encode("utf-8"))
        digest.update(b"\0")
        with path.open("rb") as stream:
            for chunk in iter(lambda: stream.read(1024 * 1024), b""):
                digest.update(chunk)
        digest.update(b"\0")
    return digest.hexdigest()


def append_marker(path: Path, marker: str) -> None:
    data = path.read_bytes()
    separator = b"" if data.endswith((b"\n", b"\r")) else b"\n"
    path.write_bytes(data + separator + marker.encode("utf-8") + b"\n")


def choose_mutation_file(source_root: Path, mutation: str) -> Path | None:
    preferred = {
        "backend": Path("internal/gateway/gateway.go"),
        "frontend_css": Path("web/src/styles.css"),
        "verification_tool": Path("tools/verify/main.go"),
    }.get(mutation)
    if preferred:
        preferred_path = source_root / preferred
        if preferred_path.is_file() and not preferred_path.is_symlink() and not is_excluded(preferred):
            return preferred_path

    patterns = {
        "backend": (("internal", "*.go"), ("cmd", "*.go")),
        "frontend_css": (("web", "*.css"),),
        "verification_tool": (("tools/verify", "*.go"),),
    }
    for directory, pattern in patterns.get(mutation, ()):
        base = source_root / directory
        if not base.exists():
            continue
        candidates = sorted(
            path
            for path in base.rglob(pattern)
            if path.is_file() and not path.is_symlink() and not is_excluded(path.relative_to(source_root))
        )
        non_tests = [path for path in candidates if not path.name.endswith("_test.go")]
        if non_tests:
            return non_tests[0]
        if candidates:
            return candidates[0]
    return None


def declares_cache_namespace(dockerfile: str) -> bool:
    return re.search(
        r"^\s*ARG\s+HOORIFIC_CACHE_NAMESPACE(?:\s*=|\s|$)",
        dockerfile,
        flags=re.IGNORECASE | re.MULTILINE,
    ) is not None


def parse_base_images(dockerfile: str) -> list[str]:
    images: list[str] = []
    stage_names: set[str] = set()
    from_images: list[str] = []
    for raw_line in dockerfile.splitlines():
        line = raw_line.strip()
        if not line or line.startswith("#"):
            continue
        match = re.match(r"^FROM\s+(?:(?:--platform=)\S+\s+)?(\S+)", line, flags=re.IGNORECASE)
        if not match:
            continue
        image = match.group(1)
        from_images.append(image)
        alias = re.search(r"\s+AS\s+([^\s]+)", line, flags=re.IGNORECASE)
        if alias:
            stage_names.add(alias.group(1).lower())
    for image in from_images:
        if image.lower() == "scratch" or image.startswith("$") or image.lower() in stage_names:
            continue
        if image not in images:
            images.append(image)
    return images


def parse_json_output(output: bytes) -> Any | None:
    text = output.decode("utf-8", errors="replace").strip()
    if not text:
        return None
    try:
        return json.loads(text)
    except json.JSONDecodeError:
        pass
    for line in reversed(text.splitlines()):
        line = line.strip()
        if not line:
            continue
        try:
            return json.loads(line)
        except json.JSONDecodeError:
            continue
    return None


def parse_history(output: bytes) -> list[Any]:
    text = output.decode("utf-8", errors="replace").strip()
    if not text:
        return []

    rows: list[Any] = []
    for line in text.splitlines():
        line = line.strip()
        if not line:
            continue
        try:
            item = json.loads(line)
        except json.JSONDecodeError:
            continue
        if isinstance(item, list):
            rows.extend(item)
        else:
            rows.append(item)
    if rows:
        return rows

    try:
        parsed = json.loads(text)
    except json.JSONDecodeError:
        parsed = None
    if isinstance(parsed, list):
        return parsed
    if isinstance(parsed, dict):
        return [parsed]
    return [{"raw": line} for line in text.splitlines() if line.strip()]


def command_summary(record: dict[str, Any] | None) -> dict[str, Any] | None:
    if record is None:
        return None
    return {
        "command_id": record["command_id"],
        "argv": record["argv"],
        "exit_code": record["exit_code"],
        "wall_seconds": record["wall_seconds"],
        "timed_out": record["timed_out"],
        "log_path": record["log_path"],
    }


class Report:
    def __init__(self, output: Path | None, log_dir: Path, initial: dict[str, Any]) -> None:
        self.output = output
        self.log_dir = log_dir
        self.data = initial

    def flush(self) -> None:
        if self.output is None:
            return
        self.output.parent.mkdir(parents=True, exist_ok=True)
        with tempfile.NamedTemporaryFile(
            mode="w", encoding="utf-8", dir=self.output.parent,
            prefix=f".{self.output.name}.", suffix=".tmp", delete=False,
        ) as handle:
            temporary = Path(handle.name)
            json.dump(self.data, handle, indent=2, ensure_ascii=False)
            handle.write("\n")
        os.replace(temporary, self.output)

    def warning(self, message: str) -> None:
        self.data.setdefault("warnings", []).append(message)
        self.flush()

    def failure(self, message: str) -> None:
        self.data.setdefault("failures", []).append(message)
        self.data["incomplete"] = True
        self.flush()


def stop_owned_command(process: subprocess.Popen[bytes]) -> bytes:
    """Stop only the process group created by CommandRunner; bound pipe draining."""
    output = b""
    for hard in (False, True):
        try:
            if hasattr(os, "killpg"):
                os.killpg(process.pid, signal.SIGKILL if hard else signal.SIGTERM)
            elif hard:
                process.kill()
            else:
                process.terminate()
        except ProcessLookupError:
            pass
        try:
            return process.communicate(timeout=5)[0]
        except subprocess.TimeoutExpired as exc:
            output = exc.output or output
    for stream in (process.stdout, process.stderr):
        if stream is not None:
            stream.close()
    return output


class CommandRunner:
    def __init__(self, report: Report, engine: str, timeout: float) -> None:
        self.report = report
        self.engine = engine
        self.timeout = timeout
        self.report.log_dir.mkdir(parents=True, exist_ok=True, mode=0o700)

    def run(self, argv: Sequence[str], kind: str, cwd: Path | None = None) -> tuple[dict[str, Any], bytes]:
        command_id = f"cmd-{len(self.report.data['commands']) + 1:04d}"
        started = time.perf_counter()
        started_at = utc_now()
        output = b""
        error: str | None = None
        timed_out = False
        interrupted = False
        process: subprocess.Popen[bytes] | None = None
        try:
            process = subprocess.Popen(
                list(argv),
                cwd=str(cwd) if cwd else None,
                stdin=subprocess.DEVNULL,
                stdout=subprocess.PIPE,
                stderr=subprocess.STDOUT,
                start_new_session=True,
            )
            try:
                output, _ = process.communicate(timeout=self.timeout)
            except (subprocess.TimeoutExpired, KeyboardInterrupt) as exc:
                interrupted = isinstance(exc, KeyboardInterrupt)
                timed_out = not interrupted
                error = "command interrupted" if interrupted else f"command exceeded {self.timeout:g}s timeout"
                output = stop_owned_command(process)
        except OSError as exc:
            error = f"could not execute command: {exc}"
        wall_seconds = round(time.perf_counter() - started, 6)
        if process is None:
            exit_code: int | None = None
        else:
            exit_code = process.returncode
        safe_kind = re.sub(r"[^A-Za-z0-9_.-]+", "-", kind).strip("-") or "command"
        log_path = self.report.log_dir / f"{command_id}-{safe_kind}.log"
        log_path.write_bytes(output)
        record: dict[str, Any] = {
            "command_id": command_id,
            "kind": kind,
            "argv": [str(arg) for arg in argv],
            "cwd": str(cwd) if cwd else None,
            "started_at": started_at,
            "finished_at": utc_now(),
            "wall_seconds": wall_seconds,
            "exit_code": exit_code,
            "timed_out": timed_out,
            "interrupted": interrupted,
            "ok": exit_code == 0 and not timed_out and not interrupted,
            "log_path": str(log_path),
            "log_bytes": len(output),
        }
        if error:
            record["error"] = error
        self.report.data["commands"].append(record)
        self.report.flush()
        if interrupted:
            raise KeyboardInterrupt("image build measurement interrupted")
        return record, output


def engine_command(engine: str, *arguments: str) -> list[str]:
    """Return an argv list with Podman's global local-mode switch in place."""
    if engine == "podman":
        return [engine, "--remote=false", *arguments]
    return [engine, *arguments]


def build_command(
    engine: str,
    context: Path,
    tag: str,
    namespace: str | None,
    no_cache: bool,
) -> list[str]:
    pull = "--pull=never" if engine == "podman" else "--pull=false"
    command = engine_command(engine, "build", pull)
    if engine == "podman":
        command.append("--layers")
    if no_cache:
        command.append("--no-cache")
    if namespace is not None:
        command.extend(["--build-arg", f"HOORIFIC_CACHE_NAMESPACE={namespace}"])
    command.extend(
        [
            "--tag",
            tag,
            "--file",
            str(context / "Dockerfile"),
            str(context),
        ]
    )
    return command




def numeric_or_none(value: Any) -> int | None:
    try:
        return int(value) if value is not None else None
    except (TypeError, ValueError):
        return None


def inspect_image(
    commands: CommandRunner,
    engine: str,
    tag: str,
) -> tuple[dict[str, Any] | None, dict[str, Any], bytes]:
    record, output = commands.run(engine_command(engine, "image", "inspect", tag), "image-inspect")
    summary = command_summary(record)
    if not record["ok"]:
        return None, {"command": summary, "error": record.get("error", "inspect failed")}, output
    parsed = parse_json_output(output)
    if isinstance(parsed, list) and parsed:
        parsed = parsed[0]
    if not isinstance(parsed, dict):
        return None, {"command": summary, "error": "image inspect did not return a JSON object"}, output
    size = numeric_or_none(parsed.get("Size"))
    virtual_size = numeric_or_none(parsed.get("VirtualSize"))
    result: dict[str, Any] = {
        "command": summary,
        "image_id": parsed.get("Id") or parsed.get("ID"),
        "created": parsed.get("Created"),
        "architecture": parsed.get("Architecture"),
        "os": parsed.get("Os") or parsed.get("OS"),
        "size_bytes": size,
        "size_field": "Size",
        "virtual_size_bytes": virtual_size,
        "virtual_size_field": "VirtualSize",
        "size_semantics": "engine-reported image inspect Size and VirtualSize; values are not derived from history display sizes",
        "rootfs_layers": (parsed.get("RootFS") or {}).get("Layers") if isinstance(parsed.get("RootFS"), dict) else None,
    }
    return parsed, result, output


def inspect_history(
    commands: CommandRunner,
    engine: str,
    tag: str,
) -> tuple[dict[str, Any], bool]:
    if engine == "podman":
        format_arg = "json"
    else:
        format_arg = "{{json .}}"
    record, output = commands.run(
        engine_command(engine, "image", "history", "--no-trunc", "--format", format_arg, tag),
        "image-history",
    )
    summary = command_summary(record)
    rows = parse_history(output) if record["ok"] else []
    result = {
        "command": summary,
        "rows": rows,
        "size_semantics": "per-layer engine history rows only; no human-readable history-size sum is used",
    }
    return result, bool(record["ok"])


def _blob_path(layout: Path, digest: str) -> Path | None:
    if ":" not in digest:
        return None
    algorithm, value = digest.split(":", 1)
    if not re.fullmatch(r"[A-Za-z0-9_-]+", algorithm) or not re.fullmatch(r"[A-Fa-f0-9]+", value):
        return None
    return layout / "blobs" / algorithm / value


def oci_blob_sizes(layout: Path) -> tuple[int, int, int, list[str]]:
    blob_root = layout / "blobs"
    blobs = (
        [path for path in blob_root.rglob("*") if path.is_file() and not path.is_symlink()]
        if blob_root.exists()
        else []
    )
    total = sum(path.stat().st_size for path in blobs)
    layer_paths: set[Path] = set()
    media_types: set[str] = set()
    index_path = layout / "index.json"
    if index_path.exists():
        try:
            index = json.loads(index_path.read_text(encoding="utf-8"))
        except (OSError, json.JSONDecodeError):
            index = {}
        manifests = index.get("manifests", []) if isinstance(index, dict) else []
        for descriptor in manifests:
            if not isinstance(descriptor, dict):
                continue
            manifest_path = _blob_path(layout, str(descriptor.get("digest", "")))
            if not manifest_path or not manifest_path.exists():
                continue
            try:
                manifest = json.loads(manifest_path.read_text(encoding="utf-8"))
            except (OSError, json.JSONDecodeError):
                continue
            layers = manifest.get("layers", []) if isinstance(manifest, dict) else []
            for layer in layers:
                if not isinstance(layer, dict):
                    continue
                media_type = str(layer.get("mediaType", ""))
                if media_type:
                    media_types.add(media_type)
                path = _blob_path(layout, str(layer.get("digest", "")))
                if path and path.exists():
                    layer_paths.add(path)
    return total, sum(path.stat().st_size for path in layer_paths), len(blobs), sorted(media_types)


def measure_export(
    commands: CommandRunner,
    engine: str,
    tag: str,
    temporary_root: Path,
    skip: bool,
) -> dict[str, Any]:
    if skip:
        return {
            "available": False,
            "skipped": True,
            "semantics": "OCI export measurement was disabled with --skip-oci-export",
        }
    export_root = Path(tempfile.mkdtemp(prefix="export-", dir=temporary_root))
    try:
        if engine == "podman":
            layout = export_root / "oci"
            record, _ = commands.run(
                engine_command(engine, "save", "--format", "oci-dir", "--output", str(layout), tag),
                "oci-export",
            )
            result: dict[str, Any] = {
                "available": bool(record["ok"]),
                "format": "oci-dir",
                "command": command_summary(record),
                "semantics": "sum of OCI layout blob files from podman save; layer_compressed_blob_bytes is valid only when manifest media types confirm gzip/zstd; excludes directory metadata and is distinct from inspect.Size",
            }
            if record["ok"]:
                total, layers, count, media_types = oci_blob_sizes(layout)
                compressed = bool(media_types) and all(
                    "gzip" in media_type.lower() or "zstd" in media_type.lower() for media_type in media_types
                )
                result.update(
                    {
                        "all_blob_bytes": total,
                        "layer_blob_bytes": layers,
                        "layer_compressed_blob_bytes": layers if compressed else None,
                        "compression_verified": compressed,
                        "layer_media_types": media_types,
                        "blob_count": count,
                    }
                )
            else:
                result["reason"] = record.get("error", "OCI export failed")
            return result

        archive = export_root / "image.tar"
        record, _ = commands.run(
            engine_command(engine, "save", "--output", str(archive), tag),
            "docker-archive-export",
        )
        result = {
            "available": bool(record["ok"]),
            "format": "docker-archive",
            "command": command_summary(record),
            "semantics": "Docker save archive byte size; Docker CLI has no portable OCI-dir save format, so this is not an OCI blob metric",
        }
        if record["ok"] and archive.exists():
            result["archive_bytes"] = archive.stat().st_size
        elif not record["ok"]:
            result["reason"] = record.get("error", "Docker archive export failed")
        return result
    except OSError as exc:
        return {
            "available": False,
            "format": "oci-dir" if engine == "podman" else "docker-archive",
            "semantics": "export measurement could not inspect its temporary output",
            "reason": str(exc),
        }
    finally:
        shutil.rmtree(export_root, ignore_errors=True)


def base_image_checks(
    commands: CommandRunner,
    engine: str,
    dockerfile: str,
) -> list[dict[str, Any]]:
    checked: list[dict[str, Any]] = []
    for image in parse_base_images(dockerfile):
        record, _ = commands.run(engine_command(engine, "image", "inspect", image), "base-image-check")
        checked.append(
            {
                "image": image,
                "preexisting": bool(record["ok"]),
                "command": command_summary(record),
            }
        )
    return checked


def make_context(
    source_root: Path,
    context: Path,
    dockerfile_bytes: bytes,
    dockerfile_text: str,
    dockerignore_bytes: bytes | None,
    all_dockerfiles: Sequence[str],
    mutation: str | None,
    mutation_relative: Path | None,
) -> dict[str, Any]:
    included = copy_source_context(source_root, context, all_dockerfiles)
    for empty_directory in GENERATED_EMPTY_DIRS:
        if (source_root / empty_directory).is_dir():
            (context / empty_directory).mkdir(parents=True, exist_ok=True)
    (context / "Dockerfile").write_bytes(dockerfile_bytes)
    if dockerignore_bytes is not None:
        (context / ".dockerignore").write_bytes(dockerignore_bytes)
    if mutation and mutation_relative:
        target = context / mutation_relative
        if not target.exists() or target.is_symlink():
            raise RuntimeError(f"scenario mutation target was not copied: {mutation_relative.as_posix()}")
        append_marker(target, MUTATION_MARKERS[mutation])
    return {
        "path": str(context),
        "source_files": included,
        "source_hash": tree_hash(context, exclude_context_files=True),
        "context_hash": tree_hash(context),
        "dockerfile_sha256": sha256_bytes(dockerfile_bytes),
        "dockerignore_sha256": sha256_bytes(dockerignore_bytes) if dockerignore_bytes is not None else None,
        "dockerfile_text_bytes": len(dockerfile_bytes),
    }


def measure_variant(
    commands: CommandRunner,
    report: Report,
    engine: str,
    context_info: dict[str, Any],
    scenario: dict[str, Any],
    role: str,
    tag: str,
    namespace: str | None,
    temporary_root: Path,
    skip_oci_export: bool,
    cleanup_images: bool,
) -> dict[str, Any]:
    context = Path(context_info["path"])
    build_record, _ = commands.run(
        build_command(engine, context, tag, namespace, bool(scenario["no_cache"])),
        f"build-{scenario['id']}-{role}",
        cwd=context,
    )
    result: dict[str, Any] = {
        "role": role,
        "tag": tag,
        "image_retained": not cleanup_images,
        "cache_namespace": namespace,
        "no_layer_cache": bool(scenario["no_cache"]),
        "build": command_summary(build_record),
    }
    if not build_record["ok"]:
        result["status"] = "failed"
        result["error"] = build_record.get("error", "image build failed")
        report.failure(f"{scenario['id']}/{role}: image build failed")
        return result

    _, inspect, _ = inspect_image(commands, engine, tag)
    result["inspect"] = inspect
    inspect_ok = inspect.get("size_bytes") is not None and not inspect.get("error")
    history, history_ok = inspect_history(commands, engine, tag)
    result["history"] = history
    export = measure_export(commands, engine, tag, temporary_root, skip_oci_export)
    result["oci_export"] = export
    if not inspect_ok:
        result["status"] = "failed"
        result["error"] = inspect.get("error", "image inspect did not provide Size")
        report.failure(f"{scenario['id']}/{role}: image inspect measurement failed")
    elif not history_ok:
        result["status"] = "failed"
        result["error"] = "image history command failed"
        report.failure(f"{scenario['id']}/{role}: image history measurement failed")
    else:
        result["status"] = "succeeded"

    if cleanup_images:
        cleanup_record, _ = commands.run(engine_command(engine, "image", "rm", tag), "cleanup-image")
        result["cleanup"] = command_summary(cleanup_record)
        if not cleanup_record["ok"]:
            result["status"] = "failed"
            result["error"] = "owned result image cleanup failed"
            report.failure(f"{scenario['id']}/{role}: owned image cleanup failed")
    return result


def resolve_path(value: str | None, base: Path) -> Path | None:
    if value is None:
        return None
    path = Path(value).expanduser()
    if not path.is_absolute():
        path = base / path
    return path.resolve()


def parser_for(repo_root: Path) -> argparse.ArgumentParser:
    example = (
        "Reproduce a report from the captured baseline:\n  "
        "python tools/bench/image_build.py --engine podman "
        "--baseline-dockerfile .artifacts/image-review-baseline/Dockerfile "
        "--baseline-dockerignore .artifacts/image-review-baseline/.dockerignore "
        "--output .artifacts/image-build-report.json"
    )
    parser = argparse.ArgumentParser(
        description=(
            "Serially compare local Podman/Docker image builds using isolated safe source contexts. "
            "No global prune is performed; result images are retained unless explicitly cleaned."
        ),
        epilog=example,
        formatter_class=argparse.RawDescriptionHelpFormatter,
    )
    parser.add_argument("--engine", choices=("podman", "docker"), required=True, help="local image engine")
    parser.add_argument("--baseline-dockerfile", required=True, help="baseline Dockerfile snapshot")
    parser.add_argument("--baseline-dockerignore", help="optional baseline .dockerignore snapshot")
    parser.add_argument("--candidate-dockerfile", help="candidate Dockerfile (default: SOURCE_ROOT/Dockerfile)")
    parser.add_argument("--candidate-dockerignore", help="candidate .dockerignore (default: SOURCE_ROOT/.dockerignore if present)")
    parser.add_argument(
        "--source-root",
        default=str(repo_root),
        help="checkout root to copy from (default: repository containing this script)",
    )
    parser.add_argument("--output", default=".artifacts/image-build-report.json", help="JSON report path, or '-' for stdout")
    parser.add_argument(
        "--timeout",
        "--command-timeout",
        dest="timeout",
        type=positive_float,
        default=1800.0,
        help="bounded timeout in seconds for every engine command (default: 1800)",
    )
    parser.add_argument(
        "--skip-oci-export",
        action="store_true",
        help="skip optional post-build OCI/Docker archive measurement",
    )
    parser.add_argument(
        "--cleanup-images",
        action="store_true",
        help="remove only this run's uniquely tagged result images after measurement (default retains them)",
    )
    return parser


def run_measurement(args: argparse.Namespace, repo_root: Path, report: Report) -> int:
    source_root = resolve_path(args.source_root, Path.cwd())
    baseline_dockerfile = resolve_path(args.baseline_dockerfile, Path.cwd())
    candidate_dockerfile = resolve_path(args.candidate_dockerfile, source_root) if args.candidate_dockerfile else source_root / "Dockerfile"
    baseline_ignore = resolve_path(args.baseline_dockerignore, Path.cwd())
    if args.candidate_dockerignore:
        candidate_ignore = resolve_path(args.candidate_dockerignore, Path.cwd())
    else:
        candidate_ignore = source_root / ".dockerignore"
        if not candidate_ignore.exists():
            candidate_ignore = None

    for label, path, must_be_file in (
        ("source root", source_root, False),
        ("baseline Dockerfile", baseline_dockerfile, True),
        ("candidate Dockerfile", candidate_dockerfile, True),
    ):
        if path is None or (path.is_file() if must_be_file else path.is_dir()) is False:
            raise FileNotFoundError(f"{label} not found: {path}")
    if baseline_ignore is not None and not baseline_ignore.is_file():
        raise FileNotFoundError(f"baseline dockerignore not found: {baseline_ignore}")
    if candidate_ignore is not None and not candidate_ignore.is_file():
        raise FileNotFoundError(f"candidate dockerignore not found: {candidate_ignore}")
    if shutil.which(args.engine) is None:
        raise FileNotFoundError(f"engine executable not found on PATH: {args.engine}")

    baseline_bytes = baseline_dockerfile.read_bytes()
    candidate_bytes = candidate_dockerfile.read_bytes()
    baseline_text = baseline_bytes.decode("utf-8", errors="replace")
    candidate_text = candidate_bytes.decode("utf-8", errors="replace")
    baseline_ignore_bytes = baseline_ignore.read_bytes() if baseline_ignore else None
    candidate_ignore_bytes = candidate_ignore.read_bytes() if candidate_ignore else None

    report_output = None if args.output == "-" else resolve_path(args.output, Path.cwd())
    run_id = f"{_datetime.datetime.now(_datetime.timezone.utc).strftime('%Y%m%dt%H%M%Sz')}-{uuid.uuid4().hex[:10]}"
    if report_output is None:
        log_dir = source_root / ".artifacts" / f"image-build-logs-{run_id}"
    else:
        log_dir = report_output.parent / f"{report_output.stem}.logs-{run_id}"
    report.output = report_output
    report.log_dir = log_dir
    report.data = (
        {
            "schema_version": 1,
            "status": "running",
            "incomplete": True,
            "run_id": run_id,
            "started_at": utc_now(),
            "finished_at": None,
            "engine": {"name": args.engine, "version": None, "architecture": None, "host_architecture": platform.machine()},
            "command_timeout_seconds": args.timeout,
            "logs_dir": str(log_dir),
            "images_retained_by_default": True,
            "images_retained": not args.cleanup_images,
            "cache_semantics": {
                "cold": "--no-cache disables reusable image layers; registry/OS caches and cache mounts are not globally cleared",
                "warm": "cache-enabled build after cold; cache reuse is engine-local and may include preexisting layers",
                "candidate_namespace": "one unique value per runner invocation, reused for candidate cold/warm/probe scenarios and passed as HOORIFIC_CACHE_NAMESPACE",
                "baseline_namespace": "no HOORIFIC_CACHE_NAMESPACE argument is passed to baseline; baseline follows Dockerfile defaults and is not globally pruned",
                "namespace_limit": "HOORIFIC_CACHE_NAMESPACE is an annotation unless the supplied Dockerfile declares and uses it for cache mounts; the report records declaration status for each input",
                "comparison": "baseline and candidate use separate temporary contexts and are run serially; every scenario copies identical source bytes (including the same temporary probe mutation), while only Dockerfile/.dockerignore are intended to differ",
            },
            "base_image_policy": {
                "pull": "never",
                "preexisting_required": True,
                "meaning": "cold builds do not download base images; each FROM image is inspected before builds and a missing base is reported/build fails",
            },
            "inputs": {},
            "scenarios": [],
            "commands": [],
            "warnings": [],
            "failures": [],
        }
    )
    report.flush()
    commands = CommandRunner(report, args.engine, args.timeout)

    report.data["inputs"] = {
        "source_root": str(source_root),
        "baseline": {
            "dockerfile": str(baseline_dockerfile),
            "dockerfile_sha256": sha256_bytes(baseline_bytes),
            "dockerignore": str(baseline_ignore) if baseline_ignore else None,
            "dockerignore_sha256": sha256_bytes(baseline_ignore_bytes) if baseline_ignore_bytes is not None else None,
            "cache_namespace_arg_declared": declares_cache_namespace(baseline_text),
        },
        "candidate": {
            "dockerfile": str(candidate_dockerfile),
            "dockerfile_sha256": sha256_bytes(candidate_bytes),
            "dockerignore": str(candidate_ignore) if candidate_ignore else None,
            "dockerignore_sha256": sha256_bytes(candidate_ignore_bytes) if candidate_ignore_bytes is not None else None,
            "cache_namespace_arg_declared": declares_cache_namespace(candidate_text),
        },
        "safe_context_policy": {
            "included_root_dirs": list(DEFAULT_SOURCE_DIRS),
            "included_root_manifests": list(DEFAULT_ROOT_FILES),
            "excluded_components": sorted(EXCLUDED_COMPONENTS),
            "excluded_private_patterns": [".env*", "credential files", "private-key/certificate suffixes", "symlinks"],
        },
    }
    report.flush()

    version_record, version_output = commands.run(engine_command(args.engine, "--version"), "engine-version")
    report.data["engine"]["version"] = version_output.decode("utf-8", errors="replace").strip() or None
    report.data["engine"]["version_command"] = command_summary(version_record)
    if not version_record["ok"]:
        report.warning("engine --version failed; version field is incomplete")

    architecture_format = "{{.Host.Arch}}" if args.engine == "podman" else "{{.Architecture}}"
    architecture_record, architecture_output = commands.run(
        engine_command(args.engine, "info", "--format", architecture_format),
        "engine-architecture",
    )
    architecture = architecture_output.decode("utf-8", errors="replace").strip()
    report.data["engine"]["architecture"] = architecture or None
    report.data["engine"]["architecture_command"] = command_summary(architecture_record)
    if not architecture_record["ok"] or not architecture:
        report.warning("engine architecture query failed; host_architecture is recorded separately and is not an engine claim")

    baseline_bases = base_image_checks(commands, args.engine, baseline_text)
    candidate_bases = base_image_checks(commands, args.engine, candidate_text)
    report.data["base_images"] = {"baseline": baseline_bases, "candidate": candidate_bases}
    report.data["base_image_policy"]["all_checked_preexisting"] = all(
        item["preexisting"] for rows in (baseline_bases, candidate_bases) for item in rows
    )
    report.flush()

    candidate_namespace = f"hoorific-bench-{run_id}-candidate"
    baseline_namespace = None
    with tempfile.TemporaryDirectory(prefix="hoorific-image-build-") as temporary_name:
        temporary_root = Path(temporary_name)
        report.data["temporary_context_root"] = str(temporary_root)
        report.data["temporary_contexts_cleaned"] = True
        report.flush()
        all_dockerfiles = (baseline_text, candidate_text)

        mutation_files: dict[str, Path | None] = {}
        for specification in SCENARIOS:
            mutation = specification["mutation"]
            mutation_files[specification["id"]] = choose_mutation_file(source_root, mutation) if mutation else None

        for specification in SCENARIOS:
            scenario_id = specification["id"]
            mutation = specification["mutation"]
            scenario: dict[str, Any] = {
                "id": scenario_id,
                "label": specification["label"],
                "status": "running",
                "no_layer_cache": bool(specification["no_cache"]),
                "source_mutation": None,
                "contexts": {},
                "builds": {},
            }
            target = mutation_files[scenario_id]
            if mutation:
                if target is None:
                    scenario["status"] = "failed"
                    scenario["error"] = f"no source file found for {mutation} scenario"
                    report.data["scenarios"].append(scenario)
                    report.failure(f"{scenario_id}: {scenario['error']}")
                    continue
                relative_target = target.relative_to(source_root)
                scenario["source_mutation"] = {
                    "kind": mutation,
                    "path": relative_target.as_posix(),
                    "marker": MUTATION_MARKERS[mutation],
                    "applied_to": ["baseline", "candidate"],
                    "applied_only_in_temporary_contexts": True,
                }
            else:
                relative_target = None

            scenario_root = temporary_root / scenario_id
            contexts: dict[str, dict[str, Any]] = {}
            try:
                contexts["baseline"] = make_context(
                    source_root,
                    scenario_root / "baseline",
                    baseline_bytes,
                    baseline_text,
                    baseline_ignore_bytes,
                    all_dockerfiles,
                    mutation,
                    relative_target,
                )
                contexts["candidate"] = make_context(
                    source_root,
                    scenario_root / "candidate",
                    candidate_bytes,
                    candidate_text,
                    candidate_ignore_bytes,
                    all_dockerfiles,
                    mutation,
                    relative_target,
                )
            except (OSError, RuntimeError) as exc:
                scenario["status"] = "failed"
                scenario["error"] = f"could not create isolated contexts: {exc}"
                report.data["scenarios"].append(scenario)
                report.failure(f"{scenario_id}: {scenario['error']}")
                continue

            scenario["contexts"] = contexts
            scenario["source_hashes_match"] = contexts["baseline"]["source_hash"] == contexts["candidate"]["source_hash"]
            if not scenario["source_hashes_match"]:
                scenario["status"] = "failed"
                scenario["error"] = "baseline and candidate source context hashes differ"
                report.data["scenarios"].append(scenario)
                report.failure(f"{scenario_id}: source contexts are not identical")
                continue

            scenario["builds"]["baseline"] = measure_variant(
                commands,
                report,
                args.engine,
                contexts["baseline"],
                specification,
                "baseline",
                f"hoorific-bench-{run_id}-{scenario_id}-baseline:run",
                baseline_namespace,
                temporary_root,
                args.skip_oci_export,
                args.cleanup_images,
            )
            scenario["builds"]["candidate"] = measure_variant(
                commands,
                report,
                args.engine,
                contexts["candidate"],
                specification,
                "candidate",
                f"hoorific-bench-{run_id}-{scenario_id}-candidate:run",
                candidate_namespace,
                temporary_root,
                args.skip_oci_export,
                args.cleanup_images,
            )
            statuses = [build.get("status") for build in scenario["builds"].values()]
            scenario["status"] = "succeeded" if statuses and all(status == "succeeded" for status in statuses) else "failed"
            report.data["scenarios"].append(scenario)
            report.flush()

    successful = bool(report.data["scenarios"]) and all(
        scenario.get("status") == "succeeded" for scenario in report.data["scenarios"]
    )
    report.data["finished_at"] = utc_now()
    report.data["incomplete"] = not successful
    report.data["status"] = "complete" if successful else "failed"
    report.flush()
    if report_output is None:
        print(json.dumps(report.data, indent=2, ensure_ascii=False))
    return 0 if successful else 1


def main(argv: Sequence[str] | None = None) -> int:
    repo_root = Path(__file__).resolve().parents[2]
    parser = parser_for(repo_root)
    args = parser.parse_args(argv)
    output = None if args.output == "-" else resolve_path(args.output, Path.cwd())
    run_id = f"preflight-{uuid.uuid4().hex[:10]}"
    if output is None:
        log_dir = repo_root / ".artifacts" / f"image-build-logs-{run_id}"
    else:
        log_dir = output.parent / f"{output.stem}.logs-{run_id}"
    report = Report(
        output,
        log_dir,
        {
            "schema_version": 1,
            "status": "incomplete",
            "incomplete": True,
            "run_id": run_id,
            "started_at": utc_now(),
            "finished_at": None,
            "commands": [],
            "scenarios": [],
            "warnings": [],
            "failures": [],
            "logs_dir": str(log_dir),
        },
    )
    report.flush()
    try:
        return run_measurement(args, repo_root, report)
    except (Exception, KeyboardInterrupt) as exc:
        report.failure(str(exc))
        report.data["error"] = str(exc)
        report.data["finished_at"] = utc_now()
        report.data["status"] = "incomplete"
        report.data["incomplete"] = True
        report.flush()
        if output is None:
            print(json.dumps(report.data, indent=2, ensure_ascii=False))
        else:
            print(f"image build measurement incomplete; report: {output}", file=sys.stderr)
        return 130 if isinstance(exc, KeyboardInterrupt) else 2


if __name__ == "__main__":
    raise SystemExit(main())
