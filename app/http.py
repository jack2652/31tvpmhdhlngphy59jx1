"""HTTP 层的响应头处理。"""

from __future__ import annotations

from typing import Any, Awaitable, Callable


class ETagMiddleware:
    """API GET 直接返回正文，并禁止缓存。

    类名保留，避免已有引用失效。这里以前会按正文生成 ETag 并回 304。
    jQuery 把 304 当成成功但没有数据，Gamma 轮询就会一直停在上一次的 running。
    """

    def __init__(self, app: Callable[..., Awaitable[Any]]):
        self.app = app

    async def __call__(self, scope: dict[str, Any], receive, send) -> None:
        if scope.get("type") != "http" or scope.get("method") != "GET" or not str(scope.get("path", "")).startswith("/api/"):
            await self.app(scope, receive, send)
            return

        async def send_no_store(message: dict[str, Any]) -> None:
            if message.get("type") != "http.response.start":
                await send(message)
                return
            headers = []
            for key, value in message.get("headers") or []:
                name = key.lower() if isinstance(key, (bytes, bytearray)) else str(key).lower().encode("latin-1")
                if name in {b"etag", b"cache-control"}:
                    continue
                headers.append((key, value))
            headers.append((b"cache-control", b"no-store"))
            await send({**message, "headers": headers})

        await self.app(scope, receive, send_no_store)
