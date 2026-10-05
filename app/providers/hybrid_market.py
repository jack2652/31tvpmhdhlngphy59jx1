"""按市场时段组合主、延迟和夜盘行情源。"""
from __future__ import annotations
import logging
from datetime import datetime, timezone
from typing import Any, Callable
from app.providers.market_common import MARKET_TIMEZONE, ProviderError, is_session_trading_day, safe_value, session_of
from app.services.market_calendar import is_regular_session

logger = logging.getLogger("app.providers.market")

class HybridMarketDataProvider:
    """按交易时段路由期权链，并在夜盘用 Alpaca 补充股票现货参考价。"""

    name = "hybrid"

    def __init__(
        self,
        regular_provider: Any,
        delayed_provider: Any,
        now_factory: Callable[[], datetime] | None = None,
        overnight_provider: Any | None = None,
    ):
        self.regular_provider = regular_provider
        self.delayed_provider = delayed_provider
        self.overnight_provider = overnight_provider
        self._now_factory = now_factory or (lambda: datetime.now(MARKET_TIMEZONE))

    @staticmethod
    def normalize_symbol(symbol: str) -> str:
        from app.providers.market import MarketDataProvider

        return MarketDataProvider.normalize_symbol(symbol)

    def uses_delayed_options(self) -> bool:
        moment = self._now_factory()
        if moment.tzinfo is None:
            moment = moment.replace(tzinfo=MARKET_TIMEZONE)
        moment = moment.astimezone(MARKET_TIMEZONE)
        return not is_regular_session(moment)

    def uses_overnight_quote(self) -> bool:
        """只在美东 20:00–04:00 请求 Alpaca 夜盘股票快照。"""
        moment = self._now_factory()
        if moment.tzinfo is None:
            moment = moment.replace(tzinfo=MARKET_TIMEZONE)
        moment = moment.astimezone(MARKET_TIMEZONE)
        return session_of(moment) == "overnight" and is_session_trading_day(moment)

    def expirations(self, symbol: str) -> list[str]:
        provider = self.delayed_provider if self.uses_delayed_options() else self.regular_provider
        return provider.expirations(symbol)

    def quote(self, symbol: str) -> dict[str, Any]:
        base: dict[str, Any] | None = None
        try:
            base = self.regular_provider.quote(symbol)
        except Exception:
            if not self.uses_overnight_quote() or self.overnight_provider is None:
                raise
        if (
            not self.uses_overnight_quote()
            or self.overnight_provider is None
            or not getattr(self.overnight_provider, "can_request", lambda: True)()
        ):
            return base or self.regular_provider.quote(symbol)
        try:
            overnight = self.overnight_provider.quote(symbol)
        except ProviderError as exc:
            # Alpaca 凭据失效后由适配器进入冷却期；这里统一回退原行情，避免夜盘把整页打成 502。
            logger.warning("Alpaca 夜盘现货不可用，沿用主行情：%s", exc)
            return base or self.regular_provider.quote(symbol)
        if base is None:
            return overnight
        sessions = dict(base.get("sessions") or {})
        sessions.update(overnight.get("sessions") or {})
        # Alpaca 的 prevDailyBar 在夜盘可能指向更早的数据日。盘后摘要里的
        # reference_close 才是最近一次已经完成的正常盘收盘（例如昨收 132.60），
        # 因此优先使用它，再回退主行情 previous_close，最后才使用 Alpaca 的值。
        base_sessions = base.get("sessions") or {}
        post_reference = (base_sessions.get("post") or {}).get("reference_close")
        overnight_reference = (base_sessions.get("overnight") or {}).get("reference_close")
        previous = post_reference or overnight_reference or base.get("previous_close") or overnight.get("previous_close")
        price = overnight.get("price") or base.get("price")
        change = None
        if price is not None and previous not in (None, 0):
            change = (float(price) - float(previous)) / float(previous) * 100
        return {
            **base,
            "price": price,
            "change_percent": safe_value(change),
            "today_open": overnight.get("today_open") or base.get("today_open"),
            "previous_close": previous,
            "market_state": "OVERNIGHT",
            "sessions": sessions,
            "provider": self.name,
            "raw": {**(base.get("raw") or {}), "overnight_provider": overnight.get("raw") or {}},
        }

    def ensure_fair_value(self, symbol: str) -> dict[str, Any]:
        """把轻量估值触发转发给主行情适配器，避免切换时重新抓期权链。"""
        ensure = getattr(self.regular_provider, "ensure_fair_value", None)
        if callable(ensure):
            return ensure(symbol)
        return {"value": None, "source": None, "status": "unavailable"}

    def history(self, symbol: str, period: str = "6mo") -> list[dict[str, Any]]:
        return self.regular_provider.history(symbol, period)

    def benchmark_history(self, symbol: str = "^GSPC", period: str = "2y") -> list[dict[str, Any]]:
        return self.regular_provider.benchmark_history(symbol, period)

    def earnings_dates(self, symbol: str) -> list[str]:
        """财报日期走主行情适配器。混合适配器本身不继承主行情类，必须显式转发。"""
        return self.regular_provider.earnings_dates(symbol)

    def chain(self, symbol: str, expiration: str) -> list[dict[str, Any]]:
        provider = self.delayed_provider if self.uses_delayed_options() else self.regular_provider
        return provider.chain(symbol, expiration)

    def fetch(
        self,
        symbol: str,
        expiration: str,
        *,
        quote_override: dict[str, Any] | None = None,
    ) -> tuple[dict[str, Any], list[dict[str, Any]], str]:
        if not self.uses_delayed_options():
            if quote_override is None:
                return self.regular_provider.fetch(symbol, expiration)
            try:
                return self.regular_provider.fetch(symbol, expiration, quote_override=quote_override)
            except TypeError as exc:
                if "quote_override" not in str(exc):
                    raise
                return self.regular_provider.fetch(symbol, expiration)

        quote = quote_override
        if quote is None:
            try:
                quote = self.quote(symbol)
            except ProviderError as exc:
                logger.warning("非盘中主行情请求失败，改用 Cboe 标的延迟价：%s", exc)
                quote = self.delayed_provider.quote(symbol)
        rows = self.delayed_provider.chain(symbol, expiration)
        if quote.get("price") is None:
            delayed_quote = self.delayed_provider.quote(symbol)
            delayed_quote["sessions"] = quote.get("sessions") or delayed_quote.get("sessions", {})
            quote = delayed_quote
        for row in rows:
            if row.get("gamma") is None:
                row["gamma"] = MarketDataProvider.estimate_gamma(
                    quote.get("price"), row.get("strike"), row.get("implied_volatility"), expiration
                )
        return quote, rows, datetime.now(timezone.utc).isoformat()
