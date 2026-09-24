"""FastAPI 应用入口。"""

from __future__ import annotations

import logging
from contextlib import asynccontextmanager
from pathlib import Path
from time import time_ns

from fastapi import FastAPI
from fastapi.responses import HTMLResponse
from fastapi.staticfiles import StaticFiles
from starlette.middleware.gzip import GZipMiddleware

from app.api import create_router, install_access_guard
from app.config import Settings
from app.db import Database
from app.http import ETagMiddleware
from app.providers.market import HybridMarketDataProvider, MarketDataProvider
from app.providers.cboe import CboeOptionsProvider
from app.services.scheduler import Scheduler
from app.services.concurrency import UpstreamGate
from app.services.snapshots import SnapshotService


settings = Settings.from_env()
database = Database(settings.database_path)
upstream_gate = UpstreamGate(settings.upstream_concurrency, settings.upstream_wait_seconds)
regular_provider = MarketDataProvider(proxy=settings.proxy_url, upstream_gate=upstream_gate)
delayed_provider = CboeOptionsProvider(proxy=settings.proxy_url, upstream_gate=upstream_gate)
provider = HybridMarketDataProvider(regular_provider, delayed_provider)
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
app.add_middleware(GZipMiddleware, minimum_size=1024)
app.add_middleware(ETagMiddleware)
install_access_guard(app, settings)
app.include_router(create_router(database, snapshots, provider, settings))
static_dir = Path(__file__).parent / "static"
app.mount("/static", StaticFiles(directory=static_dir), name="static")


@app.get("/", include_in_schema=False)
def index():
    # 默认标的取自 DEFAULT_SYMBOLS 的第一项，避免页面写死的默认值与服务端配置不一致。
    page = (static_dir / "index.html").read_text(encoding="utf-8")
    page = page.replace("__DEFAULT_SYMBOL__", settings.default_symbols[0])
    page = page.replace("__ACCESS_KEY_REQUIRED__", "true" if settings.access_key else "false")
    page = page.replace("__AUTO_REFRESH_SECONDS__", str(settings.auto_refresh_seconds))
    # 每次源码更新后自动生成新的静态资源版本号，避免浏览器继续使用旧版 app.js。
    # 时间戳只用于缓存键，不参与业务数据计算；使用纳秒可覆盖同一秒内的快速更新。
    asset_paths = (
        static_dir / "common/css/styles.css",
        static_dir / "common/js/request.js",
        static_dir / "common/js/charts.js",
        static_dir / "common/js/app.js",
    )
    asset_version = max((path.stat().st_mtime_ns for path in asset_paths if path.exists()), default=time_ns())
    page = page.replace("__ASSET_VERSION__", str(asset_version))
    return HTMLResponse(page, headers={"Cache-Control": "no-store"})


@app.get("/health", include_in_schema=False)
def health() -> dict[str, str]:
    return {"status": "ok"}
