"""FastAPI 应用入口。"""

from __future__ import annotations

import gc
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
from app.providers.alpaca import AlpacaOvernightProvider
from app.runtime import memory_limit_mb, release_memory
from app.services.scheduler import Scheduler
from app.services.concurrency import UpstreamGate
from app.services.snapshots import SnapshotService


settings = Settings.from_env()
database = Database(settings.database_path)
upstream_gate = UpstreamGate(settings.upstream_concurrency, settings.upstream_wait_seconds)
regular_provider = MarketDataProvider(
    proxy=settings.proxy_url,
    upstream_gate=upstream_gate,
    analysis_cache=database,
)
delayed_provider = CboeOptionsProvider(proxy=settings.proxy_url, upstream_gate=upstream_gate)
overnight_provider = AlpacaOvernightProvider(
    settings.alpaca_api_key,
    settings.alpaca_api_secret,
    proxy=settings.proxy_url,
    upstream_gate=upstream_gate,
)
provider = HybridMarketDataProvider(
    regular_provider,
    delayed_provider,
    overnight_provider=overnight_provider,
)
snapshots = SnapshotService(database, provider)
scheduler = Scheduler(settings, database)

# uvicorn 默认只放行 WARNING 及以上，后台刷新与体积清理的进度日志需要显式配置才可见。
app_logger = logging.getLogger("app")
app_logger.setLevel(logging.INFO)
if not app_logger.handlers:
    _stream_handler = logging.StreamHandler()
    _stream_handler.setFormatter(logging.Formatter("%(asctime)s %(levelname)s %(message)s"))
    app_logger.addHandler(_stream_handler)


@asynccontextmanager
async def lifespan(_: FastAPI):
    # Alpaca 只作为可选的夜盘现货补充：未配置或凭据失效都不能阻止主行情服务启动。
    alpaca_status = overnight_provider.validate_credentials()
    if alpaca_status == "disabled":
        app_logger.info("Alpaca 夜盘现货未配置，沿用原行情源")
    elif alpaca_status == "invalid_config":
        app_logger.warning("Alpaca 夜盘现货配置不完整，沿用原行情源")
    elif alpaca_status == "invalid_credentials":
        app_logger.warning("Alpaca 夜盘现货凭据无效，沿用原行情源；冷却后自动重试")
    elif alpaca_status == "unavailable":
        app_logger.warning("Alpaca 夜盘现货启动校验暂不可用，运行时按需重试")
    else:
        app_logger.info("Alpaca 夜盘现货凭据校验通过")
    if settings.low_memory:
        # 更早回收临时对象，避免期权链和日线在两次刷新之间叠在一起。
        gc.set_threshold(400, 5, 5)
        release_memory()
        limit = memory_limit_mb()
        app_logger.info(
            "低内存保护已启用：内存上限 %sMB，重任务串行，分析缓存 %s 条",
            limit if limit is not None else "未知",
            settings.analysis_cache_entries,
        )
    interrupted = database.fail_orphaned_analysis_jobs()
    if interrupted:
        app_logger.info("已中断 %s 个重启前未完成的后台分析", interrupted)
    await scheduler.start()
    yield
    await scheduler.stop()


app = FastAPI(title="Option Scope", version="0.1.0", lifespan=lifespan)
# gzip 要先把整份响应收进内存。256MB 机器上这一份拷贝就可能把进程打爆。
# API JSON 一律 no-store，低内存机器也要装上：304 会让轮询一直读到旧的 running。
if not settings.low_memory:
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
    # 每次源码更新后自动生成新的静态资源版本号，避免浏览器继续使用旧版 app.js。
    # 时间戳只用于缓存键，不参与业务数据计算；使用纳秒可覆盖同一秒内的快速更新。
    asset_paths = (
        static_dir / "common/css/styles.css",
        static_dir / "common/img/option-scope-mark.svg",
        static_dir / "common/js/request.js",
        static_dir / "common/js/charts.js",
        static_dir / "common/js/app.js",
    )
    asset_version = max((path.stat().st_mtime_ns for path in asset_paths if path.exists()), default=time_ns())
    page = page.replace("__ASSET_VERSION__", str(asset_version))
    return HTMLResponse(page, headers={"Cache-Control": "no-store"})


@app.get("/health", include_in_schema=False)
async def health() -> dict[str, str]:
    # 使用 async 健康检查，避免同步 API 线程池被上游慢请求占满时看门狗也被拖住。
    return {"status": "ok"}
