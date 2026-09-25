"""Cboe 公共延迟期权链适配器。

Cboe 的延迟页面接口无需 API key，返回整条期权链和上一交易日的未平仓量。
它不是带 SLA 的正式 API，因此本模块只负责解析和校验；请求失败交给上层缓存处理。
"""

from __future__ import annotations

import json
import logging
import re
from datetime import date, datetime, timezone
from time import monotonic
from threading import Lock
from typing import Any, Callable
from urllib.error import HTTPError, URLError
from urllib.parse import quote
from urllib.request import Request, ProxyHandler, build_opener

from app.providers.market import MARKET_TIMEZONE, ProviderError, MarketDataProvider, current_session_state, safe_value
from app.runtime import low_memory_enabled
from app.services.concurrency import SingleFlight, UpstreamBusyError, UpstreamGate


logger = logging.getLogger(__name__)

CBOE_OPTIONS_URL = "https://cdn.cboe.com/api/global/delayed_quotes/options/{symbol}.json"
CBOE_CACHE_SECONDS = 15
_OCC_OPTION_RE = re.compile(r"^(?P<root>.+?)(?P<date>\d{6})(?P<type>[CP])(?P<strike>\d{8})$")


def download_json(url: str, timeout: float = 15.0, proxy: str | None = None) -> dict[str, Any]:
    """下载并解析 JSON；把网络层异常统一转换为 ProviderError。"""
    request = Request(url, headers={"User-Agent": "option-scope/1.0"})
    # 配置了 MARKET_PROXY 时显式覆盖系统代理，未配置时继续读取系统环境代理。
    proxy_handler = ProxyHandler({"http": proxy, "https": proxy}) if proxy else ProxyHandler()
    opener = build_opener(proxy_handler)
    try:
        with opener.open(request, timeout=timeout) as response:
            payload = json.loads(response.read())
    except HTTPError as exc:
        try:
            detail = exc.read().decode("utf-8", errors="replace")[:500]
        except OSError:
            detail = str(exc)
        raise ProviderError(f"Cboe 请求失败（HTTP {exc.code}）：{detail}") from exc
    except (URLError, TimeoutError, OSError, json.JSONDecodeError) as exc:
        raise ProviderError(f"Cboe 请求失败：{exc}") from exc
    if not isinstance(payload, dict):
        raise ProviderError("Cboe 返回格式无效：顶层不是对象")
    return payload


def parse_occ_option(contract_symbol: str) -> dict[str, Any] | None:
    """解析 OCC 期权代码的到期日、类型和行权价。

    OCC 代码最后 15 位固定为 YYMMDD + C/P + 八位千分位行权价，标的根代码长度可变。
    """
    matched = _OCC_OPTION_RE.fullmatch(str(contract_symbol).strip().upper())
    if not matched:
        return None
    try:
        date_code = matched.group("date")
        expiration = date(2000 + int(date_code[:2]), int(date_code[2:4]), int(date_code[4:6])).isoformat()
        strike = int(matched.group("strike")) / 1000
    except ValueError:
        return None
    return {
        "root": matched.group("root"),
        "expiration": expiration,
        "contract_type": "call" if matched.group("type") == "C" else "put",
        "strike": strike,
    }


def _integer(value: Any) -> int | None:
    try:
        number = float(value)
    except (TypeError, ValueError):
        return None
    return max(0, int(number))


class CboeOptionsProvider:
    """读取 Cboe 延迟期权链，并映射为应用内部字段。"""

    name = "cboe-delayed"

    def __init__(
        self,
        request_json: Callable[[str], dict[str, Any]] | None = None,
        timeout: float = 15.0,
        cache_seconds: float = CBOE_CACHE_SECONDS,
        proxy: str | None = None,
        upstream_gate: UpstreamGate | None = None,
    ):
        self.proxy = proxy.strip() if proxy and proxy.strip() else None
        self._request_json = request_json or (lambda url: download_json(url, timeout, self.proxy))
        self.cache_seconds = cache_seconds
        self._cache: dict[str, tuple[float, dict[str, Any]]] = {}
        self._cache_lock = Lock()
        self._payload_flight: SingleFlight[str, dict[str, Any]] = SingleFlight()
        self.upstream_gate = upstream_gate or UpstreamGate()

    @staticmethod
    def normalize_symbol(symbol: str) -> str:
        return MarketDataProvider.normalize_symbol(symbol)

    def _payload(self, symbol: str) -> dict[str, Any]:
        normalized = self.normalize_symbol(symbol)
        with self._cache_lock:
            cached = self._cache.get(normalized)
        if cached and monotonic() - cached[0] < self.cache_seconds:
            return cached[1]

        def download() -> dict[str, Any]:
            with self._cache_lock:
                cached_inner = self._cache.get(normalized)
            if cached_inner and monotonic() - cached_inner[0] < self.cache_seconds:
                return cached_inner[1]
            url = CBOE_OPTIONS_URL.format(symbol=quote(normalized, safe=".-"))
            with self.upstream_gate.slot():
                payload = self._request_json(url)
            data = payload.get("data")
            if not isinstance(data, dict) or not isinstance(data.get("options"), list):
                raise ProviderError(f"Cboe 返回 {normalized} 的期权链格式无效")
            with self._cache_lock:
                # 整份延迟链很大。小内存机器只留最近一个标的，避免换标的后旧链还占着堆。
                if low_memory_enabled():
                    self._cache.clear()
                self._cache[normalized] = (monotonic(), payload)
            return payload

        try:
            return self._payload_flight.do(normalized, download)
        except UpstreamBusyError as exc:
            raise ProviderError(str(exc)) from exc

    @staticmethod
    def _data(payload: dict[str, Any]) -> dict[str, Any]:
        return payload["data"]

    def expirations(self, symbol: str) -> list[str]:
        normalized = self.normalize_symbol(symbol)
        payload = self._payload(normalized)
        values = {
            parsed["expiration"]
            for raw in self._data(payload)["options"]
            if isinstance(raw, dict)
            for parsed in [parse_occ_option(str(raw.get("option") or ""))]
            if parsed and parsed["root"].replace(".", "") == normalized.replace(".", "")
        }
        if not values:
            raise ProviderError(f"Cboe 没有 {normalized} 的有效期权到期日")
        return sorted(values)

    def quote(self, symbol: str) -> dict[str, Any]:
        """返回 Cboe 延迟的标的价格，作为现货行情主源失败时的兜底。"""
        normalized = self.normalize_symbol(symbol)
        data = self._data(self._payload(normalized))
        price = safe_value(data.get("current_price"))
        previous = safe_value(data.get("prev_day_close"))
        change = safe_value(data.get("price_change_percent"))
        if change is None and price is not None and previous not in (None, 0):
            change = (price - previous) / previous * 100
        now = datetime.now(timezone.utc).astimezone(MARKET_TIMEZONE)
        return {
            "symbol": normalized,
            "price": price,
            "change_percent": change,
            "today_open": None,
            "previous_close": previous,
            "currency": "USD",
            "market_state": current_session_state(now, None),
            "sessions": {},
            "provider": self.name,
            "raw": {"last_price": price, "previous_close": previous},
        }

    def chain(self, symbol: str, expiration: str) -> list[dict[str, Any]]:
        normalized = self.normalize_symbol(symbol)
        payload = self._payload(normalized)
        data = self._data(payload)
        spot = safe_value(data.get("current_price"))
        rows: list[dict[str, Any]] = []
        for raw in data["options"]:
            if not isinstance(raw, dict):
                continue
            contract_symbol = str(raw.get("option") or "").strip().upper()
            parsed = parse_occ_option(contract_symbol)
            if not parsed or parsed["expiration"] != expiration:
                continue
            if parsed["root"].replace(".", "") != normalized.replace(".", ""):
                continue
            strike = parsed["strike"]
            in_the_money = None
            try:
                spot_value = float(spot)
                in_the_money = spot_value >= strike if parsed["contract_type"] == "call" else spot_value <= strike
            except (TypeError, ValueError):
                pass
            rows.append({
                "symbol": normalized,
                "expiration": expiration,
                "contract_type": parsed["contract_type"],
                "contract_symbol": contract_symbol,
                "strike": strike,
                "last_price": safe_value(raw.get("last_trade_price")),
                "bid": safe_value(raw.get("bid")),
                "ask": safe_value(raw.get("ask")),
                "volume": _integer(raw.get("volume")),
                "open_interest": _integer(raw.get("open_interest")),
                "implied_volatility": safe_value(raw.get("iv")),
                "gamma": safe_value(raw.get("gamma")),
                "in_the_money": in_the_money,
                "change_percent": safe_value(raw.get("percent_change")),
                "provider": self.name,
            })
        if not rows:
            raise ProviderError(f"Cboe 没有 {normalized} {expiration} 的期权数据")
        return rows

    def fetch(self, symbol: str, expiration: str) -> tuple[dict[str, Any], list[dict[str, Any]], str]:
        fetched_at = datetime.now(timezone.utc).isoformat()
        quote = self.quote(symbol)
        rows = self.chain(symbol, expiration)
        for row in rows:
            if row.get("gamma") is None:
                row["gamma"] = MarketDataProvider.estimate_gamma(
                    quote.get("price"), row.get("strike"), row.get("implied_volatility"), expiration
                )
        return quote, rows, fetched_at
