#!/usr/bin/env python3
"""Regenerate public console WebPs from a fresh, verifier-owned local fixture."""
from __future__ import annotations

import argparse
import base64
import json
import os
import re
import signal
import subprocess
import sys
import tempfile
import time
from contextlib import contextmanager
from dataclasses import dataclass
from pathlib import Path
from typing import Any
from urllib.parse import urlsplit

from browser_runner import safe_error

REPO_ROOT = Path(__file__).resolve().parents[3]
CASE_TIMEOUT_MS = 20_000
ROUTE_TIMEOUT_MS = 15_000


@dataclass(frozen=True)
class Shot:
    filename: str
    width: int
    height: int


SHOTS = (
    Shot("console.webp", 1440, 1000),
    Shot("console-login.webp", 1365, 900),
    Shot("console-models.webp", 1440, 1000),
    Shot("console-model-editor.webp", 1440, 1000),
    Shot("console-playground.webp", 1440, 1000),
    Shot("console-mobile.webp", 390, 844),
)
SHOT_BY_NAME = {shot.filename: shot for shot in SHOTS}


class CaptureError(RuntimeError):
    pass


@contextmanager
def fixture(binary: Path, verifier: Path):
    # TMPDIR confines even a failure before the readiness banner to this owned
    # directory. Never retain or publish logs, cookies, keys, or the database.
    with tempfile.TemporaryDirectory(prefix="hoorific-doc-capture-") as directory:
        root = Path(directory).resolve()
        report = root / "report.json"
        log_path = root / "verifier.log"
        environment = {key: value for key, value in os.environ.items()
                       if not key.startswith("HOORIFIC_")}
        environment.update(TMPDIR=str(root), TZ="UTC", LANG="C.UTF-8", LC_ALL="C.UTF-8")
        with log_path.open("w+b") as log:
            process = subprocess.Popen(
                [str(verifier), "--binary", str(binary), "--mode", "standalone",
                 "--scenario", "docs-capture", "--keep-alive", "5m", "--output", str(report)],
                cwd=REPO_ROOT, env=environment, stdin=subprocess.DEVNULL,
                stdout=log, stderr=log, start_new_session=True,
            )
            try:
                deadline = time.monotonic() + 90
                endpoint = None
                while time.monotonic() < deadline:
                    if log_path.stat().st_size > 4 * 1024 * 1024:
                        raise CaptureError("verifier output exceeded 4 MiB")
                    text = log_path.read_text(errors="replace")
                    match = re.search(r"browser-proof endpoints ([^\n]+)\n", text)
                    if match:
                        endpoint = dict(re.findall(r"(\w+)=(\S+)", match[1]))
                        break
                    if process.poll() is not None:
                        raise CaptureError("verifier exited before fixture readiness")
                    time.sleep(0.1)
                if endpoint is None:
                    raise CaptureError("verifier fixture did not become ready within 90 seconds")
                console = endpoint.get("management", "")
                origin = urlsplit(console)
                if origin.scheme != "http" or origin.hostname != "127.0.0.1" or not origin.port:
                    raise CaptureError("fixture management origin is not local HTTP")
                storage = Path(endpoint.get("storage_state", "")).resolve()
                if not storage.is_relative_to(root) or not storage.is_file():
                    raise CaptureError("fixture storage state is outside the owned directory")
                if process.poll() is not None:
                    raise CaptureError("verifier exited before browser capture")
                yield console, storage
            finally:
                # The Go verifier allows 35 seconds for its child to stop.
                # Its private process group also covers startup failures where
                # no endpoint or storage state has been returned yet.
                try:
                    if process.poll() is None:
                        process.send_signal(signal.SIGINT)
                    process.wait(timeout=50)
                finally:
                    try:
                        os.killpg(process.pid, signal.SIGKILL)
                    except ProcessLookupError:
                        pass
                    process.wait(timeout=10)
            if process.returncode != 0:
                raise CaptureError(f"verifier exited with status {process.returncode}")
            result = json.loads(report.read_text())
            if result.get("failed") != 0:
                raise CaptureError("verifier fixture setup failed")


def _wait_fonts(page: Any) -> None:
    page.wait_for_function("() => document.fonts.status === 'loaded'", timeout=ROUTE_TIMEOUT_MS)


def _assert_route(page: Any, route: str) -> None:
    if urlsplit(page.url).path.rstrip("/") != route.rstrip("/"):
        raise CaptureError(f"expected browser route {route}")


def _capture(page: Any, converter: Any, stage_dir: Path, expected: Shot) -> dict[str, Any]:
    _wait_fonts(page)
    page.wait_for_function(
        "() => document.getAnimations().every(animation => animation.playState !== 'running')",
        timeout=ROUTE_TIMEOUT_MS,
    )
    # Playwright captures PNG/JPEG, not WebP. Chromium's canvas encoder converts
    # the real rendered pixels without an additional image-library dependency.
    png = page.screenshot(type="png", full_page=False, animations="disabled", caret="hide")
    source = "data:image/png;base64," + base64.b64encode(png).decode("ascii")
    encoded = converter.evaluate("""async (source) => {
      const image = new Image();
      image.src = source;
      await image.decode();
      const canvas = document.createElement('canvas');
      canvas.width = image.naturalWidth;
      canvas.height = image.naturalHeight;
      canvas.getContext('2d').drawImage(image, 0, 0);
      return canvas.toDataURL('image/webp', 0.92);
    }""", source)
    prefix = "data:image/webp;base64,"
    if not encoded.startswith(prefix):
        raise CaptureError("Chromium did not encode WebP")
    path = stage_dir / expected.filename
    path.write_bytes(base64.b64decode(encoded[len(prefix):], validate=True))
    # Read the saved file back and actually decode it, not just its extension.
    raw = path.read_bytes()
    if raw[:4] != b"RIFF" or raw[8:12] != b"WEBP":
        raise CaptureError(f"{expected.filename} is not WebP")
    dimensions = converter.evaluate("""async (source) => {
      const image = new Image();
      image.src = source;
      await image.decode();
      return [image.naturalWidth, image.naturalHeight];
    }""", prefix + base64.b64encode(raw).decode("ascii"))
    if dimensions != [expected.width, expected.height]:
        raise CaptureError(f"{expected.filename} has incorrect dimensions: {dimensions}")
    return {"file": expected.filename, "width": dimensions[0],
            "height": dimensions[1], "bytes": len(raw)}


def _capture_login(page: Any, converter: Any, stage_dir: Path, console: str) -> dict[str, Any]:
    page.goto(console + "/admin/", wait_until="domcontentloaded", timeout=CASE_TIMEOUT_MS)
    _assert_route(page, "/admin/")
    page.get_by_role("heading", name="Hoorific operations", exact=True).wait_for(
        state="visible", timeout=ROUTE_TIMEOUT_MS
    )
    _wait_fonts(page)
    bootstrap = page.get_by_label("Bootstrap code", exact=True)
    bootstrap.wait_for(state="visible", timeout=ROUTE_TIMEOUT_MS)
    if bootstrap.input_value() != "":
        raise CaptureError("unauthenticated login capture had a non-empty bootstrap field")
    page.get_by_role("button", name="Redeem code", exact=True).wait_for(
        state="visible", timeout=ROUTE_TIMEOUT_MS
    )
    page.get_by_role("link", name="Sign in with OIDC", exact=True).wait_for(
        state="visible", timeout=ROUTE_TIMEOUT_MS
    )
    if page.context.cookies():
        raise CaptureError("unauthenticated login context unexpectedly received cookies")
    return _capture(page, converter, stage_dir, SHOT_BY_NAME["console-login.webp"])


def _capture_authenticated(
    page: Any,
    converter: Any,
    stage_dir: Path,
    console: str,
    storage_state: Path,
) -> dict[str, dict[str, Any]]:
    # Importing here keeps this script's route logic aligned with the existing
    # BrowserUI without invoking its broad case inventory or cleanup mutations.
    from browser_runner import BrowserUI, wait_for_stream

    ui = BrowserUI(page, console, stage_dir)
    page.set_default_timeout(ROUTE_TIMEOUT_MS)
    page.set_default_navigation_timeout(CASE_TIMEOUT_MS)
    captured: dict[str, dict[str, Any]] = {}

    ui.goto("/admin/", "Operations overview")
    _assert_route(page, "/admin/")
    _wait_fonts(page)
    page.get_by_text("Start with a workflow", exact=True).wait_for(
        state="visible", timeout=ROUTE_TIMEOUT_MS
    )
    captured["console.webp"] = _capture(page, converter, stage_dir, SHOT_BY_NAME["console.webp"])

    ui.resource("models")
    _assert_route(page, "/admin/models")
    _wait_fonts(page)
    row = page.get_by_role("button", name="fixture-model", exact=True)
    row.wait_for(state="visible", timeout=ROUTE_TIMEOUT_MS)
    captured["console-models.webp"] = _capture(
        page, converter, stage_dir, SHOT_BY_NAME["console-models.webp"]
    )

    row.click()
    page.locator("form.editor").first.wait_for(state="visible", timeout=ROUTE_TIMEOUT_MS)
    page.get_by_role("heading", name="Edit model", exact=True).wait_for(
        state="visible", timeout=ROUTE_TIMEOUT_MS
    )
    _wait_fonts(page)
    page.get_by_label("Connection ID", exact=True).wait_for(
        state="visible", timeout=ROUTE_TIMEOUT_MS
    )
    captured["console-model-editor.webp"] = _capture(
        page, converter, stage_dir, SHOT_BY_NAME["console-model-editor.webp"]
    )

    ui.goto("/admin/playground", "Inference playground")
    _assert_route(page, "/admin/playground")
    _wait_fonts(page)
    operation = page.get_by_label("Operation", exact=True)
    operation.select_option("chat")
    payload = {
        "model": "assistant",
        "messages": [{"role": "user", "content": "Reply exactly OK"}],
        "stream": True,
        "stream_options": {"include_usage": True},
    }
    page.locator("textarea.play-editor").first.fill(json.dumps(payload, indent=2))
    path = page.get_by_label("Gateway operation path", exact=True)
    if path.input_value() != "/playground/v1/chat/completions":
        path.fill("/playground/v1/chat/completions")
    pre = page.locator(".stream-output pre").first
    send = page.get_by_role("button", name="Send request", exact=True)
    send.wait_for(state="visible", timeout=ROUTE_TIMEOUT_MS)
    if not send.is_enabled():
        raise CaptureError("playground Send request control was disabled")
    response = ui.expect_suffix_response(
        "POST",
        "/admin/api/v1/playground/v1/chat/completions",
        lambda: send.click(),
        read_body=False,
    )
    if response.get("status") != 200:
        raise CaptureError(f"playground stream returned HTTP {response.get('status')}")
    raw_stream, parsed = wait_for_stream(pre, timeout_seconds=CASE_TIMEOUT_MS / 1000)
    if parsed.get("text") != "OK":
        raise CaptureError("playground fixture stream did not produce the expected OK text")
    if not parsed.get("done") or parsed.get("finish_reason") is None:
        raise CaptureError("playground fixture stream omitted terminal completion evidence")
    if parsed.get("usage_total") != 11:
        raise CaptureError("playground fixture stream did not report usage total 11")
    if "[DONE]" not in raw_stream:
        raise CaptureError("playground fixture stream omitted the [DONE] marker")
    page.get_by_text("Completed", exact=True).wait_for(
        state="visible", timeout=ROUTE_TIMEOUT_MS
    )
    pre.evaluate("(element) => { element.scrollTop = element.scrollHeight; }")
    captured["console-playground.webp"] = _capture(
        page, converter, stage_dir, SHOT_BY_NAME["console-playground.webp"]
    )

    page.set_viewport_size({"width": 390, "height": 844})
    ui.goto("/admin/models", "Models")
    _assert_route(page, "/admin/models")
    _wait_fonts(page)
    page.get_by_role("button", name="fixture-model", exact=True).wait_for(
        state="visible", timeout=ROUTE_TIMEOUT_MS
    )
    menu = page.get_by_role("button", name="Open navigation", exact=True)
    menu.wait_for(state="visible", timeout=ROUTE_TIMEOUT_MS)
    menu.click()
    page.wait_for_function(
        "() => document.querySelector('#operations-sidebar')?.classList.contains('translate-x-0')",
        timeout=ROUTE_TIMEOUT_MS,
    )
    navigation = page.get_by_role("navigation", name="Operations", exact=True)
    navigation.wait_for(state="visible", timeout=ROUTE_TIMEOUT_MS)
    models_link = navigation.get_by_role("link", name="Models", exact=True)
    models_link.wait_for(state="visible", timeout=ROUTE_TIMEOUT_MS)
    if models_link.get_attribute("aria-current") != "page":
        raise CaptureError("mobile navigation did not mark Models as the active route")
    _wait_fonts(page)
    captured["console-mobile.webp"] = _capture(
        page, converter, stage_dir, SHOT_BY_NAME["console-mobile.webp"]
    )
    return captured


def run(args: argparse.Namespace) -> list[dict[str, Any]]:
    from playwright.sync_api import sync_playwright

    binary, verifier = Path(args.binary).resolve(), Path(args.verifier).resolve()
    for executable in (binary, verifier):
        if not executable.is_file() or not os.access(executable, os.X_OK):
            raise CaptureError(f"missing executable: {executable}")
    output = Path(args.output_dir).absolute()
    if output.is_symlink():
        raise CaptureError("output directory must not be a symlink")
    output.mkdir(parents=True, exist_ok=True)
    for shot in SHOTS:
        target = output / shot.filename
        if target.is_symlink() or (target.exists() and not target.is_file()):
            raise CaptureError(f"output must be a regular file: {target}")

    # Same-filesystem staging allows atomic per-file replacement, only after
    # all six captures, file read-backs and verifier shutdown have succeeded.
    with tempfile.TemporaryDirectory(prefix=".capture-", dir=output) as directory:
        stage = Path(directory)
        with fixture(binary, verifier) as (console, storage):
            with sync_playwright() as playwright:
                browser = playwright.chromium.launch(headless=True)
                try:
                    options = dict(device_scale_factor=1, color_scheme="light",
                                   reduced_motion="reduce", locale="en-US", timezone_id="UTC")
                    login = browser.new_context(viewport={"width": 1365, "height": 900}, **options)
                    login_page = login.new_page()
                    converter = login.new_page()
                    results = [_capture_login(login_page, converter, stage, console)]
                    login.close()
                    context = browser.new_context(storage_state=str(storage),
                                                  viewport={"width": 1440, "height": 1000}, **options)
                    captured = _capture_authenticated(context.new_page(), context.new_page(),
                                                       stage, console, storage)
                    results.extend(captured.values())
                    context.close()
                finally:
                    browser.close()
        for shot in SHOTS:
            path = stage / shot.filename
            path.chmod(0o644)
            os.replace(path, output / shot.filename)
    return results


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--binary", required=True, help="built release gateway")
    parser.add_argument("--verifier", required=True, help="built tools/verify executable")
    parser.add_argument("--output-dir", required=True, help="public WebP output directory")
    args = parser.parse_args()

    def interrupted(_signal: int, _frame: Any) -> None:
        raise KeyboardInterrupt

    signal.signal(signal.SIGTERM, interrupted)
    try:
        results = run(args)
    except KeyboardInterrupt:
        print("capture_docs: interrupted", file=sys.stderr)
        return 130
    except Exception as exc:
        print(f"capture_docs: {safe_error(exc)}", file=sys.stderr)
        return 1
    print(json.dumps({"status": "passed", "outputs": results}, indent=2))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
