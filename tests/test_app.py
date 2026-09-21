from __future__ import annotations

import asyncio
import threading
import time
from concurrent.futures import ThreadPoolExecutor
from types import SimpleNamespace
from datetime import date, datetime, timedelta
from pathlib import Path
from zoneinfo import ZoneInfo

import pandas as pd
import pytest
from fastapi import FastAPI
from fastapi.testclient import TestClient

import app.api as api_module
from app.api import create_router, install_access_guard, trend_market_data
from app.config import Settings
from app.db import NO_FLOOR, Database, iso, parse_sessions, utc_now
from app.levels import absorption_levels, annotate_level_history, average_true_range, average_true_ranges, best_trade_points, build_levels, chip_peaks, fibonacci_levels, level_strength_tier, merge_candidates, option_levels, price_extremes, select_visible_levels, split_support_plan, touch_probability, trade_recommendation, trend_channel
from app.providers import market
from app.providers import cboe
from app.providers.market import (
    ProviderError,
    HybridMarketDataProvider,
    MarketDataProvider,
    current_session_state,
    is_session_trading_day,
    safe_value,
    summarize_extended_hours,
)
from app.providers.cboe import CboeOptionsProvider, parse_occ_option
from app.services.history import HistoryService, calculate_beta
from app.services.concurrency import SingleFlight, UpstreamGate
from app.services.market_calendar import is_regular_session, is_trading_day
from app.http import ETagMiddleware
from app.gamma import (
    annotate_model_greeks,
    black_scholes_price,
    contract_gex,
    find_zero_gamma,
    implied_volatility_from_price,
    years_to_expiry,
)
from app.services.snapshots import SnapshotService, active_expirations, has_valid_two_sided_quotes, market_today
from app.services.scheduler import Scheduler


def sample_quote(symbol: str = "AAPL") -> dict:
    return {"symbol": symbol, "price": 200.5, "change_percent": 1.25, "currency": "USD", "market_state": "REGULAR", "provider": "fake", "raw": {}}


def sample_rows(symbol: str = "AAPL", expiration: str = "2026-12-18") -> list[dict]:
    return [
        {"symbol": symbol, "expiration": expiration, "contract_symbol": "AAPL261218C00200000", "contract_type": "call", "strike": 200, "last_price": 5.2, "bid": 5.1, "ask": 5.3, "volume": 100, "open_interest": 500, "implied_volatility": 0.31, "gamma": 0.02, "in_the_money": True, "change_percent": 2.0, "provider": "fake", "raw": {}},
        {"symbol": symbol, "expiration": expiration, "contract_symbol": "AAPL261218P00200000", "contract_type": "put", "strike": 200, "last_price": 4.8, "bid": 4.7, "ask": 4.9, "volume": 80, "open_interest": 400, "implied_volatility": 0.29, "gamma": 0.018, "in_the_money": False, "change_percent": -1.0, "provider": "fake", "raw": {}},
    ]


def sample_bars(count: int = 140) -> list[dict]:
    """确定性日线：下跌 200 → 185、上涨到 216、再回落到 199，现价落在摆动区间中部。"""
    bars: list[dict] = []
    turning = count // 3
    rally_end = count - 20
    for index in range(count):
        if index < turning:
            price = 200 - index * (15 / turning)
        elif index < rally_end:
            price = 185 + (index - turning) * (31 / (rally_end - turning))
        else:
            price = 216 - (index - rally_end + 1) * (17 / (count - rally_end))
        bars.append({
            "date": (date(2026, 3, 2) + timedelta(days=index)).isoformat(),
            "open": round(price, 2),
            "high": round(price + 1.5, 2),
            "low": round(price - 1.5, 2),
            "close": round(price, 2),
            "volume": 1000 + index * 10,
        })
    return bars


def sample_option_rows(spot: float, expiration: str = "2026-12-18") -> list[dict]:
    """现价上下各三个执行价：上方看涨持仓重、下方看跌持仓重，便于验证分侧口径。"""
    rows: list[dict] = []
    for offset in (-20, -10, -5, 5, 10, 20):
        strike = spot + offset
        for contract_type, heavy in (("call", offset > 0), ("put", offset < 0)):
            price = 6.0 if abs(offset) <= 10 else 3.0
            rows.append({
                "symbol": "AAPL", "expiration": expiration, "contract_symbol": f"AAPL{strike}{contract_type}",
                "contract_type": contract_type, "strike": strike, "last_price": price,
                "bid": price - 0.1, "ask": price + 0.1, "volume": 800 if heavy else 200,
                "open_interest": 3000 if heavy else 800, "implied_volatility": 0.3,
                "in_the_money": False, "change_percent": 0.0, "provider": "fake", "raw": {},
            })
    return rows


class FakeProvider:
    def normalize_symbol(self, symbol: str) -> str:
        return MarketDataProvider.normalize_symbol(symbol)

    def expirations(self, symbol: str) -> list[str]:
        return ["2026-12-18", "2027-01-15"]

    def quote(self, symbol: str) -> dict:
        return sample_quote(symbol)

    def fetch(self, symbol: str, expiration: str):
        return sample_quote(symbol), sample_rows(symbol, expiration), iso()

    def history(self, symbol: str, period: str = "6mo") -> list[dict]:
        return sample_bars()

    def benchmark_history(self, symbol: str = "^GSPC", period: str = "2y") -> list[dict]:
        return sample_bars()


def test_safe_value_and_symbol_validation():
    import numpy as np
    import pandas as pd

    assert safe_value(np.float64("nan")) is None
    assert safe_value(np.int64(4)) == 4
    assert safe_value(pd.NA) is None
    assert safe_value(pd.NaT) is None
    assert MarketDataProvider.normalize_symbol(" brk.b ") == "BRK.B"
    assert MarketDataProvider.normalize_symbol("BF-B") == "BF-B"
    with pytest.raises(ValueError):
        MarketDataProvider.normalize_symbol("AAPL/")


def test_single_flight_runs_same_key_once():
    """并发请求同一分析键时共享结果，不重复执行计算。"""
    flight = SingleFlight()
    calls = {"count": 0}

    def compute():
        calls["count"] += 1
        time.sleep(0.05)
        return {"value": 42}

    with ThreadPoolExecutor(max_workers=8) as executor:
        results = list(executor.map(lambda _: flight.do("same", compute), range(8)))
    assert results == [{"value": 42}] * 8
    assert calls["count"] == 1


def test_upstream_gate_limits_concurrent_calls():
    """上游闸门限制并发请求数量，释放后其余任务继续执行。"""
    gate = UpstreamGate(limit=2, wait_seconds=2)
    active = {"now": 0, "peak": 0}
    guard = threading.Lock()

    def request():
        with gate.slot():
            with guard:
                active["now"] += 1
                active["peak"] = max(active["peak"], active["now"])
            time.sleep(0.03)
            with guard:
                active["now"] -= 1
        return True

    with ThreadPoolExecutor(max_workers=6) as executor:
        assert all(executor.map(lambda _: request(), range(6)))
    assert active["peak"] <= 2


def test_etag_middleware_returns_not_modified_for_same_api_body():
    """API 条件请求命中 ETag 时不再传输完整 JSON。"""
    async def endpoint(scope, receive, send):
        await send({"type": "http.response.start", "status": 200, "headers": [(b"content-type", b"application/json")]})
        await send({"type": "http.response.body", "body": b'{"ok":true}'})

    async def run(headers):
        sent = []
        scope = {"type": "http", "method": "GET", "path": "/api/levels/AAPL", "headers": headers}
        async def send(message):
            sent.append(message)
        await ETagMiddleware(endpoint)(scope, None, send)
        return sent

    first = asyncio.run(run([]))
    etag = dict(first[0]["headers"])[b"etag"]
    second = asyncio.run(run([(b"if-none-match", etag)]))
    assert first[0]["status"] == 200
    assert second[0]["status"] == 304
    assert second[1]["body"] == b""


def test_parse_occ_option_and_map_cboe_chain():
    assert parse_occ_option("QQQ261218C00599780") == {
        "root": "QQQ",
        "expiration": "2026-12-18",
        "contract_type": "call",
        "strike": 599.78,
    }
    payload = {
        "data": {
            "current_price": 600.0,
            "prev_day_close": 598.0,
            "options": [
                {
                    "option": "QQQ261218C00600000", "bid": 12.3, "ask": 12.5,
                    "last_trade_price": 12.4, "volume": 12.0, "open_interest": 345.0,
                    "iv": 0.31, "gamma": 0.02, "percent_change": 1.5,
                },
                {
                    "option": "QQQ261218P00600000", "bid": 11.3, "ask": 11.5,
                    "last_trade_price": 11.4, "volume": 8.0, "open_interest": 123.0,
                    "iv": 0.29, "gamma": 0.018, "percent_change": -1.5,
                },
                {"option": "QQQ270115C00600000", "open_interest": 999},
            ],
        }
    }
    provider = CboeOptionsProvider(request_json=lambda _: payload)
    rows = provider.chain("QQQ", "2026-12-18")
    assert provider.expirations("QQQ") == ["2026-12-18", "2027-01-15"]
    assert [row["contract_type"] for row in rows] == ["call", "put"]
    assert rows[0]["open_interest"] == 345
    assert rows[0]["in_the_money"] is True
    assert rows[1]["in_the_money"] is True
    assert rows[0]["provider"] == "cboe-delayed"


def test_cboe_provider_forwards_market_proxy(monkeypatch):
    observed: dict[str, object] = {}
    payload = {
        "data": {
            "options": [{"option": "QQQ261218C00600000", "open_interest": 1}],
        }
    }

    def fake_download(url, timeout=15.0, proxy=None):
        observed.update({"url": url, "timeout": timeout, "proxy": proxy})
        return payload

    monkeypatch.setattr(cboe, "download_json", fake_download)
    provider = CboeOptionsProvider(proxy=" http://127.0.0.1:7890 ", timeout=9.0)
    assert provider.expirations("QQQ") == ["2026-12-18"]
    assert observed == {
        "url": "https://cdn.cboe.com/api/global/delayed_quotes/options/QQQ.json",
        "timeout": 9.0,
        "proxy": "http://127.0.0.1:7890",
    }


def test_hybrid_provider_routes_options_by_market_session():
    calls: list[str] = []

    class Primary:
        def expirations(self, symbol):
            calls.append("primary-expirations")
            return ["2026-12-18"]

        def quote(self, symbol):
            calls.append("primary-quote")
            return sample_quote(symbol)

        def chain(self, symbol, expiration):
            calls.append("primary-chain")
            return sample_rows(symbol, expiration)

        def fetch(self, symbol, expiration):
            calls.append("primary-fetch")
            return sample_quote(symbol), sample_rows(symbol, expiration), iso()

        def history(self, symbol, period="6mo"):
            return sample_bars()

    class Delayed(Primary):
        def expirations(self, symbol):
            calls.append("delayed-expirations")
            return ["2026-12-18"]

        def chain(self, symbol, expiration):
            calls.append("delayed-chain")
            return sample_rows(symbol, expiration)

        def quote(self, symbol):
            calls.append("delayed-quote")
            return sample_quote(symbol)

    eastern = ZoneInfo("America/New_York")
    provider = HybridMarketDataProvider(
        Primary(), Delayed(), now_factory=lambda: datetime(2026, 9, 18, 8, 0, tzinfo=eastern)
    )
    provider.expirations("QQQ")
    provider.fetch("QQQ", "2026-12-18")
    assert "delayed-expirations" in calls and "delayed-chain" in calls
    assert "primary-fetch" not in calls
    assert "primary-quote" in calls

    calls.clear()
    provider._now_factory = lambda: datetime(2026, 9, 18, 10, 0, tzinfo=eastern)
    provider.expirations("QQQ")
    provider.fetch("QQQ", "2026-12-18")
    assert "primary-expirations" in calls and "primary-fetch" in calls
    assert "delayed-chain" not in calls

    calls.clear()
    provider._now_factory = lambda: datetime(2026, 7, 3, 10, 0, tzinfo=eastern)
    provider.expirations("QQQ")
    provider.fetch("QQQ", "2026-12-18")
    assert "delayed-expirations" in calls and "delayed-chain" in calls
    assert "primary-fetch" not in calls


def test_nonregular_cboe_failure_keeps_cached_chain(tmp_path: Path):
    """盘外免费源故障时不覆盖已有快照，页面仍可读取旧期权链。"""
    database = Database(tmp_path / "options.db")
    database.write_snapshot(sample_quote(), sample_rows(), iso())

    class Primary:
        def normalize_symbol(self, symbol):
            return MarketDataProvider.normalize_symbol(symbol)

        def quote(self, symbol):
            return sample_quote(symbol)

        def history(self, symbol, period="6mo"):
            return sample_bars()

    class BrokenDelayed:
        def expirations(self, symbol):
            raise ProviderError("Cboe 请求失败")

    eastern = ZoneInfo("America/New_York")
    provider = HybridMarketDataProvider(
        Primary(), BrokenDelayed(), now_factory=lambda: datetime(2026, 9, 18, 8, 0, tzinfo=eastern)
    )
    service = SnapshotService(database, provider)
    result = service.refresh("AAPL", "2026-12-18")
    assert result["stale"] is True
    assert result["warning"] == "Cboe 请求失败"
    cached = database.latest_chain("AAPL", "2026-12-18")
    assert cached["data"] and cached["data"][0]["open_interest"] == 500


def test_market_provider_passes_proxy_to_ticker_factory():
    calls = {}

    def ticker_factory(symbol: str, *, proxy: str | None = None):
        calls.update(symbol=symbol, proxy=proxy)
        return object()

    provider = MarketDataProvider(ticker_factory=ticker_factory, proxy=" http://127.0.0.1:7890 ")
    provider._ticker("aapl")
    assert calls == {"symbol": "AAPL", "proxy": "http://127.0.0.1:7890"}


def test_market_provider_sets_upstream_global_proxy(monkeypatch):
    """默认工厂路径应把代理写进上游 SDK 的全局配置。"""
    fake_config = SimpleNamespace(network=SimpleNamespace(proxy=None))
    fake_sdk = SimpleNamespace(config=fake_config, Ticker=lambda symbol, **kwargs: object())
    monkeypatch.setattr(market, "load_upstream_sdk", lambda: fake_sdk)

    provider = MarketDataProvider(proxy="http://127.0.0.1:7890")

    assert fake_config.network.proxy == "http://127.0.0.1:7890"
    assert provider.proxy == "http://127.0.0.1:7890"


def test_market_provider_uses_history_when_fast_info_is_empty():
    class FakeTicker:
        fast_info = {"last_price": None, "previous_close": None, "currency": "USD"}

        def history(self, **kwargs):
            return pd.DataFrame({"Close": [100.0, 102.5]})

    provider = MarketDataProvider(ticker_factory=lambda symbol: FakeTicker())
    quote = provider.quote("AAPL")
    assert quote["price"] == 102.5
    assert quote["change_percent"] == pytest.approx(2.5)


def extended_hours_frame():
    """构造含盘前、盘中、盘后、夜盘的四段分钟线，收盘价 100 是盘前盘后的共同基准。"""
    eastern = ZoneInfo("America/New_York")
    index = pd.DatetimeIndex([
        "2026-09-16 08:10",
        "2026-09-16 15:59",
        "2026-09-16 19:55",
        "2026-09-17 02:00",
        "2026-09-17 08:05",
    ]).tz_localize(eastern)
    return pd.DataFrame({"Close": [98.0, 100.0, 101.0, 101.5, 102.0]}, index=index)


def test_summarize_extended_hours_uses_previous_close_as_base():
    """盘前/盘后/夜盘取各时段最后一根 K 线，涨跌幅以该时段之前最近一次盘中收盘为基准。"""
    eastern = ZoneInfo("America/New_York")
    summary = summarize_extended_hours(extended_hours_frame(), now=datetime(2026, 9, 17, 8, 6, tzinfo=eastern))
    assert summary["state"] == "PRE"
    assert summary["sessions"]["pre"] == {
        "price": 102.0,
        "change_percent": pytest.approx(2.0),
        "as_of": "2026-09-17T08:05:00-04:00",
        "reference_close": 100.0,
    }
    assert summary["sessions"]["post"]["price"] == 101.0
    assert summary["sessions"]["post"]["change_percent"] == pytest.approx(1.0)
    assert summary["sessions"]["overnight"]["change_percent"] == pytest.approx(1.5)


def test_summarize_extended_hours_without_data_returns_empty_sessions():
    eastern = ZoneInfo("America/New_York")
    summary = summarize_extended_hours(pd.DataFrame(), now=datetime(2026, 9, 17, 8, 6, tzinfo=eastern))
    assert summary == {"state": "PRE", "sessions": {}}
    assert summarize_extended_hours(None, now=datetime(2026, 9, 17, 18, 0, tzinfo=eastern)) == {"state": "POST", "sessions": {}}


def test_current_session_state_falls_back_to_eastern_clock():
    eastern = ZoneInfo("America/New_York")
    # 周日夜盘属于周一交易日；周五晚间已进入周末，不应继续标成夜盘。
    assert current_session_state(datetime(2026, 9, 20, 21, 0, tzinfo=eastern), None) == "OVERNIGHT"
    assert current_session_state(datetime(2026, 9, 18, 21, 0, tzinfo=eastern), None) == "CLOSED"
    assert current_session_state(datetime(2026, 9, 21, 2, 0, tzinfo=eastern), None) == "OVERNIGHT"
    assert current_session_state(datetime(2026, 9, 18, 18, 0, tzinfo=eastern), None) == "POST"
    assert current_session_state(datetime(2026, 9, 19, 10, 0, tzinfo=eastern), None) == "CLOSED"
    assert is_session_trading_day(datetime(2026, 9, 20, 21, 0, tzinfo=eastern)) is True
    assert is_session_trading_day(datetime(2026, 9, 18, 21, 0, tzinfo=eastern)) is False
    # K 线足够新时以 K 线所属时段为准，避免本地时钟与数据源时区口径不一致时误判。
    assert current_session_state(datetime(2026, 9, 18, 12, 0, tzinfo=eastern), datetime(2026, 9, 18, 11, 59, tzinfo=eastern)) == "REGULAR"
    assert current_session_state(datetime(2026, 9, 18, 19, 58, tzinfo=eastern), datetime(2026, 9, 18, 19, 55, tzinfo=eastern)) == "POST"


def test_market_calendar_recognizes_holidays_and_early_closes():
    """交易日历识别完整节假日，并将提前收盘后的时段视为盘后。"""
    eastern = ZoneInfo("America/New_York")
    assert not is_trading_day(datetime(2026, 7, 3, 10, 0, tzinfo=eastern))
    assert not is_regular_session(datetime(2026, 7, 3, 10, 0, tzinfo=eastern))
    assert current_session_state(datetime(2026, 7, 3, 10, 0, tzinfo=eastern), None) == "CLOSED"
    assert is_trading_day(datetime(2026, 11, 27, 12, 59, tzinfo=eastern))
    assert is_regular_session(datetime(2026, 11, 27, 12, 59, tzinfo=eastern))
    assert not is_regular_session(datetime(2026, 11, 27, 13, 1, tzinfo=eastern))
    assert current_session_state(datetime(2026, 11, 27, 13, 1, tzinfo=eastern), None) == "POST"


def test_market_provider_quote_includes_extended_sessions():
    class FakeTicker:
        fast_info = {"last_price": 102.5, "previous_close": 100.0, "currency": "USD"}

        def history(self, **kwargs):
            assert kwargs["prepost"] is True
            return extended_hours_frame()

    quote = MarketDataProvider(ticker_factory=lambda symbol: FakeTicker()).quote("AAPL")
    assert quote["sessions"]["pre"]["price"] == 102.0
    assert quote["sessions"]["pre"]["change_percent"] == pytest.approx(2.0)


def test_market_provider_quote_survives_extended_hours_failure(monkeypatch):
    """盘前盘后是附加信息，被限流时只记日志，行情快照本身仍要成功。"""
    def broken_history(**kwargs):
        raise RuntimeError("Too Many Requests. Rate limited.")

    class FakeTicker:
        fast_info = {"last_price": 102.5, "previous_close": 100.0, "currency": "USD"}
        history = staticmethod(broken_history)

    quote = MarketDataProvider(ticker_factory=lambda symbol: FakeTicker()).quote("AAPL")
    assert quote["price"] == 102.5
    assert quote["sessions"] == {}
    assert quote["market_state"] is None


def test_estimate_gamma_from_option_inputs():
    gamma = MarketDataProvider.estimate_gamma(100, 100, 0.2, "2099-12-18")
    assert gamma is not None
    assert gamma > 0


def test_database_snapshot_lookup_and_cleanup(tmp_path: Path):
    database = Database(tmp_path / "options.db")
    old = iso(utc_now() - timedelta(days=40))
    database.write_snapshot(sample_quote(), sample_rows(), old)
    assert database.latest_quote("AAPL")["price"] == 200.5
    assert database.latest_chain("AAPL", "2026-12-18")["data"]
    assert database.latest_chain("AAPL", "2026-12-18")["data"][0]["gamma"] == 0.02
    profile = database.latest_chains("AAPL", horizon_days=365)
    assert profile["expirations"] == ["2026-12-18"]
    assert profile["data"][0]["expiration"] == "2026-12-18"
    result = database.cleanup(30)
    assert result == {"quotes": 1, "options": 2, "runs": 0, "history": 0, "extremes": 0}
    assert database.latest_quote("AAPL") is None
    assert database.latest_expirations("AAPL") == []


def test_latest_option_batch_index_keeps_newest_snapshot(tmp_path: Path):
    """最新批次索引只加速定位，不改变按 fetched_at 取最新链的口径。"""
    database = Database(tmp_path / "options.db")
    newest = iso(utc_now() - timedelta(minutes=1))
    older = iso(utc_now() - timedelta(minutes=5))
    database.write_snapshot(sample_quote(), sample_rows(), newest)
    database.write_snapshot(sample_quote(), sample_rows(), older)
    assert database.latest_chain("AAPL", "2026-12-18")["fetched_at"] == newest
    with database.connect() as connection:
        indexed = connection.execute(
            "SELECT fetched_at FROM option_latest_batches WHERE symbol=? AND expiration=?",
            ("AAPL", "2026-12-18"),
        ).fetchone()[0]
    assert indexed == newest


def test_existing_database_gets_gamma_column(tmp_path: Path):
    database = Database(tmp_path / "options.db")
    with database.connect() as connection:
        columns = {row[1] for row in connection.execute("PRAGMA table_info(option_snapshots)")}
    assert "gamma" in columns


def test_existing_database_gets_sessions_column(tmp_path: Path):
    """旧库启动时自动补 sessions_json 列，历史快照解析为空字典。"""
    database = Database(tmp_path / "options.db")
    with database.connect() as connection:
        columns = {row[1] for row in connection.execute("PRAGMA table_info(quote_snapshots)")}
    assert "sessions_json" in columns
    assert parse_sessions(None) == {}
    assert parse_sessions("{坏数据") == {}
    assert parse_sessions("[1, 2]") == {}


def test_snapshot_stores_extended_hours_and_api_exposes_them(tmp_path: Path):
    """行情快照落库时保存盘前/盘后，读取层把 JSON 解析成 sessions 对象下发。"""
    database = Database(tmp_path / "options.db")
    quote = sample_quote()
    quote["sessions"] = {"pre": {"price": 201.5, "change_percent": 0.5, "as_of": "2026-09-17T08:05:00-04:00"}}
    database.write_snapshot(quote, sample_rows(), iso())
    assert parse_sessions(database.latest_quote("AAPL")["sessions_json"])["pre"]["price"] == 201.5
    settings = Settings(database_path=tmp_path / "options.db", proxy_url=None, default_symbols=("AAPL",), refresh_interval_seconds=60, raw_retention_days=30, cleanup_interval_seconds=86400, scheduler_enabled=False)
    test_app = FastAPI()
    test_app.include_router(create_router(database, SnapshotService(database, FakeProvider()), FakeProvider(), settings))
    with TestClient(test_app) as client:
        payload = client.get("/api/quote/AAPL").json()
        assert payload["sessions"]["pre"]["price"] == 201.5
        assert "sessions_json" not in payload
        assert client.get("/api/quote/MSFT").json()["sessions"] == {}
        assert client.get("/api/status/AAPL").json()["quote"]["sessions"]["pre"]["price"] == 201.5
        # 新快照缺时段数据（限流或旧进程写入）时回退最近一次有效值，卡片不会闪空。
        database.write_snapshot(sample_quote(), sample_rows(), iso())
        assert database.latest_quote("AAPL")["sessions_json"] == "{}"
        assert client.get("/api/quote/AAPL").json()["sessions"]["pre"]["price"] == 201.5
        # 超出 24 小时窗口的旧值不再回退。
        assert database.latest_sessions("AAPL", max_age_seconds=0) == {}


def test_snapshot_service_keeps_previous_sessions_when_provider_returns_none(tmp_path: Path):
    """盘前盘后抓取失败时沿用上一份快照，避免卡片闪空。"""
    database = Database(tmp_path / "options.db")
    quote = sample_quote()
    quote["sessions"] = {"post": {"price": 199.0, "change_percent": -0.5, "as_of": "2026-09-16T19:55:00-04:00"}}
    database.write_snapshot(quote, sample_rows(), iso())

    class NoSessionProvider(FakeProvider):
        def fetch(self, symbol: str, expiration: str):
            payload = sample_quote(symbol)
            payload["sessions"] = {}
            return payload, sample_rows(symbol, expiration), iso()

    SnapshotService(database, NoSessionProvider()).refresh("AAPL", "2026-12-18")
    assert parse_sessions(database.latest_quote("AAPL")["sessions_json"])["post"]["price"] == 199.0


def test_snapshot_service_refresh_and_api(tmp_path: Path):
    database = Database(tmp_path / "options.db")
    service = SnapshotService(database, FakeProvider())
    result = service.refresh("aapl", "2026-12-18")
    assert result["rows"] == 2
    settings = Settings(database_path=tmp_path / "options.db", proxy_url=None, default_symbols=("AAPL",), refresh_interval_seconds=60, raw_retention_days=30, cleanup_interval_seconds=86400, scheduler_enabled=False)
    router = create_router(database, service, FakeProvider(), settings)
    test_app = FastAPI()
    test_app.include_router(router)
    with TestClient(test_app) as client:
        assert client.get("/api/chain/AAPL", params={"expiration": "2026-12-18"}).status_code == 200
        assert client.get("/api/chain/AAPL", params={"expiration": "bad"}).status_code == 422
        assert client.get("/api/quote/AAPL").json()["price"] == 200.5
        gamma = client.get("/api/gamma/AAPL", params={"horizon_days": 365})
        assert gamma.status_code == 200
        assert gamma.json()["expirations"] == ["2026-12-18"]

def test_page_query_helpers_are_exported_in_frontend():
    source = Path("app/static/common/js/app.js").read_text(encoding="utf-8")
    assert "function parsePageQuery(search)" in source
    assert "function buildPageQuery(symbol, expiration, pathname)" in source
    assert "history.replaceState(null, \"\", next);" in source
    assert "const initialQuery = parsePageQuery(location.search);" in source


def test_chart_tooltip_floats_above_chart_container():
    source = Path("app/static/common/js/charts.js").read_text(encoding="utf-8")
    styles = Path("app/static/common/css/styles.css").read_text(encoding="utf-8")
    # 数据卡用 bottom 锚定图表容器上沿，纵向不跟随鼠标，保证卡片停在图表外部上方。
    assert 'tooltip.style.top = "auto";' in source
    assert 'tooltip.style.bottom = `${targetRect.height + 4}px`;' in source
    # 坐标换算改走屏幕矩阵，避免 SVG 等比缩放留白让卡片与十字虚线错位。
    assert "chartSvg.getScreenCTM()" in source
    assert "matrix.inverse()" in source
    # 右键菜单、右键拖动和指针取消都会清理悬浮窗，避免异常坐标把卡片定位到左上角。
    assert source.count('if (event.buttons & 2)') == 2
    assert source.count('event.button !== 2') == 2
    assert source.count('addEventListener("contextmenu"') == 2
    # 两张图表共用按帧调度器，由调度器统一绑定 pointercancel 清理逻辑。
    assert source.count('addEventListener("pointercancel"') == 1
    assert 'const stop = () => { cancel(); hideTooltip(); };' in source
    # 面板放开裁剪，卡片悬浮到图表上方时不会被面板上沿截断。
    assert ".analysis-panel{overflow:visible}" in styles


def test_buyer_structure_scenario_explains_profit_and_loss():
    source = Path("app/static/common/js/app.js").read_text(encoding="utf-8")
    page = Path("app/static/index.html").read_text(encoding="utf-8")
    styles = Path("app/static/common/css/styles.css").read_text(encoding="utf-8")
    assert "目标价下${scenarioLabel} ${scenario}" in source
    assert "formatStructureScenarioAmount" in source
    assert "预计盈利 · 已覆盖成本" in source
    assert "预计亏损 · 未覆盖成本" in source
    assert "扣除时间价值后仍未覆盖成本" in source
    assert "未来 5 个交易日到达目标价估算" in page
    assert 'key: `${item.kind || "structure"}-${item.direction || "unknown"}-${item.expiration || "unknown"}-${strikes || "unknown"}-${index}`' in source
    assert ".buyer-structure-scenario{font-weight:600}" in styles
    assert "margin:7px 0 14px" in styles
    assert "background:var(--table-head)" in styles


def test_buyer_structure_title_drops_only_trailing_period():
    source = Path("app/static/common/js/app.js").read_text(encoding="utf-8")
    assert 'title: title.replace(/。$/, "")' in source


def test_quote_price_follows_current_session():
    """现货现价按时段动态取值：盘前用盘前价、盘后/夜盘用盘后价、盘中用常规价；不再单列时段价格卡。"""
    source = Path("app/static/common/js/app.js").read_text(encoding="utf-8")
    page = Path("app/static/index.html").read_text(encoding="utf-8")
    styles = Path("app/static/common/css/styles.css").read_text(encoding="utf-8")
    # 独立的盘后/盘前价格卡及其渲染、样式全部移除。
    assert "quote-sessions" not in page and "quote-sessions" not in source
    assert "renderSessions" not in source and "SESSION_LABELS" not in source
    assert ".session-chip" not in styles and ".quote-sessions" not in styles and ".quote-foot" not in styles
    # 现价按时段动态取值，取不到对应时段数据时回退常规价。
    assert "function activeSessionQuote(quote)" in source
    assert 'if (marketState === "PRE" && sessions.pre?.price != null) return sessions.pre;' in source
    assert 'if ((marketState === "POST" || marketState === "OVERNIGHT") && sessions.post?.price != null) return sessions.post;' in source
    assert "const active = activeSessionQuote(quote);" in source
    assert "const price = active?.price ?? quote?.price;" in source
    assert "const change = active?.change_percent ?? quote?.change_percent;" in source
    # 时段标签映射与顶栏时段展示保留。
    assert 'const MARKET_STATE_LABELS = { PRE: "盘前", REGULAR: "正常交易", POST: "盘后", OVERNIGHT: "夜盘", CLOSED: "休市" };' in source
    assert 'state.view.marketState = marketStateLabel(quote?.market_state, "快照数据");' in source
    assert "function quoteMarketLabel(quote)" in source
    assert 'return state.levelBasisMode === "close" ? "盘后" : "收盘价";' in source
    assert "state.view.quoteMarket = quoteMarketLabel(quote);" in source
    assert "if (state.lastQuote) state.view.quoteMarket = quoteMarketLabel(state.lastQuote);" in source


def test_palette_uses_green_up_red_down_tokens():
    """全局涨跌配色：绿涨红跌，令牌按 --up / --down 语义命名，不再按颜色命名。"""
    styles = Path("app/static/common/css/styles.css").read_text(encoding="utf-8")
    source = Path("app/static/common/js/app.js").read_text(encoding="utf-8")
    # 颜色令牌只在 :root 定义一次，旧的颜色命名令牌不残留。
    assert "--up:#19bd83" in styles and "--down:#f04d68" in styles
    assert "var(--green)" not in styles and "var(--red)" not in styles
    # 行情上涨 / 看涨 / Call 仍走 --up，行情下跌 / 看跌 / Put 仍走 --down；支撑/压力区间按操作提示配色。
    assert ".chart-call{fill:var(--up)}" in styles
    assert ".chart-put{fill:var(--down)}" in styles
    assert ".type-call{color:var(--up)}" in styles
    assert ".type-put{color:var(--down)}" in styles
    assert ".legend-dot.call{background:var(--up)}" in styles
    assert ".legend-dot.put{background:var(--down)}" in styles
    assert ".levels-panel-resistance .level-strike{color:var(--down)}" in styles
    assert ".levels-panel-support .level-strike{color:var(--up)}" in styles
    # 行情百分比：涨用 --up，跌用 --down，与全局口径保持一致。
    assert 'state.view.quoteChangeColor = change == null ? "var(--muted)" : (change < 0 ? "var(--down)" : "var(--up)");' in source


def test_theme_defaults_to_dark_with_light_override():
    """主题：默认黑夜模式，白天通过 data-theme="light" 覆盖，偏好写入 localStorage 记忆。"""
    styles = Path("app/static/common/css/styles.css").read_text(encoding="utf-8")
    html = Path("app/static/index.html").read_text(encoding="utf-8")
    source = Path("app/static/common/js/app.js").read_text(encoding="utf-8")
    # 黑夜令牌定义在 :root（默认值），白天令牌挂在 data-theme="light" 上覆盖。
    assert ":root{color-scheme:dark;" in styles
    assert ':root[data-theme="light"]{color-scheme:light;' in styles
    assert "--bg:#0b1118" in styles and "--bg:#f2f5f9" in styles
    # 首屏引导脚本：默认黑夜，只有本地存过白天偏好时才切白天，避免刷新闪烁；
    # 浏览器禁用本地存储时必须静默回退，不能抛出异常导致整段脚本中断。
    assert 'var theme="dark";' in html
    assert 'if(localStorage.getItem("option-scope-theme")==="light")theme="light";' in html
    assert "catch(error){}document.documentElement.dataset.theme=theme;" in html
    assert 'id="theme-toggle"' in html
    assert ':class="view.themeIcon"' in html
    # 顶栏切换按钮与主题逻辑。
    assert 'const THEME_KEY = "option-scope-theme";' in source
    assert "function applyTheme(theme)" in source
    assert "function initTheme()" in source
    assert "initTheme();" in source
    assert 'state.view.themeIcon = next === "light" ? "el-icon-sunny" : "el-icon-moon-night";' in source
    # 正文配色一律走令牌：除前两行 :root 定义外不应残留裸十六进制色值。
    body = "\n".join(line for index, line in enumerate(styles.splitlines(), 1) if index not in (2, 3))
    assert "#" not in body


def test_chain_table_drops_contract_column_and_price_columns():
    """期权链表格：去掉合约列与最新价/买价/卖价，类型并入行权价后共 7 列，数据单元格统一居中。"""
    html = Path("app/static/index.html").read_text(encoding="utf-8")
    source = Path("app/static/common/js/app.js").read_text(encoding="utf-8")
    styles = Path("app/static/common/css/styles.css").read_text(encoding="utf-8")
    # 表头去掉「合约」与「类型」（类型改由行权价文字颜色表达），共 7 列（<thead> 不计入）。
    assert "<th>合约</th>" not in html
    assert "<th>类型</th>" not in html
    assert '<th class="num">行权价</th><th class="num">成交量</th>' in html
    assert html.count("<th ") + html.count("<th>") == 7
    # 买卖价与最新价不再展示（数据仍保留在快照里，只是不占表格列）。
    assert "最新价" not in html and "买价" not in html and "卖价" not in html
    assert "row.last_price" not in source and "row.bid" not in source and "row.ask" not in source
    # 数据行不再渲染合约代码列。
    assert 'key: row.contract_symbol ||' in source
    # 空态与数据行的 colspan 与列数一致。
    assert 'colspan="7"' in html
    assert html.count('colspan="7"') == 1
    # 期权链单元格统一居中（覆盖默认左对齐与 .num 的右对齐）。
    assert ".data-panel table th,.data-panel table td{text-align:center}" in styles


def test_chain_rows_heat_up_by_volume_and_open_interest():
    """期权链：成交量与未平仓按本屏强弱铺底色（看涨绿 / 看跌红），并新增 GEX 数字列。"""
    html = Path("app/static/index.html").read_text(encoding="utf-8")
    source = Path("app/static/common/js/app.js").read_text(encoding="utf-8")
    styles = Path("app/static/common/css/styles.css").read_text(encoding="utf-8")
    # 表头新增 GEX 列，位置在 Gamma 与 IV（估）之间
    assert '<th class="num">Gamma</th>' in html
    assert html.index(">Gamma</th>") < html.index(">GEX</th>") < html.index(">IV（估）</th>")
    # 底色只覆盖成交量与未平仓两列，口径为「各自列本屏最大值」
    assert "function heatPercent(value, peak)" in source
    assert "function heatCellModel(value, peak, label, hotLevel)" in source
    # 客户端只算 0~100 的相对强度，透明度区间交给 CSS 主题变量（黑夜里必须比白天更实，否则强弱看不出来）
    # 热点阈值按方向分开：绿底更亮，绿色格子更早换深色字（阈值取自两种字色的对比度交叉点）
    assert "const HEAT_HOT_LEVEL = { call: 68, put: 80 };" in source
    assert ':style="row.volumeStyle"' in html and ':style="row.interestStyle"' in html
    assert 'style: percent == null ? "" : `--heat:${percent}`' in source
    assert "color-mix(in srgb,var(--heat-tone) calc((var(--heat-floor) + var(--heat-gain) * var(--heat) / 100) * 1%),transparent)" in styles
    assert "--heat-floor:10;--heat-gain:68;--heat-tone-up:#34e0a1;--heat-tone-down:#ff8fa0;--heat-ink:#06211a" in styles
    assert "--heat-floor:12;--heat-gain:70;--heat-tone-up:#0f9d6a;--heat-tone-down:#d92b4b;--heat-ink:#0b241b" in styles
    assert 'heatCellModel(row.volume, volumePeak, "成交量", isCall ? HEAT_HOT_LEVEL.call : HEAT_HOT_LEVEL.put)' in source
    assert 'heatCellModel(row.open_interest, interestPeak, "未平仓", isCall ? HEAT_HOT_LEVEL.call : HEAT_HOT_LEVEL.put)' in source
    assert "const volumePeak = Math.max(0, ...rows.map((row) => Number(row.volume) || 0));" in source
    assert "const interestPeak = Math.max(0, ...rows.map((row) => Number(row.open_interest) || 0));" in source
    # GEX 与 Gamma 敞口图同口径（模型 Gamma × 未平仓 × 100 × 现价² × 0.01），按中文单位展示
    assert "function contractGex(row, spot)" in source
    assert "formatGex(gex / 1000000)" in source
    # 方向沿用图表约定：看涨为正、看跌为负（与表头 title 一致，整列之和即净 Gamma）
    assert 'const sign = row.contract_type === "call" ? 1 : -1;' in source
    assert "看涨为正、看跌为负" in html
    # 图例给出归一基准，等待数据时复位
    assert 'id="chain-heat-note"' in html
    assert 'state.view.chainHeatNote = "等待数据";' in source
    assert "function chainHeatNote(volumePeak, interestPeak)" in source
    # 配色走主题令牌，白天/黑夜自动适配；热点格文字换深色墨色，保证亮底上仍然读得清
    assert ".chain-call .chain-heat{--heat-tone:var(--heat-tone-up)}" in styles
    assert ".chain-put .chain-heat{--heat-tone:var(--heat-tone-down)}" in styles
    assert ".chain-heat-hot{font-weight:600;color:var(--heat-ink)}" in styles


def test_chain_type_filter_select():
    """期权链：类型筛选下拉框（全部 / 看涨 / 看跌），切换后只重绘表格且底色按当前显示的行归一。"""
    html = Path("app/static/index.html").read_text(encoding="utf-8")
    source = Path("app/static/common/js/app.js").read_text(encoding="utf-8")
    styles = Path("app/static/common/css/styles.css").read_text(encoding="utf-8")
    # 下拉框放在期权链面板标题右侧，三个选项齐全
    assert 'id="chain-type-filter"' in html
    assert '<el-option value="all" label="全部"></el-option>' in html
    assert '<el-option value="call" label="看涨"></el-option>' in html
    assert '<el-option value="put" label="看跌"></el-option>' in html
    assert html.index('id="chain-title"') < html.index('id="chain-type-filter"') < html.index('id="chain-body"')
    # 选项文案与状态键走同一份常量，避免两边写错
    assert 'const CHAIN_FILTERS = { all: "全部", call: "看涨", put: "看跌" };' in source
    assert 'chainFilter: "all"' in source
    # 切换筛选只重绘表格：保留最近一次链数据，不重新请求接口
    assert "function renderChainTable()" in source
    assert 'state.chainRows = rows; state.chainSpot = quote?.price ?? null;' in source
    assert 'v-model="chainFilter"' in html and '@change="filterChanged"' in html
    assert 'filterChanged() { return renderChainTable(); }' in source
    # 归一基准跟着当前显示的行走
    assert 'const shown = state.chainFilter === "all" ? rows : rows.filter((row) => row.contract_type === state.chainFilter);' in source
    # 筛选框属于表格工具条：整块放在折叠区里（收起时随内容一起隐藏），并与表格左侧对齐
    assert ".chain-toolbar{display:flex;justify-content:flex-start;padding:12px 18px 10px}" in styles
    assert html.index('id="chain-fold"') < html.index('id="chain-type-filter"') < html.index('id="chain-body"')
    # 旧的标题行工具条（.panel-tools）已随结构改造删除，避免两条规则各说一套
    assert ".panel-tools" not in styles and ".panel-tools" not in html
    assert ".chain-filter{display:flex;align-items:center;gap:8px" in styles
    assert ".chain-filter select{min-width:0;height:30px" in styles
    assert ".chain-filter .el-select .el-input__inner{height:30px;border:1px solid var(--field-line);background:var(--field);color:var(--text);font:inherit}" in styles
    assert 'class="chain-filter"><span class="chain-filter-label">类型</span><el-select' in html
    assert '<label class="chain-filter">' not in html


def test_chain_panel_expands_by_default():
    """期权链面板：表格与图例默认展开；标题行（标的 · 到期日）与右侧状态栏始终可见。"""
    page = Path("app/static/index.html").read_text(encoding="utf-8")
    source = Path("app/static/common/js/app.js").read_text(encoding="utf-8")
    styles = Path("app/static/common/css/styles.css").read_text(encoding="utf-8")
    # 折叠按钮挂在标题行左侧（caret + 展开/收起），受控内容是包住工具栏、表格与图例的 chain-fold
    assert 'id="chain-toggle" type="button" aria-expanded="true" aria-controls="chain-fold"' in page
    assert 'id="chain-action"' in page
    assert 'id="chain-fold" hidden' not in page
    fold = page[page.index('id="chain-fold"') : page.index('</section>', page.index('id="chain-fold"'))]
    assert 'class="chain-toolbar"' in fold and 'class="table-wrap"' in fold and 'id="chain-heat-note"' in fold
    # 报错提示不跟着折叠：出错时即使面板收起也要看得见
    assert page.index('id="error-box"') < page.index('id="chain-fold"')
    # 默认展开 + 状态记 sessionStorage（同一标签页换标的、跳 URL 不必重复展开）
    assert 'const CHAIN_KEY = "option-scope-chain";' in source
    # 折叠逻辑走通用实现：与「分析详情」「图表」共用同一份 bindFoldGroup
    assert "function bindFoldGroup({ headerId, toggleId, bodyId, actionId, storageKey, defaultExpanded, onChange, shouldIgnore })" in source
    assert 'bodyId: "chain-fold",' in source
    assert 'storageKey: CHAIN_KEY,' in source
    # 期权链组默认展开（注意：图表组也是 true，这里靠 bodyId 定位到本组）
    chain_group = source[source.index("function initChainGroup()") : source.index("function initChartGroup()")]
    assert "defaultExpanded: true," in chain_group
    assert "function initChainGroup()" in source
    assert "initChainGroup();" in source
    # 通用实现按 storageKey 读写本次会话的偏好
    assert "sessionStorage.getItem(storageKey)" in source and "sessionStorage.setItem(storageKey" in source
    # 标题行右侧是数据来源与快照时间，点它不折叠
    assert 'shouldIgnore: (event) => Boolean(closestElement(event.target, ".panel-status")),' in chain_group
    # 样式：hidden 生效；分隔线从标题行挪到内容体上，收起时不会出现双层边框
    assert ".chain-fold[hidden]{display:none}" in styles
    assert ".chain-fold{border-top:1px solid var(--line)}" in styles
    # 折叠按钮直接复用「分析详情」的基础样式（含 border:0，避开 UA 默认白边），不再有期权链专用覆盖
    assert 'class="detail-toggle" id="chain-toggle"' in page
    assert ".panel-toggle" not in styles and ".panel-heading" not in styles


def test_chart_group_collapses_but_defaults_expanded():
    """图表折叠组：Gamma 敞口 / 压力位·支撑位 / 成交量分布 / 持仓量分布四张图，默认展开。"""
    page = Path("app/static/index.html").read_text(encoding="utf-8")
    source = Path("app/static/common/js/app.js").read_text(encoding="utf-8")
    charts = Path("app/static/common/js/charts.js").read_text(encoding="utf-8")
    styles = Path("app/static/common/css/styles.css").read_text(encoding="utf-8")
    # 四张图整块包进一个折叠组，标题栏给出四张图的名字
    assert 'id="chart-group"' in page
    assert 'id="chart-toggle" type="button" aria-expanded="true" aria-controls="chart-fold"' in page
    assert 'id="chart-action"' in page
    group = page[page.index('id="chart-fold"') : page.index('class="data-panel panel"')]
    for chart_id in ('id="gex-chart"', 'id="levels-chart"', 'id="volume-chart"', 'id="oi-chart"'):
        assert chart_id in group
    # 默认展开：内容体不带 hidden，按钮 aria-expanded=true，右侧文案是「收起」
    assert 'id="chart-fold"' in page and 'id="chart-fold" hidden' not in page
    assert '<span class="detail-action" id="chart-action">收起</span>' in page
    # 交互：走通用折叠实现，默认值 true，展开后补一次图表重绘（隐藏时量到的尺寸不对）
    assert 'const CHART_KEY = "option-scope-charts";' in source
    assert "function initChartGroup()" in source
    assert "initChartGroup();" in source
    assert 'bodyId: "chart-fold",' in source
    assert 'storageKey: CHART_KEY,' in source
    assert "defaultExpanded: true," in source
    assert "onChange: (expanded) => { if (expanded) requestAnimationFrame(() => redrawChartsIfResized()); }," in source
    assert "function redrawChartsIfResized()" in source
    assert "chartResizeTimer = setTimeout(redrawChartsIfResized, 200);" in source
    # 高频鼠标移动按帧合并，避免每个 pointermove 都同步重排 SVG；时钟也不应触发 Vue 根实例更新。
    assert "function scheduleChartPointerMove(chartSvg, onMove, hideTooltip)" in charts
    assert "const cancelPointerMove = scheduleChartPointerMove(chartSvg" in charts
    assert "function updateClock()" in source
    assert "setInterval(updateClock, 1000);" in source
    assert 'id="clock" v-text=' not in page
    # 样式：内容体沿用折叠组的 .detail-body，内部栅格不再叠加下边距
    assert ".chart-fold .analysis-grid{margin-bottom:0}" in styles


def test_fold_handlers_bind_after_vue_mount():
    """折叠事件必须绑定在 Vue 挂载之后，避免 Vue 重建模板节点时丢失原生监听器。"""
    source = Path("app/static/common/js/app.js").read_text(encoding="utf-8")
    mount = source.index("const optionScopeApp = new Vue({")
    mounted = source.index("window.optionScopeApp = optionScopeApp;")
    assert mounted > mount
    assert source.index("initDetailGroup();", mounted) > mounted
    assert source.index("initChainGroup();", mounted) > mounted
    assert source.index("initChartGroup();", mounted) > mounted
    assert "function closestElement(target, selector)" in source


def test_chain_strike_column_carries_contract_type():
    """期权链：类型列并入行权价——行权价文字看涨绿、看跌红，仍保持加粗。"""
    html = Path("app/static/index.html").read_text(encoding="utf-8")
    source = Path("app/static/common/js/app.js").read_text(encoding="utf-8")
    styles = Path("app/static/common/css/styles.css").read_text(encoding="utf-8")
    # 表头不再有类型列，行权价是首列
    assert "<th>类型</th>" not in html
    assert html.index(">行权价</th>") < html.index(">成交量</th>")
    # 行权价单元格同时带 chain-strike 与看涨/看跌类，颜色由后者决定
    assert 'typeClass: isCall ? "type-call" : "type-put"' in source
    assert 'strike: formatMoney(row.strike)' in source
    # 旧的类型列样式与单元格类已清掉（注意 chain-type-filter 是筛选下拉框，不受影响）
    assert 'class="chain-type' not in source and "td.chain-type" not in styles
    # 行权价保留加粗；取色规则必须盖过 td:first-child 的蓝色强调
    assert "#chain-body td.chain-strike{font-weight:650}" in styles
    assert "#chain-body td.type-call{color:var(--up)}" in styles
    assert "#chain-body td.type-put{color:var(--down)}" in styles
    # 图例说明「行权价颜色即类型」，否则类型列消失后用户看不出红绿含义
    assert "行权价绿=看涨、红=看跌" in html
    assert "行权价绿=看涨、红=看跌" in source


def test_charts_render_in_container_pixels_for_mobile():
    charts = Path("app/static/common/js/charts.js").read_text(encoding="utf-8")
    source = Path("app/static/common/js/app.js").read_text(encoding="utf-8")
    # viewBox 按容器像素绘制（1:1），手机窄屏不会把柱子和坐标文字整体等比缩小。
    assert "function chartContentBox(target)" in charts
    assert "const { width, height } = chartContentBox(target);" in charts
    assert "target.dataset.chartWidth = String(width);" in charts
    # 横轴刻度数量按可用宽度自适应，避免窄屏标签重叠。
    assert "Math.floor(innerWidth / 84)" in charts
    # 窗口尺寸变化（含手机横竖屏切换）后按新尺寸重绘。
    assert 'window.addEventListener("resize"' in source
    assert "renderAnalysis(rows, spot, analysisPayload, expirationRows || [], ivModel || {}, basis || null);" in source

def test_cached_chain_returns_sqlite_without_refresh(tmp_path: Path):
    database = Database(tmp_path / "options.db")
    database.write_snapshot(sample_quote(), sample_rows(), iso())

    class BlockingProvider(FakeProvider):
        def expirations(self, symbol: str) -> list[str]:
            raise AssertionError("有本地到期日时不应请求供应商")

        def fetch(self, symbol: str, expiration: str):
            raise AssertionError("有本地期权链时不应刷新供应商")

    settings = Settings(database_path=tmp_path / "options.db", proxy_url=None, default_symbols=("AAPL",), refresh_interval_seconds=60, raw_retention_days=30, cleanup_interval_seconds=86400, scheduler_enabled=False)
    service = SnapshotService(database, BlockingProvider())
    router = create_router(database, service, BlockingProvider(), settings)
    test_app = FastAPI()
    test_app.include_router(router)
    with TestClient(test_app) as client:
        expirations = client.get("/api/expirations/AAPL")
        assert expirations.status_code == 200
        assert expirations.json()["source"] == "sqlite"
        assert expirations.json()["expirations"] == ["2026-12-18"]
        chain = client.get("/api/chain/AAPL", params={"expiration": "2026-12-18"})
        assert chain.status_code == 200
        assert chain.json()["source"] == "sqlite"
        assert chain.json()["data"]


def test_recent_snapshot_requires_quote_and_chain(tmp_path: Path):
    """只有期权链没有有效现价时不能误判为新鲜快照。"""
    database = Database(tmp_path / "options.db")
    quote = sample_quote()
    quote["price"] = None
    database.write_snapshot(quote, sample_rows(), iso())
    service = SnapshotService(database, FakeProvider())
    assert service.recent_snapshot("AAPL", "2026-12-18", 60) is None


def test_recent_snapshot_requires_bid_ask_for_buyer_structures(tmp_path: Path):
    """只有成交量/持仓量而没有买卖价的旧缓存必须自动进入补抓流程。"""
    database = Database(tmp_path / "options.db")
    rows = [{**row, "bid": None, "ask": None} for row in sample_rows()]
    database.write_snapshot(sample_quote(), rows, iso())
    service = SnapshotService(database, FakeProvider())
    assert has_valid_two_sided_quotes(rows) is False
    assert service.recent_snapshot("AAPL", "2026-12-18", 60) is None
    assert has_valid_two_sided_quotes(sample_rows()) is True


def test_refresh_backfills_fresh_cache_without_bid_ask(tmp_path: Path):
    """缓存时间虽新但缺少买卖价时，刷新必须回源写入完整期权链。"""
    database = Database(tmp_path / "options.db")
    rows = [{**row, "bid": None, "ask": None} for row in sample_rows()]
    database.write_snapshot(sample_quote(), rows, iso())
    service = SnapshotService(database, FakeProvider())

    result = service.refresh("AAPL", "2026-12-18", max_age_seconds=60)

    assert result.get("skipped") is None
    assert has_valid_two_sided_quotes(database.latest_chain("AAPL", "2026-12-18")["data"]) is True


def test_zero_gamma_uses_nearby_expirations_and_regime_root():
    now_price = 330.0
    rows = [
        {"expiration": "2099-01-02", "contract_type": "put", "strike": 320, "open_interest": 5000, "implied_volatility": 0.3},
        {"expiration": "2099-01-02", "contract_type": "call", "strike": 340, "open_interest": 8000, "implied_volatility": 0.3},
        {"expiration": "2099-12-18", "contract_type": "call", "strike": 200, "open_interest": 900000, "implied_volatility": 0.8},
    ]
    from datetime import datetime, timezone
    result = find_zero_gamma(rows, now_price, horizon_days=7, now=datetime(2099, 1, 1, 18, 0, tzinfo=timezone.utc))
    assert result is not None
    assert 300 < result["price"] < 360
    assert result["horizon_days"] == 7
    assert result["expirations"] == ["2099-01-02"]

def test_missing_cache_does_not_block_on_provider(tmp_path: Path):
    database = Database(tmp_path / "options.db")

    class BlockingProvider(FakeProvider):
        def expirations(self, symbol: str) -> list[str]:
            raise AssertionError("无 refresh 时不应请求供应商到期日")

        def quote(self, symbol: str) -> dict:
            raise AssertionError("无 refresh 时不应请求供应商行情")

        def fetch(self, symbol: str, expiration: str):
            raise AssertionError("读取接口不应同步刷新供应商")

    settings = Settings(database_path=tmp_path / "options.db", proxy_url=None, default_symbols=("AAPL",), refresh_interval_seconds=60, raw_retention_days=30, cleanup_interval_seconds=86400, scheduler_enabled=False)
    service = SnapshotService(database, BlockingProvider())
    router = create_router(database, service, BlockingProvider(), settings)
    test_app = FastAPI()
    test_app.include_router(router)
    with TestClient(test_app) as client:
        expirations = client.get("/api/expirations/MSFT")
        assert expirations.status_code == 200
        assert expirations.json()["source"] == "pending"
        assert expirations.json()["expirations"] == []
        quote = client.get("/api/quote/MSFT")
        assert quote.status_code == 200
        assert quote.json()["source"] == "pending"
        chain = client.get("/api/chain/MSFT", params={"expiration": "2026-12-18"})
        assert chain.status_code == 200
        assert chain.json()["source"] == "pending"
        assert chain.json()["data"] == []


def test_active_expirations_drops_past_dates():
    today = market_today()
    past = (today - timedelta(days=1)).isoformat()
    future = (today + timedelta(days=2)).isoformat()
    assert active_expirations([past, today.isoformat(), future]) == [today.isoformat(), future]


def test_expirations_endpoint_hides_expired_dates(tmp_path: Path):
    database = Database(tmp_path / "options.db")
    today = market_today()
    past = (today - timedelta(days=1)).isoformat()
    future = (today + timedelta(days=2)).isoformat()
    database.write_snapshot(sample_quote(), sample_rows("AAPL", past) + sample_rows("AAPL", future), iso())

    class BlockingProvider(FakeProvider):
        def expirations(self, symbol: str) -> list[str]:
            raise AssertionError("有本地到期日时不应请求供应商")

    settings = Settings(database_path=tmp_path / "options.db", proxy_url=None, default_symbols=("AAPL",), refresh_interval_seconds=60, raw_retention_days=30, cleanup_interval_seconds=86400, scheduler_enabled=False)
    service = SnapshotService(database, BlockingProvider())
    router = create_router(database, service, BlockingProvider(), settings)
    test_app = FastAPI()
    test_app.include_router(router)
    with TestClient(test_app) as client:
        payload = client.get("/api/expirations/AAPL").json()
        assert payload["source"] == "sqlite"
        # 已过期（9/16 到期）的合约不再出现在可选到期日里，页面不会停在刷不动的历史快照上
        assert payload["expirations"] == [future]
        # 历史到期日的链数据仍可回看
        assert client.get("/api/chain/AAPL", params={"expiration": past}).json()["data"]


def test_volume_and_open_interest_charts_mark_peak_strikes():
    source = Path("app/static/common/js/app.js").read_text(encoding="utf-8") + Path("app/static/common/js/charts.js").read_text(encoding="utf-8")
    # 成交量、持仓量图分别标注最高看涨柱与最高看跌柱，样式与看涨墙/看跌墙一致
    # 峰值同样取所选到期日的分布数据（与柱状图口径一致）
    assert 'maxPoint(points, "callVolume")' in source
    assert 'maxPoint(points, "putVolume")' in source
    assert 'maxPoint(points, "callOi")' in source
    assert 'maxPoint(points, "putOi")' in source
    assert 'label: "看涨", position: "top"' in source
    assert 'label: "看跌", position: "bottom"' in source
    # 通用柱端标记：Gamma 图传墙位，成交/持仓图传最高柱
    assert "const markers = (options.markers || [])" in source
    # 长柱贴边时柱端文字翻到柱端内侧，并限制在安全带内，避免与横轴刻度、坐标文字重叠
    # 柱端文字始终在柱端外侧，落在上下预留的标签通道内，最长柱也压不到坐标刻度
    assert "const gutter = Math.min(44" in source
    assert "maxBarHeight" in source
    assert "tipY - 14 : tipY + 22" in source
    # 最左行权价向右避让左侧数值列，最左/最右按 SVG 视口收边，避免文字被裁剪
    assert "markerLabelWidth" in source
    assert "labelColumnRight" in source
    assert "Math.min(Math.max(x, halfLabel), width - halfLabel)" in source


def test_open_interest_falls_back_to_latest_valid_snapshot(tmp_path: Path):
    database = Database(tmp_path / "options.db")
    valid_at = iso(utc_now() - timedelta(hours=6))
    database.write_snapshot(sample_quote(), sample_rows(), valid_at)
    blank = sample_rows()
    for row in blank:
        row["open_interest"] = 0
    database.write_snapshot(sample_quote(), blank, iso())

    chain = database.latest_chain("AAPL", "2026-12-18")
    assert [row["open_interest"] for row in chain["data"]] == [500, 400]
    assert chain["oi_fallback"] == {"restored": 2, "as_of": valid_at}
    profile = database.latest_chains("AAPL", horizon_days=365)
    assert [row["open_interest"] for row in profile["data"]] == [500, 400]
    assert profile["oi_fallback"] == {"restored": 2, "as_of": valid_at}

    fresh = sample_rows()
    fresh[0]["open_interest"] = 999
    fresh[1]["open_interest"] = 888
    database.write_snapshot(sample_quote(), fresh, iso())
    updated = database.latest_chain("AAPL", "2026-12-18")
    # 最新快照自带未平仓量时不做任何回溯替换
    assert [row["open_interest"] for row in updated["data"]] == [999, 888]
    assert updated["oi_fallback"] == {"restored": 0, "as_of": None}


def test_chain_and_gamma_endpoints_expose_open_interest_fallback(tmp_path: Path):
    database = Database(tmp_path / "options.db")
    database.write_snapshot(sample_quote(), sample_rows(), iso(utc_now() - timedelta(hours=6)))
    blank = sample_rows()
    for row in blank:
        row["open_interest"] = 0
    database.write_snapshot(sample_quote(), blank, iso())

    settings = Settings(database_path=tmp_path / "options.db", proxy_url=None, default_symbols=("AAPL",), refresh_interval_seconds=60, raw_retention_days=30, cleanup_interval_seconds=86400, scheduler_enabled=False)
    service = SnapshotService(database, FakeProvider())
    router = create_router(database, service, FakeProvider(), settings)
    test_app = FastAPI()
    test_app.include_router(router)
    with TestClient(test_app) as client:
        payload = client.get("/api/chain/AAPL", params={"expiration": "2026-12-18"}).json()
        assert [row["open_interest"] for row in payload["data"]] == [500, 400]
        assert payload["oi_fallback"]["restored"] == 2
        gamma = client.get("/api/gamma/AAPL", params={"horizon_days": 365}).json()
        assert gamma["oi_fallback"]["restored"] == 2
        assert [row["open_interest"] for row in gamma["data"]] == [500, 400]


def test_open_interest_fallback_is_surfaced_in_frontend():
    source = Path("app/static/common/js/app.js").read_text(encoding="utf-8")
    # 回溯只展示到日期，期权链来源标签与 Gamma 范围角标都要标注，避免把回溯值当成实时未平仓量
    assert "function formatDay(value)" in source
    assert "payload.oi_fallback" in source
    assert "analysisPayload?.oi_fallback" in source
    assert source.count("未平仓量回溯") == 2


def test_symbol_load_navigates_via_url():
    source = Path("app/static/common/js/app.js").read_text(encoding="utf-8")
    # 载入按钮与输入框回车都走真实跳转：地址栏即状态，刷新/前进后退/分享链接都能复现同一视图
    assert "function navigateToSymbol()" in source
    # 换标的时清空 URL 里的到期日参数，统一回到列表第一个（最近）到期日；同标的重复载入保留当前选择
    assert 'const expiration = symbol === state.symbol ? state.expiration : "";' in source
    assert "buildPageQuery(symbol, expiration, location.pathname)" in source
    assert "location.assign(target)" in source
    assert "location.reload()" in source
    # 到期日列表加载后，URL 未指定或指定的日期不在列表里时回退到第一个（最近）到期日
    assert "state.expiration = unique.includes(requested) ? requested : unique[0];" in source
    assert '@click="navigateToSymbol"' in Path("app/static/index.html").read_text(encoding="utf-8")
    assert '@keyup.enter.native="navigateToSymbol"' in Path("app/static/index.html").read_text(encoding="utf-8")
    # 载入不再走页内切换，避免两套入口行为不一致
    assert '$("load-button").addEventListener("click", loadSymbol)' not in source


def test_gex_values_use_chinese_units():
    source = Path("app/static/common/js/app.js").read_text(encoding="utf-8") + Path("app/static/common/js/charts.js").read_text(encoding="utf-8")
    # GEX 内部按「百万美元」存储，展示时统一换算成「亿 / 万」，不再出现英文 M
    assert "function formatGex(value, digits = 2)" in source
    assert "${formatNumber(absolute / 100, digits)}亿" in source
    assert "${formatNumber(absolute * 100, 0)}万" in source
    assert "function formatChartValue(value, digits, unit)" in source
    assert '净 Gamma ${formatGex(' in source
    # 坐标轴、柱子提示、悬停卡片、右上角角标共用同一套换算，避免一处中文一处 M
    assert source.count("formatChartValue(") >= 6
    assert "${formatNumber(positive, 2)}${unit}" not in source
    assert "${formatNumber(Math.abs(negative), 2)}${unit}" not in source


def test_distribution_charts_match_reference_layout():
    source = Path("app/static/common/js/app.js").read_text(encoding="utf-8") + Path("app/static/common/js/charts.js").read_text(encoding="utf-8")
    page = Path("app/static/index.html").read_text(encoding="utf-8")
    # 成交量/持仓量图启用右轴、五档刻度与十字光标取值标签
    assert 'axis: "right", crosshairTags: true, valueLabel: "成交量"' in source
    assert 'axis: "right", crosshairTags: true, valueLabel: "持仓量"' in source
    assert "const axisOnRight = options.axis" in source
    assert "chart-crosshair-h" in source and "chart-tag-x" in source and "chart-tag-y" in source
    # 图例（含统计范围）与「全部 / 价内 / 价外」汇总表
    assert 'id="volume-summary"' in page and 'id="oi-summary"' in page
    # 图例共三处：成交量分布、持仓量分布，以及新增的压力位/支撑位柱状图
    assert page.count("chart-legend") == 3 and page.count("legend-dot") == 6
    assert "function contractInTheMoney(" in source
    assert "function summarizeDistribution(" in source
    assert "function renderDistributionSummary(" in source
    assert "<th></th><th>全部</th><th>价内</th><th>价外</th>" in source
    # 成交量/持仓量按中文计数单位展示
    assert "function formatCount(value, digits = 2)" in source


def test_levels_chart_panel_sits_beside_gamma():
    source = Path("app/static/common/js/charts.js").read_text(encoding="utf-8")
    app_source = Path("app/static/common/js/app.js").read_text(encoding="utf-8")
    page = Path("app/static/index.html").read_text(encoding="utf-8")
    # 新增的压力位/支撑位柱状图面板紧跟在 Gamma 敞口之后，两者同一行左右排布
    assert 'id="levels-chart"' in page and 'id="levels-basis"' in page
    assert page.index('id="gex-chart"') < page.index('id="levels-chart"') < page.index('id="volume-chart"')
    assert "analysis-wide" not in page
    # 原压力位/支撑位表格保留
    assert 'id="resistance-levels"' in page and 'id="support-levels"' in page
    assert "function renderLevelsChart(" in source
    assert "renderLevelsChart(payload)" in source
    assert "renderLevelsChart(null)" in app_source

def test_support_panel_sits_left_of_resistance():
    """支撑位在左、压力位在右；区间按买入/卖出提示配色，换位置不会串色。"""
    page = Path("app/static/index.html").read_text(encoding="utf-8")
    styles = Path("app/static/common/css/styles.css").read_text(encoding="utf-8")
    assert page.index('id="support-levels"') < page.index('id="resistance-levels"')
    assert ".levels-panel-resistance .level-strike{color:var(--down)}" in styles
    assert ".levels-panel-support .level-strike{color:var(--up)}" in styles


def test_model_iv_is_solved_from_option_prices():
    expiration = "2099-12-18"
    years = years_to_expiry(expiration)
    price = black_scholes_price(100.0, 100.0, 0.42, years, True)
    assert implied_volatility_from_price(price, 100.0, 100.0, years, True) == pytest.approx(0.42, abs=0.005)
    # 价格低于内在价值（或超出可行上限）时判定为不可用，避免污染样本
    assert implied_volatility_from_price(0.01, 100.0, 50.0, years, True) is None


def test_annotate_model_greeks_replaces_placeholder_iv():
    expiration = "2099-12-18"
    years = years_to_expiry(expiration)
    rows = []
    for strike, is_call in ((99.0, True), (100.0, True), (101.0, True), (95.0, False), (105.0, False)):
        rows.append({
            "expiration": expiration,
            "contract_type": "call" if is_call else "put",
            "strike": strike,
            # 用 0.35 的波动率生成理论价，再交给模型反解
            "last_price": black_scholes_price(100.0, strike, 0.35, years, is_call),
            "bid": 0.0,
            "ask": 0.0,
            "open_interest": 100,
            # 数据源常见占位值：直接用会把 Gamma 放大上百倍
            "implied_volatility": 0.00001,
        })
    summary = annotate_model_greeks(rows, 100.0)
    assert summary[expiration]["source"] == "price"
    assert summary[expiration]["iv"] == pytest.approx(0.35, abs=0.01)
    assert all(row["model_iv"] == pytest.approx(0.35, abs=0.01) for row in rows)
    assert all(row["model_gamma"] > 0 for row in rows)
    expected = rows[0]["model_gamma"] * 100 * 100 * 100.0 ** 2 * 0.01
    assert contract_gex(rows[0], 100.0) == pytest.approx(expected)


def test_chain_and_gamma_endpoints_expose_model_iv(tmp_path: Path):
    database = Database(tmp_path / "options.db")
    service = SnapshotService(database, FakeProvider())
    service.refresh("AAPL", "2026-12-18")
    settings = Settings(database_path=tmp_path / "options.db", proxy_url=None, default_symbols=("AAPL",), refresh_interval_seconds=60, raw_retention_days=30, cleanup_interval_seconds=86400, scheduler_enabled=False)
    router = create_router(database, service, FakeProvider(), settings)
    test_app = FastAPI()
    test_app.include_router(router)
    with TestClient(test_app) as client:
        chain = client.get("/api/chain/AAPL", params={"expiration": "2026-12-18"}).json()
        assert chain["iv_model"]["2026-12-18"]["iv"] > 0
        assert chain["data"][0]["model_iv"] == pytest.approx(chain["iv_model"]["2026-12-18"]["iv"], abs=1e-6)
        assert chain["data"][0]["model_gamma"] > 0
        profile = client.get("/api/gamma/AAPL", params={"horizon_days": 365}).json()
        assert profile["iv_model"]["2026-12-18"]["source"] in {"price", "provider", "default"}
        assert profile["data"][0]["model_iv"] > 0


def test_analysis_panels_share_selected_expiration():
    source = Path("app/static/common/js/app.js").read_text(encoding="utf-8")
    # Gamma 敞口与两张分布图统一只统计上方所选到期日
    assert "function renderAnalysis(rows, spot, analysisPayload, expirationRows = [], ivModel = {}, basis = null)" in source
    assert "const points = aggregateByStrike(expirationRows, spot);" in source
    assert "renderAnalysis(analysisRows, quote?.price, analysisPayload, rows, payload.iv_model || {}, activeBasis(quote));" in source
    assert '`到期日 ${state.expiration || "--"} · ${expirationRows.length} 个合约`' in source
    assert 'OptionScopeCharts.renderDistributionSummary(byId("volume-summary"), expirationRows, spot, "volume", "总成交量");' in source
    assert 'OptionScopeCharts.renderDistributionSummary(byId("oi-summary"), expirationRows, spot, "open_interest", "总持仓量");' in source
    assert 'renderSignedChart("gex-chart", points' in source
    assert 'renderSignedChart("volume-chart", points' in source
    assert 'renderSignedChart("oi-chart", points' in source
    # 旧的 45 天窗口与按到期日加权逻辑已删除
    assert "rowsWithinHorizon" not in source
    assert "gammaChartWeight" not in source
    assert "GAMMA_CHART_HORIZON_DAYS" not in source
    # 零 Gamma 与柱状图同口径扫描，仅在扫描不到穿越点时回退服务端结果
    assert "findGammaFlip(expirationRows, spot, state.expiration)" in source
    assert 'maxPoint(points, "callVolume")' in source
    assert 'maxPoint(points, "putOi")' in source


def test_refresh_skips_upstream_when_snapshot_is_fresh(tmp_path: Path):
    """本地快照仍在新鲜期内时，刷新接口复用 SQLite 并返回 skipped，不请求上游接口。"""
    database = Database(tmp_path / "options.db")
    database.write_snapshot(sample_quote(), sample_rows(), iso())

    class BlockingProvider(FakeProvider):
        def expirations(self, symbol: str) -> list[str]:
            raise AssertionError("快照仍新鲜时不应请求供应商到期日")

        def fetch(self, symbol: str, expiration: str):
            raise AssertionError("快照仍新鲜时不应请求供应商期权链")

    settings = Settings(database_path=tmp_path / "options.db", proxy_url=None, default_symbols=("AAPL",), refresh_interval_seconds=60, raw_retention_days=30, cleanup_interval_seconds=86400, scheduler_enabled=False)
    service = SnapshotService(database, BlockingProvider())
    router = create_router(database, service, BlockingProvider(), settings)
    test_app = FastAPI()
    test_app.include_router(router)
    with TestClient(test_app) as client:
        response = client.post("/api/refresh/AAPL", params={"expiration": "2026-12-18", "max_age": 60})
        assert response.status_code == 200
        body = response.json()
        assert body["skipped"] is True
        assert body["expiration"] == "2026-12-18"
        assert body["fetched_at"]
        assert body["age_seconds"] < 60
        assert body["quote"]["price"] == 200.5


def test_refresh_requests_provider_after_fresh_window(tmp_path: Path):
    """快照超过 max_age 后必须回源；max_age=0 表示强制刷新。"""
    database = Database(tmp_path / "options.db")
    database.write_snapshot(sample_quote(), sample_rows(), iso(utc_now() - timedelta(minutes=5)))
    settings = Settings(database_path=tmp_path / "options.db", proxy_url=None, default_symbols=("AAPL",), refresh_interval_seconds=60, raw_retention_days=30, cleanup_interval_seconds=86400, scheduler_enabled=False)
    service = SnapshotService(database, FakeProvider())
    assert service.recent_snapshot("AAPL", "2026-12-18", 60) is None
    stale = service.refresh("AAPL", "2026-12-18", max_age_seconds=60)
    assert stale.get("skipped") is None
    assert stale["rows"] == len(sample_rows())
    fresh = service.refresh("AAPL", "2026-12-18", max_age_seconds=60)
    assert fresh["skipped"] is True


def test_gamma_window_refreshes_fresh_chain_without_open_interest(tmp_path: Path):
    """刚写入但未平仓量全为 0 的链不能阻止 Gamma 窗口补抓。"""
    database = Database(tmp_path / "options.db")
    expiration = (market_today() + timedelta(days=7)).isoformat()
    blank_rows = sample_rows(expiration=expiration)
    for row in blank_rows:
        row["open_interest"] = 0
    database.write_snapshot(sample_quote(), blank_rows, iso())

    calls = {"fetch": 0}

    class CountingProvider(FakeProvider):
        def expirations(self, symbol: str) -> list[str]:
            return [expiration]

        def fetch(self, symbol: str, requested_expiration: str):
            calls["fetch"] += 1
            return sample_quote(symbol), sample_rows(symbol, requested_expiration), iso()

    service = SnapshotService(database, CountingProvider())
    result = service.refresh_window("AAPL", horizon_days=45)

    assert calls["fetch"] == 1
    assert result["results"]
    assert any(row["open_interest"] > 0 for row in database.latest_chain("AAPL", expiration)["data"])


def test_refreshes_from_separate_workers_share_sqlite_lease(tmp_path: Path):
    """两个 worker 实例同时刷新同一标的时只允许一次回源，其余复用新快照。"""
    database_path = tmp_path / "options.db"
    first_database = Database(database_path)
    second_database = Database(database_path)
    calls = {"expirations": 0, "fetch": 0}
    calls_lock = threading.Lock()
    start = threading.Barrier(2)

    class CountingProvider(FakeProvider):
        def expirations(self, symbol: str) -> list[str]:
            with calls_lock:
                calls["expirations"] += 1
            return super().expirations(symbol)

        def fetch(self, symbol: str, expiration: str):
            with calls_lock:
                calls["fetch"] += 1
            time.sleep(0.08)
            return super().fetch(symbol, expiration)

    first = SnapshotService(first_database, CountingProvider())
    second = SnapshotService(second_database, CountingProvider())

    def refresh(service: SnapshotService):
        start.wait()
        return service.refresh("AAPL", "2026-12-18", max_age_seconds=0)

    with ThreadPoolExecutor(max_workers=2) as executor:
        results = list(executor.map(refresh, (first, second)))
    assert calls == {"expirations": 1, "fetch": 1}
    assert any(result.get("coalesced") for result in results)
    assert first_database.latest_chain("AAPL", "2026-12-18")["data"]


def test_refresh_button_reads_sqlite_before_hitting_upstream():
    source = Path("app/static/common/js/app.js").read_text(encoding="utf-8")
    # 刷新入口先读 SQLite 判断新鲜度，只有过期才请求上游接口，并用 state.refreshing 拦截连点。
    assert "if (state.refreshing) return;" in source
    assert "function isSnapshotFresh(snapshot)" in source
    assert "function quoteIsReady(quote)" in source
    assert "function hasValidOptionQuotes(rows)" in source
    assert "snapshot?.optionsQuotesReady" in source
    assert "optionsQuotesReady: hasValidOptionQuotes(payload.data)" in source
    assert "snapshot?.shown && snapshot?.quoteReady" in source
    assert "age !== null && age < SNAPSHOT_FRESH_SECONDS" in source
    assert "if (isSnapshotFresh(snapshot)) {" in source
    assert "showFreshStatus(snapshot);" in source
    assert 'const params = new URLSearchParams({ max_age: String(SNAPSHOT_FRESH_SECONDS) });' in source
    assert "if (refreshResult?.skipped)" in source
    assert "const snapshot = await renderSnapshot(loadId, refreshResult.quote || null);" in source
    assert "let resolvedQuote = quoteIsReady(quote) ? quote : refreshResult?.quote;" in source
    assert "?refresh=true`" in source
    assert '@click="refreshNow"' in Path("app/static/index.html").read_text(encoding="utf-8")
    assert ':disabled="refreshing"' in Path("app/static/index.html").read_text(encoding="utf-8")
    assert "setInterval(() => refresh(true), AUTO_REFRESH_SECONDS * 1000);" in source
    # 跨期限 Gamma 窗口刷新改为后台任务，表格渲染完成后不再等待窗口。
    assert "function refreshAnalysisWindow(loadId, payload, quote)" in source
    assert "refreshAnalysisWindow(loadId, payload, resolvedQuote);" in source
    assert "async function latestSelectedChain(loadId, symbol, fallbackPayload)" in source
    assert "const selectedPayload = await latestSelectedChain(loadId, symbol, payload);" in source
    assert "?horizon_days=45&refresh=true" in source
    # 首次拿到选中期限的链后立即请求综合价位，不再等待慢速的跨期限 Gamma 窗口。
    assert "loadFactorLevels(points, levelSpot);" in source
    assert "renderChain(payload, quote, state.lastAnalysis?.analysisPayload || null);" in source
    assert "deferLevels: true" not in source
    assert "if (!snapshot.analysisReady && snapshot.payload?.data?.length && snapshot.quote)" in source
    # 页面加载、手动点击、定时刷新共用同一条刷新链路与网络互斥。
    assert "if (state.refreshInFlight === symbol) return;" in source
    assert "await loadChain({ loadId, refresh: true });" in source
    assert 'id="refresh-note"' in Path("app/static/index.html").read_text(encoding="utf-8")


def test_refresh_toolbar_places_note_before_button_and_uses_blue_hover():
    page = Path("app/static/index.html").read_text(encoding="utf-8")
    styles = Path("app/static/common/css/styles.css").read_text(encoding="utf-8")
    toolbar = page[page.index('<div class="toolbar-actions">') : page.index('</div>', page.index('<div class="toolbar-actions">'))]
    assert toolbar.index('id="refresh-note"') < toolbar.index('id="refresh-button"')
    assert ".toolbar-actions .el-button.secondary.el-button--button:hover,.toolbar-actions .el-button.secondary.el-button--button:focus{border-color:var(--blue);background:var(--blue);color:var(--on-blue)}" in styles


def test_symbol_without_expirations_falls_back_to_quote_only(tmp_path: Path):
    """没有挂牌期权的标的（例如 SPCX 这类）只写现货快照：刷新返回 quote_only，页面不再整页无数据。"""
    database = Database(tmp_path / "options.db")

    class NoOptionProvider(FakeProvider):
        def expirations(self, symbol: str) -> list[str]:
            return []

        def fetch(self, symbol: str, expiration: str):
            raise AssertionError("没有到期日时不应请求期权链")

    service = SnapshotService(database, NoOptionProvider())
    result = service.refresh("SPCX", None, max_age_seconds=60)
    assert result["quote_only"] is True
    assert result["expiration"] is None
    assert result["rows"] == 0
    # 现货仍然要落库：现货卡片与盘前盘后都依赖它。
    assert database.latest_quote("SPCX")["price"] == 200.5
    assert database.latest_expirations("SPCX") == []
    # 仅现货的快照同样有新鲜期，定时刷新不再重复打上游接口。
    skipped = service.refresh("SPCX", None, max_age_seconds=60)
    assert skipped["skipped"] is True
    assert skipped["quote_only"] is True
    assert skipped["expiration"] is None
    assert skipped["age_seconds"] < 60
    # 新鲜期外（本地只有 5 分钟前的现货快照）必须回源。
    stale_database = Database(tmp_path / "stale.db")
    stale_database.write_snapshot(sample_quote("SPCX"), [], iso(utc_now() - timedelta(minutes=5)))
    assert SnapshotService(stale_database, NoOptionProvider()).recent_snapshot("SPCX", None, 60) is None


def test_refresh_endpoint_returns_quote_only_without_expirations(tmp_path: Path):
    """接口层：标的不支持期权时刷新返回 quote_only，行情接口仍能读到现货。"""
    database = Database(tmp_path / "options.db")

    class NoOptionProvider(FakeProvider):
        def expirations(self, symbol: str) -> list[str]:
            return []

    provider = NoOptionProvider()
    settings = Settings(database_path=tmp_path / "options.db", proxy_url=None, default_symbols=("SPCX",), refresh_interval_seconds=60, raw_retention_days=30, cleanup_interval_seconds=86400, scheduler_enabled=False)
    service = SnapshotService(database, provider)
    router = create_router(database, service, provider, settings)
    test_app = FastAPI()
    test_app.include_router(router)
    with TestClient(test_app) as client:
        fresh = client.post("/api/refresh/SPCX", params={"max_age": 0}).json()
        assert fresh["quote_only"] is True
        assert fresh["expiration"] is None
        assert fresh["fetched_at"]
        quote = client.get("/api/quote/SPCX").json()
        assert quote["price"] == 200.5
        assert quote["source"] == "sqlite"
        assert client.get("/api/expirations/SPCX").json()["expirations"] == []
        # 再次刷新命中新鲜期，直接复用本地现货快照。
        skipped = client.post("/api/refresh/SPCX", params={"max_age": 60}).json()
        assert skipped["skipped"] is True
        assert skipped["quote_only"] is True


def test_frontend_loads_symbol_without_cached_expiration():
    source = Path("app/static/common/js/app.js").read_text(encoding="utf-8")
    # 首次载入一个从未抓过的标的时本地没有到期日：必须继续走到后台回源，否则页面永远停在无数据状态。
    assert "if (!state.expiration) return;" not in source
    assert "async function loadExpirations(loadId)" in source
    assert "await refreshInBackground(loadId);" in source
    # 仅现货标的单独一条渲染分支：现货照常画，期权面板统一提示没有期权数据。
    assert "async function loadQuoteOnly(loadId)" in source
    assert "if (refreshResult?.quote_only) { await loadQuoteOnly(loadId); return; }" in source
    assert 'applyExpirations([], null, "无期权到期日");' in source
    # 本地无缓存（source=pending）与「该标的确实没有期权」必须给出不同占位文案。
    assert 'payload.source === "pending" ? "正在获取到期日…" : "该标的没有期权到期日"' in source


def test_zero_gamma_scan_reuses_expiry_cache():
    source = Path("app/static/common/js/app.js").read_text(encoding="utf-8")
    # 到期日时间戳按到期日缓存：扫描十几万次时重复构造 Date/Intl 会占满主线程。
    assert "const expiryCache = new Map();" in source
    assert "if (expiryCache.has(expiration)) return expiryCache.get(expiration);" in source
    assert "expiryCache.set(expiration, result);" in source
    # 扫描前先算好每行常量，热点循环里只做一次指数运算。
    assert "function prepareGexRows(rows, expiration)" in source
    assert "function totalGexAtSpot(prepared, spotValue)" in source
    assert "const prepared = prepareGexRows(rows, expiration);" in source
    assert "totalGexAtSpot(prepared, nextSpot)" in source


def test_support_and_resistance_panels_render_ten_levels():
    """压力位/支撑位面板：各 10 条，以基准价（默认盘后价）上下分侧取持仓最集中的行权价。"""
    source = Path("app/static/common/js/app.js").read_text(encoding="utf-8")
    page = Path("app/static/index.html").read_text(encoding="utf-8")
    styles = Path("app/static/common/css/styles.css").read_text(encoding="utf-8")
    assert "const LEVEL_COUNT = 10;" in source
    assert "function pickLevels(points, spot, side, valueOf, count = LEVEL_COUNT)" in source
    assert 'side === "above" ? point.strike >= spot : point.strike <= spot' in source
    # 与看涨墙/看跌墙同口径排序；盘前整链没有未平仓量时退回成交量分布。
    assert "const byGex = openInterest > 0 && openInterest >= volume * 0.2;" in source
    assert 'const callValue = byGex ? (point) => Math.max(point.callGex, 0) : (point) => point.callVolume;' in source
    assert 'const metricLabel = byGex ? "Gamma 敞口" : "成交量";' in source
    # 逐条渲染 行权价 / 距现价 / 排序口径数值，离现价近的在前；同一段价位不重复占用名额。
    assert "return picked.sort((a, b) => Math.abs(a.strike - spot) - Math.abs(b.strike - spot));" in source
    assert "if (picked.some((item) => Math.abs(item.strike - point.strike) < minGap)) continue;" in source
    assert '<div class="level-row level-head level-factor-row"><span>价位区间</span><span>距现价</span>' in page
    assert "renderLevels(points, spot);" in source
    assert 'id="resistance-levels"' in page
    assert 'id="support-levels"' in page
    assert "levels-panel-resistance" in page and "levels-panel-support" in page
    assert ".analysis-levels-grid{display:grid;grid-template-columns:repeat(2,minmax(0,1fr));gap:12px;margin-bottom:16px}" in styles
    assert ".plan-grid,.levels-grid{display:contents}" in styles
    # 多因子改造：页面改为请求后端合成接口（斐波那契 + 筹码密集 + 承接位 + 期权持仓），
    # 结果逐条展示组成该价位的因子标签；接口不可用时退回上面的单因子口径。
    assert "function loadFactorLevels(points, spot)" in source
    assert "function renderFactorFallback(points, spot)" in source
    assert "function requestFactorLevels(points, spot, key, attempt = 0)" in source
    assert "state.levelsRetryTimer = setTimeout" in source
    assert "renderFactorFallback(points, spot);" in source
    assert "function renderFactorLevels(payload)" in source
    assert "function buildFactorViews(levels, spot, side, isAdd = false)" in source
    assert "function levelTooltipText(level, score, strengthTag)" in source
    assert "function levelDetailText(level)" in source
    assert 'class="level-note-row"' in page
    assert "function levelStrengthIntensity(level)" in source
    assert "level-strength-${levelStrengthIntensity(level)}" in source
    assert 'class="level-note-row"' in page
    assert 'class="level-note-content"' in page
    assert 'class="level-note-strength level-note-strength-support"' in page
    assert 'class="level-note-strength level-note-strength-resistance"' in page
    assert 'class="level-note-divider"> · </span>' in page
    assert 'const prefix = Number.isFinite(representative) && representative > 0 ? `代表价 ${formatMoney(representative)} · ` : "";' in source
    assert "历史回踩：暂无样本" in source
    assert "const numericSpot = Number(spot);" in source
    assert 'const spotQuery = Number.isFinite(numericSpot) && numericSpot > 0' in source
    assert "request(`/api/levels/${encodedSymbol}?expiration=${encodedExpiration}${spotQuery}`)" in source
    assert "loadFactorLevels(points, levelSpot);" in source
    # 基准价：优先盘后价，其次盘前价，最后常规价。
    assert "function levelBasis(quote, fallbackPrice)" in source
    assert 'for (const [key, label] of [["post", "盘后"], ["pre", "盘前"]])' in source
    assert "renderAnalysis(analysisRows, quote?.price, analysisPayload, rows, payload.iv_model || {}, activeBasis(quote));" in source
    # 到达概率列位于「距现价」与「综合依据」之间，四列布局。
    assert "function formatProbability(value)" in source
    assert '<span>距现价</span><span title="在所选到期日之前触及该价位的概率' in page
    assert "level-prob" in page
    assert ".level-factor-row,.level-plan-row{grid-template-columns:minmax(0,1.25fr) minmax(0,.85fr) minmax(0,.85fr) minmax(0,1.35fr);column-gap:0}" in styles
    assert ".level-factor-row>span,.level-plan-row>span{min-width:0;padding-inline:8px;text-align:center!important}" in styles
    assert ".level-factor-row>span+span,.level-plan-row>span+span{border-left:1px solid var(--row-line)}" in styles
    assert ".level-factor-row .level-factors,.level-plan-row .level-factors{padding-inline:0;gap:4px;justify-content:center;text-align:center}" in styles
    assert ".level-strong .level-factors{font-size:12px;gap:2px}" in styles
    assert ".level-strong .level-strength-badge{padding-inline:3px;font-size:10px}" in styles
    assert ".level-strong .level-factors{display:flex;flex-direction:column;align-items:center;justify-content:center;gap:2px;flex-wrap:nowrap;text-align:center}" in styles
    assert ".level-strong .level-strength-badge{position:static;transform:none}" in styles
    assert ".level-strong .level-factor-text{width:auto;min-width:0;max-width:100%;text-align:center!important}" in styles
    assert "@media(max-width:500px){.level-factor-row,.level-plan-row{grid-template-columns:minmax(0,2fr) minmax(0,1fr) minmax(0,1fr) minmax(0,1.7fr);column-gap:8px}.level-factor-row>span,.level-plan-row>span{padding-inline:0}.level-factor-row>span+span,.level-plan-row>span+span{border-left:0}.level-strong .level-factors{display:flex;justify-content:center}.level-strong .level-strength-badge{position:static;transform:none}.level-strong .level-factor-text{width:auto}}" in styles
    assert ".level-factor-row .level-strike{white-space:nowrap;overflow-wrap:normal}" in styles
    assert ".levels-panel-resistance .level-strike{color:var(--down)}" in styles
    assert ".levels-panel-support .level-strike{color:var(--up)}" in styles
    assert "斐波那契 · 筹码密集 · 承接位 · 期权持仓" in source
    assert ".level-factors" in styles
    assert ".level-note-row{display:grid;grid-template-columns:minmax(0,1fr);padding:2px 8px 6px;border-bottom:1px solid var(--row-line);background:transparent;color:var(--muted);font-size:11px;font-weight:400;line-height:1.35;text-align:center;white-space:normal;overflow-wrap:anywhere}" in styles
    assert ".level-note-content{display:inline;max-width:100%;min-width:0}" in styles
    assert ".level-note-strength{display:inline;padding:0;border:0;border-radius:0;background:transparent;font-size:inherit;font-weight:650;line-height:inherit;white-space:nowrap}" in styles
    assert ".level-note-strength-support{color:var(--up)}" in styles
    assert ".level-note-divider{color:var(--muted);font-weight:400}" in styles
    assert ".level-strength-1{--level-fill:8%;--level-edge:2px;--level-edge-fill:42%}" in styles
    assert ".level-strength-5{--level-fill:34%;--level-edge:4px;--level-edge-fill:100%}" in styles
    assert "颜色越深，综合强度越高" in page
    assert "body{font-size:14px}" in styles
    # Vue 2 在多根 template v-for 发生列表重排时可能访问到空虚拟节点；每条主行和说明行必须由同一个稳定 key 的容器包裹。
    assert '<div v-for="level in view.levels.add" :key="level.key" class="level-item">' in page
    assert '<div v-for="level in view.levels.support" :key="level.key" class="level-item">' in page
    assert '<div v-for="level in view.levels.resistance" :key="level.key" class="level-item">' in page
    assert '<div v-for="item in view.buyer.items" :key="item.key" class="buyer-structure-item">' in page
    assert '<template v-for="level in view.levels.' not in page
    assert '<template v-for="item in view.buyer.items">' not in page
    assert ".level-item{display:block}" in styles
    # 趋势可用状态必须也是单根节点，避免 Vue 2 在基准价切换时 patch 多根 template 产生空 vnode。
    assert '<div v-if="view.trend.available" class="trend-content">' in page
    assert '<template v-if="view.trend.available">' not in page
    assert ".trend-content{display:flex;flex:1 1 auto;flex-direction:column;min-height:0}" in styles
    assert ".level-note-row{font-size:12px}" in styles
    assert ".level-factors{font-size:13px}" in styles
    assert ".trend-meta{font-size:14px" in styles
    assert ".level-factor-row .level-strike,.level-plan-row .level-strike{white-space:normal;overflow-wrap:anywhere;line-height:1.2}" in styles
    assert ".level-factor-row .level-gap,.level-factor-row .level-prob,.level-plan-row .level-gap,.level-plan-row .level-prob{white-space:nowrap}" in styles
    assert ".level-factor-row.level-head>span:nth-child(3),.level-plan-row.level-head>span:nth-child(3){white-space:nowrap;letter-spacing:0}" in styles


def test_fibonacci_levels_follow_swing_leg():
    """斐波那契：低点在前按高点向下回撤，50% 与 61.8% 权重最高。"""
    bars = []
    for index in range(21):
        price = 100 - index
        bars.append({"date": f"2026-06-{index + 1:02d}", "open": price, "high": price + 0.5, "low": price - 0.5, "close": price, "volume": 1000.0})
    for index in range(41):
        price = 80 + index
        bars.append({"date": f"2026-07-{index + 1:02d}", "open": price, "high": price + 0.5, "low": price - 0.5, "close": price, "volume": 1000.0})
    levels = fibonacci_levels(bars, 121.0)
    by_tag = {tag: price for price, _, tag in levels}
    weights = {tag: weight for _, weight, tag in levels}
    high, low = 120.5, 79.5
    assert by_tag["斐波那契 61.8%"] == pytest.approx(high - (high - low) * 0.618)
    assert by_tag["斐波那契 50%"] == pytest.approx((high + low) / 2)
    assert weights["斐波那契 61.8%"] == 1.0
    assert weights["斐波那契 23.6%"] == 0.6


def test_chip_peaks_find_dense_price_zone():
    """筹码分布：成交集中在 100-104 区间，密集区必须落在这一段而不是零星成交的高位。"""
    bars = []
    for index in range(100):
        bars.append({"date": f"2026-06-{index % 28 + 1:02d}", "open": 102.0, "high": 104.0, "low": 100.0, "close": 102.5, "volume": 5000.0})
    for index in range(5):
        bars.append({"date": f"2026-09-{index + 1:02d}", "open": 140.0, "high": 142.0, "low": 139.0, "close": 140.0, "volume": 50.0})
    levels = chip_peaks(bars, 102.0)
    assert levels
    assert all(95 <= price <= 110 for price, _, _ in levels)
    assert levels[0][2] == "筹码密集"
    assert levels[0][1] == pytest.approx(1.0)


def test_absorption_levels_keep_held_dips():
    """承接位：被砸下去又收回、之后没有被跌破的低点才算数。"""
    bars = []
    for _ in range(10):
        bars.append({"date": "2026-06-01", "open": 110.0, "high": 111.0, "low": 108.0, "close": 110.0, "volume": 100.0})
    # 第一次下探：长下影收回，随后反弹且再未跌破 → 保留
    bars.append({"date": "2026-06-11", "open": 109.0, "high": 110.0, "low": 100.0, "close": 109.5, "volume": 300.0})
    for _ in range(6):
        bars.append({"date": "2026-06-12", "open": 110.0, "high": 116.0, "low": 109.0, "close": 115.0, "volume": 120.0})
    # 第二次下探：随后行情再次跌破 → 剔除
    bars.append({"date": "2026-06-18", "open": 114.0, "high": 115.0, "low": 104.0, "close": 114.5, "volume": 300.0})
    bars.append({"date": "2026-06-19", "open": 112.0, "high": 113.0, "low": 102.0, "close": 108.0, "volume": 150.0})
    bars.append({"date": "2026-06-20", "open": 110.0, "high": 112.0, "low": 103.0, "close": 109.0, "volume": 150.0})
    # 等低平台：不算被打下来的摆动低点
    for _ in range(2):
        bars.append({"date": "2026-06-21", "open": 108.0, "high": 110.0, "low": 103.0, "close": 109.0, "volume": 150.0})
    for _ in range(6):
        bars.append({"date": "2026-06-22", "open": 110.0, "high": 114.0, "low": 109.0, "close": 113.0, "volume": 120.0})
    levels = absorption_levels(bars, 113.0)
    prices = [price for price, _, _ in levels]
    assert 100.0 in prices
    assert 104.0 not in prices
    # 越近的承接位排在前面
    assert levels[0][0] == pytest.approx(102.0)
    assert prices.index(102.0) < prices.index(100.0)


def test_touch_probability_uses_volatility_and_horizon():
    """到达概率：零漂移首次触及模型，概率随波动率上升、随期限缩短下降，缺参数返回 None。"""
    assert touch_probability(105.0, 100.0, 0.2, 1.0) == pytest.approx(0.807, abs=0.005)
    assert touch_probability(120.0, 100.0, 0.2, 1.0) == pytest.approx(0.362, abs=0.005)
    assert touch_probability(95.0, 100.0, 0.2, 1.0) == pytest.approx(0.798, abs=0.005)
    assert touch_probability(110.0, 100.0, 0.4, 1.0) > touch_probability(110.0, 100.0, 0.2, 1.0)
    assert touch_probability(110.0, 100.0, 0.2, 0.1) < touch_probability(110.0, 100.0, 0.2, 1.0)
    assert touch_probability(110.0, 100.0, None, 1.0) is None
    assert touch_probability(110.0, 100.0, 0.2, None) is None


def test_average_true_range_uses_recent_true_ranges():
    """ATR 区域宽度使用最近真实波幅，并能识别前收盘跳空。"""
    bars = [
        {"high": 101, "low": 99, "close": 100},
        {"high": 106, "low": 104, "close": 105},
        {"high": 108, "low": 107, "close": 107.5},
    ]
    assert average_true_range(bars, period=2) == pytest.approx((6 + 3) / 2)
    assert average_true_range([], period=14) is None
    assert average_true_range(bars, period=0) is None


def test_average_true_ranges_matches_each_prefix_atr():
    """批量 ATR 与逐前缀口径一致，供历史回踩验证共享结果。"""
    bars = [
        {"high": 101, "low": 99, "close": 100},
        {"high": 106, "low": 104, "close": 105},
        {"high": 108, "low": 107, "close": 107.5},
    ]
    assert average_true_ranges(bars, period=2) == pytest.approx([2.0, 4.0, 4.5])


def test_merge_candidates_groups_same_price_zone():
    """合成：同一段价位（现价 0.5% 内）合并成一条，按稳定综合强度排序。"""
    spot = 100.0
    levels = merge_candidates([(101.0, 1.0, "A"), (101.3, 0.5, "B"), (110.0, 0.4, "C")], spot, "above")
    assert [item["price"] for item in levels] == [101.0, 110.0]
    assert levels[0]["factors"] == ["A", "B"]
    assert levels[0]["score"] == pytest.approx(0.7, abs=0.01)
    assert levels[1]["score"] == pytest.approx(0.37, abs=0.01)
    assert levels[0]["zone_low"] == pytest.approx(100.75)
    assert levels[0]["zone_high"] == pytest.approx(101.55)
    assert all(item["price"] > spot for item in levels)
    many = [(100 + index, 1.0, f"F{index}") for index in range(1, 13)]
    picked = merge_candidates(many, spot, "above")
    assert len(picked) == 10
    assert [item["price"] for item in picked] == [101.0, 102.0, 103.0, 104.0, 105.0, 106.0, 107.0, 108.0, 109.0, 110.0]


def test_merge_candidates_zone_boundaries_do_not_follow_spot():
    """区域边界由候选价位决定，现价靠近区域时不应把边界裁到现价。"""
    resistance_near = merge_candidates([(101.0, 1.0, "A")], 100.2, "above", zone_width=1.0)
    resistance_far = merge_candidates([(101.0, 1.0, "A")], 99.0, "above", zone_width=1.0)
    support_near = merge_candidates([(99.0, 1.0, "A")], 99.8, "below", zone_width=1.0)
    support_far = merge_candidates([(99.0, 1.0, "A")], 101.0, "below", zone_width=1.0)

    assert resistance_near[0]["zone_low"] == resistance_far[0]["zone_low"] == 100.0
    assert resistance_near[0]["zone_high"] == resistance_far[0]["zone_high"] == 102.0
    assert support_near[0]["zone_low"] == support_far[0]["zone_low"] == 98.0
    assert support_near[0]["zone_high"] == support_far[0]["zone_high"] == 100.0


def test_merge_candidates_score_does_not_depend_on_current_side_peak():
    """稳定综合强度：加入更强的远端候选，不应重新压低已有价位的分数。"""
    base = merge_candidates([(95.0, 0.4, "看跌持仓")], 100.0, "below", limit=10)
    with_stronger = merge_candidates([(95.0, 0.4, "看跌持仓"), (99.0, 1.0, "看跌墙")], 100.0, "below", limit=10)
    base_score = next(item["score"] for item in base if item["price"] == 95.0)
    stronger_score = next(item["score"] for item in with_stronger if item["price"] == 95.0)
    assert stronger_score == base_score


def test_merge_candidates_requires_independent_evidence_for_strong_score():
    """综合强度：普通单因子不标强，明确墙位或多类别共振才可达到强阈值。"""
    ordinary = merge_candidates([(101.0, 1.0, "看涨持仓")], 100.0, "above", limit=10)
    wall = merge_candidates([(101.0, 1.0, "看涨墙")], 100.0, "above", limit=10)
    resonance = merge_candidates(
        [(101.0, 0.8, "看涨持仓"), (101.2, 0.8, "筹码密集")],
        100.0,
        "above",
        limit=10,
    )
    assert ordinary[0]["score"] < 0.7
    assert wall[0]["score"] >= 0.7
    assert resonance[0]["score"] >= 0.7

    # 强锚点只传播部分数值，不能把相邻普通期权位抬过强阈值。
    clustered = merge_candidates(
        [(101.0, 1.0, "看涨墙"), (102.5, 1.0, "看涨持仓")],
        100.0,
        "above",
        limit=10,
    )
    nearby = next(item for item in clustered if item["price"] == 102.5)
    assert nearby["score"] < 0.7


def test_option_wall_requires_absolute_concentration_not_only_side_peak():
    """期权墙：本侧所有价位都接近时，最大值不能单独被标成墙。"""
    def point(strike, call_volume):
        return {
            "strike": strike, "callVolume": call_volume, "putVolume": 0.0,
            "callOi": 0.0, "putOi": 0.0, "callGex": 0.0, "putGex": 0.0,
        }

    ordinary, _, metric = option_levels([point(101, 10), point(102, 9), point(103, 8)], 100.0)
    assert metric == "volume"
    assert all(tag == "看涨持仓" for _, _, tag in ordinary)
    wall, _, _ = option_levels([point(101, 40), point(102, 10), point(103, 8)], 100.0)
    assert any(tag == "看涨墙" for _, _, tag in wall)


def test_level_history_validation_requires_hold_samples():
    """历史验证：长期守住才是强位，近期反复反弹的多因子位保留重点提示。"""
    bars = []
    for index in range(26):
        if index in (5, 12, 19):
            bars.append({"high": 103, "low": 99, "close": 101, "volume": 1000})
        elif index in (6, 7, 8, 13, 14, 15, 20, 21, 22):
            bars.append({"high": 104, "low": 100, "close": 102, "volume": 1000})
        else:
            bars.append({"high": 106, "low": 104, "close": 105, "volume": 1000})
    level = {"price": 100.0, "zone_low": 99.0, "zone_high": 101.0, "score": 0.8, "factors": ["筹码密集", "看跌持仓"]}
    validated = annotate_level_history([level], bars, "support")[0]
    assert validated["history_samples"] == 3
    assert validated["history_hold_rate"] == pytest.approx(1.0)
    assert validated["history_break_rate"] == pytest.approx(0.0)
    assert validated["history_adjusted_hold_rate"] == pytest.approx(0.8333)
    assert validated["history_adjusted_break_rate"] == pytest.approx(0.1667)
    assert validated["history_confidence"] == pytest.approx(0.5)
    assert validated["strength_tier"] == "reinforced"

    enough_samples = {
        **level,
        "history_samples": 8,
        "history_hold_rate": 0.875,
        "history_break_rate": 0.125,
        "history_adjusted_hold_rate": 0.75,
        "history_adjusted_break_rate": 0.25,
        "history_confidence": 8 / 11,
    }
    assert level_strength_tier(enough_samples) == "strong"

    insufficient = annotate_level_history([dict(level)], bars[:8], "support")[0]
    assert insufficient["strength_tier"] == "reinforced"
    absorption = annotate_level_history([{"price": 100.0, "zone_low": 99.0, "zone_high": 101.0, "score": 0.58, "factors": ["承接位"]}], bars[:8], "support")[0]
    assert absorption["strength_tier"] == "reinforced"

    broken_bars = list(bars)
    broken_bars[6] = {"high": 104, "low": 97, "close": 98, "volume": 1000}
    broken = annotate_level_history([dict(level)], broken_bars, "support")[0]
    assert broken["history_break_rate"] > 0
    assert broken["recent_reactions"] >= 2
    assert broken["strength_tier"] == "reinforced"


def test_recent_reaction_reinforces_multifactor_level_without_making_it_strong():
    """近期反弹可保留重点色，但不能绕过长期回踩验证直接成为强位。"""
    recent = {
        "score": 0.55,
        "model_score": 0.72,
        "factors": ["筹码密集", "看跌持仓"],
        "history_samples": 10,
        "history_hold_rate": 0.2,
        "history_break_rate": 0.8,
        "recent_samples": 5,
        "recent_reactions": 4,
        "recent_reaction_rate": 0.8,
    }
    assert level_strength_tier(recent) == "reinforced"
    assert level_strength_tier({**recent, "factors": ["筹码密集"], "recent_reactions": 2}) == "normal"


def test_level_history_uses_atr_available_at_each_historical_touch():
    """历史验证：后续极端波动不能改写早期触及时的突破阈值。"""
    bars = []
    for _ in range(5):
        bars.append({"high": 98, "low": 97, "close": 97.5, "volume": 1000})
    bars.append({"high": 101, "low": 99, "close": 100.5, "volume": 1000})
    # 早期触及后的低点已经跌破区域下沿；后续日线的巨大波动只用于制造未来 ATR 干扰。
    bars.append({"high": 100, "low": 98, "close": 98.5, "volume": 1000})
    for _ in range(14):
        bars.append({"high": 120, "low": 110, "close": 115, "volume": 1000})
    level = {"price": 100.0, "zone_low": 99.0, "zone_high": 101.0, "score": 0.8, "factors": ["筹码密集", "看跌持仓"]}
    validated = annotate_level_history([level], bars, "support")[0]
    assert validated["history_samples"] == 1
    assert validated["history_break_rate"] == pytest.approx(1.0)


def test_split_support_plan_prefers_deeper_quality_levels_for_add():
    """加仓位：应比近端买入位更深，并优先保留更高质量的深层支撑。"""
    levels = [
        {"price": 99, "score": 0.4}, {"price": 97, "score": 0.9},
        {"price": 95, "score": 0.8}, {"price": 93, "score": 0.3},
    ]
    buy, add = split_support_plan(levels, 100.0)
    assert [item["price"] for item in buy] == [99, 97]
    assert [item["price"] for item in add] == [95, 93]
    assert max(item["price"] for item in add) < max(item["price"] for item in buy)


def test_select_visible_levels_keeps_remote_strong_levels():
    """展示名额：保留近端价位的同时，远端强支撑/强压力不能被距离全部淘汰。"""
    levels = [{"price": float(100 - index * 2), "score": 0.2} for index in range(15)]
    levels[-1]["score"] = 0.9
    selected = select_visible_levels(levels, 100.0, 10)
    assert len(selected) == 10
    assert selected[-1]["price"] == 72.0


def test_merge_candidates_splits_overlapping_display_zones():
    """相邻候选的 ATR 区间按代表价中点切开，避免显示重复区间。"""
    levels = merge_candidates([(101.0, 1.0, "A"), (102.0, 0.9, "B"), (110.0, 0.8, "C")], 100.0, "above", zone_width=2.0)
    ordered = sorted(levels, key=lambda item: item["price"])
    assert ordered[0]["zone_high"] == pytest.approx(101.5)
    assert ordered[1]["zone_low"] == pytest.approx(101.5)
    assert all(left["zone_high"] <= right["zone_low"] for left, right in zip(ordered, ordered[1:]))
    assert all(item["zone_low"] <= item["price"] <= item["zone_high"] for item in ordered)


def test_build_levels_mixes_factors_and_degrades_without_history():
    """四类因子合成：技术面 + 期权分侧；没有历史行情时退化为纯期权口径。"""
    bars = sample_bars()
    spot = bars[-1]["close"]
    rows = sample_option_rows(spot)
    payload = build_levels(bars, rows, spot, "2026-12-18")
    assert payload["history_bars"] == len(bars)
    assert payload["options_metric"] in {"gex", "volume"}
    assert spot - 10 < payload["spot"] < spot + 3
    for side in ("resistance", "support"):
        assert 0 < len(payload[side]) <= 10
    assert all(item["price"] > payload["spot"] for item in payload["resistance"])
    assert all(item["price"] < payload["spot"] for item in payload["support"])
    resistance_tags = {tag for item in payload["resistance"] for tag in item["factors"]}
    support_tags = {tag for item in payload["support"] for tag in item["factors"]}
    assert any(tag.startswith("斐波那契") for tag in resistance_tags)
    assert any(tag.startswith("看涨") for tag in resistance_tags)
    assert any(tag.startswith("看跌") for tag in support_tags)
    degraded = build_levels([], rows, spot, "2026-12-18")
    assert degraded["history_bars"] == 0
    assert degraded["resistance"] and degraded["support"]


def test_build_levels_preserves_trend_bias_in_scores():
    """上行趋势偏向支撑、下行趋势偏向压力，且分数仍限制在 0 到 1。"""
    rising = [{
        "close": 100 + index * 0.5,
        "high": 101 + index * 0.5,
        "low": 99 + index * 0.5,
        "volume": 1000,
    } for index in range(60)]
    falling = [{
        "close": 130 - index * 0.5,
        "high": 131 - index * 0.5,
        "low": 129 - index * 0.5,
        "volume": 1000,
    } for index in range(60)]
    up = build_levels(rising, sample_option_rows(129.5), 129.5, "2026-12-18")
    down = build_levels(falling, sample_option_rows(100.5), 100.5, "2026-12-18")
    assert up["trend"]["direction"] == "up"
    assert down["trend"]["direction"] == "down"
    assert max(item["score"] for item in up["support"]) > max(item["score"] for item in up["resistance"])
    assert max(item["score"] for item in down["resistance"]) > max(item["score"] for item in down["support"])
    assert all(0 < item["score"] <= 1 for side in ("resistance", "support") for item in up[side] + down[side])


def test_levels_endpoint_combines_factors(tmp_path: Path):
    """接口：按所选到期日返回两侧压力位/支撑位与各因子标签，并带日线历史元信息。"""
    database = Database(tmp_path / "options.db")
    database.write_snapshot(sample_quote(), sample_rows(), iso())
    settings = Settings(database_path=tmp_path / "options.db", proxy_url=None, default_symbols=("AAPL",), refresh_interval_seconds=60, raw_retention_days=30, cleanup_interval_seconds=86400, scheduler_enabled=False)
    service = SnapshotService(database, FakeProvider())
    router = create_router(database, service, FakeProvider(), settings)
    test_app = FastAPI()
    test_app.include_router(router)
    with TestClient(test_app) as client:
        payload = client.get("/api/levels/AAPL", params={"expiration": "2026-12-18"}).json()
        assert payload["symbol"] == "AAPL"
        assert payload["expiration"] == "2026-12-18"
        assert payload["spot"] == pytest.approx(200.5)
        assert payload["history"]["bars"] == len(sample_bars())
        assert payload["history"]["source"] == "upstream"
        assert payload["trend_market"]["today_open"] == pytest.approx(sample_bars()[-1]["open"])
        assert payload["trend_market"]["previous_close"] == pytest.approx(sample_bars()[-1]["close"])
        assert payload["beta"]["benchmark"] == "标普500"
        assert payload["beta"]["period_label"] == "2年"
        for side in ("resistance", "support"):
            assert 0 < len(payload[side]) <= 10
            for item in payload[side]:
                assert item["factors"] and 0 < item["score"] <= 1
                assert 0 < item["probability"] <= 1
        assert all(item["price"] > payload["spot"] for item in payload["resistance"])
        assert all(item["price"] < payload["spot"] for item in payload["support"])
        resistance_tags = {tag for item in payload["resistance"] for tag in item["factors"]}
        support_tags = {tag for item in payload["support"] for tag in item["factors"]}
        assert any(tag.startswith("斐波那契") for tag in resistance_tags)
        assert any(tag.startswith(("看跌", "斐波那契", "筹码", "承接")) for tag in support_tags)
        # 趋势通道与交易计划（买入/加仓/卖出各最多 10 条）随合成结果一并返回
        assert payload["trend"]["direction"] in {"up", "down", "range"}
        assert payload["trend"]["lower"] <= payload["trend"]["upper"]
        plan = payload["plan"]
        # 买入/卖出各取最近 10 条；加仓是再往下的 10 条，候选不足时允许少于 10 条
        assert 0 < len(plan["buy"]) <= 10 and 0 < len(plan["sell"]) <= 10
        assert all(len(plan[key]) <= 10 for key in ("buy", "add", "sell"))
        trade_points = payload["trade_points"]
        assert set(trade_points) == {"buy", "sell"}
        assert payload["trade_points_horizon"] == {"trading_days": 5, "label": "未来 5 个交易日"}
        for point in trade_points.values():
            if point is not None:
                assert point["zone_low"] <= point["price"] <= point["zone_high"]
                assert 0 <= point["confidence"] <= 1
                assert 0 <= point["model_confidence"] <= 1
                assert 0 <= point["history_sample_confidence"] <= 1
                assert point["history_samples"] >= 0


def test_levels_endpoint_reuses_same_snapshot_analysis(tmp_path: Path, monkeypatch):
    """同一输入快照重复读取时只执行一次价位合成，快照变化后缓存键会自然失效。"""
    database = Database(tmp_path / "options.db")
    database.write_snapshot(sample_quote(), sample_rows(), iso())
    settings = Settings(database_path=tmp_path / "options.db", proxy_url=None, default_symbols=("AAPL",), refresh_interval_seconds=60, raw_retention_days=30, cleanup_interval_seconds=86400, scheduler_enabled=False)
    service = SnapshotService(database, FakeProvider())
    calls = {"count": 0}
    original = api_module.build_levels

    def counted_build_levels(*args, **kwargs):
        calls["count"] += 1
        return original(*args, **kwargs)

    monkeypatch.setattr(api_module, "build_levels", counted_build_levels)
    router = create_router(database, service, FakeProvider(), settings)
    test_app = FastAPI()
    test_app.include_router(router)
    with TestClient(test_app) as client:
        first = client.get("/api/levels/AAPL", params={"expiration": "2026-12-18"})
        second = client.get("/api/levels/AAPL", params={"expiration": "2026-12-18"})
    assert first.status_code == 200 and second.status_code == 200
    assert first.json()["support"] == second.json()["support"]
    assert calls["count"] == 1


def test_levels_endpoint_uses_previous_close_as_stable_candidate_anchor(tmp_path: Path):
    """现价变化时，候选池锚点沿用昨收，避免短时价格波动重建价位簇。"""
    database = Database(tmp_path / "options.db")
    quote = sample_quote()
    quote["previous_close"] = 198.0
    database.write_snapshot(quote, sample_rows(), iso())
    settings = Settings(database_path=tmp_path / "options.db", proxy_url=None, default_symbols=("AAPL",), refresh_interval_seconds=60, raw_retention_days=30, cleanup_interval_seconds=86400, scheduler_enabled=False)
    service = SnapshotService(database, FakeProvider())
    router = create_router(database, service, FakeProvider(), settings)
    test_app = FastAPI()
    test_app.include_router(router)
    with TestClient(test_app) as client:
        lower = client.get("/api/levels/AAPL", params={"expiration": "2026-12-18", "spot": 196.0}).json()
        higher = client.get("/api/levels/AAPL", params={"expiration": "2026-12-18", "spot": 201.0}).json()
    assert lower["candidate_spot"] == higher["candidate_spot"] == pytest.approx(198.0)


def test_levels_endpoint_aggregates_multiple_expirations_without_changing_selected_chain(tmp_path: Path):
    """价位接口聚合近期期限与选中远期期限；期权链图表接口仍按单一选中期限返回。"""
    database = Database(tmp_path / "options.db")
    selected = sample_rows(expiration="2026-12-18")
    near = sample_rows(expiration="2026-10-16")
    for index, row in enumerate(near):
        row["strike"] = 240 if row["contract_type"] == "call" else 160
        row["contract_symbol"] = f"AAPL261016{row['contract_type']}{index}"
        row["open_interest"] = 2500
        row["volume"] = 500
    database.write_snapshot(sample_quote(), selected, iso(utc_now() - timedelta(minutes=10)))
    database.write_snapshot(sample_quote(), near, iso(utc_now() - timedelta(minutes=5)))
    settings = Settings(database_path=tmp_path / "options.db", proxy_url=None, default_symbols=("AAPL",), refresh_interval_seconds=60, raw_retention_days=30, cleanup_interval_seconds=86400, scheduler_enabled=False)
    service = SnapshotService(database, FakeProvider())
    router = create_router(database, service, FakeProvider(), settings)
    test_app = FastAPI()
    test_app.include_router(router)
    with TestClient(test_app) as client:
        payload = client.get("/api/levels/AAPL", params={"expiration": "2026-12-18"}).json()
        chain = client.get("/api/chain/AAPL", params={"expiration": "2026-12-18"}).json()
        gamma = client.get("/api/gamma/AAPL", params={"horizon_days": 45}).json()
        assert payload["options_expirations"] == ["2026-10-16", "2026-12-18"]
        assert payload["options_horizon_days"] == 45
        assert any(item["price"] == pytest.approx(240) for item in payload["resistance"])
        assert {row["expiration"] for row in gamma["data"]} == {"2026-10-16"}
        assert {row["expiration"] for row in chain["data"]} == {"2026-12-18"}


def test_level_zones_contain_their_representative_price():
    """压力位和支撑位区间围绕代表价生成，不再被现价裁剪。"""
    payload = build_levels(sample_bars(), sample_option_rows(200.5), 200.5, "2026-12-18")
    assert payload["resistance"] and payload["support"]
    assert all(item["zone_low"] <= item["price"] <= item["zone_high"] for side in ("resistance", "support") for item in payload[side])


def test_trend_channel_classifies_direction():
    """趋势通道：按最近日线收盘价的最小二乘拟合判定方向，样本不足或缺数据时返回 None。"""
    rising = [{"close": 100 + index * 0.5} for index in range(60)]
    falling = [{"close": 100 - index * 0.5} for index in range(60)]
    flat = [{"close": 100 + (index % 2)} for index in range(60)]
    assert trend_channel(rising)["direction"] == "up"
    assert trend_channel(falling)["direction"] == "down"
    assert trend_channel(flat)["direction"] == "range"
    assert trend_channel(rising[:10]) is None
    assert trend_channel([]) is None


def test_trend_channel_recognizes_confirmed_rebound_after_medium_term_drop():
    """中期下跌后最近 20 根日线持续反弹时，趋势切换为反弹上行。"""
    bars = [{"close": 200 - index * 2.0} for index in range(40)]
    bars.extend({"close": 120 + index * 2.2} for index in range(20))
    trend = trend_channel(bars)
    assert trend and trend["direction"] == "up"
    assert trend["label"] == "反弹上行 · 上涨趋势"
    assert trend["background_direction"] == "down"
    assert trend["reversal_confirmed"] is True
    assert trend["bars"] == 20


def test_calculate_beta_uses_common_daily_returns():
    """Beta 使用共同交易日收益率；同比例缩放的价格序列 Beta 应为 1。"""
    days = [{"date": (date(2025, 1, 1) + timedelta(days=index)).isoformat(), "close": 100 + index} for index in range(40)]
    scaled = [{**bar, "close": bar["close"] * 2} for bar in days]
    result = calculate_beta(scaled, days)
    assert result and result["value"] == pytest.approx(1.0)
    assert result["benchmark"] == "标普500"
    assert result["period_label"] == "2年"
    assert calculate_beta(days[:20], days[:20]) is None


def test_trend_market_data_falls_back_to_last_trading_day(monkeypatch):
    """当日没有日线时，今开/昨收回退到前一个交易日的开盘/收盘。"""
    monkeypatch.setattr("app.api.market_today", lambda: date(2026, 9, 19))
    bars = [
        {"date": "2026-09-18", "open": 100, "close": 105},
        {"date": "2026-09-19", "open": 110, "close": 115},
    ]
    current = trend_market_data(bars, {"market_state": "REGULAR"})
    assert current["today_open"] == 110 and current["previous_close"] == 105
    fallback = trend_market_data(bars[:1], {"market_state": "PRE"})
    assert fallback["today_open"] == 100 and fallback["previous_close"] == 105


def test_trade_recommendation_combines_trend_and_nearby_levels():
    """操作建议：上行靠近支撑买入，下行靠近压力卖出，信号不明确时持有。"""
    up = {"direction": "up", "lower": 95, "upper": 110}
    down = {"direction": "down", "lower": 90, "upper": 110}
    assert trade_recommendation(up, [{"price": 99}], [{"price": 109}], 100)["action"] == "buy"
    assert trade_recommendation(down, [{"price": 90}], [{"price": 101}], 100)["action"] == "sell"
    assert trade_recommendation(up, [{"price": 90}], [{"price": 110}], 100)["action"] == "hold"
    assert trade_recommendation(None, [], [], 100)["action"] == "hold"


def test_best_trade_points_selects_multi_factor_zones():
    """最佳买卖点：按强度、因子共振、触及概率、距离和趋势方向综合评分。"""
    result = best_trade_points(
        {"direction": "up"},
        [
            {"price": 95, "zone_low": 94, "zone_high": 96, "score": 0.7, "probability": 0.8, "factors": ["承接位"]},
            {"price": 98, "zone_low": 97, "zone_high": 99, "score": 1.0, "probability": 0.9, "factors": ["斐波那契", "筹码密集", "承接位"]},
        ],
        [{"price": 105, "zone_low": 104, "zone_high": 106, "score": 0.9, "probability": 0.8, "factors": ["筹码密集", "看涨持仓"]}],
        100,
    )
    assert result["buy"]["zone_low"] == 97
    assert result["buy"]["zone_high"] == 99
    assert result["buy"]["confidence"] >= 0.8
    assert result["sell"]["zone_low"] == 104
    assert result["sell"]["zone_high"] == 106


def test_best_trade_points_separates_overlapping_buy_and_sell_zones():
    """近期最佳买卖区间保留各自的原始边界，不因另一侧候选变化而裁剪。"""
    result = best_trade_points(
        {"direction": "range"},
        [{"price": 148, "zone_low": 145.65, "zone_high": 150.47, "score": 0.8, "factors": ["承接位"]}],
        [{"price": 150, "zone_low": 147.53, "zone_high": 151.25, "score": 0.8, "factors": ["看涨持仓"]}],
        149.17,
    )
    assert result["buy"]["zone_high"] == pytest.approx(150.47)
    assert result["sell"]["zone_low"] == pytest.approx(147.53)
    assert result["buy"]["zone_low"] == pytest.approx(145.65)
    assert result["sell"]["zone_high"] == pytest.approx(151.25)


def test_best_trade_points_uses_history_to_calibrate_confidence():
    """历史样本充分且守住率较高时，最佳点综合评分应高于纯模型评分。"""
    result = best_trade_points(
        {"direction": "up"},
        [{
            "price": 95,
            "zone_low": 94,
            "zone_high": 96,
            "model_score": 0.7,
            "history_samples": 12,
            "history_adjusted_hold_rate": 0.85,
            "history_adjusted_break_rate": 0.15,
            "history_confidence": 0.8,
            "factors": ["承接位", "筹码密集"],
        }],
        [],
        100,
    )
    assert result["buy"]["model_confidence"] < result["buy"]["confidence"]
    assert result["buy"]["history_samples"] == 12
    assert result["buy"]["history_hold_rate"] == pytest.approx(0.85)


def test_frontend_confirms_trade_point_before_replacing_it():
    """前端候选点需要连续两次快照确认，避免实时刷新导致最佳点闪烁。"""
    source = Path("app/static/common/js/app.js").read_text(encoding="utf-8")
    page = Path("app/static/index.html").read_text(encoding="utf-8")
    assert "const TRADE_POINT_CONFIRMATIONS = 2;" in source
    assert "function stabilizeTradePoints(points, context)" in source
    assert "nextCount >= TRADE_POINT_CONFIRMATIONS" in source
    assert "const stableTradePoints = stabilizeTradePoints(payload?.trade_points, tradePointContext);" in source
    assert "综合评分 {{ item.confidence }}" in page
    assert "{{ item.historySummary }}" in page


def test_levels_analysis_cache_namespace_matches_current_scoring_model():
    """最佳点评分字段变化时必须跳过旧版分析缓存。"""
    source = Path("app/api.py").read_text(encoding="utf-8")
    assert '"levels-v8"' in source
    assert '"levels-v7"' not in source


def test_build_levels_reuses_stable_candidate_anchor_across_basis_prices():
    """实时价和盘后价只改变当前口径评分，不应重建候选价位池。"""
    rows = sample_option_rows(200.5)
    live = build_levels(sample_bars(), rows, 200.5, "2026-12-18", candidate_spot=200.5)
    close = build_levels(sample_bars(), rows, 199.5, "2026-12-18", candidate_spot=200.5)

    assert live["candidate_spot"] == close["candidate_spot"] == pytest.approx(200.5)
    assert live["trade_points"]["sell"]["price"] == close["trade_points"]["sell"]["price"]
    assert live["trade_points"]["sell"]["zone_low"] == close["trade_points"]["sell"]["zone_low"]
    assert live["trade_points"]["sell"]["zone_high"] == close["trade_points"]["sell"]["zone_high"]


def test_build_levels_exposes_trend_and_plan():
    """交易计划：买入取最近的支撑、加仓取更深一档支撑、卖出取最近的压力，各最多 10 条。"""
    spot = 200.5
    payload = build_levels(sample_bars(), sample_option_rows(spot), spot, "2026-12-18")
    trend = payload["trend"]
    assert trend and trend["direction"] in {"up", "down", "range"}
    assert trend["lower"] <= trend["upper"]
    plan = payload["plan"]
    for key in ("buy", "add", "sell"):
        assert len(plan[key]) <= 10
    assert plan["buy"] and plan["sell"]
    price = payload["spot"]
    assert all(item["price"] < price for item in plan["buy"] + plan["add"])
    assert all(item["price"] > price for item in plan["sell"])
    # 买入位比加仓位更靠近现价；两侧列表与压力位/支撑位同源
    if plan["add"]:
        assert plan["buy"][0]["price"] >= plan["add"][0]["price"]
    assert plan["buy"][0]["price"] == payload["support"][0]["price"]
    assert plan["sell"][0]["price"] == payload["resistance"][0]["price"]
    # 没有现价时给出空计划，页面显示占位符
    empty = build_levels([], [], None, None)
    assert empty["trend"] is None
    assert empty["plan"] == {"buy": [], "add": [], "sell": []}


def test_trading_plan_panels_render_under_headline():
    source = Path("app/static/common/js/app.js").read_text(encoding="utf-8")
    page = Path("app/static/index.html").read_text(encoding="utf-8")
    styles = Path("app/static/common/css/styles.css").read_text(encoding="utf-8")
    # 趋势通道 + 加仓价位 + 支撑/压力位四个面板紧跟在标的现货（headline-grid）之后
    assert page.index('id="quote-price"') < page.index('id="trend-body"')
    assert 'id="buy-levels"' not in page and 'id="sell-levels"' not in page
    assert 'class="panel plan-panel trend-panel"' in page
    assert page.index('id="trend-body"') < page.index('id="add-levels"') < page.index('id="support-levels"') < page.index('id="resistance-levels"')
    # 压力位/支撑位数据紧贴图表上方（Gamma 敞口与压力位/支撑位柱状图都在它下面）
    assert page.index('id="support-levels"') < page.index('id="gex-chart"')
    assert page.index('id="support-levels"') < page.index('id="levels-chart"')
    assert page.index('id="levels-chart"') < page.index('id="chain-body"')
    assert "function renderTrend(trend, extremes, spot, historyMeta, recommendation = null, tradePoints = null, tradePointsHorizon = null, trendMarket = null, beta = null)" in source
    assert "function renderPlanRows(levels, spot)" in source
    assert "function renderPlan(plan, spot)" in source
    assert "const PLAN_COUNT = 10;" in source
    assert "const stableTradePoints = stabilizeTradePoints(payload?.trade_points, tradePointContext);" in source
    assert "renderTrend(payload?.trend || null, payload?.extremes || null, spot, payload?.history || null, payload?.recommendation || null, stableTradePoints," in source
    assert '"今开"' in source and '"昨收"' in source and '["Beta", betaText, betaTitle]' in source
    assert '"Beta（2年）"' not in source
    assert page.index('class="trend-side"') < page.index('class="trend-core"')
    assert "基准指数：标普500" in source and "前一个交易日的开盘价" in source
    assert 'trend-beta-sub' not in source
    assert '"近期最佳买入点"' in source and '"近期最佳卖出点"' in source
    assert "未来 5 个交易日" in source
    assert "未来 5 个交易日（约 1 周）" not in source
    assert 'class="trend-opportunities"' in page
    assert 'class="trend-layout"' in page and 'class="trend-core"' in page and 'class="trend-side"' in page
    assert 'v-for="row in view.trend.rows.slice(0, 2)"' in page
    assert 'v-for="row in view.trend.rows.slice(2)"' in page
    assert page.index('class="trend-opportunities"') > page.index('class="trend-core"')
    assert 'class="trend-meta trend-current-price" :title="view.trend.priceTitle"' in page
    assert "formatLevelRange(point)" in source and "formatProbability(point.confidence)" in source
    assert 'const priceLabel = state.levelBasisMode === "live" && selectedBasis?.label === "收盘"' in source
    assert 'price: validPrice ? formatMoney(displayedPrice) : "--"' in source
    assert ".trend-opportunity.buy strong{color:var(--up)}" in styles
    assert ".trend-opportunity.sell strong{color:var(--down)}" in styles
    assert ".trend-opportunity-label small{color:var(--muted)" in styles
    assert ".trend-opportunity strong small{color:var(--muted)" in styles
    assert ".trend-opportunities .trend-meta{display:grid;grid-template-columns:minmax(0,1fr) auto;align-items:center;gap:10px;min-width:0;border-bottom:0}" in styles
    assert ".trend-opportunity strong{display:flex;flex-direction:column;align-items:flex-end" in styles
    assert ".trend-core .trend-opportunities{display:flex;flex-direction:column;min-width:0}" in styles
    assert ".trend-core .trend-opportunities .trend-meta{flex:0 0 auto;min-height:0;padding-block:9px;line-height:1.35}" in styles
    assert ".trend-core .trend-opportunity strong{gap:3px;line-height:1.25}" in styles
    assert ".trend-core .trend-opportunity-label{gap:3px;line-height:1.35}" in styles
    assert ".trend-layout{display:grid;grid-template-columns:repeat(2,minmax(0,1fr));gap:0 24px;align-items:stretch;flex:1;min-height:0}" in styles
    assert ".trend-side{display:flex;flex-direction:column;min-height:100%}" in styles
    assert ".trend-side>.trend-meta{flex:1;min-height:40px}" in styles
    assert ".trend-core .trend-meta{flex:1;min-height:40px}" in styles
    assert ".trend-panel{display:flex;flex-direction:column;min-height:0}" in styles
    assert 'class="trend-signal"' in page
    assert '<strong class="trend-label">{{ view.trend.label }}<span v-if="view.trend.action" class="trend-action"> · {{ view.trend.action }}</span></strong>' in page
    assert ".trend-signal.up .trend-label{color:var(--up)}" in styles
    assert ".trend-signal.down .trend-label{color:var(--down)}" in styles
    assert ".trend-signal .trend-action{font-size:inherit}" in styles
    assert "renderPlan(payload?.plan, spot);" in source
    # 回退口径（合成接口不可用）也要给出趋势占位与三段计划
    assert "renderTrend(null);" in source
    assert "const planSplit = Math.min(PLAN_COUNT, Math.ceil(planSupportSeries.length / 2));" in source
    assert "renderPlan({ add: planSupportSeries.slice(planSplit, planSplit + PLAN_COUNT) }, price);" in source
    assert ".analysis-levels-grid{display:grid;grid-template-columns:repeat(2,minmax(0,1fr));gap:12px;margin-bottom:16px}" in styles


def test_trading_plan_rows_expose_composite_basis_column():
    """加仓价位表显示「综合依据」列，不再只藏在悬停提示里。"""
    source = Path("app/static/common/js/app.js").read_text(encoding="utf-8")
    page = Path("app/static/index.html").read_text(encoding="utf-8")
    styles = Path("app/static/common/css/styles.css").read_text(encoding="utf-8")
    plan_section = source[source.index("function renderPlanRows("):source.index("function renderPlan(")]
    assert "buildFactorViews(levels, spot, \"support\", true)" in plan_section
    assert '<span>综合依据</span>' in page
    assert 'class="level-factors"><span class="level-factor-text">{{ level.factors }}</span>' in page
    assert 'class="level-factors"><span v-if="level.strengthTag" class="level-strength-badge' not in page
    assert "level-plan-row" in page
    # 加仓表使用统一渲染函数，买入/卖出改由支撑位/压力位面板展示
    assert 'renderPlanRows(plan?.add || [], price);' in source
    # 表头与数据行统一四列，避免距现价、触及概率、综合依据错位
    assert ".level-factor-row,.level-plan-row{grid-template-columns:minmax(0,1.25fr) minmax(0,.85fr) minmax(0,.85fr) minmax(0,1.35fr);column-gap:0}" in styles
    assert ".level-factor-row>span,.level-plan-row>span{min-width:0;padding-inline:8px;text-align:center!important}" in styles
    assert ".level-factor-row>span+span,.level-plan-row>span+span{border-left:1px solid var(--row-line)}" in styles
    assert ".level-factor-row .level-factors,.level-plan-row .level-factors{padding-inline:0;gap:4px;justify-content:center;text-align:center}" in styles
    assert ".level-strong .level-factors{font-size:12px;gap:2px}" in styles
    assert ".level-strong .level-strength-badge{padding-inline:3px;font-size:10px}" in styles
    assert ".level-strong .level-factors{display:flex;flex-direction:column;align-items:center;justify-content:center;gap:2px;flex-wrap:nowrap;text-align:center}" in styles
    assert ".level-strong .level-strength-badge{position:static;transform:none}" in styles
    assert ".level-strong .level-factor-text{width:auto;min-width:0;max-width:100%;text-align:center!important}" in styles
    assert "@media(max-width:500px){.level-factor-row,.level-plan-row{grid-template-columns:minmax(0,2fr) minmax(0,1fr) minmax(0,1fr) minmax(0,1.7fr);column-gap:8px}.level-factor-row>span,.level-plan-row>span{padding-inline:0}.level-factor-row>span+span,.level-plan-row>span+span{border-left:0}.level-strong .level-factors{display:flex;justify-content:center}.level-strong .level-strength-badge{position:static;transform:none}.level-strong .level-factor-text{width:auto}}" in styles
    assert ".level-plan-row>span:last-child{grid-column:auto}" in styles
    assert ".level-plan-row.level-head>span:last-child{grid-column:auto}" in styles
    assert ".plan-panel.levels-panel-support .level-strike{color:var(--text)}" in styles
    assert "const STRONG_LEVEL_SCORE = 0.7;" in source
    assert "function levelStrengthTag(level, side, isAdd = false)" in source
    assert "function hasStrongLevelEvidence(factors)" in source
    assert "|| !hasStrongLevelEvidence(factors)" in source
    assert ".level-strong-support,.level-reinforced-support{--level-color:var(--up)}" in styles
    assert ".level-strong-resistance,.level-reinforced-resistance{--level-color:var(--down)}" in styles
    assert "历史验证强位或多因子重点承接" in page
    assert "strength_tier" in source
    assert ".level-reinforced-support" in styles
    # 回退口径（合成接口不可用时）也要给出依据文案
    assert "factors: [metricLabel]" in source


def test_levels_module_shows_ten_per_side():
    """压力位/支撑位每侧 10 条；期权候选上限 12，保证纯期权口径也能凑齐 10 条。"""
    from app import levels

    assert levels.LEVEL_COUNT == 10
    assert levels.OPTION_LIMIT == 12
    assert levels.PLAN_COUNT == 10
    assert levels.TRADE_POINT_TRADING_DAYS == 5


def test_levels_endpoint_accepts_spot_override(tmp_path: Path):
    """接口：传入 spot 时以它为基准价；缺省时退回快照里的常规价。"""
    database = Database(tmp_path / "options.db")
    database.write_snapshot(sample_quote(), sample_rows(), iso())
    settings = Settings(database_path=tmp_path / "options.db", proxy_url=None, default_symbols=("AAPL",), refresh_interval_seconds=60, raw_retention_days=30, cleanup_interval_seconds=86400, scheduler_enabled=False)
    service = SnapshotService(database, FakeProvider())
    router = create_router(database, service, FakeProvider(), settings)
    test_app = FastAPI()
    test_app.include_router(router)
    with TestClient(test_app) as client:
        default_payload = client.get("/api/levels/AAPL", params={"expiration": "2026-12-18"}).json()
        assert default_payload["spot"] == pytest.approx(200.5)
        overridden = client.get("/api/levels/AAPL", params={"expiration": "2026-12-18", "spot": 205.0}).json()
        assert overridden["spot"] == pytest.approx(205.0)
        assert all(item["price"] > 205.0 for item in overridden["resistance"])
        assert all(item["price"] < 205.0 for item in overridden["support"])
        assert all(item["zone_low"] <= item["price"] <= item["zone_high"] for side in ("resistance", "support") for item in overridden[side])


def test_levels_endpoint_degrades_without_history(tmp_path: Path):
    """历史行情抓取失败时接口仍返回期权口径的价位，并标注降级原因。"""
    database = Database(tmp_path / "options.db")
    database.write_snapshot(sample_quote(), sample_rows(), iso())

    class OfflineHistoryProvider(FakeProvider):
        def history(self, symbol: str, period: str = "6mo") -> list[dict]:
            raise ProviderError("离线")

    settings = Settings(database_path=tmp_path / "options.db", proxy_url=None, default_symbols=("AAPL",), refresh_interval_seconds=60, raw_retention_days=30, cleanup_interval_seconds=86400, scheduler_enabled=False)
    service = SnapshotService(database, OfflineHistoryProvider())
    router = create_router(database, service, OfflineHistoryProvider(), settings)
    test_app = FastAPI()
    test_app.include_router(router)
    with TestClient(test_app) as client:
        payload = client.get("/api/levels/AAPL", params={"expiration": "2026-12-18"}).json()
        assert payload["history"]["bars"] == 0
        assert payload["history"]["warning"]
        assert payload["spot"] == pytest.approx(200.5)
        assert payload["support"]



def test_price_extremes_splits_52_week_and_all_time():
    """日线极值：52 周窗口按最后交易日回推 365 天，历史极值覆盖全量序列。"""
    bars = [
        {"date": "2024-01-02", "high": 400.0, "low": 380.0},
        {"date": "2026-08-01", "high": 210.0, "low": 200.0},
        {"date": "2026-09-16", "high": 220.5, "low": 205.25},
    ]
    payload = price_extremes(bars)
    assert payload["window_days"] == 365
    assert payload["reference_date"] == "2026-09-16"
    assert payload["window_bars"] == 2 and payload["bars"] == 3
    # 52 周窗口里只剩近两根日线，历史极值仍取到 2024 年的高点
    assert payload["week52"]["high"] == {"price": 220.5, "date": "2026-09-16"}
    assert payload["week52"]["low"] == {"price": 200.0, "date": "2026-08-01"}
    assert payload["all_time"]["high"] == {"price": 400.0, "date": "2024-01-02"}
    assert payload["all_time"]["low"] == {"price": 200.0, "date": "2026-08-01"}
    # 缺日期或高低价的行直接跳过；全空时返回 None
    assert price_extremes([{"close": 10}]) is None
    assert price_extremes([]) is None


def test_trend_channel_renders_extremes_rows():
    """趋势通道面板：新增 52 周与历史最高/最低四行，数据来自 /api/levels 的 extremes。"""
    source = Path("app/static/common/js/app.js").read_text(encoding="utf-8")
    assert "function trendExtremeRows(extremes, spot)" in source
    for label in ("52周最高", "52周最低", "历史最高", "历史最低"):
        assert f'["{label}", extremes?.week52?.high]' in source or f'["{label}",' in source
    assert "const stableTradePoints = stabilizeTradePoints(payload?.trade_points, tradePointContext);" in source
    assert "renderTrend(payload?.trend || null, payload?.extremes || null, spot, payload?.history || null, payload?.recommendation || null, stableTradePoints," in source
    # 取不到数据时整组不渲染，趋势行不受影响
    assert "const hasExtremes = extremeRows.some((row) => row.valid);" in source


def test_history_service_caches_history(tmp_path: Path):
    """日线历史：新鲜期内复用 SQLite，只有过期才回源上游接口。"""
    database = Database(tmp_path / "options.db")
    calls = {"count": 0}

    class CountingProvider(FakeProvider):
        def history(self, symbol: str, period: str = "6mo") -> list[dict]:
            calls["count"] += 1
            return sample_bars()

    service = HistoryService(database, CountingProvider(), max_age_seconds=3600)
    first = service.bars("aapl")
    second = service.bars("AAPL")
    assert first["source"] == "upstream"
    assert second["source"] == "sqlite"
    assert calls["count"] == 1
    assert len(second["bars"]) == len(sample_bars())


def test_history_service_caches_extremes_separately(tmp_path: Path):
    """日线极值：全量日线单独一套缓存，默认一天内不重复回源，且与日线历史互不影响。"""
    database = Database(tmp_path / "options.db")
    periods: list[str] = []

    class PeriodProvider(FakeProvider):
        def history(self, symbol: str, period: str = "6mo") -> list[dict]:
            periods.append(period)
            return sample_bars()

    service = HistoryService(database, PeriodProvider(), max_age_seconds=3600, extremes_max_age_seconds=86400)
    first = service.extremes("aapl")
    second = service.extremes("AAPL")
    assert first["source"] == "upstream" and first["extremes"]["week52"]["high"]
    assert second["source"] == "sqlite"
    assert periods == ["max"]
    # 日线历史走自己的周期，不会顺手把极值缓存顶掉
    service.bars("AAPL")
    assert service.extremes("AAPL")["source"] == "sqlite"
    assert periods == ["max", "2y"]


def test_extremes_degrade_when_provider_fails(tmp_path: Path):
    """日线极值抓取失败时不影响压力位/支撑位，只在元信息里标注降级。"""
    database = Database(tmp_path / "options.db")
    database.write_snapshot(sample_quote(), sample_rows(), iso())

    class FailingProvider(FakeProvider):
        def history(self, symbol: str, period: str = "6mo") -> list[dict]:
            if period == "max":
                raise ProviderError("全量历史不可用")
            return sample_bars()

    settings = Settings(database_path=tmp_path / "options.db", proxy_url=None, default_symbols=("AAPL",), refresh_interval_seconds=60, raw_retention_days=30, cleanup_interval_seconds=86400, scheduler_enabled=False)
    service = SnapshotService(database, FailingProvider())
    router = create_router(database, service, FailingProvider(), settings)
    test_app = FastAPI()
    test_app.include_router(router)
    with TestClient(test_app) as client:
        payload = client.get("/api/levels/AAPL", params={"expiration": "2026-12-18"}).json()
    assert payload["extremes"] is None
    assert payload["history"]["extremes_source"] == "none"
    assert "全量历史不可用" in payload["history"]["extremes_warning"]
    # 压力位/支撑位与趋势通道仍照常返回
    assert payload["support"] and payload["trend"]["direction"] in {"up", "down", "range"}
def test_analysis_detail_group_collapses_by_default():
    """分析详情折叠面板：趋势通道 + 交易计划 + 压力位/支撑位包在一个默认折叠的分组里，点标题展开。"""
    page = Path("app/static/index.html").read_text(encoding="utf-8")
    source = Path("app/static/common/js/app.js").read_text(encoding="utf-8")
    styles = Path("app/static/common/css/styles.css").read_text(encoding="utf-8")
    # 分组从 detail-body 开始，把四张明细表面板（趋势通道 / 加仓价位 / 压力位支撑位）全包进去，
    # 图表区（gex-chart）仍在分组之外。
    assert page.index('id="analysis-detail"') < page.index('id="detail-body"')
    body = page[page.index('id="detail-body"') : page.index('id="gex-chart"')]
    for token in ('class="analysis-levels-grid"', 'class="plan-grid"', 'class="levels-grid"', 'id="trend-body"', 'id="add-levels"', 'id="support-levels"', 'id="resistance-levels"'):
        assert token in body
    assert 'id="buy-levels"' not in body and 'id="sell-levels"' not in body
    order = [body.index(token) for token in ('id="trend-body"', 'id="add-levels"', 'id="support-levels"', 'id="resistance-levels"')]
    assert order == sorted(order)
    # 默认折叠：body 带 hidden，按钮 aria-expanded=false
    assert 'id="detail-body" hidden' in page
    assert 'id="detail-toggle" type="button" aria-expanded="false" aria-controls="detail-body"' in page
    assert 'id="detail-hint"' in page and 'id="detail-action"' in page
    # 展开状态记在 sessionStorage，换标的后仍保持；折叠逻辑走通用实现 bindFoldGroup
    assert 'const DETAIL_KEY = "option-scope-detail";' in source
    assert "function initDetailGroup()" in source
    assert "initDetailGroup();" in source
    assert "sessionStorage.getItem(storageKey)" in source and "sessionStorage.setItem(storageKey" in source
    assert 'const DETAIL_KEY = "option-scope-detail";' in source
    assert "function bindFoldGroup(" in source
    assert 'bodyId: "detail-body",' in source
    # 折叠态样式：hidden 生效 + caret 旋转
    assert ".detail-body[hidden]{display:none}" in styles
    assert '.detail-toggle[aria-expanded="true"] .detail-caret{transform:rotate(90deg)}' in styles
    # 标题按钮必须显式清掉 UA 默认的 2px outset 白边框（全局按钮重置只作用于 .toolbar 内的按钮）
    assert "border:0" in styles.split(".detail-toggle{")[1].split("}")[0]
    # 内容体与标题分隔线之间留出上间距：面板不再贴着分隔线（padding 首值即上间距）
    body_rule = styles.split(".detail-body{")[1].split("}")[0]
    assert int(body_rule.split("padding:")[1].split("px")[0]) >= 12
def test_basis_price_switch_defaults_to_live():
    """基准价开关：实时价（默认）/ 盘后价两档，放在折叠组标题栏里，仅展开时显示。"""
    page = Path("app/static/index.html").read_text(encoding="utf-8")
    source = Path("app/static/common/js/app.js").read_text(encoding="utf-8")
    styles = Path("app/static/common/css/styles.css").read_text(encoding="utf-8")
    # 开关挂在标题栏（detail-header）里、位于内容体之前，初始隐藏（折叠态），默认按下「实时价」。
    assert page.index('id="detail-header"') < page.index('id="basis-live"') < page.index('id="detail-body"')
    assert 'id="detail-modes" hidden' in page
    assert 'id="basis-live" type="button" data-basis="live" aria-pressed="true"' in page
    assert 'id="basis-close" type="button" data-basis="close" aria-pressed="false"' in page
    assert 'role="group" aria-label="压力位/支撑位的计算基准"' in page
    # 实时价 = 当前时段生效价；盘后价 = 旧口径（盘后 → 盘前 → 常规）。
    assert 'const BASIS_MODES = { live: "实时价", close: "盘后价" };' in source
    assert "function activeBasis(quote)" in source
    assert 'if (state.levelBasisMode === "close") return levelBasis(quote, quote?.price);' in source
    assert 'if (quote?.market_state === "OVERNIGHT")' in source
    assert 'return { price: close, label: "收盘" };' in source
    assert "const price = Number(activeSessionQuote(quote)?.price);" in source
    assert 'state.levelBasisMode === "live" && selectedBasis?.label === "收盘"' in source
    # 默认实时价：state 初始值 + 分析渲染改用 activeBasis。
    assert "levelBasisMode: \"live\"" in source
    assert "}, activeBasis(quote));" in source
    # 切换时用最近一次快照重算压力位/支撑位（不重新请求上游接口），并同步按钮的按下状态。
    assert "function applyBasisMode(mode)" in source
    assert 'state.levelBasisMode = mode === "close" ? "close" : "live";' in source
    assert "loadFactorLevels(last.points || [], basis.price);" in source
    assert "state.lastAnalysis = { rows, spot, analysisPayload, expirationRows, ivModel, basis, points };" in source
    assert "state.lastQuote = quote || null;" in source
    assert '${state.levelBasisMode}' in source
    # 开关只在展开时出现；点击开关不会连带折叠，点标题栏其它区域仍然折叠/展开。
    assert 'const modes = byId("detail-modes");' in source
    assert "if (modes) modes.hidden = !expanded;" in source
    assert 'const basisButton = closestElement(event.target, "[data-basis]");' in source
    # 开关容器里的空白点击不改折叠：ignore 回调里用 closest 命中 #detail-modes 就吃掉这次点击
    assert 'return Boolean(closestElement(event.target, "#detail-modes"));' in source
    # 样式：隐藏态生效 + 选中态用 --blue 实心。
    assert ".detail-modes[hidden]{display:none}" in styles
    assert ".detail-segmented{display:inline-flex;" in styles
    assert '.detail-seg[aria-pressed="true"]{background:var(--blue);color:var(--on-blue)}' in styles


def test_expiration_switch_discards_stale_response():
    """到期日切换：请求在飞时禁用下拉框，旧期限的响应回来时整份作废，不覆盖后来切换的选择。"""
    source = Path("app/static/common/js/app.js").read_text(encoding="utf-8")
    # 切换入口：拉链式 token + 立即禁用下拉框，并用加载中的期限去请求。
    assert "let expirationSwitchToken = 0;" in source
    assert "async function switchExpiration(expiration)" in source
    assert "const token = ++expirationSwitchToken;" in source
    assert "if (token === expirationSwitchToken) setBusy(false);" in source
    assert 'state.expiration = expiration;' in source
    # 双向防覆盖之一：renderSnapshot 用「发起请求时的到期日」做落地校验，不再是只看 loadId。
    assert "const expiration = state.expiration;" in source
    assert "if (!isCurrentLoad(loadId, expiration)) return { shown: false, source: null };" in source
    # 双向防覆盖之二：后台刷新发现期限变了就先还回网络互斥再按新期限重来，避免把用户的选择拽回旧期限。
    assert "if (state.expiration !== currentExpiration) {" in source
    assert "state.refreshInFlight = null;\n      await refreshInBackground(loadId);" in source
    assert source.index("if (state.expiration !== currentExpiration) {") < source.index("applyExpirations(expirations.expirations, currentExpiration);")
    # 下拉框的禁用兜底：网络卡死时不至于永久禁用。
    assert "const EXPIRATION_SWITCH_TIMEOUT_MS = 20000;" in source
    assert "clearTimeout(releaseTimer);" in source
    # 旧的「静默重载」绑定已删除，切换只走 switchExpiration 一条路径。
    assert 'expirationChanged() { return switchExpiration(this.expiration); }' in source
    assert 'loadChain({ silent: true })' not in source


def test_chain_header_matches_other_fold_groups():
    """期权链标题行：与「分析详情」「图表」两个折叠组同构 —— 左侧折叠按钮、右侧状态与展开/收起文案。"""
    page = Path("app/static/index.html").read_text(encoding="utf-8")
    styles = Path("app/static/common/css/styles.css").read_text(encoding="utf-8")
    # 标题行复用同一个 .detail-header，三个折叠组外观与交互一致
    assert 'class="detail-header" id="chain-header"' in page
    header = page[page.index('id="chain-header"') : page.index('id="error-box"')]
    # 子元素顺序：折叠按钮 → 标题 → 状态栏 → 展开/收起文案（文案靠右）
    assert header.index('id="chain-toggle"') < header.index('class="detail-text"') < header.index('class="panel-status"') < header.index('id="chain-action"')
    # 状态栏与文案都靠右：容器用 margin-left:auto 挤到右侧，文案自身 flex:none 不被压缩
    assert ".detail-header .panel-status{margin-left:auto}" in styles
    assert ".detail-action{flex:none;margin-left:auto" in styles
    # 状态行内部保持中线对齐（间距/分隔线也跟着走同一条中线）
    assert ".top-meta,.panel-status,footer{display:flex;align-items:center;gap:10px" in styles
    # 旧的标题行工具条规则已删除，筛选改由折叠区内的 .chain-toolbar 承载
    assert ".panel-tools" not in styles and ".panel-tools" not in page
    assert ".chain-toolbar{display:flex;justify-content:flex-start;padding:12px 18px 10px}" in styles


def test_default_symbol_defaults_to_qqq(monkeypatch):
    """未配置 DEFAULT_SYMBOLS 时默认标的为 QQQ，并保持去重、大写与顺序。"""
    monkeypatch.delenv("DEFAULT_SYMBOLS", raising=False)
    assert Settings.from_env().default_symbols == ("QQQ",)
    monkeypatch.setenv("DEFAULT_SYMBOLS", "spy, qqq ,SPY")
    assert Settings.from_env().default_symbols == ("SPY", "QQQ")


def test_page_default_symbol_comes_from_server_config():
    """页面默认标的由服务端按 DEFAULT_SYMBOLS 注入，避免前后端默认值不一致。"""
    page = Path("app/static/index.html").read_text(encoding="utf-8")
    source = Path("app/static/common/js/app.js").read_text(encoding="utf-8")
    main = Path("app/main.py").read_text(encoding="utf-8")
    # 输入框与现货卡片都留占位符，等待服务端替换（占位符不留在前端脚本里）。
    assert page.count("__DEFAULT_SYMBOL__") == 2
    assert 'value="__DEFAULT_SYMBOL__"' in page
    assert '"__DEFAULT_SYMBOL__"' not in source
    assert 'page.replace("__DEFAULT_SYMBOL__", settings.default_symbols[0])' in main
    # 注入缺失时才走的前端兜底值同样是 QQQ。
    assert ' : "QQQ"' in source
    assert 'symbol: defaultSymbol' in source


def test_database_max_mb_parsing(monkeypatch):
    """DATABASE_MAX_MB 默认按 MB 解释，也接受 M/MB/G/GB 后缀，0 表示不限制。"""
    monkeypatch.setenv("DATABASE_MAX_MB", "300")
    assert Settings.from_env().database_max_mb == 300
    monkeypatch.setenv("DATABASE_MAX_MB", "512M")
    assert Settings.from_env().database_max_mb == 512
    monkeypatch.setenv("DATABASE_MAX_MB", "512mb")
    assert Settings.from_env().database_max_mb == 512
    monkeypatch.setenv("DATABASE_MAX_MB", "1G")
    assert Settings.from_env().database_max_mb == 1024
    monkeypatch.setenv("DATABASE_MAX_MB", " 0 ")
    assert Settings.from_env().database_max_mb == 0
    # 无法识别的写法（含负数、多余单位）在启动时就报错，并给出可照抄的写法提示。
    for invalid in ("-1", "512X", "abc"):
        monkeypatch.setenv("DATABASE_MAX_MB", invalid)
        with pytest.raises(ValueError, match="也可以写成 300M、1G"):
            Settings.from_env()


def test_snapshot_write_skips_raw_json(tmp_path: Path):
    """冗余报文列只保留给旧库兼容，新写入不再落库（小磁盘环境的关键优化）。"""
    database = Database(tmp_path / "options.db")
    database.write_snapshot(sample_quote(), sample_rows(), iso())
    with database.connect() as connection:
        assert connection.execute("SELECT COUNT(*) FROM option_snapshots WHERE raw_json IS NOT NULL").fetchone()[0] == 0
        assert connection.execute("SELECT COUNT(*) FROM quote_snapshots WHERE raw_json IS NOT NULL").fetchone()[0] == 0


def test_size_cleanup_clears_legacy_raw_json(tmp_path: Path):
    """体积清理的第一步：清空没有读取方的冗余报文列，业务字段与批次全部保留。"""
    database = Database(tmp_path / "options.db")
    stale = iso(utc_now() - timedelta(hours=30))
    newest = iso(utc_now())
    database.write_snapshot(sample_quote(), sample_rows(), stale)
    database.write_snapshot(sample_quote(), sample_rows(), newest)
    with database.connect() as connection:
        # 模拟旧版本进程写入的 raw_json。
        connection.execute("UPDATE option_snapshots SET raw_json='{\"legacy\": true}'")
        connection.execute("UPDATE quote_snapshots SET raw_json='{\"legacy\": true}'")
    assert database._clear_raw_json(5000, float("inf")) == 6  # 2 批 × (2 条期权 + 1 条报价)
    with database.connect() as connection:
        assert connection.execute("SELECT COUNT(*) FROM option_snapshots WHERE raw_json IS NOT NULL").fetchone()[0] == 0
        assert connection.execute("SELECT COUNT(*) FROM quote_snapshots WHERE raw_json IS NOT NULL").fetchone()[0] == 0
        # 行数与业务字段不受影响：清理只丢掉冗余报文。
        assert connection.execute("SELECT COUNT(*) FROM option_snapshots").fetchone()[0] == 4
        assert connection.execute("SELECT COUNT(*) FROM option_snapshots WHERE fetched_at=?", (stale,)).fetchone()[0] == 2


def test_size_cleanup_protects_recent_batches(tmp_path: Path):
    """瘦身旧批次时保留保护期内的时间桶样本，供未平仓量回退使用。"""
    database = Database(tmp_path / "options.db")
    stale = iso(utc_now() - timedelta(hours=30))
    recent = iso(utc_now() - timedelta(hours=2))
    newest = iso(utc_now())
    for fetched_at in (stale, recent, newest):
        database.write_snapshot(sample_quote(), sample_rows(), fetched_at)
    floor = iso(utc_now() - timedelta(hours=24))
    keys = ("symbol", "expiration")
    # 不设时间桶时每组只留最新一批，保护期之外的旧批次被删除。
    assert database._thin_superseded("option_snapshots", keys, floor, None, 5000, float("inf")) == 2
    # 换成按小时保留后：保护期内的 recent 作为该小时代表被保留，最新批次不受影响。
    assert database._thin_superseded("option_snapshots", keys, floor, 13, 5000, float("inf")) == 0
    # 保护期完全不设限时退化为每组只留最新一批，此时 hour 桶不再兜底。
    assert database._thin_superseded("option_snapshots", keys, NO_FLOOR, 13, 5000, float("inf")) == 2
    with database.connect() as connection:
        remaining = [row[0] for row in connection.execute("SELECT DISTINCT fetched_at FROM option_snapshots ORDER BY fetched_at")]
    assert remaining == [newest]


def test_cleanup_by_size_shrinks_database_and_keeps_latest(tmp_path: Path):
    """超限时按阶梯清理旧快照并回收文件体积，最新批次与查询路径保持可用。"""
    database = Database(tmp_path / "options.db")
    database.write_snapshot(sample_quote(), sample_rows(), iso(utc_now() - timedelta(hours=30)))
    database.write_snapshot(sample_quote(), sample_rows(), iso())
    result = database.cleanup_by_size(max_bytes=1)
    assert result["before_bytes"] > result["limit_bytes"]
    assert result["superseded_options"] == 2
    assert result["superseded_quotes"] == 1
    assert result["vacuumed"] is True
    assert result["after_bytes"] < result["before_bytes"]
    # 最新批次完整保留，页面读取路径不受影响。
    assert database.latest_quote("AAPL")["price"] == 200.5
    assert len(database.latest_chain("AAPL", "2026-12-18")["data"]) == 2


def test_cleanup_by_size_disabled_when_unlimited(tmp_path: Path):
    """DATABASE_MAX_MB=0 时不触发任何清理。"""
    database = Database(tmp_path / "options.db")
    database.write_snapshot(sample_quote(), sample_rows(), iso(utc_now() - timedelta(days=90)))
    result = database.cleanup_by_size(max_bytes=0)
    assert result["vacuumed"] is False
    assert result["after_bytes"] == result["before_bytes"]
    assert result["superseded_options"] == 0
    assert database.latest_quote("AAPL") is not None


def test_cleanup_endpoint_reports_database_size(tmp_path: Path):
    """POST /api/cleanup 同时返回删除行数与数据库体积统计。"""
    database = Database(tmp_path / "options.db")
    database.write_snapshot(sample_quote(), sample_rows(), iso(utc_now() - timedelta(days=40)))
    settings = Settings(database_path=tmp_path / "options.db", proxy_url=None, default_symbols=("AAPL",), refresh_interval_seconds=60, raw_retention_days=30, cleanup_interval_seconds=86400, scheduler_enabled=False)
    test_app = FastAPI()
    test_app.include_router(create_router(database, SnapshotService(database, FakeProvider()), FakeProvider(), settings))
    with TestClient(test_app) as client:
        payload = client.post("/api/cleanup").json()
    assert payload["deleted"] == {"quotes": 1, "options": 2, "runs": 0, "history": 0, "extremes": 0}
    assert payload["size"]["limit_bytes"] == 0
    assert payload["size"]["after_bytes"] == payload["size"]["before_bytes"]
    assert database.latest_quote("AAPL") is None


def test_legacy_raw_json_pruned_on_start(tmp_path: Path):
    """旧库里的冗余报文在调度器启动时被清空，业务行一条不丢。"""
    database = Database(tmp_path / "options.db")
    database.write_snapshot(sample_quote(), sample_rows(), iso())
    with database.connect() as connection:
        connection.execute("UPDATE option_snapshots SET raw_json='{\"legacy\": true}'")
        connection.execute("UPDATE quote_snapshots SET raw_json='{\"legacy\": true}'")
    settings = Settings(database_path=tmp_path / "options.db", proxy_url=None, default_symbols=("QQQ",), refresh_interval_seconds=60, raw_retention_days=30, cleanup_interval_seconds=86400, scheduler_enabled=False)
    scheduler = Scheduler(settings, SnapshotService(database, FakeProvider()), database)
    asyncio.run(scheduler._prune_legacy_raw_json())
    with database.connect() as connection:
        assert connection.execute("SELECT COUNT(*) FROM option_snapshots WHERE raw_json IS NOT NULL").fetchone()[0] == 0
        assert connection.execute("SELECT COUNT(*) FROM quote_snapshots WHERE raw_json IS NOT NULL").fetchone()[0] == 0
        assert connection.execute("SELECT COUNT(*) FROM option_snapshots").fetchone()[0] == 2
    # 再跑一次无事可做，返回 0 且不重复回收。
    assert database.prune_legacy_raw_json() == 0
    assert database.latest_quote("AAPL")["price"] == 200.5


def test_scheduler_survives_cleanup_failure(tmp_path: Path, monkeypatch):
    """清理步骤抛错时调度循环必须继续活着。

    历史现象：清理抛错会让调度协程直接结束，进程还活着、/health 依然正常，
    但页面数据永远停在旧快照上，表现为「数据不再刷新」。
    """
    database = Database(tmp_path / "options.db")
    snapshots = SnapshotService(database, FakeProvider())
    calls: list[tuple[str, ...]] = []

    def boom(*_args, **_kwargs):
        raise RuntimeError("模拟磁盘写满")

    def fake_refresh_default(symbols):
        calls.append(tuple(symbols))
        return []

    monkeypatch.setattr(snapshots, "refresh_default", fake_refresh_default)
    monkeypatch.setattr(database, "prune_legacy_raw_json", boom)
    monkeypatch.setattr(database, "cleanup", boom)
    monkeypatch.setattr(database, "cleanup_by_size", boom)
    # 上限设成 1MB 让体积清理真的被执行到，从而命中失败分支
    settings = Settings(
        database_path=tmp_path / "options.db",
        proxy_url=None,
        default_symbols=("QQQ",),
        refresh_interval_seconds=3600,
        raw_retention_days=30,
        cleanup_interval_seconds=3600,
        scheduler_enabled=True,
        database_max_mb=1,
    )

    async def scenario() -> bool:
        scheduler = Scheduler(settings, snapshots, database)
        await scheduler.start()
        await asyncio.sleep(0.05)
        alive = scheduler._task is not None and not scheduler._task.done()
        await scheduler.stop()
        return alive

    # 启动清理失败不影响刷新：调度协程仍然存活，且启动时的刷新照常执行
    assert asyncio.run(scenario()) is True
    assert calls == [("QQQ",)]


def test_access_key_guard_blocks_pages_and_api_without_key(tmp_path: Path):
    """配置访问密钥后，页面和 API 都必须携带正确 key，静态资源和健康检查保持可用。"""
    settings = Settings(
        database_path=tmp_path / "options.db",
        proxy_url=None,
        default_symbols=("QQQ",),
        refresh_interval_seconds=60,
        raw_retention_days=30,
        cleanup_interval_seconds=86400,
        scheduler_enabled=False,
        access_key="abc123",
    )
    guarded_app = FastAPI()
    install_access_guard(guarded_app, settings)

    @guarded_app.get("/")
    def page() -> dict[str, str]:
        return {"page": "ok"}

    @guarded_app.get("/api/ping")
    def ping() -> dict[str, str]:
        return {"status": "ok"}

    @guarded_app.get("/health")
    def health() -> dict[str, str]:
        return {"status": "ok"}

    with TestClient(guarded_app) as client:
        forbidden_page = client.get("/")
        assert forbidden_page.status_code == 403
        assert "403 Forbidden" in forbidden_page.text
        assert "?key=" not in forbidden_page.text
        assert "访问" not in forbidden_page.text
        assert client.get("/", headers={"X-Access-Key": "abc123"}).status_code == 200
        assert client.get("/?key=abc123").status_code == 200
        assert client.get("/?key=wrong").status_code == 403
        assert client.get("/", headers={"X-Access-Key": "wrong"}).status_code == 403
        forbidden_api = client.get("/api/ping")
        assert forbidden_api.status_code == 403
        assert forbidden_api.json() == {"detail": "403 Forbidden"}
        assert client.get("/api/ping", headers={"X-Access-Key": "abc123"}).status_code == 200
        assert client.get("/api/ping", headers={"X-Access-Key": "wrong"}).status_code == 403
        assert client.get("/api/ping?key=abc123").status_code == 200
        assert client.get("/api/ping?key=wrong").status_code == 403
        assert client.get("/health").status_code == 200

    page = Path("app/static/index.html").read_text(encoding="utf-8")
    assert '<h1>403 Forbidden</h1>' in page
    assert "access-denied-message" not in page


def test_access_key_guard_disabled_when_not_configured(tmp_path: Path):
    """ACCESS_KEY 留空时维持旧部署行为，不对页面和 API 做拦截。"""
    settings = Settings(
        database_path=tmp_path / "options.db",
        proxy_url=None,
        default_symbols=("QQQ",),
        refresh_interval_seconds=60,
        raw_retention_days=30,
        cleanup_interval_seconds=86400,
        scheduler_enabled=False,
    )
    unguarded_app = FastAPI()
    install_access_guard(unguarded_app, settings)

    @unguarded_app.get("/api/ping")
    def ping() -> dict[str, str]:
        return {"status": "ok"}

    with TestClient(unguarded_app) as client:
        assert client.get("/api/ping").status_code == 200


def test_access_key_from_env(monkeypatch):
    """ACCESS_KEY 会去除首尾空白并写入配置对象。"""
    monkeypatch.setenv("ACCESS_KEY", " abc123 ")
    assert Settings.from_env().access_key == "abc123"


def test_access_key_supports_browsers_with_disabled_storage():
    """浏览器禁用 localStorage/sessionStorage 时仍使用 URL key，不能让 URL 重写或 AJAX 丢凭证。"""
    source = Path("app/static/common/js/app.js").read_text(encoding="utf-8")
    page = Path("app/static/index.html").read_text(encoding="utf-8")
    assert "function currentAccessKey()" in source
    assert "sessionStorage.getItem(ACCESS_KEY_STORAGE)" in source
    assert "const accessKey = currentAccessKey();" in source
    assert "state.storageAvailable = storageWritable();" in source
    assert "function withAccessKey(path, accessKey)" in source
    assert "OptionScopeRequest.request(path, options" in source
    assert "fetch(withAccessKey(path, accessKey)" not in source
    # URL key 存在时必须无条件写回内存，storage 写入失败也不能把它置空。
    assert "state.accessKey = queryKey;" in source
    # 首屏主题脚本也不能因为存储被禁用而抛错。
    assert "catch(error){}document.documentElement.dataset.theme=theme;" in page
