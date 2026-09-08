#!/usr/bin/env python3
"""Exercise the authenticated Hoorific operations console with real Chromium.

The browser proof is deliberately a real-process qualification run.  It uses the
storage state emitted by ``tools/verify`` and talks to the running management
listener; it does not replace the gateway with a mock or contact an upstream
provider.  Every case records a bounded, redacted result and a screenshot, while
independent cases continue after a failure so that one broken route does not hide
the remaining console surface.
"""
from __future__ import annotations

import json
import os
import re
import sys
import time
from pathlib import Path
from typing import Any, Callable
from urllib.parse import quote, urlparse


CASE_TIMEOUT_MS = 20_000
ROUTE_TIMEOUT_MS = 15_000

RESOURCE_LABELS = {
    "tenants": "Tenants",
    "operators": "Operators",
    "role_bindings": "Role bindings",
    "connections": "Connections",
    "credentials": "Credentials",
    "api_keys": "API keys",
    "account_pools": "Account pools",
    "oauth_sessions": "OAuth sessions",
    "models": "Models",
    "model_aliases": "Aliases",
    "route_policies": "Routes",
    "policy_limits": "Limits",
    "usage_ledger": "Usage",
    "audit_events": "Audit",
    "upstream_operations": "Jobs",
    "admissions": "Admissions",
    "reconciliations": "Reconciliations",
}
RESOURCE_ROUTES = tuple(RESOURCE_LABELS)
WRITABLE_KINDS = (
    "tenants",
    "operators",
    "role_bindings",
    "connections",
    "account_pools",
    "models",
    "model_aliases",
    "route_policies",
    "policy_limits",
)


def parse_sse(raw: str) -> dict[str, object]:
    """Decode SSE events and return semantic text/terminal/usage evidence.

    The console displays the raw stream, so looking for a literal ``OK`` would
    miss whether the gateway emitted deltas and a terminal event.  Parse the
    OpenAI-compatible shape used by the fixture and the Anthropic/native shapes
    that can also pass through the playground.
    """

    text_parts: list[str] = []
    saw_done = False
    finish_reason: str | None = None
    usage: dict[str, object] | None = None
    normalized = raw.replace("\r\n", "\n").replace("\r", "\n")
    for event in normalized.split("\n\n"):
        data_lines = [
            line[5:].lstrip()
            for line in event.splitlines()
            if line.startswith("data:")
        ]
        if not data_lines:
            continue
        payload = "\n".join(data_lines).strip()
        if payload == "[DONE]":
            saw_done = True
            continue
        try:
            message = json.loads(payload)
        except json.JSONDecodeError:
            continue
        if not isinstance(message, dict):
            continue
        if message.get("type") in {"message_stop", "response.completed"}:
            saw_done = True
        candidate_usage = message.get("usage")
        if isinstance(candidate_usage, dict):
            usage = candidate_usage
        choices = message.get("choices")
        if isinstance(choices, list):
            for choice in choices:
                if not isinstance(choice, dict):
                    continue
                delta = choice.get("delta")
                if isinstance(delta, dict):
                    content = delta.get("content")
                    if isinstance(content, str):
                        text_parts.append(content)
                reason = choice.get("finish_reason")
                if isinstance(reason, str) and reason:
                    finish_reason = reason
        delta = message.get("delta")
        if isinstance(delta, dict):
            text = delta.get("text")
            if isinstance(text, str):
                text_parts.append(text)
            reason = delta.get("stop_reason")
            if isinstance(reason, str) and reason:
                finish_reason = reason
        output_delta = message.get("output_text")
        if isinstance(output_delta, str):
            text_parts.append(output_delta)
        response_delta = message.get("response")
        if isinstance(response_delta, dict):
            status = response_delta.get("status")
            if status in {"completed", "failed", "cancelled"}:
                saw_done = True
        if message.get("type") == "message_delta":
            message_delta = message.get("delta")
            if isinstance(message_delta, dict):
                reason = message_delta.get("stop_reason")
                if isinstance(reason, str) and reason:
                    finish_reason = reason
    usage_total: int | None = None
    if usage is not None:
        total = usage.get("total_tokens")
        if isinstance(total, int) and total >= 0:
            usage_total = total
        else:
            prompt = usage.get("prompt_tokens", usage.get("input_tokens"))
            completion = usage.get("completion_tokens", usage.get("output_tokens"))
            if (
                isinstance(prompt, int)
                and prompt >= 0
                and isinstance(completion, int)
                and completion >= 0
            ):
                usage_total = prompt + completion
    return {
        "text": "".join(text_parts),
        "done": saw_done,
        "finish_reason": finish_reason,
        "usage": usage,
        "usage_total": usage_total,
        "complete": saw_done and finish_reason is not None and usage_total is not None,
    }


def wait_for_stream(pre: object, timeout_seconds: float = 20.0) -> tuple[str, dict[str, object]]:
    """Wait for semantic terminal evidence without logging raw response content."""

    deadline = time.monotonic() + timeout_seconds
    raw = ""
    parsed = parse_sse(raw)
    while time.monotonic() < deadline:
        raw = pre.inner_text()  # type: ignore[attr-defined]
        parsed = parse_sse(raw)
        if parsed["complete"]:
            return raw, parsed
        time.sleep(0.1)
    summary = {
        "text_length": len(str(parsed.get("text", ""))),
        "done": parsed.get("done"),
        "finish_reason": parsed.get("finish_reason"),
        "usage_total": parsed.get("usage_total"),
    }
    raise RuntimeError(f"real console stream did not reach semantic terminal: {summary}")


def _sensitive_key(key: str) -> bool:
    lowered = key.lower()
    exact = {
        "token",
        "secret",
        "password",
        "access_token",
        "refresh_token",
        "authorization_url",
        "authorization_url_complete",
        "verifier",
        "private_key",
        "cookie",
        "csrf",
        "csrf_token",
        "credential_material",
        "flow_id",
        "user_code",
        "device_code",
        "authorization_code",
        "oauth_state",
    }
    return (
        lowered in exact
        or lowered.endswith("_token")
        or lowered.endswith("_secret")
        or lowered.endswith("_password")
    )


def redact(value: Any, key: str = "") -> Any:
    """Redact secret-bearing fields before they reach evidence or stdout."""

    if _sensitive_key(key):
        return "[redacted]"
    if isinstance(value, dict):
        return {str(k): redact(v, str(k)) for k, v in value.items()}
    if isinstance(value, list):
        return [redact(item, key) for item in value]
    if isinstance(value, tuple):
        return [redact(item, key) for item in value]
    if isinstance(value, str):
        # Covers one-time tokens accidentally returned under an unexpected field.
        if re.fullmatch(r"(?:Bearer )?[A-Za-z0-9+/=_-]{40,}", value):
            return "[redacted]"
        return value[:2000]
    return value


def safe_error(exc: BaseException) -> str:
    text = str(exc)
    text = re.sub(
        r"(?i)(secret|password|access[_-]?token|refresh[_-]?token|csrf[_-]?token)"
        r"([\"'\s:=]+)[^,\s}\"']+",
        r"\1=[redacted]",
        text,
    )
    return text[:800]


def safe_response_body(response: object) -> tuple[Any, bool]:
    """Read a bounded JSON action response and report token presence only."""

    try:
        raw = response.text()  # type: ignore[attr-defined]
    except Exception:
        return {"bytes": 0}, False
    if len(raw) > 1_000_000:
        raw = raw[:1_000_000]
    try:
        parsed: Any = json.loads(raw) if raw else None
    except json.JSONDecodeError:
        return {"bytes": len(raw)}, False
    token_present = isinstance(parsed, dict) and bool(parsed.get("token"))
    return redact(parsed), token_present


def _json_write(path: Path, value: Any) -> None:
    path.write_text(json.dumps(value, indent=2, sort_keys=True) + "\n", encoding="utf-8")
    try:
        os.chmod(path, 0o600)
    except OSError:
        pass


def _slug(name: str) -> str:
    return re.sub(r"[^A-Za-z0-9_.-]+", "-", name).strip("-") or "case"


class BrowserUI:
    """Small wrapper around actual Playwright page actions and API observations."""

    def __init__(self, page: Any, console: str, evidence_dir: Path) -> None:
        self.page = page
        self.console = console.rstrip("/")
        self.evidence_dir = evidence_dir
        self.created: list[tuple[str, str]] = []
        self.issued_keys: list[tuple[str, int]] = []
        self.cleanup_errors: list[str] = []
        self.uncovered: list[str] = []

    @staticmethod
    def _admin_path(path: str) -> str:
        return "/admin/api/v1" + (path if path.startswith("/") else "/" + path)

    def wait_shell(self) -> None:
        self.page.wait_for_selector(".layout", state="visible", timeout=ROUTE_TIMEOUT_MS)
        self.page.wait_for_selector(
            'nav[aria-label="Operations"]', state="visible", timeout=ROUTE_TIMEOUT_MS
        )

    def goto(self, route: str, heading: str | None = None) -> None:
        self.page.goto(
            self.console + route,
            wait_until="domcontentloaded",
            timeout=CASE_TIMEOUT_MS,
        )
        self.wait_shell()
        if heading is not None:
            self.page.get_by_role("heading", name=heading, exact=True).wait_for(
                state="visible", timeout=ROUTE_TIMEOUT_MS
            )

    def resource(self, kind: str) -> Any:
        self.goto("/admin/" + kind, RESOURCE_LABELS[kind])
        section = self.page.locator("section.resource").first
        section.wait_for(state="visible", timeout=ROUTE_TIMEOUT_MS)
        self.wait_collection()
        return section
    def wait_collection(self) -> None:
        self.page.wait_for_function(
            """() => {
                const button = document.querySelector(
                    'section.resource .section-head button'
                );
                const error = document.querySelector(
                    'section.resource .notice.error,section.resource [role="alert"]'
                );
                return Boolean(
                    (button && !button.disabled) ||
                    (error && error.textContent && error.textContent.trim())
                );
            }""",
            timeout=ROUTE_TIMEOUT_MS,
        )

    def new_resource(self) -> Any:
        if self.page.locator("form.editor").count():
            raise RuntimeError("resource list unexpectedly opened an editor")
        self.page.get_by_role("button", name="New resource", exact=True).click()
        return self.editor()

    def editor(self) -> Any:
        editor = self.page.locator("form.editor").first
        editor.wait_for(state="visible", timeout=ROUTE_TIMEOUT_MS)
        return editor

    def table(self) -> Any:
        table = self.page.locator("section.resource table").first
        table.wait_for(state="visible", timeout=ROUTE_TIMEOUT_MS)
        self.wait_collection()
        return table

    def select_resource(self, resource_id: str) -> None:
        row = self.table().get_by_role("button", name=resource_id, exact=True)
        row.wait_for(state="visible", timeout=ROUTE_TIMEOUT_MS)
        row.click()

    def panel(self, heading: str) -> Any:
        panel = self.page.locator("aside.actions").filter(has_text=heading).first
        panel.wait_for(state="visible", timeout=ROUTE_TIMEOUT_MS)
        return panel

    def wait_text(self, text: str) -> None:
        self.page.get_by_text(text, exact=True).wait_for(
            state="visible", timeout=ROUTE_TIMEOUT_MS
        )

    def _response_matches(self, response: Any, method: str, path: str) -> bool:
        expected = self._admin_path(path).rstrip("/")
        actual = urlparse(response.url).path.rstrip("/")
        return response.request.method.upper() == method.upper() and actual == expected

    def expect_response(
        self,
        method: str,
        path: str,
        action: Callable[[], Any],
        *,
        read_body: bool = True,
    ) -> dict[str, Any]:
        with self.page.expect_response(
            lambda response: self._response_matches(response, method, path),
            timeout=CASE_TIMEOUT_MS,
        ) as pending:
            action()
        response = pending.value
        observed: dict[str, Any] = {
            "method": method.upper(),
            "path": urlparse(response.url).path,
            "status": response.status,
        }
        if read_body:
            body, token_present = safe_response_body(response)
            observed["body"] = body
            if token_present:
                observed["token_present"] = True
        return observed

    def expect_suffix_response(
        self,
        method: str,
        suffix: str,
        action: Callable[[], Any],
        *,
        read_body: bool = True,
    ) -> dict[str, Any]:
        with self.page.expect_response(
            lambda response: response.request.method.upper() == method.upper()
            and urlparse(response.url).path.endswith(suffix),
            timeout=CASE_TIMEOUT_MS,
        ) as pending:
            action()
        response = pending.value
        observed: dict[str, Any] = {
            "method": method.upper(),
            "path": urlparse(response.url).path,
            "status": response.status,
        }
        if read_body:
            body, token_present = safe_response_body(response)
            observed["body"] = body
            if token_present:
                observed["token_present"] = True
        return observed

    def admin_fetch(
        self,
        method: str,
        path: str,
        body: Any = None,
        *,
        version: int | None = None,
        include_csrf: bool = True,
        expected_tenant: str | None = None,
    ) -> dict[str, Any]:
        """Make a bounded authenticated management request from the real page.

        This is used only for security probes and cleanup, where no UI action is
        intended.  UI-owned mutations continue to use visible controls above.
        """

        result = self.page.evaluate(
            """
            async ({method, path, body, version, includeCsrf, expectedTenant}) => {
              const sessionResponse = await fetch('/admin/api/v1/session', {
                credentials: 'include',
                headers: {Accept: 'application/json'}
              });
              let session = {};
              try { session = await sessionResponse.json(); } catch {}
              const headers = {Accept: 'application/json'};
              if (body !== null && body !== undefined) headers['Content-Type'] = 'application/json';
              if (includeCsrf && session.csrf_token) {
                headers['X-CSRF-Token'] = session.csrf_token;
                headers['Origin'] = window.location.origin;
              }
              if (expectedTenant) headers['X-Hoorific-Expected-Tenant'] = expectedTenant;
              if (version !== null && version !== undefined) headers['If-Match'] = `"${version}"`;
              const init = {method, credentials: 'include', headers};
              if (body !== null && body !== undefined) init.body = JSON.stringify(body);
              const response = await fetch(path, init);
              const text = await response.text();
              let parsed = text;
              try { parsed = text ? JSON.parse(text) : null; } catch {}
              return {status: response.status, body: parsed};
            }
            """,
            {
                "method": method.upper(),
                "path": path,
                "body": body,
                "version": version,
                "includeCsrf": include_csrf,
                "expectedTenant": expected_tenant,
            },
        )
        return {
            "status": int(result.get("status", 0)),
            "body": redact(result.get("body")),
        }


    def track(self, kind: str, resource_id: str) -> None:
        entry = (kind, resource_id)
        if entry not in self.created:
            self.created.append(entry)

    def untrack(self, kind: str, resource_id: str) -> None:
        try:
            self.created.remove((kind, resource_id))
        except ValueError:
            pass
    def track_api_key(self, resource_id: str, version: int) -> None:
        entry = (resource_id, version)
        if not any(existing_id == resource_id for existing_id, _ in self.issued_keys):
            self.issued_keys.append(entry)

    def update_api_key_version(self, resource_id: str, version: int) -> None:
        for index, (existing_id, _) in enumerate(self.issued_keys):
            if existing_id == resource_id:
                self.issued_keys[index] = (resource_id, version)
                return
        self.track_api_key(resource_id, version)

    def untrack_api_key(self, resource_id: str) -> None:
        self.issued_keys = [
            (existing_id, version)
            for existing_id, version in self.issued_keys
            if existing_id != resource_id
        ]

    def safe_clean_ui(self) -> None:
        """Remove transient results and sensitive form values before screenshots."""

        try:
            if self.page.locator("textarea.play-editor").count() > 0:
                # Reload clears output and any caller-supplied payload from the DOM.
                self.page.reload(wait_until="domcontentloaded", timeout=CASE_TIMEOUT_MS)
                self.wait_shell()
            else:
                for _ in range(8):
                    dismiss = self.page.locator('button[aria-label^="Dismiss"]')
                    if dismiss.count() == 0:
                        break
                    try:
                        dismiss.last.click(timeout=1_000)
                    except Exception:
                        break
            for index in range(self.page.locator('input[type="password"]').count()):
                try:
                    self.page.locator('input[type="password"]').nth(index).fill("")
                except Exception:
                    pass
        except Exception:
            # A failed case must still get a best-effort screenshot.
            pass

    def screenshot(self, path: Path) -> None:
        self.safe_clean_ui()
        self.page.screenshot(path=str(path), full_page=True)
        try:
            os.chmod(path, 0o600)
        except OSError:
            pass

    def cleanup(self) -> list[str]:
        """Remove UI-created resources in reverse dependency order.

        Tenants intentionally have no delete endpoint.  Disable those temporary
        tenants instead, which leaves no active fixture state and avoids claiming
        unsupported deletion as successful.
        """

        failures: list[str] = []
        for resource_id, _ in reversed(self.issued_keys):
            encoded = quote(resource_id, safe="")
            path = self._admin_path(f"/api_keys/{encoded}")
            try:
                fetched = self.admin_fetch("GET", path)
                if fetched["status"] == 404:
                    continue
                if fetched["status"] != 200 or not isinstance(fetched["body"], dict):
                    failures.append(f"api_keys/{resource_id}: cleanup GET {fetched['status']}")
                    continue
                version = fetched["body"].get("version")
                if not isinstance(version, int):
                    failures.append(f"api_keys/{resource_id}: cleanup response missing version")
                    continue
                revoked = self.admin_fetch(
                    "POST",
                    path + "/revoke",
                    {"data": {}},
                    version=version,
                )
                if revoked["status"] != 200:
                    failures.append(
                        f"api_keys/{resource_id}: cleanup revoke {revoked['status']}"
                    )
            except Exception as exc:
                failures.append(f"api_keys/{resource_id}: {safe_error(exc)}")

        for kind, resource_id in reversed(self.created):
            encoded = quote(resource_id, safe="")
            path = self._admin_path(f"/{kind}/{encoded}")
            try:
                fetched = self.admin_fetch("GET", path)
                if fetched["status"] == 404:
                    continue
                if fetched["status"] != 200 or not isinstance(fetched["body"], dict):
                    failures.append(f"{kind}/{resource_id}: cleanup GET {fetched['status']}")
                    continue
                body = fetched["body"]
                version = body.get("version")
                data = body.get("data")
                if not isinstance(version, int) or not isinstance(data, dict):
                    failures.append(f"{kind}/{resource_id}: cleanup response missing version/data")
                    continue
                if kind == "tenants":
                    disabled = dict(data)
                    disabled["enabled"] = False
                    result = self.admin_fetch(
                        "PUT", path, {"data": disabled}, version=version
                    )
                else:
                    result = self.admin_fetch("DELETE", path, version=version)
                if result["status"] not in (200, 204):
                    failures.append(
                        f"{kind}/{resource_id}: cleanup mutation {result['status']}"
                    )
            except Exception as exc:
                failures.append(f"{kind}/{resource_id}: {safe_error(exc)}")
        self.cleanup_errors.extend(failures)
        return self.cleanup_errors


class BrowserCases:
    def __init__(self, ui: BrowserUI, run_id: str) -> None:
        self.ui = ui
        self.run_id = run_id
        self.sequence = 0
        self.fixture_base_url = ""

    def uid(self, label: str) -> str:
        self.sequence += 1
        return f"browser-{label}-{self.run_id}-{self.sequence}"

    def add_list(self, editor: Any, label: str, values: list[str]) -> None:
        for index, value in enumerate(values, 1):
            editor.get_by_role(
                "button",
                name=f"Add {label.lower()} entry",
                exact=True,
            ).click()
            editor.get_by_label(f"{label} {index}", exact=True).fill(value)

    def set_json_map(self, editor: Any, label: str, value: dict[str, str]) -> None:
        editor.get_by_label(
            f"{label} (JSON object of string values)", exact=True
        ).fill(json.dumps(value))

    def create(
        self,
        kind: str,
        resource_id: str,
        fill: Callable[[Any], None],
    ) -> Any:
        self.ui.resource(kind)
        editor = self.ui.new_resource()
        editor.get_by_label("ID", exact=True).fill(resource_id)
        fill(editor)
        observed = self.ui.expect_response(
            "POST",
            f"/{kind}",
            lambda: editor.get_by_role("button", name="Create", exact=True).click(),
        )
        if observed["status"] != 200:
            raise RuntimeError(
                f"{kind} create returned {observed['status']}: "
                f"{redact(observed.get('body'))}"
            )
        self.ui.track(kind, resource_id)
        self.ui.wait_text("Created.")
        return editor

    def save(self, kind: str, editor: Any) -> dict[str, Any]:
        observed = self.ui.expect_response(
            "PUT",
            f"/{kind}/" + quote(self.selected_id, safe=""),
            lambda: editor.get_by_role(
                "button", name="Save changes", exact=True
            ).click(),
        )
        if observed["status"] != 200:
            raise RuntimeError(
                f"{kind} update returned {observed['status']}: "
                f"{redact(observed.get('body'))}"
            )
        self.ui.wait_text("Saved.")
        return observed

    def delete_selected(self, kind: str, resource_id: str) -> dict[str, Any]:
        editor = self.ui.editor()
        self.ui.page.once("dialog", lambda dialog: dialog.accept())
        observed = self.ui.expect_response(
            "DELETE",
            f"/{kind}/" + quote(resource_id, safe=""),
            lambda: editor.get_by_role("button", name="Delete", exact=True).click(),
        )
        if observed["status"] not in (200, 204):
            raise RuntimeError(
                f"{kind} delete returned {observed['status']}: "
                f"{redact(observed.get('body'))}"
            )
        self.ui.untrack(kind, resource_id)
        return observed

    def fixture_connection(self) -> str:
        self.ui.resource("connections")
        self.ui.select_resource("fixture-connection")
        base = self.ui.editor().get_by_label("Base URL", exact=True).input_value()
        if not base:
            raise RuntimeError("seeded fixture connection has no base URL")
        self.fixture_base_url = base
        return base

    def current_tenant_id(self) -> str:
        observed = self.ui.admin_fetch("GET", "/admin/api/v1/session")
        body = observed.get("body")
        principal = body.get("principal") if isinstance(body, dict) else None
        tenant_id = (
            principal.get("TenantID")
            if isinstance(principal, dict)
            else None
        )
        if not isinstance(tenant_id, str) or not tenant_id:
            raise RuntimeError("authenticated session omitted current tenant ID")
        return tenant_id

    def tenant_lifecycle(self) -> dict[str, Any]:
        original_tenant = self.current_tenant_id()
        resource_id = self.uid("tenant")
        editor = self.create(
            "tenants",
            resource_id,
            lambda form: (
                form.get_by_label("Tenant name", exact=True).fill(
                    "Browser lifecycle tenant"
                ),
                self.add_list(form, "Allowed browser origins", ["http://localhost"]),
            ),
        )
        editor.get_by_label("Tenant name", exact=True).fill(
            "Browser lifecycle tenant updated"
        )
        self.selected_id = resource_id
        self.save("tenants", editor)
        if editor.get_by_role("button", name="Delete", exact=True).count() != 0:
            raise RuntimeError("tenant delete control must not be offered")
        enabled = editor.get_by_label("Enabled", exact=True)
        if not enabled.is_checked():
            raise RuntimeError("new tenant was not enabled")
        enabled.uncheck()
        self.save("tenants", editor)
        fetched = self.ui.admin_fetch(
            "GET",
            f"/admin/api/v1/tenants/{quote(resource_id, safe='')}",
            expected_tenant=original_tenant,
        )
        body = fetched.get("body")
        data = body.get("data") if isinstance(body, dict) else None
        if (
            fetched["status"] != 200
            or not isinstance(data, dict)
            or data.get("enabled") is not False
        ):
            raise RuntimeError(
                f"tenant disable was not persisted: {fetched['status']} / "
                f"{redact(body)}"
            )
        return {
            "created": True,
            "updated": True,
            "disabled": True,
            "delete_offered": False,
            "retained_for_cleanup": True,
        }

    def operator_lifecycle(self) -> dict[str, Any]:
        resource_id = self.uid("operator")
        editor = self.create(
            "operators",
            resource_id,
            lambda form: (
                form.get_by_label("Operator ID (subject)", exact=True).fill(resource_id),
                form.get_by_label("Identity issuer", exact=True).fill(
                    "https://browser.example.test"
                ),
                form.get_by_label(
                    "Identity subject (issuer sub claim)", exact=True
                ).fill(resource_id + "-subject"),
                form.get_by_label("Display name", exact=True).fill(
                    "Browser lifecycle operator"
                ),
            ),
        )
        for label in (
            "Operator ID (subject)",
            "Identity issuer",
            "Identity subject (issuer sub claim)",
        ):
            if editor.get_by_label(label, exact=True).is_editable():
                raise RuntimeError(f"operator identity field remained editable: {label}")
        self.selected_id = resource_id
        editor.get_by_label("Display name", exact=True).fill(
            "Browser lifecycle operator updated"
        )
        self.save("operators", editor)
        self.delete_selected("operators", resource_id)
        return {"created": True, "updated": True, "deleted": True}

    def role_binding_lifecycle(self) -> dict[str, Any]:
        operator_id = self.uid("binding-operator")
        operator_editor = self.create(
            "operators",
            operator_id,
            lambda form: (
                form.get_by_label("Operator ID (subject)", exact=True).fill(operator_id),
                form.get_by_label("Identity issuer", exact=True).fill(
                    "https://browser.example.test"
                ),
                form.get_by_label(
                    "Identity subject (issuer sub claim)", exact=True
                ).fill(operator_id + "-subject"),
                form.get_by_label("Display name", exact=True).fill(
                    "Browser binding operator"
                ),
            ),
        )
        for label in (
            "Operator ID (subject)",
            "Identity issuer",
            "Identity subject (issuer sub claim)",
        ):
            if operator_editor.get_by_label(label, exact=True).is_editable():
                raise RuntimeError(f"operator identity field remained editable: {label}")
        del operator_editor
        self.ui.resource("role_bindings")
        self.ui.select_resource(operator_id)
        binding_editor = self.ui.editor()
        subject = binding_editor.get_by_label(
            "Operator subject (existing operator ID)", exact=True
        )
        if subject.is_editable() or subject.input_value() != operator_id:
            raise RuntimeError("role binding subject was not immutable existing operator ID")
        tenant_binding = binding_editor.get_by_label("Current tenant binding", exact=True)
        if tenant_binding.is_editable():
            raise RuntimeError("role binding tenant was not immutable")
        role_select = binding_editor.get_by_label("Role", exact=True)
        if role_select.input_value() != "viewer":
            raise RuntimeError("operator creation did not seed a viewer role binding")
        role_select.select_option("operator")
        self.selected_id = operator_id
        self.save("role_bindings", binding_editor)
        self.delete_selected("role_bindings", operator_id)
        # The operator is deliberately cleaned after its binding has gone away.
        self.ui.track("operators", operator_id)
        return {
            "created": True,
            "updated": True,
            "binding_deleted": True,
            "operator_cleanup_deferred": True,
        }



    def connection_lifecycle(self) -> dict[str, Any]:
        base = self.fixture_connection()
        resource_id = self.uid("connection")
        editor = self.create(
            "connections",
            resource_id,
            lambda form: (
                form.get_by_label("Connector", exact=True).fill("anthropic"),
                form.get_by_label("Account ID", exact=True).fill(
                    resource_id + "-account"
                ),
                form.get_by_label("Base URL", exact=True).fill(base),
                form.get_by_label("Region", exact=True).fill("browser-region"),
                form.get_by_label("Dedicated", exact=True).check(),
                self.set_json_map(
                    form,
                    "Connection settings",
                    {"allow_private": "true", "allowed_cidrs": "127.0.0.1/32"},
                ),
            ),
        )
        self.selected_id = resource_id
        editor.get_by_label("Region", exact=True).fill("browser-region-updated")
        self.save("connections", editor)
        actions = self.ui.panel("Connection actions")
        disabled = self.ui.expect_response(
            "POST",
            f"/connections/{quote(resource_id, safe='')}/disable",
            lambda: actions.get_by_role("button", name="Disable", exact=True).click(),
        )
        if disabled["status"] != 200:
            raise RuntimeError(f"connection disable returned {disabled['status']}")
        # Disable refreshes the resource version; reload and select it before delete.
        self.ui.resource("connections")
        self.ui.select_resource(resource_id)
        self.delete_selected("connections", resource_id)
        return {
            "created": True,
            "updated": True,
            "disabled_status": disabled["status"],
            "deleted": True,
        }

    def account_pool_lifecycle(self) -> dict[str, Any]:
        resource_id = self.uid("account-pool")
        editor = self.create(
            "account_pools",
            resource_id,
            lambda form: form.get_by_label("Provider", exact=True).fill("anthropic"),
        )
        self.selected_id = resource_id
        self.add_list(editor, "Account IDs", ["fixture-account"])
        self.save("account_pools", editor)
        self.delete_selected("account_pools", resource_id)
        return {"created": True, "updated": True, "deleted": True}

    def model_lifecycle(self) -> dict[str, Any]:
        resource_id = self.uid("model")
        cache_rates = {
            "Cache-read nanodollars per million tokens": "500000000",
            "Cache-write nanodollars per million tokens": "3000000000",
            "5-minute cache-write nanodollars per million tokens": "4000000000",
            "1-hour cache-write nanodollars per million tokens": "5000000000",
        }

        def fill_model(form: Any) -> None:
            form.get_by_label("Connection ID", exact=True).fill("fixture-connection")
            form.get_by_label("Upstream model ID", exact=True).fill(
                resource_id + "-upstream"
            )
            self.add_list(form, "Operations", ["generate"])
            self.add_list(form, "Input modalities", ["text"])
            self.add_list(form, "Output modalities", ["text"])
            self.set_json_map(form, "Features", {"streaming": "supported"})
            form.get_by_label("Provenance", exact=True).fill(
                "browser resource lifecycle"
            )
            form.get_by_label("Provide price schedule", exact=True).check()
            form.get_by_label("Price version", exact=True).fill("browser-price-v1")
            form.get_by_label(
                "Input nanodollars per million tokens", exact=True
            ).fill("1000000000")
            form.get_by_label(
                "Output nanodollars per million tokens", exact=True
            ).fill("2000000000")
            form.get_by_label(
                "Maximum nanodollars per operation unit", exact=True
            ).fill("1000000000000")
            for label, value in cache_rates.items():
                form.get_by_label(label, exact=True).fill(value)

        editor = self.create("models", resource_id, fill_model)
        self.selected_id = resource_id
        self.save("models", editor)

        self.ui.resource("models")
        self.ui.select_resource(resource_id)
        self.selected_id = resource_id
        reloaded = self.ui.editor()
        persisted_cache_rates = {
            label: reloaded.get_by_label(label, exact=True).input_value()
            for label in cache_rates
        }
        if persisted_cache_rates != cache_rates:
            raise RuntimeError(
                f"model cache rates did not survive save/reload: {persisted_cache_rates}"
            )
        reloaded.get_by_label("Upstream model ID", exact=True).fill(
            resource_id + "-upstream-updated"
        )
        self.save("models", reloaded)
        self.delete_selected("models", resource_id)
        return {
            "created": True,
            "updated": True,
            "deleted": True,
            "cache_rates_saved": True,
            "cache_rates_reloaded": persisted_cache_rates,
        }

    def alias_lifecycle(self) -> dict[str, Any]:
        resource_id = self.uid("alias")
        editor = self.create(
            "model_aliases",
            resource_id,
            lambda form: (
                self.add_list(form, "Model IDs", ["fixture-model"]),
                form.get_by_label("Description", exact=True).fill(
                    "Browser alias lifecycle"
                ),
            ),
        )
        self.selected_id = resource_id
        editor.get_by_label("Description", exact=True).fill(
            "Browser alias lifecycle updated"
        )
        self.save("model_aliases", editor)
        self.delete_selected("model_aliases", resource_id)
        return {"created": True, "updated": True, "deleted": True}

    def route_lifecycle(self) -> dict[str, Any]:
        alias_id = self.uid("route-alias")
        alias_editor = self.create(
            "model_aliases",
            alias_id,
            lambda form: self.add_list(form, "Model IDs", ["fixture-model"]),
        )
        del alias_editor
        route_id = self.uid("route")
        route_editor = self.create(
            "route_policies",
            route_id,
            lambda form: (
                form.get_by_label("Model alias", exact=True).fill(alias_id),
                form.get_by_role("button", name="Add target", exact=True).click(),
                form.get_by_label("Connection ID", exact=True).fill(
                    "fixture-connection"
                ),
                form.get_by_label("Model ID", exact=True).fill("fixture-model"),
                form.get_by_label("Priority", exact=True).fill("1"),
                form.get_by_label("Weight", exact=True).fill("100"),
            ),
        )
        self.selected_id = route_id
        route_editor.get_by_label("Allow fallback", exact=True).check()
        self.save("route_policies", route_editor)
        self.delete_selected("route_policies", route_id)
        self.ui.resource("model_aliases")
        self.ui.select_resource(alias_id)
        self.selected_id = alias_id
        self.delete_selected("model_aliases", alias_id)
        return {"alias_created": True, "route_created": True, "route_updated": True, "deleted": True}

    def policy_limit_lifecycle(self) -> dict[str, Any]:
        scope_id = self.current_tenant_id()
        resource_id = self.uid("policy")
        editor = self.create(
            "policy_limits",
            resource_id,
            lambda form: (
                form.get_by_label("Policy scope", exact=True).select_option("tenant"),
                form.get_by_label("Scope ID", exact=True).fill(scope_id),
                form.get_by_label("Requests per minute", exact=True).fill("10"),
                form.get_by_label("Cost window", exact=True).select_option("daily"),
            ),
        )
        self.selected_id = resource_id
        editor.get_by_label("Requests per minute", exact=True).fill("20")
        self.save("policy_limits", editor)
        self.delete_selected("policy_limits", resource_id)
        return {"created": True, "updated": True, "deleted": True}

    def stale_version_conflict(self) -> dict[str, Any]:
        scope_id = self.current_tenant_id()
        resource_id = self.uid("conflict-policy")
        editor = self.create(
            "policy_limits",
            resource_id,
            lambda form: (
                form.get_by_label("Policy scope", exact=True).select_option("tenant"),
                form.get_by_label("Scope ID", exact=True).fill(scope_id),
                form.get_by_label("Requests per minute", exact=True).fill("30"),
            ),
        )
        self.selected_id = resource_id
        path = "/admin/api/v1/policy_limits/" + quote(resource_id, safe="")
        external_data = {
            "scope": "tenant",
            "scope_id": scope_id,
            "requests_per_minute": 31,
            "tokens_per_minute": 0,
            "max_cost": 0,
            "concurrency": 0,
            "cost_window": "total",
            "outstanding_jobs": 0,
        }
        external = self.ui.admin_fetch(
            "PUT",
            path,
            {"data": external_data},
            version=1,
        )
        if external["status"] != 200:
            raise RuntimeError(f"external version bump returned {external['status']}")
        editor.get_by_label("Requests per minute", exact=True).fill("32")
        observed = self.ui.expect_response(
            "PUT",
            f"/policy_limits/{quote(resource_id, safe='')}",
            lambda: editor.get_by_role(
                "button", name="Save changes", exact=True
            ).click(),
        )
        if observed["status"] != 412:
            raise RuntimeError(f"stale edit returned {observed['status']}, expected 412")
        self.ui.wait_text("This resource changed on the server. Your draft is retained.")
        self.ui.page.get_by_role(
            "button", name="Reload server version", exact=True
        ).click()
        self.ui.wait_text("Reloaded server version.")
        self.delete_selected("policy_limits", resource_id)
        return {
            "external_update_status": external["status"],
            "stale_update_status": observed["status"],
            "conflict_editor_retained": True,
            "server_version_reloaded": True,
            "deleted": True,
        }
    def stale_tenant_context(self) -> dict[str, Any]:
        """Prove a stale tab cannot mutate after its shared cookie changes tenant."""

        original_tenant = self.current_tenant_id()
        target_tenant = self.uid("stale-tenant")
        resource_id = self.uid("stale-policy")
        resource_path = f"/admin/api/v1/policy_limits/{quote(resource_id, safe='')}"
        target_created = False
        page2 = None
        page2_ui: BrowserUI | None = None

        def session_tenant(ui: BrowserUI) -> str | None:
            # Compare the actual tenant ID, not evidence-redacted response data:
            # long resource IDs can resemble opaque secrets to the redactor.
            return ui.page.evaluate(
                "async () => (await (await fetch('/admin/api/v1/session', "
                "{credentials: 'include'})).json()).principal?.TenantID ?? null"
            )

        def remove_wrong_resource() -> None:
            if page2_ui is None:
                return
            found = page2_ui.admin_fetch(
                "GET",
                resource_path,
                expected_tenant=target_tenant,
            )
            if found["status"] == 404:
                return
            if found["status"] == 200 and isinstance(found.get("body"), dict):
                version = found["body"].get("version")
                if isinstance(version, int):
                    removed = page2_ui.admin_fetch(
                        "DELETE",
                        resource_path,
                        version=version,
                        expected_tenant=target_tenant,
                    )
                    if removed["status"] not in (200, 204):
                        raise RuntimeError(
                            f"wrong-tenant stale resource cleanup returned {removed['status']}"
                        )
            raise RuntimeError(
                f"stale submit left resource in switched tenant: {found['status']}"
            )

        try:
            created = self.ui.admin_fetch(
                "POST",
                "/admin/api/v1/tenants",
                {
                    "id": target_tenant,
                    "data": {
                        "name": "Browser stale context tenant",
                        "enabled": True,
                        "allowed_origins": ["http://localhost"],
                        "max_body_bytes": 0,
                        "max_event_bytes": 0,
                    },
                },
                expected_tenant=original_tenant,
            )
            if created["status"] != 200:
                raise RuntimeError(
                    f"stale-context tenant create returned {created['status']}: "
                    f"{redact(created.get('body'))}"
                )
            self.ui.track("tenants", target_tenant)
            target_created = True

            self.ui.resource("policy_limits")
            editor = self.ui.new_resource()
            editor.get_by_label("ID", exact=True).fill(resource_id)
            scope_id = editor.get_by_label("Scope ID", exact=True)
            if scope_id.input_value() != original_tenant:
                raise RuntimeError("stale draft did not load the original tenant scope")
            requests = editor.get_by_label("Requests per minute", exact=True)
            requests.fill("17")

            page2 = self.ui.page.context.new_page()
            page2.set_default_timeout(ROUTE_TIMEOUT_MS)
            page2_ui = BrowserUI(page2, self.ui.console, self.ui.evidence_dir)
            page2_ui.goto("/admin/", "Operations overview")
            if session_tenant(page2_ui) != original_tenant:
                raise RuntimeError("shared-cookie second page did not start in original tenant")
            switched = page2_ui.admin_fetch(
                "POST",
                "/admin/api/v1/session/tenant",
                {"tenant_id": target_tenant},
                expected_tenant=original_tenant,
            )
            if switched["status"] != 200 or session_tenant(page2_ui) != target_tenant:
                raise RuntimeError(
                    f"second page tenant switch returned {switched['status']} / "
                    f"{redact(switched.get('body'))}"
                )

            observed = self.ui.expect_response(
                "POST",
                "/policy_limits",
                lambda: editor.get_by_role("button", name="Create", exact=True).click(),
            )
            body = observed.get("body")
            code = body.get("code") if isinstance(body, dict) else None
            if observed["status"] != 409 or code != "tenant_context_changed":
                remove_wrong_resource()
                raise RuntimeError(
                    f"stale tenant submit returned {observed['status']} / {code}, "
                    "expected 409 / tenant_context_changed"
                )
            editor.wait_for(state="visible", timeout=ROUTE_TIMEOUT_MS)
            if (
                editor.get_by_label("ID", exact=True).input_value() != resource_id
                or requests.input_value() != "17"
                or scope_id.input_value() != original_tenant
            ):
                raise RuntimeError("stale tenant submit did not retain the original draft")
            remove_wrong_resource()

            restored = page2_ui.admin_fetch(
                "POST",
                "/admin/api/v1/session/tenant",
                {"tenant_id": original_tenant},
                expected_tenant=target_tenant,
            )
            if restored["status"] != 200 or session_tenant(page2_ui) != original_tenant:
                raise RuntimeError(
                    f"original tenant restoration returned {restored['status']} / "
                    f"{redact(restored.get('body'))}"
                )
            if session_tenant(self.ui) != original_tenant:
                raise RuntimeError("shared cookie did not restore original tenant")
            return {
                "tenant_create_status": created["status"],
                "switch_status": switched["status"],
                "stale_submit_status": observed["status"],
                "stale_submit_code": code,
                "wrong_tenant_resource_status": 404,
                "draft_retained": True,
                "original_tenant_restored": True,
                "owned_tenant_retained_for_cleanup": target_created,
            }
        finally:
            if page2_ui is not None and target_created:
                try:
                    if session_tenant(page2_ui) == target_tenant:
                        try:
                            remove_wrong_resource()
                        finally:
                            restore = page2_ui.admin_fetch(
                                "POST",
                                "/admin/api/v1/session/tenant",
                                {"tenant_id": original_tenant},
                                expected_tenant=target_tenant,
                            )
                            if restore["status"] != 200:
                                raise RuntimeError(
                                    f"cleanup tenant restoration returned {restore['status']}"
                                )
                except Exception as exc:
                    self.ui.cleanup_errors.append(
                        "stale tenant context restoration: " + safe_error(exc)
                    )
            if page2 is not None:
                try:
                    page2.close()
                except Exception as exc:
                    self.ui.cleanup_errors.append(
                        "stale tenant context page close: " + safe_error(exc)
                    )

    def connection_actions(self) -> dict[str, Any]:
        self.ui.resource("connections")
        self.ui.select_resource("fixture-connection")
        actions = self.ui.panel("Connection actions")
        discovered = self.ui.expect_response(
            "POST",
            "/connections/fixture-connection/discover",
            lambda: actions.get_by_role(
                "button", name="Discover models", exact=True
            ).click(),
        )
        discovered_body = discovered.get("body")
        candidates = (
            discovered_body.get("candidates")
            if isinstance(discovered_body, dict)
            else None
        )
        if (
            discovered["status"] != 200
            or not isinstance(discovered_body, dict)
            or discovered_body.get("connection_id") != "fixture-connection"
            or not isinstance(candidates, list)
            or not candidates
            or discovered_body.get("approved") is not False
        ):
            raise RuntimeError(
                f"fixture model discovery did not return unapproved candidates: "
                f"{discovered['status']}"
            )
        actions.locator(".result").first.wait_for(
            state="visible", timeout=ROUTE_TIMEOUT_MS
        )
        tested = self.ui.expect_response(
            "POST",
            "/connections/fixture-connection/test",
            lambda: actions.get_by_role("button", name="Test", exact=True).click(),
        )
        test_body = tested.get("body")
        if (
            tested["status"] != 200
            or not isinstance(test_body, dict)
            or test_body.get("status") != "ok"
            or test_body.get("status_code") != 200
        ):
            raise RuntimeError(
                f"fixture connection test did not reach its provider: {tested['status']}"
            )
        actions.locator(".result").first.wait_for(
            state="visible", timeout=ROUTE_TIMEOUT_MS
        )
        return {
            "discover_status": discovered["status"],
            "discovered_candidate_count": len(candidates),
            "discovery_approved": discovered_body.get("approved"),
            "test_status": tested["status"],
            "provider_status": test_body.get("status_code"),
            "test_result_visible": True,
            "seed_connection_preserved": True,
        }

    def credential_metadata(self) -> dict[str, Any]:
        self.ui.resource("credentials")
        table = self.ui.table()
        rows = table.locator("tbody button")
        if rows.count() == 0:
            reason = "fixture did not seed credential metadata"
            self.ui.uncovered.append(reason)
            return {
                "_detail": "credential metadata route had no fixture row",
                "covered": False,
                "uncovered_dependencies": [reason],
            }
        credential_id = rows.first.inner_text().strip()
        rows.first.click()
        self.ui.page.get_by_role(
            "button", name="Refresh encrypted-store metadata", exact=True
        ).wait_for(state="visible", timeout=ROUTE_TIMEOUT_MS)
        observed = self.ui.expect_response(
            "POST",
            f"/connections/{quote(credential_id, safe='')}/status",
            lambda: self.ui.page.get_by_role(
                "button", name="Refresh encrypted-store metadata", exact=True
            ).click(),
        )
        if observed["status"] != 200:
            raise RuntimeError(f"credential status returned {observed['status']}")
        metadata_pre = self.ui.page.locator("div.metadata pre").first
        metadata_pre.wait_for(state="visible", timeout=ROUTE_TIMEOUT_MS)
        metadata = redact(metadata_pre.inner_text())
        return {
            "credential_id": credential_id,
            "status_request": observed["status"],
            "metadata_visible": bool(metadata),
            "metadata_secret_free_fields": True,
        }

    def restore_fixture_credential(self) -> str | None:
        """Restore the deterministic fixture token after a lifecycle case."""

        try:
            status = self.ui.admin_fetch(
                "POST",
                "/admin/api/v1/connections/fixture-connection/status",
                {
                    "data": {
                        "provider": "anthropic",
                        "account_id": "fixture-account",
                    }
                },
            )
            if status["status"] != 200 or not isinstance(status["body"], dict):
                return f"status request returned {status['status']}"
            current_version = status["body"].get("version")
            if not isinstance(current_version, int):
                current_version = 0
            connection = self.ui.admin_fetch(
                "GET", "/admin/api/v1/connections/fixture-connection"
            )
            if connection["status"] != 200 or not isinstance(connection["body"], dict):
                return f"connection lookup returned {connection['status']}"
            connection_version = connection["body"].get("version")
            if not isinstance(connection_version, int):
                return "connection lookup omitted version"
            imported = self.ui.admin_fetch(
                "POST",
                "/admin/api/v1/connections/fixture-connection/import",
                {
                    "data": {
                        "provider": "anthropic",
                        "account_id": "fixture-account",
                        "kind": "api_key",
                        "secret": "fixture-upstream-secret",
                        "credential_version": current_version,
                    }
                },
                version=connection_version,
            )
            if imported["status"] != 200:
                return f"fixture credential restore returned {imported['status']}"
            return None
        except Exception as exc:
            return safe_error(exc)

    def credential_lifecycle(self) -> dict[str, Any]:
        restore_error: str | None = None
        try:
            self.ui.resource("connections")
            self.ui.select_resource("fixture-connection")
            panel = self.ui.page.locator("aside.credential-actions").first
            panel.wait_for(state="visible", timeout=ROUTE_TIMEOUT_MS)
            status = self.ui.expect_response(
                "POST",
                "/connections/fixture-connection/status",
                lambda: panel.get_by_role(
                    "button", name="Refresh encrypted-store metadata", exact=True
                ).click(),
            )
            if status["status"] != 200 or not isinstance(status.get("body"), dict):
                raise RuntimeError(f"credential status returned {status['status']}")
            initial_version = status["body"].get("version")
            if not isinstance(initial_version, int) or initial_version < 1:
                raise RuntimeError("credential status omitted a usable version")
            version_field = panel.locator("label.field").filter(
                has_text="Current credential version"
            ).locator("input").first
            version_field.wait_for(state="visible", timeout=ROUTE_TIMEOUT_MS)
            if version_field.input_value() != str(initial_version):
                raise RuntimeError("status did not populate current credential version")

            # API-key import and version-checked replacement are the configured
            # fixture's connection-bound lifecycle.
            panel.get_by_label("New API key", exact=True).fill(
                "browser-credential-first"
            )
            imported = self.ui.expect_response(
                "POST",
                "/connections/fixture-connection/import",
                lambda: panel.get_by_role(
                    "button", name="Import API key", exact=True
                ).click(),
            )
            if imported["status"] != 200:
                raise RuntimeError(f"credential import returned {imported['status']}")
            imported_version = (
                imported.get("body", {}).get("version")
                if isinstance(imported.get("body"), dict)
                else None
            )
            if not isinstance(imported_version, int) or imported_version <= initial_version:
                raise RuntimeError("credential import did not advance metadata version")

            replacement_field = panel.locator("label.field").filter(
                has_text="Replacement API key"
            ).locator("input").first
            replacement_field.fill("browser-credential-second")
            rotated = self.ui.expect_response(
                "POST",
                "/connections/fixture-connection/import",
                lambda: panel.get_by_role(
                    "button", name="Rotate API key", exact=True
                ).click(),
            )
            if rotated["status"] != 200:
                raise RuntimeError(f"credential rotation returned {rotated['status']}")
            rotated_version = (
                rotated.get("body", {}).get("version")
                if isinstance(rotated.get("body"), dict)
                else None
            )
            if not isinstance(rotated_version, int) or rotated_version <= imported_version:
                raise RuntimeError("credential rotation did not advance metadata version")

            self.ui.page.once("dialog", lambda dialog: dialog.accept())
            revoked = self.ui.expect_response(
                "POST",
                "/connections/fixture-connection/revoke-credential",
                lambda: panel.get_by_role(
                    "button", name="Revoke credential", exact=True
                ).click(),
            )
            if revoked["status"] != 200:
                raise RuntimeError(f"credential revoke returned {revoked['status']}")
            revoked_version = (
                revoked.get("body", {}).get("version")
                if isinstance(revoked.get("body"), dict)
                else None
            )
            if not isinstance(revoked_version, int) or revoked_version <= rotated_version:
                raise RuntimeError("credential revoke did not advance metadata version")
            self.ui.page.get_by_role(
                "button", name="Credential revoked", exact=True
            ).wait_for(state="visible", timeout=ROUTE_TIMEOUT_MS)

            panel.get_by_label("New API key", exact=True).fill(
                "fixture-upstream-secret"
            )
            reimported = self.ui.expect_response(
                "POST",
                "/connections/fixture-connection/import",
                lambda: panel.get_by_role(
                    "button", name="Import API key", exact=True
                ).click(),
            )
            if reimported["status"] != 200:
                raise RuntimeError(
                    f"credential reimport after revoke returned {reimported['status']}"
                )
            reimported_version = (
                reimported.get("body", {}).get("version")
                if isinstance(reimported.get("body"), dict)
                else None
            )
            if not isinstance(reimported_version, int) or reimported_version <= revoked_version:
                raise RuntimeError("credential reimport did not advance metadata version")

            # OAuth/device controls are intentionally unavailable in the default
            # fixture.  Their failure is still a real, observable error contract.
            oauth = self.ui.expect_response(
                "POST",
                "/connections/fixture-connection/oauth-start",
                lambda: panel.get_by_role(
                    "button", name="Start OAuth", exact=True
                ).click(),
            )
            if oauth["status"] < 400:
                raise RuntimeError("unconfigured OAuth unexpectedly succeeded")
            device = self.ui.expect_response(
                "POST",
                "/connections/fixture-connection/device-start",
                lambda: panel.get_by_role(
                    "button", name="Start device flow", exact=True
                ).click(),
            )
            if device["status"] < 400:
                raise RuntimeError("unconfigured device flow unexpectedly succeeded")
            self.ui.page.get_by_role("alert").wait_for(
                state="visible", timeout=ROUTE_TIMEOUT_MS
            )
            secret_fields_empty = all(
                panel.locator('input[type="password"]').nth(index).input_value() == ""
                for index in range(panel.locator('input[type="password"]').count())
            )
            return {
                "initial_version": initial_version,
                "import_version": imported_version,
                "rotation_version": rotated_version,
                "revoked_version": revoked_version,
                "reimport_version": reimported_version,
                "active_after_reimport": True,
                "secret_fields_empty": secret_fields_empty,
                "oauth_unavailable_status": oauth["status"],
                "device_unavailable_status": device["status"],
            }
        finally:
            restore_error = self.restore_fixture_credential()
            if restore_error:
                self.ui.cleanup_errors.append("fixture credential restore: " + restore_error)

    def api_key_lifecycle(self) -> dict[str, Any]:
        resource_id = self.uid("key")
        self.ui.resource("api_keys")
        issue_panel = self.ui.page.locator("section.key-issue").first
        issue_panel.wait_for(state="visible", timeout=ROUTE_TIMEOUT_MS)
        issue_panel.get_by_label("Key ID", exact=True).fill(resource_id)
        issue_panel.get_by_label("Name", exact=True).fill("Browser API key")
        issue_panel.get_by_label(
            "Permissions (space or comma separated)", exact=True
        ).fill("inference:invoke")
        issue_panel.get_by_label(
            "Aliases (space or comma separated)", exact=True
        ).fill("assistant")
        issue_panel.get_by_label(
            "Connections (space or comma separated)", exact=True
        ).fill("fixture-connection")
        issue_panel.get_by_label(
            "Operations (exact operation names, space or comma separated)", exact=True
        ).fill("generate")
        issue_panel.get_by_label("Portable", exact=True).check()
        issued = self.ui.expect_response(
            "POST",
            f"/api_keys/{quote(resource_id, safe='')}/issue",
            lambda: issue_panel.get_by_role(
                "button", name="Issue API key", exact=True
            ).click(),
        )
        if issued["status"] != 200:
            raise RuntimeError(f"API key issue returned {issued['status']}")
        issued_body = issued.get("body")
        issued_resource = (
            issued_body.get("resource")
            if isinstance(issued_body, dict)
            else None
        )
        issued_version = (
            issued_resource.get("version")
            if isinstance(issued_resource, dict)
            else None
        )
        self.ui.track_api_key(
            resource_id, issued_version if isinstance(issued_version, int) else 0
        )
        if not issued.get("token_present"):
            raise RuntimeError("API key issue response omitted its one-time token")
        if not isinstance(issued_version, int):
            raise RuntimeError("API key issue response omitted resource version")
        issue_panel.get_by_role("button", name="Dismiss result", exact=True).wait_for(
            state="visible", timeout=ROUTE_TIMEOUT_MS
        )
        issue_panel.get_by_role("button", name="Dismiss result", exact=True).click()
        self.ui.table().get_by_role(
            "button", name=resource_id, exact=True
        ).click()
        key_actions = self.ui.panel("Key actions")
        rotated = self.ui.expect_response(
            "POST",
            f"/api_keys/{quote(resource_id, safe='')}/rotate",
            lambda: key_actions.get_by_role(
                "button", name="Rotate", exact=True
            ).click(),
        )
        if rotated["status"] != 200:
            raise RuntimeError(f"API key rotation returned {rotated['status']}")
        rotated_body = rotated.get("body")
        rotated_resource = (
            rotated_body.get("resource")
            if isinstance(rotated_body, dict)
            else None
        )
        rotated_version = (
            rotated_resource.get("version")
            if isinstance(rotated_resource, dict)
            else None
        )
        self.ui.update_api_key_version(
            resource_id, rotated_version if isinstance(rotated_version, int) else 0
        )
        if not rotated.get("token_present"):
            raise RuntimeError("API key rotation did not return a one-time token")
        if not isinstance(rotated_version, int):
            raise RuntimeError("API key rotation response omitted resource version")
        key_actions.get_by_role("button", name="Dismiss result", exact=True).wait_for(
            state="visible", timeout=ROUTE_TIMEOUT_MS
        )
        key_actions.get_by_role("button", name="Dismiss result", exact=True).click()
        self.ui.page.once("dialog", lambda dialog: dialog.accept())
        revoked = self.ui.expect_response(
            "POST",
            f"/api_keys/{quote(resource_id, safe='')}/revoke",
            lambda: key_actions.get_by_role(
                "button", name="Revoke", exact=True
            ).click(),
        )
        if revoked["status"] != 200:
            raise RuntimeError(f"API key revoke returned {revoked['status']}")
        self.ui.page.locator('p[role="status"]').filter(
            has_text="This key is revoked."
        ).wait_for(state="visible", timeout=ROUTE_TIMEOUT_MS)
        key_row = self.ui.table().get_by_role(
            "button", name=resource_id, exact=True
        )
        key_row.wait_for(state="detached", timeout=ROUTE_TIMEOUT_MS)
        self.ui.untrack_api_key(resource_id)
        return {
            "issued_status": issued["status"],
            "issue_token_returned_once": True,
            "rotated_status": rotated["status"],
            "rotation_token_returned_once": True,
            "revoked_status": revoked["status"],
            "revoked_row_removed": True,
        }

    def route_action(self) -> dict[str, Any]:
        self.ui.resource("route_policies")
        self.ui.select_resource("assistant")
        actions = self.ui.panel("Route analysis")
        explained = self.ui.expect_response(
            "POST",
            "/route_policies/assistant/dry-run",
            lambda: actions.get_by_role(
                "button", name="Explain dry-run", exact=True
            ).click(),
        )
        if explained["status"] != 200:
            body = explained.get("body")
            code = body.get("code") if isinstance(body, dict) else None
            raise RuntimeError(
                f"route dry-run returned {explained['status']} / {code}"
            )
        result = actions.locator(".result").first
        result.wait_for(state="visible", timeout=ROUTE_TIMEOUT_MS)
        return {"dry_run_status": explained["status"], "result_visible": True}

    def playground(self, payload: str, expected_text: str) -> dict[str, Any]:
        self.ui.goto("/admin/playground", "Inference playground")
        operation = self.ui.page.get_by_label("Operation", exact=True)
        path = self.ui.page.get_by_label("Gateway operation path", exact=True)
        mappings = {
            "chat": "/playground/v1/chat/completions",
            "responses": "/playground/v1/responses",
            "image": "/playground/v1/images/generations",
            "audio": "/playground/v1/audio/speech",
            "video": "/playground/v1/videos",
        }
        for option, expected_path in mappings.items():
            operation.select_option(option)
            if path.input_value() != expected_path:
                raise RuntimeError(
                    f"operation {option} selected unexpected path {path.input_value()}"
                )
        operation.select_option("chat")
        editor = self.ui.page.locator("textarea.play-editor").first
        editor.fill(payload)
        response = self.ui.expect_suffix_response(
            "POST",
            "/admin/api/v1/playground/v1/chat/completions",
            lambda: self.ui.page.get_by_role(
                "button", name="Send request", exact=True
            ).click(),
            read_body=False,
        )
        if response["status"] != 200:
            raise RuntimeError(f"playground stream returned {response['status']}")
        pre = self.ui.page.locator(".stream-output pre").first
        pre.wait_for(state="visible", timeout=ROUTE_TIMEOUT_MS)
        raw, stream = wait_for_stream(pre)
        text = str(stream.get("text", ""))
        if text != expected_text:
            raise RuntimeError(
                f"playground semantic text mismatch: expected length {len(expected_text)}, "
                f"observed length {len(text)}"
            )
        if not stream.get("complete"):
            raise RuntimeError("playground stream did not reach semantic terminal")
        usage = stream.get("usage")
        usage_summary = {}
        if isinstance(usage, dict):
            for field in ("prompt_tokens", "completion_tokens", "input_tokens", "output_tokens", "total_tokens"):
                if isinstance(usage.get(field), int):
                    usage_summary[field] = usage[field]
        # The stream output is intentionally not copied to evidence; it can contain
        # arbitrary provider text supplied by a caller payload.
        self.ui.page.reload(wait_until="domcontentloaded", timeout=CASE_TIMEOUT_MS)
        self.ui.wait_shell()
        self.ui.page.locator(".stream-output pre").first.wait_for(
            state="visible", timeout=ROUTE_TIMEOUT_MS
        )
        reset_output = self.ui.page.locator(".stream-output pre").first.inner_text()
        return {
            "response_status": response["status"],
            "semantic_text": expected_text if expected_text == "OK" else "[redacted]",
            "semantic_text_length": len(text),
            "terminal_done": stream["done"],
            "finish_reason": stream["finish_reason"],
            "usage": usage_summary,
            "usage_total": stream["usage_total"],
            "sse_bytes_observed": len(raw),
            "operation_paths_verified": mappings,
            "reload_reset_output": reset_output == "No output yet.",
        }

    def admissions_reconciliation(self) -> dict[str, Any]:
        self.ui.resource("admissions")
        table = self.ui.table()
        rows = table.locator("tbody button")
        if rows.count() == 0:
            reason = "fixture has no listable admission record"
            self.ui.uncovered.append(reason)
            return {
                "_detail": "admission reconciliation not applicable without a fixture admission",
                "covered": False,
                "uncovered_dependencies": [reason],
            }

        candidates: list[tuple[str, str, int, bool]] = []
        for index in range(rows.count()):
            admission_id = rows.nth(index).inner_text().strip()
            observed = self.ui.admin_fetch(
                "GET",
                f"/admin/api/v1/admissions/{quote(admission_id, safe='')}",
            )
            body = observed.get("body")
            data = body.get("data") if isinstance(body, dict) else None
            state = data.get("state") if isinstance(data, dict) else None
            version = body.get("version") if isinstance(body, dict) else None
            if (
                observed["status"] != 200
                or not isinstance(state, str)
                or not isinstance(version, int)
            ):
                raise RuntimeError(
                    f"admission {admission_id} did not expose state/version"
                )
            candidates.append(
                (admission_id, state, version, bool(data.get("reconciled")))
            )

        settled = next(
            (candidate for candidate in candidates if candidate[1] != "outcome_unknown"),
            None,
        )
        candidate = settled or next(
            (candidate for candidate in candidates if candidate[1] == "outcome_unknown"),
            None,
        )
        if candidate is None:
            raise RuntimeError("fixture admission states were not recognizable")
        admission_id, state_before, version_before, reconciled_before = candidate
        self.ui.select_resource(admission_id)
        try:
            panel = self.ui.panel("Reconcile admission")
        except Exception:
            reason = "session lacks accounting:reconcile permission"
            self.ui.uncovered.append(reason)
            return {
                "_detail": "admission row was visible but reconciliation control was unavailable",
                "covered": False,
                "admission_id": admission_id,
                "uncovered_dependencies": [reason],
            }
        panel.get_by_label("Reconciliation ID", exact=True).fill(
            self.uid("reconciliation")
        )
        panel.locator("label.field").filter(has_text="Mode").locator(
            "select"
        ).first.select_option("provider_evidence")
        panel.get_by_label("Reason", exact=True).fill("Browser fixture evidence")
        panel.get_by_label("Source reference", exact=True).fill("browser-fixture")
        panel.get_by_label("Cost (nanodollars)", exact=True).fill("0")
        panel.get_by_label("Input tokens", exact=True).fill("9")
        panel.get_by_label("Output tokens", exact=True).fill("2")
        panel.get_by_label("Total tokens", exact=True).fill("11")
        panel.get_by_label("Usage source", exact=True).fill("fixture")
        reconciled = self.ui.expect_response(
            "POST",
            f"/admissions/{quote(admission_id, safe='')}/reconcile",
            lambda: panel.get_by_role("button", name="Reconcile", exact=True).click(),
        )
        result = panel.locator(".result").first
        error = panel.locator('[role="alert"]').first
        after = self.ui.admin_fetch(
            "GET",
            f"/admin/api/v1/admissions/{quote(admission_id, safe='')}",
        )
        after_body = after.get("body")
        after_data = after_body.get("data") if isinstance(after_body, dict) else None
        if state_before != "outcome_unknown":
            code = (
                reconciled.get("body", {}).get("code")
                if isinstance(reconciled.get("body"), dict)
                else None
            )
            if reconciled["status"] != 409 or code != "attempt_not_reconcilable":
                raise RuntimeError(
                    f"settled admission reconciliation returned "
                    f"{reconciled['status']} / {code}"
                )
            error.wait_for(state="visible", timeout=ROUTE_TIMEOUT_MS)
            if (
                after["status"] != 200
                or not isinstance(after_body, dict)
                or after_body.get("version") != version_before
                or not isinstance(after_data, dict)
                or after_data.get("state") != state_before
                or bool(after_data.get("reconciled")) != reconciled_before
            ):
                raise RuntimeError("rejected settled reconciliation changed admission state")
            expected = "settled_rejection"
            result_visible = False
            alert_visible = True
        else:
            if reconciled["status"] != 200:
                raise RuntimeError(
                    f"outcome_unknown admission reconciliation returned "
                    f"{reconciled['status']}"
                )
            result.wait_for(state="visible", timeout=ROUTE_TIMEOUT_MS)
            if after["status"] != 200 or not isinstance(after_body, dict):
                raise RuntimeError("reconciled admission could not be re-read")
            expected = "unknown_success"
            result_visible = True
            alert_visible = False
        self.ui.resource("reconciliations")
        reconciliation_rows = self.ui.table().locator("tbody tr").count()
        return {
            "admission_id_present": True,
            "admission_state_before": state_before,
            "admission_version_before": version_before,
            "reconcile_status": reconciled["status"],
            "expected_outcome": expected,
            "error_visible": alert_visible,
            "result_visible": result_visible,
            "admission_unchanged_on_rejection": expected != "settled_rejection"
            or (
                after_body.get("version") == version_before
                and isinstance(after_data, dict)
                and after_data.get("state") == state_before
            ),
            "reconciliation_route_rows": reconciliation_rows,
        }

    def session_csrf_reload(self) -> dict[str, Any]:
        self.ui.goto("/admin/", "Operations overview")
        probe_id = self.uid("csrf-probe")
        denied = self.ui.admin_fetch(
            "POST",
            "/admin/api/v1/account_pools",
            {"id": probe_id, "data": {"provider": "anthropic", "account_ids": []}},
            include_csrf=False,
        )
        denied_body = denied.get("body")
        denied_detail = (
            denied_body.get("detail")
            if isinstance(denied_body, dict)
            else None
        )
        if denied["status"] != 403 or denied_detail != "mutation authentication failed":
            raise RuntimeError(
                f"mutation without CSRF returned {denied['status']} / {denied_detail}"
            )
        probe = self.ui.admin_fetch(
            "GET", f"/admin/api/v1/account_pools/{quote(probe_id, safe='')}"
        )
        if probe["status"] != 404:
            raise RuntimeError(
                f"CSRF-blocked mutation left resource status {probe['status']}"
            )
        session = self.ui.admin_fetch("GET", "/admin/api/v1/session")
        session_body = session.get("body")
        if (
            session["status"] != 200
            or not isinstance(session_body, dict)
            or not session_body.get("csrf_token")
        ):
            raise RuntimeError("authenticated session omitted CSRF token")
        self.ui.page.reload(wait_until="domcontentloaded", timeout=CASE_TIMEOUT_MS)
        self.ui.wait_shell()
        self.ui.page.get_by_role(
            "heading", name="Operations overview", exact=True
        ).wait_for(state="visible", timeout=ROUTE_TIMEOUT_MS)
        return {
            "csrfless_mutation_status": denied["status"],
            "csrf_failure_detail_observed": denied_detail,
            "blocked_resource_status": probe["status"],
            "session_status": session["status"],
            "reload_preserved_authenticated_shell": True,
        }

    def validation_errors(self) -> dict[str, Any]:
        # Required browser validation prevents a request before it reaches the API.
        self.ui.resource("tenants")
        editor = self.ui.new_resource()
        editor.get_by_label("ID", exact=True).fill(self.uid("invalid-tenant"))
        name = editor.get_by_label("Tenant name", exact=True)
        editor.get_by_role("button", name="Create", exact=True).click()
        if not name.evaluate("(element) => !element.checkValidity()"):
            raise RuntimeError("required tenant name did not block form submission")

        self.ui.resource("models")
        model_editor = self.ui.new_resource()
        context_limit = model_editor.get_by_label("Context token limit (blank = unknown)", exact=True)
        context_limit.fill("-1")
        if context_limit.get_attribute("aria-invalid") != "true":
            raise RuntimeError("negative model limit did not become invalid")
        feature_field = model_editor.get_by_label("Features (JSON object of string values)", exact=True)
        feature_field.fill('{"streaming":"invalid"}')
        feature_error_id = feature_field.get_attribute("aria-errormessage")
        if feature_field.get_attribute("aria-invalid") != "true" or not feature_error_id:
            raise RuntimeError("invalid feature map did not expose an associated error")
        model_editor.locator(f'[id="{feature_error_id}"][role="alert"]').wait_for(
            state="visible", timeout=ROUTE_TIMEOUT_MS
        )

        self.ui.goto("/admin/playground", "Inference playground")
        play_editor = self.ui.page.locator("textarea.play-editor").first
        play_editor.fill("{")
        self.ui.page.get_by_role("button", name="Send request", exact=True).click()
        self.ui.page.locator('[role="alert"]').filter(
            has_text="Request JSON is invalid."
        ).wait_for(state="visible", timeout=ROUTE_TIMEOUT_MS)

        self.ui.resource("api_keys")
        issue_panel = self.ui.page.locator("section.key-issue").first
        issue_button = issue_panel.get_by_role(
            "button", name="Issue API key", exact=True
        )
        if issue_button.is_enabled():
            raise RuntimeError("API key issue remained enabled with required fields blank")
        return {
            "required_form_blocked": True,
            "integer_custom_validity": True,
            "feature_map_custom_validity": True,
            "playground_invalid_json_alert": True,
            "api_key_required_button_disabled": True,
        }

    def read_only_routes(self) -> dict[str, Any]:
        self.ui.goto("/admin/", "Operations overview")
        links = self.ui.page.locator('nav[aria-label="Operations"] a').evaluate_all(
            "(elements) => elements.map(element => element.getAttribute('href'))"
        )
        route_results: list[dict[str, Any]] = []
        failures: list[str] = []
        for kind in RESOURCE_ROUTES:
            try:
                self.ui.resource(kind)
                # A list error leaves the table mounted; wait briefly for the
                # enabled Refresh state before classifying it as a failure.
                try:
                    self.ui.page.wait_for_function(
                        """
                        () => {
                          const button = document.querySelector('section.resource .section-head button');
                          return button && !button.disabled;
                        }
                        """,
                        timeout=5_000,
                    )
                except Exception:
                    pass
                error_notice = self.ui.page.locator("section.resource .notice.error")
                if error_notice.count() > 0 and error_notice.first.is_visible():
                    detail = redact(error_notice.first.inner_text())
                    failures.append(f"{kind}: {detail}")
                    route_results.append(
                        {"kind": kind, "label": RESOURCE_LABELS[kind], "status": "failed"}
                    )
                    continue
                rows = self.ui.table().locator("tbody tr").count()
                route_results.append(
                    {
                        "kind": kind,
                        "label": RESOURCE_LABELS[kind],
                        "status": "passed",
                        "rows": rows,
                    }
                )
            except Exception as exc:
                failures.append(f"{kind}: {safe_error(exc)}")
                route_results.append(
                    {"kind": kind, "label": RESOURCE_LABELS[kind], "status": "failed"}
                )
        if failures:
            return {
                "_status": "failed",
                "_detail": "one or more advertised console collection routes failed",
                "nav_links": links,
                "routes": route_results,
                "route_failures": failures,
            }
        return {"nav_links": links, "routes": route_results}

    def configuration(self) -> dict[str, Any]:
        self.ui.goto("/admin/config", "Configuration")
        export = self.ui.expect_response(
            "GET",
            "/config/export",
            lambda: self.ui.page.get_by_role(
                "button", name="Export", exact=True
            ).click(),
        )
        if export["status"] != 200:
            raise RuntimeError(f"configuration export returned {export['status']}")
        self.ui.page.locator("pre.result").first.wait_for(
            state="visible", timeout=ROUTE_TIMEOUT_MS
        )
        preview = self.ui.expect_response(
            "POST",
            "/config/diff",
            lambda: self.ui.page.get_by_role(
                "button", name="Preview changes", exact=True
            ).click(),
        )
        if preview["status"] != 400:
            raise RuntimeError(
                f"configuration revision guard returned {preview['status']}, expected 400"
            )
        self.ui.page.get_by_role("alert").wait_for(
            state="visible", timeout=ROUTE_TIMEOUT_MS
        )
        self.ui.goto("/admin/config", "Configuration")
        return {
            "export_status": export["status"],
            "export_result_visible": True,
            "missing_revision_status": preview["status"],
            "missing_revision_error_visible": True,
            "owner_scoped_token_panel_visible": self.ui.page.get_by_role(
                "heading", name="Scoped admin tokens", exact=True
            ).count()
            > 0,
        }

    def mobile_keyboard(self) -> dict[str, Any]:
        self.ui.page.set_viewport_size({"width": 390, "height": 844})
        self.ui.goto("/admin/models", "Models")
        menu = self.ui.page.get_by_role("button", name="Open navigation", exact=True)
        menu.wait_for(state="visible", timeout=ROUTE_TIMEOUT_MS)
        menu.focus()
        self.ui.page.keyboard.press("Enter")
        self.ui.page.get_by_role("navigation", name="Operations", exact=True).wait_for(
            state="visible", timeout=ROUTE_TIMEOUT_MS
        )
        metrics = self.ui.page.evaluate(
            """
            () => ({
              innerWidth: window.innerWidth,
              clientWidth: document.documentElement.clientWidth,
              scrollWidth: document.documentElement.scrollWidth,
              navVisible: !!document.querySelector('nav[aria-label="Operations"]')
            })
            """
        )
        if not metrics.get("navVisible") or metrics.get("innerWidth") != 390 or metrics.get("scrollWidth") > 390:
            raise RuntimeError("mobile shell did not remain reachable at 390px")
        mobile_path = self.ui.evidence_dir / "browser-mobile-layout.png"
        self.ui.page.screenshot(path=str(mobile_path), full_page=True)
        try:
            os.chmod(mobile_path, 0o600)
        except OSError:
            pass
        overview = self.ui.page.locator(
            'nav[aria-label="Operations"] a'
        ).filter(has_text="Overview").first
        overview.focus()
        self.ui.page.keyboard.press("Enter")
        self.ui.page.get_by_role(
            "heading", name="Operations overview", exact=True
        ).wait_for(state="visible", timeout=ROUTE_TIMEOUT_MS)
        menu = self.ui.page.get_by_role("button", name="Open navigation", exact=True)
        menu.wait_for(state="visible", timeout=ROUTE_TIMEOUT_MS)
        menu.click()
        overview = self.ui.page.locator(
            'nav[aria-label="Operations"] a'
        ).filter(has_text="Overview").first
        overview.focus()
        self.ui.page.keyboard.press("Tab")
        active = self.ui.page.evaluate(
            "() => ({tag: document.activeElement?.tagName || '', role: document.activeElement?.getAttribute('role') || ''})"
        )
        self.ui.page.set_viewport_size({"width": 1280, "height": 900})
        self.ui.goto("/admin/", "Operations overview")
        return {
            "mobile": {
                "inner_width": metrics.get("innerWidth"),
                "client_width": metrics.get("clientWidth"),
                "document_scroll_width": metrics.get("scrollWidth"),
                "navigation_visible": metrics.get("navVisible"),
                "menu_keyboard_opened": True,
                "screenshot": str(mobile_path),
            },
            "keyboard_enter_navigated": True,
            "keyboard_tab_active_element": active,
        }


class BrowserRunner:
    def __init__(self, page: Any, console: str, evidence_dir: Path, run_id: str) -> None:
        self.page = page
        self.evidence_dir = evidence_dir
        self.ui = BrowserUI(page, console, evidence_dir)
        self.cases = BrowserCases(self.ui, run_id)
        self.results: list[dict[str, Any]] = []

    def run_case(self, name: str, fn: Callable[[], dict[str, Any]]) -> None:
        started = time.monotonic()
        evidence: dict[str, Any] = {}
        status = "passed"
        detail = "operated actual authenticated console state and API consequences"
        try:
            output = fn() or {}
            status = str(output.pop("_status", "passed"))
            detail = str(
                output.pop(
                    "_detail",
                    "operated actual authenticated console state and API consequences",
                )
            )
            evidence = redact(output)
            uncovered = output.get("uncovered_dependencies")
            if isinstance(uncovered, list):
                for item in uncovered:
                    if isinstance(item, str) and item not in self.ui.uncovered:
                        self.ui.uncovered.append(item)
            if status not in {"passed", "failed"}:
                status = "failed"
                detail = "browser case returned an invalid status"
        except Exception as exc:
            status = "failed"
            detail = "real console interaction failed"
            evidence = {"error": safe_error(exc)}
        screenshot = self.evidence_dir / f"{_slug(name)}.png"
        try:
            self.ui.screenshot(screenshot)
            evidence["screenshot"] = str(screenshot)
        except Exception as exc:
            status = "failed"
            evidence["screenshot_error"] = safe_error(exc)
        item = {
            "Name": name,
            "Status": status,
            "Detail": detail,
            "duration_ms": int((time.monotonic() - started) * 1000),
            "evidence": redact(evidence),
        }
        record = self.evidence_dir / f"{_slug(name)}.json"
        _json_write(record, item)
        self.results.append(item)


def _setup_result(name: str, detail: str, started: float) -> dict[str, Any]:
    return {
        "Name": name,
        "Status": "failed",
        "Detail": detail,
        "duration_ms": int((time.monotonic() - started) * 1000),
        "evidence": {},
    }


def _write_report(evidence_dir: Path, results: list[dict[str, Any]]) -> None:
    evidence_dir.mkdir(parents=True, exist_ok=True)
    _json_write(evidence_dir / "browser-results.json", results)


def main() -> int:
    console = os.environ.get("HOORIFIC_VERIFY_CONSOLE", "").strip().rstrip("/")
    alias = os.environ.get("HOORIFIC_VERIFY_BROWSER_MODEL", "assistant").strip() or "assistant"
    payload = os.environ.get(
        "HOORIFIC_VERIFY_BROWSER_PAYLOAD",
        json.dumps(
            {
                "model": alias,
                "messages": [
                    {"role": "user", "content": "Reply exactly OK"}
                ],
                "stream": True,
                "stream_options": {"include_usage": True},
            }
        ),
    )
    expected_text = os.environ.get("HOORIFIC_VERIFY_BROWSER_EXPECTED_TEXT", "OK")
    evidence_dir = Path(
        os.environ.get("HOORIFIC_VERIFY_EVIDENCE_DIR", ".artifacts/sdk-browser")
    )
    storage = os.environ.get("HOORIFIC_VERIFY_BROWSER_STORAGE_STATE", "").strip()
    started = time.monotonic()
    if not console:
        results = [_setup_result("browser/setup", "HOORIFIC_VERIFY_CONSOLE is required", started)]
        _write_report(evidence_dir, results)
        print(json.dumps(results, indent=2))
        return 1
    if not storage:
        results = [
            _setup_result(
                "browser/setup",
                "HOORIFIC_VERIFY_BROWSER_STORAGE_STATE is required",
                started,
            )
        ]
        _write_report(evidence_dir, results)
        print(json.dumps(results, indent=2))
        return 1
    if not Path(storage).is_file():
        results = [
            _setup_result(
                "browser/setup",
                "authenticated browser storage state does not exist",
                started,
            )
        ]
        _write_report(evidence_dir, results)
        print(json.dumps(results, indent=2))
        return 1
    try:
        from playwright.sync_api import sync_playwright
    except Exception as exc:
        results = [
            _setup_result(
                "browser/setup",
                "Playwright import failed; install requirements-browser.txt: "
                + safe_error(exc),
                started,
            )
        ]
        _write_report(evidence_dir, results)
        print(json.dumps(results, indent=2))
        return 1

    evidence_dir.mkdir(parents=True, exist_ok=True)
    run_id = f"{time.time_ns()}-{os.getpid()}"
    results: list[dict[str, Any]] = []
    browser = None
    context = None
    try:
        with sync_playwright() as playwright:
            browser = playwright.chromium.launch(headless=True)
            context = browser.new_context(
                storage_state=storage,
                viewport={"width": 1280, "height": 900},
                color_scheme="light",
                reduced_motion="reduce",
            )
            page = context.new_page()
            page.set_default_timeout(ROUTE_TIMEOUT_MS)
            runner = BrowserRunner(page, console, evidence_dir, run_id)
            runner.run_case("browser/session-csrf-reload", runner.cases.session_csrf_reload)
            runner.run_case("browser/validation-errors", runner.cases.validation_errors)
            runner.run_case("browser/navigation-read-only-routes", runner.cases.read_only_routes)
            runner.run_case("browser/configuration-actions", runner.cases.configuration)
            runner.run_case("browser/resources-tenants", runner.cases.tenant_lifecycle)
            runner.run_case("browser/resources-operators", runner.cases.operator_lifecycle)
            runner.run_case("browser/resources-role-bindings", runner.cases.role_binding_lifecycle)
            runner.run_case("browser/resources-connections", runner.cases.connection_lifecycle)
            runner.run_case("browser/resources-account-pools", runner.cases.account_pool_lifecycle)
            runner.run_case("browser/resources-models", runner.cases.model_lifecycle)
            runner.run_case("browser/resources-model-aliases", runner.cases.alias_lifecycle)
            runner.run_case("browser/resources-route-policies", runner.cases.route_lifecycle)
            runner.run_case("browser/resources-policy-limits", runner.cases.policy_limit_lifecycle)
            runner.run_case("browser/resources-stale-version", runner.cases.stale_version_conflict)
            runner.run_case("browser/tenant-context-stale-tab", runner.cases.stale_tenant_context)
            runner.run_case("browser/connection-actions", runner.cases.connection_actions)
            runner.run_case("browser/credentials-metadata", runner.cases.credential_metadata)
            runner.run_case("browser/credentials-lifecycle", runner.cases.credential_lifecycle)
            runner.run_case("browser/api-keys-lifecycle", runner.cases.api_key_lifecycle)
            runner.run_case("browser/route-dry-run", runner.cases.route_action)
            runner.run_case(
                "browser/playground-semantic-stream",
                lambda: runner.cases.playground(payload, expected_text),
            )
            runner.run_case(
                "browser/admissions-reconciliation",
                runner.cases.admissions_reconciliation,
            )
            runner.run_case("browser/mobile-keyboard", runner.cases.mobile_keyboard)
            cleanup_errors = runner.ui.cleanup()
            results = list(runner.results)
            if cleanup_errors:
                results.append(
                    {
                        "Name": "browser/cleanup",
                        "Status": "failed",
                        "Detail": "UI-created resources could not all be cleaned up",
                        "duration_ms": 0,
                        "evidence": {"errors": cleanup_errors},
                    }
                )
            results.append(
                {
                    "Name": "browser/summary",
                    "Status": "failed"
                    if any(item["Status"] == "failed" for item in results)
                    else "passed",
                    "Detail": "real browser console journey inventory",
                    "duration_ms": int((time.monotonic() - started) * 1000),
                    "evidence": {
                        "scenario_inventory": [item["Name"] for item in results],
                        "case_count": len(results),
                        "uncovered_dependencies": sorted(set(runner.ui.uncovered)),
                        "evidence_directory": str(evidence_dir),
                        "report": str(evidence_dir / "browser-results.json"),
                    },
                }
            )
    except Exception as exc:
        results.append(
            _setup_result("browser/runner", "browser setup failed: " + safe_error(exc), started)
        )
    finally:
        try:
            if context is not None:
                context.close()
        except Exception:
            pass
        try:
            if browser is not None:
                browser.close()
        except Exception:
            pass

    _write_report(evidence_dir, results)
    print(json.dumps(results, indent=2))
    return 1 if any(item["Status"] == "failed" for item in results) else 0


if __name__ == "__main__":
    raise SystemExit(main())
