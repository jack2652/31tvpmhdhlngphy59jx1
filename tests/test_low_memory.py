"""低内存保护：闸门让路、启动参数和页面不再叠刷新。"""

from __future__ import annotations

import threading
import time
from pathlib import Path

import pytest
from fastapi import FastAPI
from fastapi.testclient import TestClient

from app.__main__ import resolve_web_workers
from app.api import create_router
from app.config import Settings
from app.db import Database, iso
from app.runtime import effective_database_max_mb, low_memory_enabled
from app.services.concurrency import HeavyWorkGate
from app.services.snapshots import SnapshotService
from tests.test_app import FakeProvider, sample_quote, sample_rows


def _settings(path: Path, **overrides) -> Settings:
    values = dict(
        database_path=path,
        proxy_url=None,
        default_symbols=("AAPL",),
        refresh_interval_seconds=60,
        raw_retention_days=30,
        cleanup_interval_seconds=86400,
        scheduler_enabled=False,
    )
    values.update(overrides)
    return Settings(**values)


def test_low_memory_flag_respects_env_and_auto(monkeypatch: pytest.MonkeyPatch):
    monkeypatch.setenv("LOW_MEMORY", "true")
    assert low_memory_enabled() is True
    monkeypatch.setenv("LOW_MEMORY", "false")
    assert low_memory_enabled() is False
    monkeypatch.setenv("LOW_MEMORY", "auto")
    monkeypatch.setattr("app.runtime.memory_limit_mb", lambda: 256)
    assert low_memory_enabled() is True
    monkeypatch.setattr("app.runtime.memory_limit_mb", lambda: 2048)
    assert low_memory_enabled() is False
    monkeypatch.setattr("app.runtime.memory_limit_mb", lambda: None)
    assert low_memory_enabled() is False


def test_resolve_web_workers_defaults_to_one_and_caps_low_memory(capsys: pytest.CaptureFixture[str]):
    assert resolve_web_workers(None, False) == 1
    assert resolve_web_workers("4", False) == 4
    assert resolve_web_workers("0", False) == 1
    assert resolve_web_workers("bad", False) == 1
    assert resolve_web_workers("3", True) == 1
    assert "已降为 1" in capsys.readouterr().out


def test_database_cap_uses_free_space_only_in_low_memory(tmp_path: Path, monkeypatch: pytest.MonkeyPatch):
    database = tmp_path / "options.db"
    database.write_bytes(b"x")

    def usage(_: Path):
        return type("Usage", (), {"free": 120 * 1048576})()

    monkeypatch.setattr("app.runtime.shutil.disk_usage", usage)
    assert effective_database_max_mb(256, database, False) == 256
    # 120MB 可用空间扣掉 96MB 余量后，上限落到 32MB，而不是配置里的 256MB。
    assert effective_database_max_mb(256, database, True) == 32
    assert effective_database_max_mb(0, database, True) == 32


def test_busy_gate_defers_when_chain_exists_and_waits_when_missing(tmp_path: Path):
    database = Database(tmp_path / "options.db")
    database.write_snapshot(sample_quote(), sample_rows(), iso())
    busy = HeavyWorkGate(1)
    assert busy.acquire(timeout=0)

    class ExplodingProvider(FakeProvider):
        def expirations(self, symbol: str) -> list[str]:
            raise AssertionError("已有期权链时不应再回源")

        def fetch(self, symbol: str, expiration: str):
            raise AssertionError("已有期权链时不应再回源")

    deferred = SnapshotService(database, ExplodingProvider(), heavy_gate=busy)
    result = deferred.refresh("AAPL", "2026-12-18", max_age_seconds=0)
    assert result["deferred"] is True
    assert result["skipped"] is True
    assert result["expiration"] == "2026-12-18"
    busy.release()

    empty = Database(tmp_path / "empty.db")
    gate = HeavyWorkGate(1)
    assert gate.acquire(timeout=0)

    def release_later() -> None:
        time.sleep(0.2)
        gate.release()

    threading.Thread(target=release_later, daemon=True).start()
    service = SnapshotService(empty, FakeProvider(), heavy_gate=gate)
    fetched = service.refresh("AAPL", "2026-12-18", max_age_seconds=0)
    assert fetched.get("deferred") is not True
    assert fetched["rows"] == 2
    assert empty.latest_chain("AAPL", "2026-12-18")["data"]


def test_gamma_degrades_while_heavy_gate_is_held(tmp_path: Path, monkeypatch: pytest.MonkeyPatch):
    database = Database(tmp_path / "options.db")
    service = SnapshotService(database, FakeProvider(), heavy_gate=HeavyWorkGate(4))
    service.refresh("AAPL", "2026-12-18")
    gate = HeavyWorkGate(1)
    assert gate.acquire(timeout=0)
    monkeypatch.setattr("app.api.get_heavy_gate", lambda: gate)
    router = create_router(database, service, FakeProvider(), _settings(database.path, low_memory=True))
    app = FastAPI()
    app.include_router(router)
    with TestClient(app) as client:
        payload = client.get("/api/gamma/AAPL", params={"horizon_days": 365, "include_rows": False}).json()
    assert payload["degraded"] is True
    assert payload["data"] == []
    assert payload["contract_count"] == 0
    assert "内存保护" in payload["warning"]
    gate.release()


def test_low_memory_settings_cap_upstream(monkeypatch: pytest.MonkeyPatch):
    monkeypatch.setenv("LOW_MEMORY", "true")
    monkeypatch.setenv("UPSTREAM_CONCURRENCY", "6")
    monkeypatch.setenv("UPSTREAM_WAIT_SECONDS", "20")
    settings = Settings.from_env()
    assert settings.low_memory is True
    assert settings.upstream_concurrency == 1
    assert settings.upstream_wait_seconds == 5
    assert settings.analysis_cache_entries == 8


def test_frontend_holds_refresh_until_heavy_work_finishes():
    source = Path("app/static/common/js/app.js").read_text(encoding="utf-8")
    assert "function refreshWorkPending()" in source
    assert "if (refreshWorkPending())" in source
    assert "if (silent && refreshWorkPending())" in source
    assert "分析计算中，刷新稍后开始" in source
    assert "内存保护：本轮刷新已让路，继续使用本地快照" in source
    assert "Array.isArray(refreshResult?.expirations)" in source
    assert "state.loading = true;" in source
    assert "state.loading = false;" in source
    # 这些字符串是原有倒计时测试的锚点，低内存改动不能把它们改掉。
    assert "if (state.refreshing) return;" in source
    assert "function armRefreshAnchor(fetchedAt)" in source
    assert "armRefreshAnchor(payload.fetched_at);" in source
    assert "Math.min(anchoredAge, snapshotAge)" in source
    assert "setTimeout(() => { refresh(true); }, delaySeconds * 1000);" in source
    assert "return `${seconds} 秒后自动更新`;" in source
