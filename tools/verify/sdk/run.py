#!/usr/bin/env python3
"""Real SDK qualification against an already running Hoorific gateway.

This runner never replaces the gateway or provider with an in-process fake.  It starts
small local HTTP upstreams, registers them through the authenticated admin API, and
then drives the public gateway using the official SDKs.  Results are a JSON array of
{Name, Status, Detail, duration_ms, evidence}; any setup, import, HTTP, or assertion
failure is a failed result and causes a non-zero exit.

Required environment:
  HOORIFIC_VERIFY_INFERENCE  gateway inference origin (host:port is accepted)
  HOORIFIC_VERIFY_MANAGEMENT gateway management origin (host:port is accepted)
  HOORIFIC_VERIFY_KEY        optional existing gateway key (a scoped key is issued otherwise)
  HOORIFIC_VERIFY_COOKIE     management session cookie (required to provision)
  HOORIFIC_VERIFY_CSRF       management CSRF token (required to provision)

The optional browser proof is deliberately separate (browser_runner.py).  Uncovered
provider operations are reported in the final `coverage` result rather than presented
as passed checks.
"""
from __future__ import annotations

import json
import os
import sys
import threading
import time
import traceback
import uuid
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from typing import Any, Callable
from urllib.error import HTTPError, URLError
from urllib.parse import urlsplit
from urllib.request import Request, urlopen


def origin(value: str) -> str:
    value = (value or "").strip().rstrip("/")
    if not value:
        return ""
    return value if "://" in value else "http://" + value


INFERENCE = origin(os.environ.get("HOORIFIC_VERIFY_INFERENCE", ""))
MANAGEMENT = origin(os.environ.get("HOORIFIC_VERIFY_MANAGEMENT", ""))
COOKIE = os.environ.get("HOORIFIC_VERIFY_COOKIE", "").strip()
CSRF = os.environ.get("HOORIFIC_VERIFY_CSRF", "").strip()
SUPPLIED_KEY = os.environ.get("HOORIFIC_VERIFY_KEY", "").strip()

results: list[dict[str, Any]] = []


def record(name: str, status: str, detail: str, started: float, evidence: Any = None) -> None:
    item: dict[str, Any] = {"Name": name, "Status": status, "Detail": detail,
                            "duration_ms": int((time.monotonic() - started) * 1000)}
    if evidence is not None:
        item["evidence"] = evidence
    results.append(item)


def run_check(name: str, fn: Callable[[], Any]) -> None:
    started = time.monotonic()
    try:
        evidence = fn()
        record(name, "passed", "observed actual gateway and upstream behavior", started, evidence)
    except Exception as exc:  # assertions and SDK errors are qualification failures
        record(name, "failed", f"{type(exc).__name__}: {exc}", started)


def require(condition: bool, message: str) -> None:
    if not condition:
        raise RuntimeError(message)


QUAL_CALLS = [
    ("call-weather", "get_weather", {"city": "Oslo", "unit": "celsius"}),
    ("call-time", "get_time", {"timezone": "UTC"}),
]

def argument_fragments(value: dict[str, Any]) -> list[str]:
    raw = json.dumps(value, separators=(",", ":"))
    cut = max(1, len(raw) // 2)
    return [raw[:cut], raw[cut:]]


class UpstreamState:
    def __init__(self, protocol: str, qualification: bool = False):
        self.protocol = protocol
        self.qualification = qualification
        self.lock = threading.Lock()
        self.requests: list[dict[str, Any]] = []
        self.next_file = 0

    def boundary(self) -> int:
        with self.lock:
            return len(self.requests)

    def since(self, boundary: int) -> list[dict[str, Any]]:
        with self.lock:
            return list(self.requests[boundary:])

    def add(self, method: str, path: str, headers: Any, body: bytes) -> None:
        decoded: Any = None
        try:
            decoded = json.loads(body) if body else None
        except Exception:
            decoded = {"raw_bytes": len(body)}
        with self.lock:
            self.requests.append({"method": method, "path": path, "headers": dict(headers),
                                  "header_items": list(headers.raw_items()), "body": decoded})

    def snapshot(self) -> list[dict[str, Any]]:
        with self.lock:
            return list(self.requests)


def assert_upstream_auth(state: UpstreamState) -> dict[str, Any]:
    expected_header, expected_value = {
        "anthropic": ("x-api-key", "sdk-upstream-secret"),
        "gemini": ("x-goog-api-key", "sdk-upstream-secret"),
        "openai": ("authorization", "Bearer sdk-upstream-secret"),
    }[state.protocol]
    requests = state.snapshot()
    require(bool(requests), f"{state.protocol} authentication check received no requests")
    credential_headers = {"authorization", "x-api-key", "x-goog-api-key", "api-key", "proxy-authorization"}
    for index, request in enumerate(requests):
        observed = [(name.lower(), value) for name, value in request["header_items"]
                    if name.lower() in credential_headers]
        require(observed == [(expected_header, expected_value)],
                f"{state.protocol} upstream authentication mismatch on request {index}: "
                f"expected exactly one {expected_header} header with the fixture API-key scheme")
    return {"protocol": state.protocol, "credential_kind": "api_key",
            "header": expected_header, "requests_checked": len(requests)}


class UpstreamHandler(BaseHTTPRequestHandler):
    server: "UpstreamHTTPServer"

    def log_message(self, *_: Any) -> None:
        return

    def _body(self) -> bytes:
        length = int(self.headers.get("Content-Length", "0") or "0")
        return self.rfile.read(min(length, 16 * 1024 * 1024))

    def _send_json(self, value: Any, status: int = 200) -> None:
        raw = json.dumps(value, separators=(",", ":")).encode()
        self.send_response(status)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(raw)))
        self.end_headers()
        self.wfile.write(raw)

    def do_GET(self) -> None:
        body = self._body()
        self.server.state.add("GET", self.path, self.headers, body)
        if "/files" in self.path:
            if self.path.rstrip("/").endswith("/files"):
                self._send_json({"object": "list", "data": [{"id": "file-sdk-proof", "object": "file", "bytes": 3, "filename": "proof.txt", "purpose": "assistants"}]})
            else:
                self._send_json({"id": "file-sdk-proof", "object": "file", "bytes": 3, "filename": "proof.txt", "purpose": "assistants"})
            return
        if "/responses/" in self.path:
            self._send_json({"id": "resp-native-proof", "object": "response", "status": "completed", "model": "fixture-chat", "output": [{"type": "message", "role": "assistant", "content": [{"type": "output_text", "text": "OK"}]}], "usage": {"input_tokens": 9, "output_tokens": 2, "total_tokens": 11}})
            return
        self._send_json({"data": [{"id": "fixture-chat", "object": "model"}]})

    def do_DELETE(self) -> None:
        body = self._body()
        self.server.state.add("DELETE", self.path, self.headers, body)
        self._send_json({"id": "file-sdk-proof", "deleted": True})

    def do_POST(self) -> None:
        body = self._body()
        self.server.state.add("POST", self.path, self.headers, body)
        try:
            payload = json.loads(body) if body else {}
        except Exception:
            payload = {}
        stream = bool(payload.get("stream")) if isinstance(payload, dict) else False
        if "/files" in self.path:
            self._send_json({"id": "file-sdk-proof", "object": "file", "bytes": 3, "filename": "proof.txt", "purpose": "assistants"})
            return
        if self.server.state.protocol == "openai" and "/responses" in self.path:
            self._send_json({"id": "resp-native-proof", "object": "response", "status": "completed", "model": payload.get("model", "fixture-chat"), "output": [{"type": "message", "role": "assistant", "content": [{"type": "output_text", "text": "OK"}]}], "usage": {"input_tokens": 9, "output_tokens": 2, "total_tokens": 11}})
            return
        if getattr(self.server.state, "qualification", False):
            self._qualification_response(payload, stream or ":streamGenerateContent" in self.path)
            return
        if self.server.state.protocol == "anthropic" or "/messages" in self.path:
            if stream:
                frames = [
                    'event: message_start\ndata: {"type":"message_start","message":{"id":"msg_sdk","type":"message","role":"assistant","model":"fixture-chat","content":[],"usage":{"input_tokens":9,"output_tokens":0}}}\n\n',
                    'event: content_block_start\ndata: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}\n\n',
                    'event: content_block_delta\ndata: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"O"}}\n\n',
                    'event: content_block_delta\ndata: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"K"}}\n\n',
                    'event: content_block_stop\ndata: {"type":"content_block_stop","index":0}\n\n',
                    'event: message_delta\ndata: {"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"output_tokens":2}}\n\n',
                    'event: message_stop\ndata: {"type":"message_stop"}\n\n',
                ]
                raw = "".join(frames).encode()
                self.send_response(200)
                self.send_header("Content-Type", "text/event-stream")
                self.send_header("Content-Length", str(len(raw)))
                self.end_headers()
                self.wfile.write(raw)
                return
            self._send_json({"id": "msg_sdk", "type": "message", "role": "assistant", "model": "fixture-chat", "content": [{"type": "text", "text": "OK"}], "stop_reason": "end_turn", "stop_sequence": None, "usage": {"input_tokens": 9, "output_tokens": 2}})
            return
        # Original OpenAI-compatible baseline fixture.
        if stream:
            raw = b'data: {"id":"chat_sdk","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":"OK"},"finish_reason":null}]}\n\ndata: [DONE]\n\n'
            self.send_response(200)
            self.send_header("Content-Type", "text/event-stream")
            self.send_header("Content-Length", str(len(raw)))
            self.end_headers()
            self.wfile.write(raw)
        else:
            self._send_json({"id": "chat_sdk", "object": "chat.completion", "model": "fixture-chat", "choices": [{"index": 0, "message": {"role": "assistant", "content": "OK"}, "finish_reason": "stop"}], "usage": {"prompt_tokens": 9, "completion_tokens": 2, "total_tokens": 11}})

    def _send_sse(self, frames: list[tuple[str, Any]]) -> None:
        raw = "".join(
            (f"event: {event}\n" if event else "")
            + "data: " + (data if isinstance(data, str) else json.dumps(data, separators=(",", ":")))
            + "\n\n" for event, data in frames
        ).encode()
        self.send_response(200)
        self.send_header("Content-Type", "text/event-stream")
        self.send_header("Content-Length", str(len(raw)))
        self.end_headers()
        self.wfile.write(raw)
        self.wfile.flush()

    def _qualification_response(self, payload: dict[str, Any], stream: bool) -> None:
        protocol = self.server.state.protocol
        tools = bool(payload.get("tools"))
        text = "Ordered tools." if tools else "Image received."
        blocks = [{"type": "text", "text": text}]
        if tools:
            blocks.extend({"type": "tool_use", "id": call_id, "name": name, "input": args}
                          for call_id, name, args in QUAL_CALLS)
        if protocol == "anthropic":
            result = {"id": "msg_qualification", "type": "message", "role": "assistant",
                      "model": "fixture-chat", "content": blocks,
                      "stop_reason": "tool_use" if tools else "end_turn", "stop_sequence": None,
                      "usage": {"input_tokens": 19, "output_tokens": 7}}
            if not stream:
                self._send_json(result)
                return
            frames = [("message_start", {"type": "message_start", "message": {
                **result, "content": [], "stop_reason": None,
                "usage": {"input_tokens": 19, "output_tokens": 0}}})]
            for index, block in enumerate(blocks):
                start = {**block, "text": ""} if block["type"] == "text" else {**block, "input": {}}
                frames.append(("content_block_start", {"type": "content_block_start", "index": index, "content_block": start}))
                fragments = [text[:6], text[6:]] if index == 0 else argument_fragments(block["input"])
                for fragment in fragments:
                    delta = {"type": "text_delta", "text": fragment} if index == 0 else {"type": "input_json_delta", "partial_json": fragment}
                    frames.append(("content_block_delta", {"type": "content_block_delta", "index": index, "delta": delta}))
                frames.append(("content_block_stop", {"type": "content_block_stop", "index": index}))
            frames.extend([
                ("message_delta", {"type": "message_delta", "delta": {"stop_reason": result["stop_reason"], "stop_sequence": None}, "usage": {"output_tokens": 7}}),
                ("message_stop", {"type": "message_stop"})])
            self._send_sse(frames)
            return
        if protocol == "gemini":
            parts = [{"text": text}]
            if tools:
                parts.extend({"functionCall": {"id": call_id, "name": name, "args": args}}
                             for call_id, name, args in QUAL_CALLS)
            def envelope(items: list[dict[str, Any]], terminal: bool = False) -> dict[str, Any]:
                candidate = {"index": 0, "content": {"role": "model", "parts": items}}
                value = {"responseId": "gemini_qualification", "modelVersion": "fixture-chat", "candidates": [candidate]}
                if terminal:
                    candidate["finishReason"] = "STOP"
                    value["usageMetadata"] = {"promptTokenCount": 19, "candidatesTokenCount": 7, "totalTokenCount": 26}
                return value
            if not stream:
                self._send_json(envelope(parts, True))
                return
            # Gemini functionCall.args is an object, never an OpenAI argument-string
            # delta. Fragment text and deliver each complete call in its own SSE
            # envelope; fake partial JSON args would not be a Gemini wire fixture.
            frames = [("", envelope([{"text": text[:6]}])), ("", envelope([{"text": text[6:]}]))]
            frames.extend(("", envelope([part])) for part in parts[1:])
            frames.append(("", envelope([], True)))
            self._send_sse(frames)
            return
        calls = [{"id": call_id, "type": "function", "function": {"name": name, "arguments": json.dumps(args, separators=(",", ":"))}}
                 for call_id, name, args in QUAL_CALLS] if tools else []
        reason = "tool_calls" if tools else "stop"
        usage = {"prompt_tokens": 19, "completion_tokens": 7, "total_tokens": 26}
        if not stream:
            message = {"role": "assistant", "content": text}
            if tools:
                message["tool_calls"] = calls
            self._send_json({"id": "chat_qualification", "object": "chat.completion", "created": 1, "model": "fixture-chat",
                             "choices": [{"index": 0, "message": message, "finish_reason": reason}], "usage": usage})
            return
        def chunk(delta: dict[str, Any], finish: str | None = None) -> dict[str, Any]:
            return {"id": "chat_qualification", "object": "chat.completion.chunk", "created": 1, "model": "fixture-chat",
                    "choices": [{"index": 0, "delta": delta, "finish_reason": finish}]}
        frames = [("", chunk({"role": "assistant", "content": text[:6]})), ("", chunk({"content": text[6:]}))]
        for index, call in enumerate(calls):
            frames.append(("", chunk({"tool_calls": [{"index": index, "id": call["id"], "type": "function",
                                                       "function": {"name": call["function"]["name"], "arguments": ""}}]})))
            for fragment in argument_fragments(QUAL_CALLS[index][2]):
                frames.append(("", chunk({"tool_calls": [{"index": index, "function": {"arguments": fragment}}]})))
        terminal = chunk({}, reason)
        terminal["usage"] = usage
        frames.extend([("", terminal), ("", "[DONE]")])
        self._send_sse(frames)

class UpstreamHTTPServer(ThreadingHTTPServer):
    def __init__(self, state: UpstreamState):
        super().__init__(("127.0.0.1", 0), UpstreamHandler)
        self.state = state


def start_upstream(protocol: str, qualification: bool = False) -> UpstreamHTTPServer:
    server = UpstreamHTTPServer(UpstreamState(protocol, qualification))
    threading.Thread(target=server.serve_forever, name=f"sdk-upstream-{protocol}", daemon=True).start()
    return server


def admin(method: str, path: str, body: Any | None = None) -> Any:
    require(MANAGEMENT and COOKIE and CSRF, "management origin, session cookie, and CSRF token are required")
    headers = {"Accept": "application/json", "Cookie": COOKIE if "=" in COOKIE else "hoorific_session=" + COOKIE,
               "Origin": MANAGEMENT, "X-CSRF-Token": CSRF}
    raw = None
    if body is not None:
        raw = json.dumps(body, separators=(",", ":")).encode()
        headers["Content-Type"] = "application/json"
    if path.endswith("/import"):
        headers["If-Match"] = "1"
    request = Request(MANAGEMENT + path, data=raw, headers=headers, method=method)
    try:
        with urlopen(request, timeout=15) as response:
            data = response.read(4 * 1024 * 1024)
            return json.loads(data) if data else {}
    except HTTPError as exc:
        detail = exc.read(2000).decode("utf-8", "replace")
        raise RuntimeError(f"admin {method} {path} returned {exc.code}: {detail}") from exc
    except (URLError, TimeoutError) as exc:
        raise RuntimeError(f"admin {method} {path} failed: {exc}") from exc


def provision(server: UpstreamHTTPServer, connector: str, alias: str, suffix: str, tenant: str, qualification: bool = False) -> tuple[str, str]:
    connection = f"sdk-{connector}-{suffix}"
    model = f"sdk-model-{connector}-{suffix}"
    base = f"http://127.0.0.1:{server.server_port}"
    features = {"streaming": "supported"}
    if qualification:
        features.update({"custom_tools": "supported", "parallel_tools": "supported"})
    resources = [
        ("connections", connection, {"connector": connector, "account_id": connection + "-account", "base_url": base, "dedicated": True, "enabled": True, "settings": {"allow_private": "true", "allowed_cidrs": "127.0.0.1/32"}}),
        ("models", model, {"connection_id": connection, "upstream_id": "fixture-chat", "operations": ["generate", "file", "response.resource"], "input_modalities": ["text", "image"] if qualification else ["text"], "output_modalities": ["text"], "features": features, "context_limit": 32768, "output_limit": 1024, "provenance": "SDK qualification upstream", "enabled": True}),
        ("model_aliases", alias, {"model_ids": [model], "description": "SDK qualification alias", "enabled": True}),
        ("route_policies", alias, {"alias": alias, "targets": [{"connection_id": connection, "model_id": model, "priority": 1, "weight": 100}], "fallback": False, "affinity": False}),
    ]
    for kind, rid, data in resources:
        admin("POST", f"/admin/api/v1/{kind}", {"id": rid, "data": data})
    admin("POST", f"/admin/api/v1/connections/{connection}/import", {"data": {"provider": connector, "account_id": connection + "-account", "kind": "api_key", "secret": "sdk-upstream-secret"}})
    key_id = f"sdk-key-{suffix}"
    key_data = {"name": "SDK qualification key", "role": "operator", "permissions": ["inference:invoke", "connection:" + connection], "aliases": [alias], "connections": [connection], "operations": ["generate", "file", "response.resource"], "portable": True, "native_account": True, "realtime": False}
    issued = admin("POST", f"/admin/api/v1/api_keys/{key_id}/issue", {"data": key_data})
    token = issued.get("token") or issued.get("secret") or issued.get("data", {}).get("token")
    require(isinstance(token, str) and token, "API key issue returned no token")
    return connection, token


def gateway_request(method: str, path: str, key: str, body: Any | None = None) -> tuple[int, bytes]:
    raw = json.dumps(body).encode() if body is not None else None
    req = Request(INFERENCE + path, data=raw, method=method, headers={"Authorization": "Bearer " + key, "Content-Type": "application/json"})
    try:
        with urlopen(req, timeout=15) as response:
            return response.status, response.read(2 * 1024 * 1024)
    except HTTPError as exc:
        return exc.code, exc.read(2 * 1024 * 1024)


def text_of(value: Any) -> str:
    return str(value or "").strip()


def main() -> int:
    started = time.monotonic()
    if not INFERENCE:
        record("setup", "failed", "HOORIFIC_VERIFY_INFERENCE is required", started)
        print(json.dumps(results, indent=2))
        return 2
    # Imports are intentionally mandatory: a missing official SDK is a failed qualification.
    try:
        import anthropic  # type: ignore
        import openai  # type: ignore
        from google import genai  # type: ignore
        from google.genai import types as genai_types  # type: ignore
    except Exception as exc:
        record("setup", "failed", f"official SDK import failed; install requirements.txt: {exc}", started)
        print(json.dumps(results, indent=2))
        return 2

    servers: list[UpstreamHTTPServer] = []
    try:
        # Baseline fixtures and isolated protocol-native qualification fixtures.
        anthropic_upstream = start_upstream("anthropic")
        openai_upstream = start_upstream("openai")
        openai_qualification = start_upstream("openai", True)
        gemini_qualification = start_upstream("gemini", True)
        servers.extend([anthropic_upstream, openai_upstream, openai_qualification, gemini_qualification])
        session = admin("GET", "/admin/api/v1/session")
        principal = session.get("principal", {}) if isinstance(session, dict) else {}
        tenant = principal.get("tenant_id", "") if isinstance(principal, dict) else ""
        suffix = uuid.uuid4().hex[:10]
        reverse_alias = f"sdk-reverse-{suffix}"
        native_alias = f"sdk-native-{suffix}"
        anthropic_openai_alias = f"sdk-anthropic-openai-{suffix}"
        openai_gemini_alias = f"sdk-openai-gemini-{suffix}"
        anthropic_gemini_alias = f"sdk-anthropic-gemini-{suffix}"
        reverse_connection, sdk_key = provision(anthropic_upstream, "anthropic", reverse_alias, suffix + "r", tenant)
        native_connection, native_key = provision(openai_upstream, "openai", native_alias, suffix + "n", tenant)
        _, anthropic_openai_key = provision(openai_qualification, "openai", anthropic_openai_alias, suffix + "ao", tenant, True)
        _, openai_gemini_key = provision(gemini_qualification, "gemini", openai_gemini_alias, suffix + "og", tenant, True)
        _, anthropic_gemini_key = provision(gemini_qualification, "gemini", anthropic_gemini_alias, suffix + "ag", tenant, True)
        # Portable requests use the alias/connection grants issued above.
        # The baseline harness key intentionally cannot access this fresh graph.
        key = sdk_key
        native = native_key
        record("setup", "passed", "official SDKs imported; local upstreams registered through admin API", started,
               {"reverse_connection": reverse_connection, "native_connection": native_connection, "issued_key": True})

        portable_openai = openai.OpenAI(api_key=key, base_url=INFERENCE + "/v1")
        portable_anthropic = anthropic.Anthropic(api_key=key, base_url=INFERENCE)
        # google-genai accepts an origin and appends its documented v1beta paths.
        portable_google = genai.Client(api_key=key, http_options=genai_types.HttpOptions(base_url=INFERENCE))
        native_openai = openai.OpenAI(api_key=native, base_url=INFERENCE + f"/connect/{native_connection}/openai/v1")
        native_openai_resources = openai.OpenAI(api_key=native, base_url=INFERENCE + f"/connect/{native_connection}/openai")
        anthropic_openai = anthropic.Anthropic(api_key=anthropic_openai_key, base_url=INFERENCE)
        openai_gemini = openai.OpenAI(api_key=openai_gemini_key, base_url=INFERENCE + "/v1")
        anthropic_gemini = anthropic.Anthropic(api_key=anthropic_gemini_key, base_url=INFERENCE)

        def qualified_requests(state: UpstreamState, boundary: int, expected_path: str, expected_query: str = "") -> list[dict[str, Any]]:
            seen = state.since(boundary)
            require(len(seen) == 1, f"expected one fresh upstream request, got {len(seen)}")
            actual = urlsplit(seen[0]["path"])
            require(actual.path == expected_path and actual.query == expected_query,
                    f"unexpected upstream descriptor {actual.path!r}?{actual.query!r}")
            return seen

        def assert_openai_tool_request(seen: list[dict[str, Any]]) -> None:
            body = seen[0]["body"]
            require([x["function"]["name"] for x in body["tools"]] == ["get_weather", "get_time"], "OpenAI tool ordering changed")
            require([m["role"] for m in body["messages"]] == ["user"], "OpenAI message ordering changed")
            require(body["messages"][0]["content"] == [{"type": "text", "text": "Call both tools"}], "OpenAI user content changed")

        def assert_gemini_tool_request(seen: list[dict[str, Any]]) -> None:
            body = seen[0]["body"]
            require(body["contents"][0]["role"] == "user", "Gemini content role changed")
            require(body["contents"][0]["parts"][0]["text"] == "Call weather", "Gemini text ordering changed")

        def assert_gemini_image_request(seen: list[dict[str, Any]]) -> None:
            body = seen[0]["body"]
            parts = body["contents"][0]["parts"]
            require(parts[0]["text"] == "Describe this image", "Gemini image text order changed")
            require(parts[1]["inlineData"] == {"mimeType": "image/png", "data": "AQID"}, "Gemini inlineData representation changed")

        def openai_unary() -> dict[str, Any]:
            r = portable_openai.chat.completions.create(model=reverse_alias, messages=[{"role": "user", "content": "Reply exactly OK"}], max_tokens=16)
            require(text_of(r.choices[0].message.content) == "OK", "OpenAI unary content was not OK")
            require(text_of(r.choices[0].finish_reason) == "stop", "OpenAI unary finish reason was not stop")
            require(int(r.usage.prompt_tokens) == 9 and int(r.usage.completion_tokens) == 2, "OpenAI usage was not translated")
            return {"text": r.choices[0].message.content, "finish_reason": r.choices[0].finish_reason, "usage": r.usage.model_dump()}
        run_check("openai/unary/reverse", openai_unary)

        def openai_stream() -> dict[str, Any]:
            stream = portable_openai.chat.completions.create(model=reverse_alias, messages=[{"role": "user", "content": "Reply exactly OK"}], max_tokens=16, stream=True)
            text = "".join(text_of(chunk.choices[0].delta.content) for chunk in stream if chunk.choices and chunk.choices[0].delta.content)
            require(text == "OK", f"OpenAI stream content was {text!r}")
            return {"text": text, "terminal": True}
        run_check("openai/stream/reverse", openai_stream)

        def anthropic_unary() -> dict[str, Any]:
            r = portable_anthropic.messages.create(model=reverse_alias, max_tokens=16, messages=[{"role": "user", "content": "Reply exactly OK"}])
            require(text_of(r.content[0].text) == "OK", "Anthropic unary content was not OK")
            require(text_of(r.stop_reason) == "end_turn", "Anthropic stop reason was not end_turn")
            require(int(r.usage.input_tokens) == 9 and int(r.usage.output_tokens) == 2, "Anthropic usage was not preserved")
            return {"text": r.content[0].text, "stop_reason": r.stop_reason, "usage": r.usage.model_dump()}
        run_check("anthropic/unary", anthropic_unary)

        def anthropic_stream() -> dict[str, Any]:
            with portable_anthropic.messages.stream(model=reverse_alias, max_tokens=16, messages=[{"role": "user", "content": "Reply exactly OK"}]) as stream:
                text = "".join(stream.text_stream)
            require(text == "OK", f"Anthropic stream content was {text!r}")
            return {"text": text, "terminal": True}
        run_check("anthropic/stream", anthropic_stream)

        def google_unary() -> dict[str, Any]:
            r = portable_google.models.generate_content(model=reverse_alias, contents="Reply exactly OK")
            require(text_of(r.text) == "OK", f"Google unary content was {r.text!r}")
            return {"text": r.text}
        run_check("google/unary/reverse", google_unary)

        def google_stream() -> dict[str, Any]:
            chunks = portable_google.models.generate_content_stream(model=reverse_alias, contents="Reply exactly OK")
            text = "".join(text_of(chunk.text) for chunk in chunks)
            require(text == "OK", f"Google stream content was {text!r}")
            return {"text": text, "terminal": True}
        run_check("google/stream/reverse", google_stream)

        def responses_stateless() -> dict[str, Any]:
            r = portable_openai.responses.create(model=reverse_alias, input="Reply exactly OK", store=False)
            require(text_of(r.output_text) == "OK", "Responses output_text was not OK")
            require(text_of(r.status) in ("completed", ""), "Responses status was not completed")
            return {"output_text": r.output_text, "status": r.status, "store": False}
        run_check("openai/responses/stateless", responses_stateless)

        def native_response_and_field() -> dict[str, Any]:
            native_model = f"sdk-model-openai-{suffix}n"
            r = native_openai.responses.create(model=native_model, input="Reply exactly OK", store=False, metadata={"sdk_proof": "native-field"})
            require(text_of(r.output_text) == "OK", "native Responses output_text was not OK")
            seen = [x for x in openai_upstream.state.snapshot() if x["path"].endswith("/responses")]
            require(seen and seen[-1]["body"].get("metadata", {}).get("sdk_proof") == "native-field", "native metadata was not preserved upstream")
            return {"output_text": r.output_text, "upstream_path": seen[-1]["path"], "native_field": "sdk_proof"}
        run_check("openai/native/responses-field", native_response_and_field)

        def native_resources() -> dict[str, Any]:
            f = native_openai_resources.files.create(file=("proof.txt", b"abc"), purpose="assistants")
            require(text_of(f.id) == "file-sdk-proof", "native file create returned wrong id")
            got = native_openai_resources.files.retrieve(f.id)
            require(text_of(got.id) == f.id, "native file retrieve returned wrong id")
            listed = native_openai_resources.files.list()
            require(any(text_of(item.id) == f.id for item in listed.data), "native file list omitted created file")
            deleted = native_openai_resources.files.delete(f.id)
            require(bool(deleted.deleted), "native file delete was not acknowledged")
            return {"file_id": f.id, "retrieved": got.id, "listed": len(listed.data), "deleted": deleted.deleted}
        run_check("openai/native/file-resource-lifecycle", native_resources)

        def anthropic_to_openai_tools() -> dict[str, Any]:
            boundary = openai_qualification.state.boundary()
            r = anthropic_openai.messages.create(model=anthropic_openai_alias, max_tokens=64,
                messages=[{"role": "user", "content": "Call both tools"}],
                tools=[{"name": "get_weather", "description": "Weather", "input_schema": {"type": "object", "properties": {"city": {"type": "string"}, "unit": {"type": "string"}}}},
                       {"name": "get_time", "description": "Time", "input_schema": {"type": "object", "properties": {"timezone": {"type": "string"}}}}])
            seen = qualified_requests(openai_qualification.state, boundary, "/chat/completions")
            assert_openai_tool_request(seen)
            require([getattr(x, "type", "") for x in r.content] == ["text", "tool_use", "tool_use"], "Anthropic tool block ordering changed")
            require(r.content[0].text == "Ordered tools.", "Anthropic tool text changed")
            require(r.content[1].name == "get_weather" and r.content[1].input == QUAL_CALLS[0][2], "first Anthropic tool changed")
            require(r.content[2].name == "get_time" and r.content[2].input == QUAL_CALLS[1][2], "second Anthropic tool changed")
            require(text_of(r.stop_reason) == "tool_use", "Anthropic tool stop reason changed")
            require(int(r.usage.input_tokens) == 19 and int(r.usage.output_tokens) == 7, "Anthropic tool usage changed")
            return {"blocks": [x.type for x in r.content], "stop_reason": r.stop_reason, "usage": r.usage.model_dump()}
        run_check("anthropic/unary/tools-to-openai", anthropic_to_openai_tools)

        def anthropic_to_openai_tools_stream() -> dict[str, Any]:
            boundary = openai_qualification.state.boundary()
            with anthropic_openai.messages.stream(model=anthropic_openai_alias, max_tokens=64,
                    messages=[{"role": "user", "content": "Call both tools"}],
                    tools=[{"name": "get_weather", "input_schema": {"type": "object"}},
                           {"name": "get_time", "input_schema": {"type": "object"}}]) as stream:
                r = stream.get_final_message()
            seen = qualified_requests(openai_qualification.state, boundary, "/chat/completions")
            assert_openai_tool_request(seen)
            require([getattr(x, "type", "") for x in r.content] == ["text", "tool_use", "tool_use"], "Anthropic stream ordering changed")
            require([x.input for x in r.content[1:]] == [x[2] for x in QUAL_CALLS], "Anthropic stream arguments changed")
            require(text_of(r.stop_reason) == "tool_use", "Anthropic stream stop reason changed")
            require(int(r.usage.input_tokens) == 19 and int(r.usage.output_tokens) == 7, "Anthropic stream usage changed")
            return {"blocks": [x.type for x in r.content], "stop_reason": r.stop_reason}
        run_check("anthropic/stream/tools-to-openai", anthropic_to_openai_tools_stream)

        def openai_to_gemini_tools() -> dict[str, Any]:
            boundary = gemini_qualification.state.boundary()
            r = openai_gemini.chat.completions.create(model=openai_gemini_alias, messages=[{"role": "user", "content": "Call weather"}],
                tools=[{"type": "function", "function": {"name": "get_weather", "description": "Weather", "parameters": {"type": "object", "properties": {"city": {"type": "string"}, "unit": {"type": "string"}}}}},
                       {"type": "function", "function": {"name": "get_time", "description": "Time", "parameters": {"type": "object", "properties": {"timezone": {"type": "string"}}}}}], max_tokens=64)
            seen = qualified_requests(gemini_qualification.state, boundary, "/v1beta/models/fixture-chat:generateContent")
            assert_gemini_tool_request(seen)
            calls = r.choices[0].message.tool_calls or []
            require(r.choices[0].message.content == "Ordered tools.", "OpenAI Gemini text changed")
            require([call.function.name for call in calls] == [item[1] for item in QUAL_CALLS],
                    f"OpenAI Gemini tool ordering changed: observed names={[call.function.name for call in calls]!r}")
            require([json.loads(call.function.arguments) for call in calls] == [item[2] for item in QUAL_CALLS],
                    f"OpenAI Gemini tool arguments changed: observed arguments={[call.function.arguments for call in calls]!r}")
            require(text_of(r.choices[0].finish_reason) == "tool_calls", "OpenAI Gemini stop reason changed")
            require(int(r.usage.prompt_tokens) == 19 and int(r.usage.completion_tokens) == 7, "OpenAI Gemini usage changed")
            return {"text": r.choices[0].message.content, "tools": [call.function.name for call in calls],
                    "arguments": [call.function.arguments for call in calls], "finish_reason": r.choices[0].finish_reason}
        run_check("openai/unary/tools-to-gemini", openai_to_gemini_tools)

        def openai_to_gemini_tools_stream() -> dict[str, Any]:
            boundary = gemini_qualification.state.boundary()
            stream = openai_gemini.chat.completions.create(model=openai_gemini_alias, messages=[{"role": "user", "content": "Call weather"}],
                tools=[{"type": "function", "function": {"name": "get_weather", "parameters": {"type": "object", "properties": {"city": {"type": "string"}}}}},
                        {"type": "function", "function": {"name": "get_time", "parameters": {"type": "object", "properties": {"timezone": {"type": "string"}}}}}],
                max_tokens=64, stream=True, stream_options={"include_usage": True})
            text = ""
            names: dict[int, str] = {}
            arguments: dict[int, str] = {}
            usage = None
            finish = ""
            for chunk in stream:
                usage = chunk.usage or usage
                if not chunk.choices:
                    continue
                delta = chunk.choices[0].delta
                text += text_of(delta.content)
                for call in delta.tool_calls or []:
                    index = int(call.index)
                    names[index] = names.get(index, "") + text_of(call.function.name)
                    arguments[index] = arguments.get(index, "") + text_of(call.function.arguments)
                finish = text_of(chunk.choices[0].finish_reason) or finish
            seen = qualified_requests(gemini_qualification.state, boundary, "/v1beta/models/fixture-chat:streamGenerateContent", "alt=sse")
            assert_gemini_tool_request(seen)
            ordered_indices = sorted(names)
            observed_names = [names[index] for index in ordered_indices]
            observed_arguments = [arguments[index] for index in ordered_indices]
            require(text == "Ordered tools." and observed_names == [item[1] for item in QUAL_CALLS],
                    f"OpenAI Gemini stream ordering changed: observed text={text!r} names={observed_names!r} arguments={observed_arguments!r}")
            require([json.loads(value) for value in observed_arguments] == [item[2] for item in QUAL_CALLS],
                    f"OpenAI Gemini stream arguments changed: observed arguments={observed_arguments!r}")
            require(usage is not None and int(usage.prompt_tokens) == 19 and int(usage.completion_tokens) == 7, "OpenAI Gemini stream usage changed")
            require(finish == "tool_calls", "OpenAI Gemini stream stop reason changed")
            return {"text": text, "tools": observed_names, "arguments": observed_arguments, "finish_reason": finish}
        run_check("openai/stream/tools-to-gemini", openai_to_gemini_tools_stream)

        def anthropic_to_gemini_vision(stream: bool = False) -> dict[str, Any]:
            boundary = gemini_qualification.state.boundary()
            message = [{"role": "user", "content": [{"type": "text", "text": "Describe this image"},
                {"type": "image", "source": {"type": "base64", "media_type": "image/png", "data": "AQID"}}]}]
            if stream:
                with anthropic_gemini.messages.stream(model=anthropic_gemini_alias, max_tokens=32, messages=message) as result:
                    final = result.get_final_message()
            else:
                final = anthropic_gemini.messages.create(model=anthropic_gemini_alias, max_tokens=32, messages=message)
            seen = qualified_requests(gemini_qualification.state, boundary,
                                      "/v1beta/models/fixture-chat:streamGenerateContent" if stream else "/v1beta/models/fixture-chat:generateContent",
                                      "alt=sse" if stream else "")
            assert_gemini_image_request(seen)
            require(final.content[0].text == "Image received.", "Anthropic Gemini vision text changed")
            require(text_of(final.stop_reason) == "end_turn", "Anthropic Gemini vision stop reason changed")
            require(final.usage.input_tokens == 19 and final.usage.output_tokens == 7, "Anthropic Gemini vision usage changed")
            return {"text": final.content[0].text, "stop_reason": final.stop_reason, "usage": final.usage.model_dump(), "stream": stream}
        run_check("anthropic/unary/vision-to-gemini", lambda: anthropic_to_gemini_vision(False))
        run_check("anthropic/stream/vision-to-gemini", lambda: anthropic_to_gemini_vision(True))

        def operation_denial() -> dict[str, Any]:
            status, body = gateway_request("POST", "/v1/embeddings", key, {"model": reverse_alias, "input": "x"})
            require(400 <= status < 500, f"unapproved embeddings operation returned {status}")
            return {"status": status, "error_bytes": len(body)}
        run_check("operation/unapproved-denial", operation_denial)

        for label, upstream in (
            ("anthropic", anthropic_upstream),
            ("openai-native", openai_upstream),
            ("openai-reverse", openai_qualification),
            ("gemini-reverse", gemini_qualification),
        ):
            run_check("upstream/auth/" + label, lambda state=upstream.state: assert_upstream_auth(state))

        reverse_seen = anthropic_upstream.state.snapshot()
        native_seen = openai_upstream.state.snapshot()
        require(reverse_seen, "reverse upstream received no requests")
        require(native_seen, "native upstream received no requests")
        record("upstream/evidence", "passed", "upstreams observed translated and native requests; SDK source cases are individually reported above.", time.monotonic(),
               {"reverse_requests": len(reverse_seen), "native_requests": len(native_seen), "reverse_paths": sorted({x["path"] for x in reverse_seen}), "native_paths": sorted({x["path"] for x in native_seen}), "implemented_cases": ["anthropic/unary/tools-to-openai", "anthropic/stream/tools-to-openai", "openai/unary/tools-to-gemini", "openai/stream/tools-to-gemini", "anthropic/unary/vision-to-gemini", "anthropic/stream/vision-to-gemini"]})
        record("coverage/browser-scope", "not-run",
               "Browser-console workflow proof is intentionally separate; run browser_runner.py with the isolated fixture storage state.",
               time.monotonic(), {"remaining_gaps": ["browser console workflow proof"],
                                  "runner": "tools/verify/sdk/browser_runner.py"})
    except Exception as exc:
        record("setup/provisioning", "failed", f"{type(exc).__name__}: {exc}", started)
        if os.environ.get("HOORIFIC_VERIFY_DEBUG"):
            traceback.print_exc(file=sys.stderr)
    finally:
        for server in servers:
            server.shutdown()
            server.server_close()

    print(json.dumps(results, indent=2, default=str))
    return 1 if any(item["Status"] == "failed" for item in results) else 0


if __name__ == "__main__":
    raise SystemExit(main())
