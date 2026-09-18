"""提供默认监听 0.0.0.0 的本地启动入口。"""

from __future__ import annotations

import os

from dotenv import load_dotenv
import uvicorn


load_dotenv()


def main() -> None:
    """启动 FastAPI 服务，允许通过环境变量覆盖端口。"""
    uvicorn.run(
        "app.main:app",
        host=os.getenv("HOST", "0.0.0.0"),
        port=int(os.getenv("PORT", "8000")),
    )


if __name__ == "__main__":
    main()
