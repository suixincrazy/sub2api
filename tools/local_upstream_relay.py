"""Narrow HTTP relay for an upstream that only accepts this machine's egress IP."""

from __future__ import annotations

import json
import os
import sys
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from urllib.parse import urlsplit

import httpx


UPSTREAM_ORIGIN = os.environ.get("UPSTREAM_ORIGIN", "https://ps.air-outer.com").rstrip("/")
RELAY_TOKEN = os.environ["RELAY_TOKEN"]
LISTEN_HOST = os.environ.get("RELAY_HOST", "127.0.0.1")
LISTEN_PORT = int(os.environ.get("RELAY_PORT", "18080"))
CODEX_VERSION = os.environ.get("CODEX_VERSION", "0.148.0-alpha.9")
CODEX_USER_AGENT = os.environ.get("CODEX_USER_AGENT", f"codex_cli_rs/{CODEX_VERSION}")
CLAUDE_USER_AGENT = os.environ.get(
    "CLAUDE_USER_AGENT", "claude-cli/2.1.233 (external, claude-vscode)"
)
RELAY_PREFIX = f"/relay/{RELAY_TOKEN}"

HOP_BY_HOP_HEADERS = {
    "connection",
    "keep-alive",
    "proxy-authenticate",
    "proxy-authorization",
    "te",
    "trailer",
    "transfer-encoding",
    "upgrade",
}


class RelayHandler(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"
    server_version = "sub2api-local-relay/1"

    def _json_error(self, status: int, message: str) -> None:
        body = json.dumps({"error": {"message": message, "type": "relay_error"}}).encode()
        self.send_response(status)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.send_header("Connection", "close")
        self.end_headers()
        self.wfile.write(body)
        self.close_connection = True

    def _relay(self) -> None:
        request_path = urlsplit(self.path)
        if not request_path.path.startswith(RELAY_PREFIX + "/"):
            self._json_error(404, "not found")
            return

        upstream_path = request_path.path[len(RELAY_PREFIX) :]
        target = UPSTREAM_ORIGIN + upstream_path
        if request_path.query:
            target += "?" + request_path.query

        content_length = int(self.headers.get("Content-Length", "0"))
        body = self.rfile.read(content_length) if content_length else None
        incoming_header_names = {name.lower() for name in self.headers}
        headers = httpx.Headers(
            {
                name: value
                for name, value in self.headers.items()
                if name.lower() not in HOP_BY_HOP_HEADERS
                and name.lower() not in {"host", "content-length", "user-agent"}
            }
        )
        headers["Accept-Encoding"] = "identity"

        is_anthropic = (
            "x-api-key" in incoming_header_names
            or "anthropic-version" in incoming_header_names
            or upstream_path == "/v1/messages"
            or upstream_path.startswith("/v1/messages/")
        )
        if is_anthropic:
            headers.update(
                {
                    "User-Agent": CLAUDE_USER_AGENT,
                    "anthropic-version": "2023-06-01",
                    "anthropic-dangerous-direct-browser-access": "true",
                    "x-app": "cli",
                    "x-stainless-arch": "x64",
                    "x-stainless-lang": "js",
                    "x-stainless-os": "Windows",
                    "x-stainless-package-version": "0.112.1",
                    "x-stainless-runtime": "node",
                    "x-stainless-runtime-version": "v26.3.0",
                }
            )
        else:
            headers.update(
                {
                    "User-Agent": CODEX_USER_AGENT,
                    "originator": "codex_cli_rs",
                    "version": CODEX_VERSION,
                    "OpenAI-Beta": "responses=experimental",
                }
            )
            headers.setdefault(
                "X-Codex-Window-ID", "sub2api-local-relay-00000000000000000000"
            )

        try:
            timeout = httpx.Timeout(connect=30, read=None, write=60, pool=60)
            with httpx.Client(timeout=timeout, follow_redirects=False, trust_env=False) as client:
                with client.stream(self.command, target, headers=headers, content=body) as response:
                    self.send_response(response.status_code)
                    for name, value in response.headers.multi_items():
                        lower_name = name.lower()
                        if lower_name in HOP_BY_HOP_HEADERS or lower_name in {
                            "content-length",
                            "content-encoding",
                            "server",
                        }:
                            continue
                        self.send_header(name, value)
                    self.send_header("Connection", "close")
                    self.end_headers()
                    if self.command != "HEAD":
                        for chunk in response.iter_bytes():
                            if chunk:
                                self.wfile.write(chunk)
                                self.wfile.flush()
                    self.close_connection = True
                    print(
                        f"{self.command} {upstream_path} -> {response.status_code}",
                        flush=True,
                    )
        except (httpx.HTTPError, OSError) as exc:
            if not self.wfile.closed:
                try:
                    self._json_error(502, f"upstream request failed: {type(exc).__name__}")
                except OSError:
                    pass
            print(f"relay failure for {upstream_path}: {exc}", file=sys.stderr, flush=True)

    do_GET = _relay
    do_POST = _relay
    do_PUT = _relay
    do_PATCH = _relay
    do_DELETE = _relay
    do_HEAD = _relay

    def log_message(self, format: str, *args: object) -> None:
        return


def main() -> None:
    server = ThreadingHTTPServer((LISTEN_HOST, LISTEN_PORT), RelayHandler)
    print(f"relay listening on http://{LISTEN_HOST}:{LISTEN_PORT}", flush=True)
    server.serve_forever()


if __name__ == "__main__":
    main()
