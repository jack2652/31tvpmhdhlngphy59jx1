"""HTTP 层的压缩和条件请求中间件。"""

from __future__ import annotations

import hashlib
from typing import Any, Awaitable, Callable


class ETagMiddleware:
    """为 API GET 响应生成 ETag，重复请求未变化时直接返回 304。"""

    def __init__(self, app: Callable[..., Awaitable[Any]]):
        self.app = app

    async def __call__(self, scope: dict[str, Any], receive, send) -> None:
        if scope.get("type") != "http" or scope.get("method") != "GET" or not scope.get("path", "").startswith("/api/"):
            await self.app(scope, receive, send)
            return

        messages: list[dict[str, Any]] = []
        body: list[bytes] = []

        async def capture(message: dict[str, Any]) -> None:
            if message["type"] == "http.response.start":
                messages.append(message)
            elif message["type"] == "http.response.body":
                body.append(message.get("body", b""))
            else:
                messages.append(message)

        await self.app(scope, receive, capture)
        if not messages or messages[0].get("type") != "http.response.start":
            return

        start = messages[0]
        payload = b"".join(body)
        status = int(start.get("status", 200))
        if status in {204, 304}:
            await send(start)
            await send({"type": "http.response.body", "body": b""})
            return

        etag = '"' + hashlib.blake2b(payload, digest_size=16).hexdigest() + '"'
        request_headers = {key.lower(): value for key, value in scope.get("headers", [])}
        response_headers = [(key, value) for key, value in start.get("headers", []) if key.lower() not in {b"content-length", b"etag"}]
        response_headers.append((b"etag", etag.encode("ascii")))
        response_headers.append((b"cache-control", b"private, max-age=2"))
        if b"content-encoding" not in {key.lower() for key, _ in response_headers}:
            response_headers.append((b"content-length", str(len(payload)).encode("ascii")))

        if request_headers.get(b"if-none-match") == etag.encode("ascii"):
            await send({"type": "http.response.start", "status": 304, "headers": [(b"etag", etag.encode("ascii"))]})
            await send({"type": "http.response.body", "body": b""})
            return

        await send({**start, "headers": response_headers})
        await send({"type": "http.response.body", "body": payload})
