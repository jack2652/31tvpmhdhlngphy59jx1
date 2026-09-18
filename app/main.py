"""FastAPI 应用入口。"""

from __future__ import annotations

import logging
from contextlib import asynccontextmanager
from pathlib import Path

from fastapi import FastAPI
from fastapi.responses import HTMLResponse
from fastapi.staticfiles import StaticFiles

from app.api import create_router
from app.config import Settings
from app.db import Database
from app.providers.yahoo import YahooProvider
from app.services.scheduler import Scheduler
from app.services.snapshots import SnapshotService


settings = Settings.from_env()
database = Database(settings.database_path)
provider = YahooProvider(proxy=settings.proxy_url)
snapshots = SnapshotService(database, provider)
scheduler = Scheduler(settings, snapshots, database)

# uvicorn 默认只放行 WARNING 及以上，后台刷新与体积清理的进度日志需要显式配置才可见。
app_logger = logging.getLogger("app")
app_logger.setLevel(logging.INFO)
if not app_logger.handlers:
    _stream_handler = logging.StreamHandler()
    _stream_handler.setFormatter(logging.Formatter("%(asctime)s %(levelname)s %(message)s"))
    app_logger.addHandler(_stream_handler)


@asynccontextmanager
async def lifespan(_: FastAPI):
    await scheduler.start()
    yield
    await scheduler.stop()


app = FastAPI(title="Option Scope", version="0.1.0", lifespan=lifespan)
app.include_router(create_router(database, snapshots, provider, settings))
static_dir = Path(__file__).parent / "static"
app.mount("/static", StaticFiles(directory=static_dir), name="static")


@app.get("/", include_in_schema=False)
def index():
    # 默认标的取自 DEFAULT_SYMBOLS 的第一项，避免页面写死的默认值与服务端配置不一致。
    page = (static_dir / "index.html").read_text(encoding="utf-8")
    return HTMLResponse(page.replace("__DEFAULT_SYMBOL__", settings.default_symbols[0]))


@app.get("/health", include_in_schema=False)
def health() -> dict[str, str]:
    return {"status": "ok"}
