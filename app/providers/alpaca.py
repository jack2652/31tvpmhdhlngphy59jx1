"""Alpaca 免费夜盘股票行情适配器。

这里只读取股票快照，不接入 Alpaca 的期权 indicative feed。Basic 账户的夜盘
``overnight`` 数据适合补充标的价格和成交量参考，期权链仍由 Cboe/Yahoo 负责。
"""

from __future__ import annotations

import json
import time
from datetime import datetime, timezone
from typing import Any, Callable
from urllib.error import HTTPError, URLError
from urllib.parse import quote
from urllib.request import ProxyHandler, Request, build_opener

from app.providers.market import MarketDataProvider, ProviderError, safe_value
from app.services.concurrency import UpstreamGate


ALPACA_STOCK_DATA_URL = "https://data.alpaca.markets/v2/stocks/snapshots"
ALPACA_REQUEST_TIMEOUT_SECONDS = 12
ALPACA_AUTH_RETRY_SECONDS = 300

class AlpacaAuthError(ProviderError):
    """Alpaca 凭据无效或已失效。"""


def _download_json(
    url: str,
    api_key: str,
    api_secret: str,
    timeout: float,
    proxy: str | None = None,
) -> dict[str, Any]:
    """请求 Alpaca 股票数据；异常统一转换为不泄露凭据的 ProviderError。"""
    request = Request(
        url,
        headers={
            "APCA-API-KEY-ID": api_key,
            "APCA-API-SECRET-KEY": api_secret,
            "User-Agent": "option-scope/1.0",
        },
    )
    proxy_handler = ProxyHandler({"http": proxy, "https": proxy}) if proxy else ProxyHandler()
    opener = build_opener(proxy_handler)
    try:
        with opener.open(request, timeout=timeout) as response:
            payload = json.loads(response.read())
    except HTTPError as exc:
        # 不把上游响应正文写进异常：错误正文可能包含账号信息，且对回退没有帮助。
        if exc.code in (401, 403):
            raise AlpacaAuthError(f"Alpaca 凭据校验失败（HTTP {exc.code}）") from exc
        raise ProviderError(f"Alpaca 夜盘行情请求失败（HTTP {exc.code}）") from exc
    except (URLError, TimeoutError, OSError, json.JSONDecodeError) as exc:
        raise ProviderError(f"Alpaca 夜盘行情请求失败：{exc}") from exc
    if not isinstance(payload, dict):
        raise ProviderError("Alpaca 夜盘行情返回格式无效")
    return payload


def _number(value: Any, *, positive: bool = False) -> float | None:
    try:
        number = float(value)
    except (TypeError, ValueError):
        return None
    if number != number or number in (float("inf"), float("-inf")):
        return None
    if positive and number <= 0:
        return None
    return number


def _timestamp(value: Any) -> str | None:
    if not value:
        return None
    text = str(value)
    try:
        # API 使用 Z 结尾的 RFC-3339，统一保存为带时区 ISO 字符串。
        moment = datetime.fromisoformat(text.replace("Z", "+00:00"))
    except ValueError:
        return text
    if moment.tzinfo is None:
        moment = moment.replace(tzinfo=timezone.utc)
    return moment.astimezone(timezone.utc).isoformat()


def _midpoint(quote: dict[str, Any]) -> float | None:
    bid = _number(quote.get("bp"), positive=True)
    ask = _number(quote.get("ap"), positive=True)
    if bid is not None and ask is not None and ask >= bid:
        return (bid + ask) / 2
    return bid or ask


class AlpacaOvernightProvider:
    """读取 Alpaca Basic 可用的股票夜盘快照。"""

    name = "alpaca-overnight"

    def __init__(
        self,
        api_key: str | None,
        api_secret: str | None,
        *,
        proxy: str | None = None,
        upstream_gate: UpstreamGate | None = None,
        request_json: Callable[[str], dict[str, Any]] | None = None,
        data_url: str = ALPACA_STOCK_DATA_URL,
    ):
        self.api_key = (api_key or "").strip()
        self.api_secret = (api_secret or "").strip()
        self.proxy = proxy.strip() if proxy and proxy.strip() else None
        self.data_url = data_url.rstrip("/")
        self.upstream_gate = upstream_gate or UpstreamGate()
        self._credential_status = self._initial_credential_status()
        self._auth_retry_at = 0.0
        self._request_json = request_json or (
            lambda url: _download_json(
                url,
                self.api_key,
                self.api_secret,
                ALPACA_REQUEST_TIMEOUT_SECONDS,
                self.proxy,
            )
        )

    @property
    def enabled(self) -> bool:
        return bool(self.api_key and self.api_secret)

    @property
    def credential_status(self) -> str:
        """返回脱敏的凭据状态，供启动日志和运行时回退使用。"""
        return self._credential_status

    @property
    def auth_retry_at(self) -> float:
        """下一次允许自动重试凭据的单调时钟时间。"""
        return self._auth_retry_at

    def _initial_credential_status(self) -> str:
        if not self.api_key and not self.api_secret:
            return "disabled"
        if not self.api_key or not self.api_secret:
            return "invalid_config"
        return "unverified"

    def can_request(self) -> bool:
        """判断混合适配器是否应尝试 Alpaca，避免未配置或失效时反复打请求。"""
        if not self.enabled or self._credential_status == "invalid_config":
            return False
        return not (self._credential_status == "invalid_credentials" and time.monotonic() < self._auth_retry_at)

    def _mark_auth_failure(self) -> None:
        self._credential_status = "invalid_credentials"
        self._auth_retry_at = time.monotonic() + ALPACA_AUTH_RETRY_SECONDS

    def _mark_auth_success(self) -> None:
        self._credential_status = "valid"
        self._auth_retry_at = 0.0

    def validate_credentials(self, *, force: bool = False) -> str:
        """启动或冷却结束后验证凭据；网络故障不误判为凭据错误。"""
        if not self.api_key and not self.api_secret:
            self._credential_status = "disabled"
            return self._credential_status
        if not self.api_key or not self.api_secret:
            self._credential_status = "invalid_config"
            return self._credential_status
        if not force and self._credential_status == "valid":
            return self._credential_status
        if not force and self._credential_status == "invalid_credentials" and time.monotonic() < self._auth_retry_at:
            return self._credential_status
        url = f"{self.data_url}?symbols=SPY&feed=overnight"
        try:
            with self.upstream_gate.slot():
                self._request_json(url)
        except AlpacaAuthError:
            self._mark_auth_failure()
            return self._credential_status
        except ProviderError:
            self._credential_status = "unavailable"
            return self._credential_status
        except Exception:  # noqa: BLE001 - 校验失败不能阻止应用启动
            self._credential_status = "unavailable"
            return self._credential_status
        self._mark_auth_success()
        return self._credential_status

    def reset_credentials(self, api_key: str | None, api_secret: str | None) -> str:
        """更新运行时凭据并清除冷却状态，便于更换密钥后恢复。"""
        self.api_key = (api_key or "").strip()
        self.api_secret = (api_secret or "").strip()
        self._credential_status = self._initial_credential_status()
        self._auth_retry_at = 0.0
        return self._credential_status

    @staticmethod
    def normalize_symbol(symbol: str) -> str:
        return MarketDataProvider.normalize_symbol(symbol)

    def quote(self, symbol: str) -> dict[str, Any]:
        normalized = self.normalize_symbol(symbol)
        if not self.enabled:
            raise ProviderError("Alpaca 夜盘凭据未完整配置")
        if self._credential_status == "invalid_credentials" and time.monotonic() < self._auth_retry_at:
            raise AlpacaAuthError("Alpaca 凭据已失效，等待冷却后重试")
        if self._credential_status == "invalid_config":
            raise ProviderError("Alpaca 夜盘凭据配置不完整")
        url = f"{self.data_url}?symbols={quote(normalized, safe='.-')}&feed=overnight"
        try:
            with self.upstream_gate.slot():
                payload = self._request_json(url)
        except AlpacaAuthError:
            self._mark_auth_failure()
            raise
        except ProviderError:
            raise
        except Exception as exc:  # noqa: BLE001 - 适配器边界统一转换异常
            raise ProviderError(f"获取 {normalized} Alpaca 夜盘行情失败：{exc}") from exc

        self._mark_auth_success()

        snapshot = payload.get(normalized)
        if not isinstance(snapshot, dict):
            raise ProviderError(f"Alpaca 没有返回 {normalized} 夜盘快照")
        latest_quote = snapshot.get("latestQuote") or {}
        latest_trade = snapshot.get("latestTrade") or {}
        minute_bar = snapshot.get("minuteBar") or {}
        daily_bar = snapshot.get("dailyBar") or {}
        if not isinstance(latest_quote, dict):
            latest_quote = {}
        if not isinstance(latest_trade, dict):
            latest_trade = {}
        if not isinstance(minute_bar, dict):
            minute_bar = {}
        if not isinstance(daily_bar, dict):
            daily_bar = {}

        # Basic 夜盘的最新报价属于指示性报价；优先用最新买卖价中间值，
        # 没有报价时再回退到成交或最近 1 分钟 Bar 收盘价。
        price = _midpoint(latest_quote)
        price_source = "indicative_quote"
        if price is None:
            price = _number(latest_trade.get("p"), positive=True)
            price_source = "trade"
        if price is None:
            price = _number(minute_bar.get("c"), positive=True)
            price_source = "minute_bar"
        if price is None:
            raise ProviderError(f"Alpaca 没有返回 {normalized} 有效夜盘价格")

        as_of = _timestamp(latest_quote.get("t") or latest_trade.get("t") or minute_bar.get("t"))
        bid = _number(latest_quote.get("bp"), positive=True)
        ask = _number(latest_quote.get("ap"), positive=True)
        session = {
            "price": safe_value(price),
            "change_percent": None,
            "as_of": as_of,
            "volume": _number(daily_bar.get("v")),
            "bar_volume": _number(minute_bar.get("v")),
            "bid": safe_value(bid),
            "ask": safe_value(ask),
            "provider": self.name,
            "price_source": price_source,
            "delayed": True,
        }
        return {
            "symbol": normalized,
            "price": safe_value(price),
            "change_percent": None,
            "today_open": _number(daily_bar.get("o"), positive=True),
            "previous_close": _number((snapshot.get("prevDailyBar") or {}).get("c"), positive=True),
            "currency": "USD",
            "market_state": "OVERNIGHT",
            "sessions": {"overnight": session},
            "provider": self.name,
            "raw": {"latest_trade_time": as_of, "price_source": price_source},
        }


