"""保守估值与财务数据分析实现。"""
from __future__ import annotations
import json
import logging
import math
import re
import threading
import time
from concurrent.futures import ThreadPoolExecutor
from datetime import date, datetime, timedelta, timezone
from typing import Any
from urllib.parse import urlencode
from urllib.request import ProxyHandler, Request, build_opener
from app.providers.market_earnings import EarningsValuationMixin
from app.providers.market_common import (FAIR_VALUE_SOURCE, MarketRegime, ProviderError, UPSTREAM_REQUEST_TIMEOUT_SECONDS, VALUATION_CONFIG, YAHOO_TIMESERIES_URL, _is_transient_upstream_error, _read_fast_info, _record_upstream_error, _valuation_debug, safe_value)
logger = logging.getLogger("app.providers.market")

class MarketValuationMixin(EarningsValuationMixin):
    _fair_value_executor = ThreadPoolExecutor(max_workers=int(VALUATION_CONFIG["fair_value_workers"]), thread_name_prefix="fair-value")

    def ensure_fair_value(self, symbol: str) -> dict[str, Any]:
        """只启动估值任务，不重复抓取现货、期权链或扩展时段行情。

        轻量 quote 轮询可能只读到 SQLite 里的旧行情快照；这时必须显式把估值
        任务重新接上，否则前端只能等待一个永远不会变化的占位结果。
        ``_fair_value`` 自身负责缓存和 inflight 去重，因此重复轮询不会叠加任务。
        """
        normalized = self.normalize_symbol(symbol)
        ticker = self._ticker(normalized)
        return self._fair_value(normalized, ticker)

    def _fair_value(self, symbol: str, ticker: Any) -> dict[str, Any]:
        """异步计算公开财务数据驱动的保守估值，并立即返回已有缓存。

        财务数据可能缺失或被上游限流，任何异常都
        只会让页面显示 ``--``，不能拖慢现货和期权快照。
        """
        now = time.monotonic()
        shared_key = f"fair-value:{VALUATION_CONFIG['cache_version']}:{symbol}"
        with self._fair_value_lock:
            cached = self._fair_value_cache.get(shared_key)
            # 成功值一天内足够稳定；失败值只短暂缓存，避免一次 Yahoo 暂时异常
            # 让新标的的估值卡片要等几分钟、甚至下一次完整刷新才恢复。
            if cached and cached[1].get("value") is not None:
                cache_ttl = 6 * 60 * 60
            elif cached and cached[1].get("status") == "retry":
                # 限流结果只短暂缓存；前端轮询时可在几秒后重新抢占任务。
                cache_ttl = 2
            else:
                cache_ttl = 15
            if cached and now - cached[0] < cache_ttl:
                return cached[1]
        # 多 worker 部署时，估值结果不能只放在当前进程的字典里。
        # 共享缓存只接受当前版本且有完整 value 的结果，旧算法不会混入新页面。
        if self.analysis_cache is not None:
            try:
                shared = self.analysis_cache.get_analysis_cache(shared_key)
            except Exception:
                shared = None
            if isinstance(shared, dict) and shared.get("source") == FAIR_VALUE_SOURCE and shared.get("value") is not None:
                with self._fair_value_lock:
                    self._fair_value_cache[shared_key] = (now, shared)
                return shared
        with self._fair_value_lock:
            if symbol not in self._fair_value_inflight:
                self._fair_value_inflight.add(symbol)
                should_start = True
            else:
                should_start = False
            # TTL 到期后的短暂更新期继续显示旧值，避免卡片闪空。
            current = cached[1] if cached else {"value": None, "source": None, "status": "pending"}
        if should_start:
            try:
                type(self)._fair_value_executor.submit(self._fetch_fair_value, symbol, ticker)
            except RuntimeError:
                with self._fair_value_lock:
                    self._fair_value_inflight.discard(symbol)
        return current

    def _fetch_fair_value(self, symbol: str, ticker: Any) -> None:
        """后台读取上游目标均值；不论成功失败都解除该标的的去重标记。"""
        now = time.monotonic()
        shared_key = f"fair-value:{VALUATION_CONFIG['cache_version']}:{symbol}"
        result: dict[str, Any] = {"value": None, "source": None, "status": "pending"}
        with self._fair_value_lock:
            cached = self._fair_value_cache.get(shared_key)
        try:
            regime, regime_signals = self._detect_regime(ticker)
            result = self._conservative_dcf(ticker, symbol, regime=regime, regime_signals=regime_signals)
            result["regime"] = regime
            result["regime_signals"] = regime_signals
            if result.get("value") is None:
                result.setdefault("status", "unavailable")
                result.setdefault("warning", "公开财务数据不足，未生成有效估值候选")
            else:
                result.setdefault("status", "ready")
        except Exception as exc:  # noqa: BLE001 - 可选字段失败不应影响行情
            logger.debug("计算 %s 保守估值失败，继续使用现货行情：%s", symbol, exc)
            result = {
                "value": None,
                "source": None,
                "status": "retry" if _is_transient_upstream_error(exc) else "failed",
                "warning": "Yahoo 数据源暂时限流，将自动重试" if _is_transient_upstream_error(exc) else "估值计算暂时失败，将在下一次刷新重试",
            }
        finally:
            with self._fair_value_lock:
                if shared_key not in self._fair_value_cache and len(self._fair_value_cache) >= 128:
                    self._fair_value_cache.pop(next(iter(self._fair_value_cache)))
                # 上游临时限流时保留最近一次成功值；时间戳仍沿用旧值，下一次请求会
                # 重新尝试，而页面不会因为一次 429 直接闪成空白。
                if result.get("value") is None and cached and cached[1].get("value") is not None:
                    self._fair_value_cache[shared_key] = cached
                else:
                    self._fair_value_cache[shared_key] = (now, result)
                if result.get("value") is not None and self.analysis_cache is not None:
                    try:
                        self.analysis_cache.put_analysis_cache(shared_key, result, max_entries=256)
                    except Exception:
                        # 估值是可选展示字段，缓存写入失败不能影响现货响应。
                        logger.debug("写入 %s 估值共享缓存失败", symbol, exc_info=True)
                self._fair_value_inflight.discard(symbol)
            if self.analysis_cache is not None and hasattr(self.analysis_cache, "publish_push_event"):
                try:
                    self.analysis_cache.publish_push_event(symbol, "fair_value")
                except Exception:
                    logger.debug("发布 %s 估值完成事件失败", symbol, exc_info=True)

    def _detect_regime(self, ticker: Any) -> tuple[str, dict[str, Any]]:
        """读取个股/宏观公开字段和基准动量，返回当前估值状态。"""
        info_failed = False
        try:
            with self.upstream_gate.slot():
                raw_info = getattr(ticker, "info", {})
        except Exception:
            raw_info = {}
            info_failed = True
        info = raw_info if isinstance(raw_info, dict) else {}
        benchmark_bars = None
        cache_key = "^GSPC"
        now_ts = time.monotonic()
        with self._benchmark_cache_lock:
            cached = self._benchmark_cache.get(cache_key)
            if cached and now_ts - cached[0] < 900:
                benchmark_bars = cached[1]
        if benchmark_bars is None and not info_failed:
            try:
                benchmark_bars = self.benchmark_history(cache_key, period="6mo")
                with self._benchmark_cache_lock:
                    self._benchmark_cache[cache_key] = (now_ts, benchmark_bars)
            except Exception:
                logger.debug("读取基准指数动量失败", exc_info=True)
                # 基准动量是可选信号；缓存空结果，避免 Yahoo 限流时每次个股
                # 估值重试都额外请求一次 ^GSPC。
                with self._benchmark_cache_lock:
                    self._benchmark_cache[cache_key] = (now_ts, [])
        return MarketRegime.detect(info, benchmark_bars)

    def _conservative_dcf(
        self,
        ticker: Any,
        symbol: str | None = None,
        *,
        regime: str = MarketRegime.NORMAL,
        regime_signals: dict[str, Any] | None = None,
    ) -> dict[str, Any]:
        """读取公开财务数据并选择适合公司阶段的多模态保守估值。"""
        upstream_errors: list[str] = []
        timeseries = self._fetch_yahoo_timeseries(symbol) if symbol else {}
        if timeseries.get("__status__") == "retry":
            upstream_errors.append("fundamentals_timeseries_rate_limited")
        try:
            with self.upstream_gate.slot():
                cashflow = getattr(ticker, "cashflow", None)
                balance = getattr(ticker, "balance_sheet", None)
                raw_info = getattr(ticker, "info", {})
        except Exception as exc:
            cashflow = balance = None
            raw_info = {}
            _record_upstream_error(upstream_errors, "yahoo_info", exc)
        info = raw_info if isinstance(raw_info, dict) else {}

        # fundamentals-timeseries 对小盘股更容易被限流；Yahoo 的 info 仍可能
        # 提供 TTM 营收，先用它恢复 P/S、资产周转等需要营收锚点的分支。
        info_revenue = self._first_info_number(
            info,
            ("totalRevenue", "trailingAnnualRevenue", "revenue", "totalRevenueTTM"),
        )

        fcf = self._statement_values(cashflow, ("Free Cash Flow",))
        if not fcf:
            operating = self._statement_values(cashflow, ("Operating Cash Flow", "Total Cash From Operating Activities"))
            capex = self._statement_values(cashflow, ("Capital Expenditure", "Capital Expenditures"))
            fcf = [op + spending for op, spending in zip(operating, capex)]
        if not fcf:
            fcf = self._timeseries_values(timeseries, "annualFreeCashFlow")
        if not fcf:
            operating = self._timeseries_values(timeseries, "annualOperatingCashFlow")
            capex = self._timeseries_values(timeseries, "annualCapitalExpenditure")
            fcf = [op + spending for op, spending in zip(operating, capex)]
        info_fcf = self._info_number(info, "freeCashflow")
        if not fcf and info_fcf is not None:
            fcf = [info_fcf]

        debt_values = self._statement_values(balance, ("Total Debt", "Total Debt And Capital Lease Obligation"))
        cash_values = self._statement_values(
            balance,
            ("Cash Cash Equivalents And Short Term Investments", "Cash And Cash Equivalents", "Cash Financial"),
        )
        shares_values = self._statement_values(balance, ("Ordinary Shares Number", "Share Issued"))
        shares_source = "balance_sheet" if shares_values else None
        equity_values = self._statement_values(balance, ("Stockholders Equity", "Total Stockholder Equity", "Common Stock Equity"))
        if not equity_values:
            equity_values = self._timeseries_values(timeseries, "annualStockholdersEquity")
        interest_values = self._statement_values(
            cashflow,
            ("Interest Expense Non Operating", "Interest Expense", "Net Non Operating Interest Income Expense"),
        )
        debt_values = debt_values or self._timeseries_values(timeseries, "annualTotalDebt")
        cash_values = cash_values or self._timeseries_values(timeseries, "annualCashCashEquivalentsAndShortTermInvestments")
        if not shares_values:
            shares_values = self._timeseries_values(timeseries, "annualDilutedAverageShares")
            if shares_values:
                shares_source = "fundamentals_timeseries"
        if not debt_values:
            debt = self._info_number(info, "totalDebt")
            debt_values = [debt] if debt is not None else []
        if not cash_values:
            cash = self._info_number(info, "totalCash")
            cash_values = [cash] if cash is not None else []
        if not shares_values:
            shares = self._info_number(info, "sharesOutstanding")
            shares_values = [shares] if shares is not None else []
            if shares_values:
                shares_source = "info"
        if not shares_values:
            try:
                with self.upstream_gate.slot():
                    shares = _read_fast_info(getattr(ticker, "fast_info", {}), "shares")
                if shares is not None:
                    shares_values = [shares]
                    shares_source = "fast_info"
            except Exception:
                pass
        current_price = self._first_info_number(info, ("regularMarketPrice", "currentPrice", "previousClose"))
        if current_price is None:
            try:
                with self.upstream_gate.slot():
                    current_price = _read_fast_info(getattr(ticker, "fast_info", {}), "last_price")
            except Exception:
                pass
        historical_price_values: list[float] = []
        # 历史价格只用于估值分位展示；时序接口已限流时跳过这次额外请求，
        # 避免 ONDS 首次估值同时触发 fundamentals、历史价格和基准三组限流。
        if timeseries.get("__status__") != "retry":
            try:
                with self.upstream_gate.slot():
                    historical_frame = ticker.history(
                        period="6y", interval="1mo", auto_adjust=True,
                        timeout=UPSTREAM_REQUEST_TIMEOUT_SECONDS,
                    )
                historical_price_values = self._annual_closing_prices(historical_frame)
            except Exception as exc:
                logger.debug("读取 %s 多年估值价格序列失败", symbol, exc_info=True)
                _record_upstream_error(upstream_errors, "historical_prices", exc)

        revenue_values = self._timeseries_values(timeseries, "annualTotalRevenue")
        if not revenue_values and info_revenue is not None and info_revenue > 0:
            revenue_values = [info_revenue]

        return self._conservative_earnings(
            info,
            symbol=symbol,
            eps_values=self._timeseries_values(timeseries, "trailingDilutedEPS"),
            annual_eps_values=self._timeseries_values(timeseries, "annualDilutedEPS"),
            fcf_values=[value for value in fcf if math.isfinite(value)],
            operating_cashflow_values=self._timeseries_values(timeseries, "annualOperatingCashFlow"),
            shares_values=shares_values,
            shares_source=shares_source,
            debt_values=debt_values,
            cash_values=cash_values,
            timeseries=timeseries,
            forward_eps=self._first_info_number(info, ("forwardEps", "epsForward", "forwardEPS")),
            current_price=current_price,
            growth_hint=self._first_info_number(info, ("earningsGrowth", "earningsQuarterlyGrowth", "revenueGrowth")),
            beta=self._first_info_number(info, ("beta", "beta3Year")),
            sector=str(info.get("sector") or ""),
            industry=str(info.get("industry") or ""),
            industry_key=str(info.get("industryKey") or info.get("industry_key") or ""),
            quarterly_revenue_values=self._timeseries_values(timeseries, "quarterlyTotalRevenue"),
            quarterly_eps_values=self._timeseries_values(timeseries, "quarterlyDilutedEPS"),
            revenue_values=revenue_values,
            operating_income_values=self._timeseries_values(timeseries, "annualOperatingIncome"),
            net_income_values=self._timeseries_values(timeseries, "annualNetIncome"),
            da_values=self._timeseries_values(timeseries, "annualDepreciationAndAmortization"),
            capex_values=self._timeseries_values(timeseries, "annualCapitalExpenditure"),
            working_capital_values=self._timeseries_values(timeseries, "annualChangeInWorkingCapital"),
            buyback_values=self._timeseries_values(timeseries, "annualRepurchaseOfCapitalStock"),
            historical_price_values=historical_price_values,
            equity_values=equity_values,
            market_cap=self._first_info_number(info, ("marketCap",)),
            dividend_rate=self._first_info_number(info, ("dividendRate", "trailingAnnualDividendRate")),
            book_value=self._first_info_number(info, ("bookValue",)),
            defense_revenue_growth=self._first_info_number(info, ("defenseRevenueGrowth", "aAndDRevenueGrowth", "aerospaceDefenseRevenueGrowth")),
            target_mean_price=self._first_info_number(info, ("targetMeanPrice", "targetMedianPrice")),
            target_low_price=self._first_info_number(info, ("targetLowPrice",)),
            target_high_price=self._first_info_number(info, ("targetHighPrice",)),
            price_to_sales=self._first_info_number(info, ("priceToSalesTrailing12Months", "priceToSales")),
            enterprise_to_ebitda=self._first_info_number(info, ("enterpriseToEbitda", "enterpriseToEbitdaForward")),
            forward_ebitda=self._first_info_number(info, ("forwardEbitda", "forwardEBITDA", "ebitdaForward")),
            interest_expense=(interest_values[0] if interest_values else self._first_info_number(info, ("interestExpense", "interestExpenseToRevenue", "interestExpenseNonOperating"))),
            revenue_growth=self._first_info_number(info, ("revenueGrowth",)),
            roe=self._first_info_number(info, ("returnOnEquity", "roe", "returnOnAverageEquity")),
            ffo_per_share=self._first_info_number(info, ("ffoPerShare", "fundsFromOperationsPerShare", "normalizedFfoPerShare")),
            affo_per_share=self._first_info_number(info, ("affoPerShare", "adjustedFundsFromOperationsPerShare")),
            nav_per_share=self._first_info_number(info, ("navPerShare", "netAssetValuePerShare")),
            rab_per_share=self._first_info_number(info, ("rabPerShare", "regulatedAssetBasePerShare")),
            embedded_value_per_share=self._first_info_number(info, ("embeddedValuePerShare", "evPerShare", "embeddedValue")),
            new_business_value_per_share=self._first_info_number(info, ("newBusinessValuePerShare", "nbvPerShare", "newBusinessValue")),
            pipeline_rnpv=self._first_info_number(info, ("pipelineRnPV", "pipelineRNPV", "riskAdjustedNPV", "rNPV")),
            approved_drug_value=self._first_info_number(info, ("approvedDrugValue", "marketedProductValue", "approvedProductsValue")),
            platform_users=self._first_info_number(info, ("monthlyActiveUsers", "activeUsers", "users", "userCount")),
            arpu=self._first_info_number(info, ("arpu", "averageRevenuePerUser")),
            churn_rate=self._first_info_number(info, ("churnRate", "userChurnRate")),
            ai_revenue_share=self._first_info_number(info, ("aiRevenueShare", "aiRevenuePercentage", "artificialIntelligenceRevenueShare")),
            analyst_count=self._first_info_number(info, ("numberOfAnalystOpinions", "analystCount", "analystCoverage")),
            regime=regime,
            regime_signals=regime_signals,
            upstream_errors=upstream_errors,
        )

    @staticmethod
    def _info_number(info: dict[str, Any], key: str) -> float | None:
        value = safe_value(info.get(key))
        try:
            number = float(value)
        except (TypeError, ValueError):
            return None
        return number if math.isfinite(number) else None

    @staticmethod
    def _owner_earnings_maintenance_ratio(
        sector: str,
        industry: str,
        industry_key: str,
        asset_heavy_transition: bool,
    ) -> tuple[float, str]:
        """按行业估计资本开支中可视为维护性支出的比例。"""
        text = f"{sector} {industry} {industry_key}".lower().replace("_", " ")
        cfg = VALUATION_CONFIG.get("owner_earnings", {})
        if asset_heavy_transition:
            return float(cfg.get("asset_heavy_transition_maintenance_capex_ratio", 0.30)), "重资产转型"
        if any(word in text for word in ("utility", "utilities", "regulated electric", "water utility", "gas utility")):
            return float(cfg.get("mature_manufacturing_maintenance_capex_ratio", 0.85)), "公用事业"
        if any(word in text for word in ("manufacturing", "industrials", "machinery", "factory")):
            return float(cfg.get("mature_manufacturing_maintenance_capex_ratio", 0.85)), "成熟制造"
        if any(word in text for word in ("technology", "software", "consumer", "internet", "semiconductor")):
            return float(cfg.get("technology_maintenance_capex_ratio", 0.50)), "科技/消费"
        return float(cfg.get("default_maintenance_capex_ratio", 0.70)), "通用"

    @staticmethod
    def _valuation_percentile(values: list[float], current: float | None) -> float | None:
        """返回当前倍数在可用历史代理序列中的百分位。"""
        valid = sorted(value for value in values if value > 0 and math.isfinite(value))
        if not valid or current is None or not math.isfinite(current) or current <= 0:
            return None
        return round(sum(value <= current for value in valid) / len(valid) * 100.0, 1)

    @classmethod
    def _historical_valuation_percentiles(
        cls,
        *,
        current_price: float | None,
        historical_price_values: list[float] | None,
        annual_eps_values: list[float] | None,
        ttm_eps: float | None,
        debt: float,
        cash: float,
        shares: float | None,
        debt_values: list[float] | None,
        cash_values: list[float] | None,
        shares_values: list[float] | None,
        equity_values: list[float] | None,
        book_value: float | None,
        operating_income_values: list[float] | None,
        da_values: list[float] | None,
    ) -> dict[str, Any] | None:
        """用年度盈利序列生成透明的“当前价格隐含倍数”分位。"""
        if not current_price or current_price <= 0:
            return None
        result: dict[str, Any] = {}
        prices = list(historical_price_values or [])[:5]
        annual_eps = list(annual_eps_values or [])[:5]
        pe_history = [price / eps for price, eps in zip(prices, annual_eps) if price > 0 and eps > 0 and math.isfinite(price) and math.isfinite(eps)]
        current_pe = current_price / ttm_eps if ttm_eps and ttm_eps > 0 else None
        if current_pe and pe_history:
            result.update(
                pe_current=round(current_pe, 2),
                pe_percentile=cls._valuation_percentile(pe_history, current_pe),
                pe_observations=len(pe_history),
            )
        historical_shares = list(shares_values or [])[:5]
        historical_equity = list(equity_values or [])[:5]
        pb_history = [
            price * share_count / equity
            for price, share_count, equity in zip(prices, historical_shares, historical_equity)
            if price > 0 and share_count > 0 and equity > 0
            and all(math.isfinite(value) for value in (price, share_count, equity))
        ]
        current_pb = current_price / book_value if book_value and book_value > 0 else None
        if current_pb and pb_history:
            result.update(
                pb_current=round(current_pb, 2),
                pb_percentile=cls._valuation_percentile(pb_history, current_pb),
                pb_observations=len(pb_history),
            )
        if shares and shares > 0:
            enterprise_value = current_price * shares + debt - cash
            ebitda_history = []
            historical_debt = list(debt_values or [])
            historical_cash = list(cash_values or [])
            for index, (price, operating) in enumerate(zip(prices, operating_income_values or [])):
                share_count = historical_shares[index] if index < len(historical_shares) else shares
                debt_value = historical_debt[index] if index < len(historical_debt) else debt
                cash_value = historical_cash[index] if index < len(historical_cash) else cash
                da = (da_values or [])[index] if index < len(da_values or []) else 0.0
                ebitda = operating + abs(da)
                historical_ev = price * share_count + debt_value - cash_value
                if ebitda > 0 and historical_ev > 0 and math.isfinite(ebitda):
                    ebitda_history.append(historical_ev / ebitda)
            current_ebitda = ((operating_income_values or [])[0] + abs((da_values or [])[0])) if operating_income_values else None
            current_ev_ebitda = enterprise_value / current_ebitda if current_ebitda and current_ebitda > 0 and enterprise_value > 0 else None
            if current_ev_ebitda and ebitda_history:
                result.update(
                    ev_ebitda_current=round(current_ev_ebitda, 2),
                    ev_ebitda_percentile=cls._valuation_percentile(ebitda_history, current_ev_ebitda),
                    ev_ebitda_observations=len(ebitda_history),
                )
        return result or None

    @staticmethod
    def _annual_closing_prices(frame: Any) -> list[float]:
        """从月线中提取每个自然年的最后收盘价，按年份从新到旧返回。"""
        if frame is None or getattr(frame, "empty", True) or "Close" not in getattr(frame, "columns", ()):
            return []
        annual: dict[int, float] = {}
        try:
            for timestamp, raw in frame["Close"].items():
                price = float(safe_value(raw))
                year = int(timestamp.year)
                if price > 0 and math.isfinite(price):
                    annual[year] = price
        except (AttributeError, TypeError, ValueError):
            return []
        return [annual[year] for year in sorted(annual, reverse=True)]

    @classmethod
    def _first_info_number(cls, info: dict[str, Any], keys: tuple[str, ...]) -> float | None:
        """从多个 Yahoo 字段别名中取第一个有效数字。"""
        for key in keys:
            value = cls._info_number(info, key)
            if value is not None:
                return value
        return None

    @staticmethod
    def _timeseries_values(payload: dict[str, Any], metric: str) -> list[float]:
        """从已按外层 timestamp 排序的时序结果中提取数值。"""
        values = payload.get(metric) if isinstance(payload, dict) else None
        if not isinstance(values, list):
            return []
        result: list[float] = []
        for item in values:
            if not isinstance(item, dict):
                continue
            raw = (item.get("reportedValue") or {}).get("raw")
            try:
                number = float(raw)
            except (TypeError, ValueError):
                continue
            if math.isfinite(number):
                result.append(number)
        return result


    @classmethod
    def _cross_validate_valuation(
        cls,
        *,
        eps: float,
        fcf_values: list[float],
        shares_values: list[float],
        debt_values: list[float],
        cash_values: list[float],
        timeseries: dict[str, list[dict[str, Any]]],
        owner_earnings: float | None = None,
        required_return: float = 0.10,
    ) -> dict[str, Any]:
        """计算所有者收益、FCF 收益率、EV/EBIT 和 DCF 交叉验证值。"""
        shares = shares_values[0] if shares_values and shares_values[0] > 0 else None
        debt = debt_values[0] if debt_values else 0.0
        cash = cash_values[0] if cash_values else 0.0
        candidates: dict[str, float] = {}
        if shares:
            if fcf_values and fcf_values[0] > 0:
                # 所有者收益近似：FCF 作为已扣资本开支的可分配现金。
                candidates["所有者收益"] = (owner_earnings if owner_earnings and owner_earnings > 0 else fcf_values[0]) * 18.0 / shares
                candidates["FCF收益率"] = fcf_values[0] / 0.06 / shares
            operating_income = cls._timeseries_values(timeseries, "annualOperatingIncome")
            if operating_income and operating_income[0] > 0:
                candidates["EV/EBIT"] = (operating_income[0] * 15.0 - debt + cash) / shares
            if fcf_values and fcf_values[0] > 0 and debt >= 0:
                discount = required_return
                growth = 0.02
                pv = sum(fcf_values[0] * (1 + growth) ** year / (1 + discount) ** year for year in range(1, 6))
                terminal = fcf_values[0] * (1 + growth) ** 5 * 1.015 / (discount - 0.015) / (1 + discount) ** 5
                candidates["DCF"] = (pv + terminal - debt + cash) / shares
        valid = [value for value in candidates.values() if math.isfinite(value) and value > 0]
        return {
            "methods": {key: round(value, 2) for key, value in candidates.items()},
            "median": round(sorted(valid)[len(valid) // 2], 2) if valid else None,
        }

    @staticmethod
    def _statement_values(frame: Any, labels: tuple[str, ...]) -> list[float]:
        """按报表行名取最新在前的年度数值，兼容 yfinance 的命名差异。"""
        if frame is None or getattr(frame, "empty", True) or not hasattr(frame, "index"):
            return []
        wanted = {re.sub(r"[^a-z0-9]", "", label.lower()) for label in labels}
        try:
            rows = list(frame.index)
            match = next((row for row in rows if re.sub(r"[^a-z0-9]", "", str(row).lower()) in wanted), None)
            if match is None:
                return []
            values = frame.loc[match].tolist()
        except Exception:
            return []
        result: list[float] = []
        for value in values:
            normalized = safe_value(value)
            try:
                number = float(normalized)
            except (TypeError, ValueError):
                continue
            if math.isfinite(number):
                result.append(number)
        return result
