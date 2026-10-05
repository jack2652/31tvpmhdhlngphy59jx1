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
from app.buyer_structures import build_buyer_structures
from app.config import Settings
from app.db import NO_FLOOR, Database, iso, parse_sessions, utc_now
from app.levels import absorption_levels, annotate_level_history, average_true_range, average_true_ranges, best_trade_points, build_levels, chip_peaks, fibonacci_levels, level_strength_tier, merge_candidates, option_levels, price_extremes, select_visible_levels, split_support_plan, stop_loss_level, touch_probability, trade_recommendation, trend_channel
from app.providers import market
from app.providers import cboe
from app.providers.market import (
    MarketRegime,
    ProviderError,
    HybridMarketDataProvider,
    MarketDataProvider,
    current_session_state,
    is_session_trading_day,
    parse_earnings_dates,
    regular_session_open,
    safe_value,
    summarize_extended_hours,
)
from app.providers.cboe import CboeOptionsProvider, parse_occ_option
from app.services.earnings import summarize_earnings
from app.services.history import EARNINGS_MAX_AGE_SECONDS, HistoryService, calculate_beta
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


def test_market_regime_extreme_signal_enters_winter_without_second_hit():
    """VIX 或指数断崖单独达到极端阈值时，不能等待第二个普通信号。"""
    regime, signals = MarketRegime.detect({"vix": 80.0, "us10y": 0.006})
    assert regime == MarketRegime.WINTER
    assert signals["extreme_hits"] == ["vix_extreme"]


def test_winter_valuation_uses_default_ai_share_and_floor_eps():
    """AI 营收拆分缺失时使用行业默认值，寒冬盈利取最低三年均值。"""
    result = MarketDataProvider._conservative_earnings(
        {"trailingEps": 5.0},
        symbol="NVDA",
        eps_values=[5.0],
        annual_eps_values=[3.0, 4.0, 5.0, 6.0, 7.0, 8.0, 9.0],
        forward_eps=30.0,
        current_price=100.0,
        growth_hint=0.50,
        beta=1.5,
        sector="Technology",
        industry="Semiconductors",
        revenue_values=[100.0, 80.0],
        operating_income_values=[30.0],
        net_income_values=[20.0],
        da_values=[5.0],
        capex_values=[5.0],
        working_capital_values=[0.0],
        operating_cashflow_values=[25.0],
        market_cap=1_000_000_000.0,
        price_to_sales=12.0,
        regime=MarketRegime.WINTER,
        regime_signals={"extreme_hits": ["vix_extreme"]},
    )
    # 3,4,5,6,7 去掉高端后取最低三年平均 4，再乘 10 倍 PE 和 40% AI 折扣。
    assert result["value"] == pytest.approx(16.0)
    assert result["regime"] == MarketRegime.WINTER
    assert any("AI 寒冬" in warning for warning in result["warnings"])


def test_storage_forward_eps_is_capped_by_consensus_anchor():
    """存储股异常远期 EPS 必须受分析师目标价反推的 35 倍 PE 上限约束。"""
    result = MarketDataProvider._conservative_earnings(
        {"trailingEps": 100.0},
        symbol="SNDK",
        eps_values=[100.0],
        annual_eps_values=[100.0, 90.0, 80.0, 70.0, 60.0],
        forward_eps=527.0,
        current_price=1719.11,
        growth_hint=0.50,
        beta=1.2,
        sector="Technology",
        industry="Computer Hardware Storage",
        revenue_values=[1000.0, 800.0],
        operating_income_values=[300.0],
        net_income_values=[200.0],
        da_values=[50.0],
        capex_values=[50.0],
        working_capital_values=[0.0],
        operating_cashflow_values=[250.0],
        market_cap=10_000_000_000.0,
        price_to_sales=12.0,
        target_mean_price=2137.0,
        regime="NORMAL",
        regime_signals={},
    )
    assert result["forward_eps"] == pytest.approx(2137.0 / 35.0, rel=1e-4)
    assert result["value"] == pytest.approx((2137.0 / 35.0) * 0.70 * 25.0)


@pytest.mark.parametrize("industry", ["Computer Hardware Storage", "Semiconductor Memory"])
def test_memory_storage_uses_cyclical_model_even_with_strong_fcf_and_growth(industry):
    """NAND/DRAM 等内存制造商不能因景气期 FCF 和增速高而套结构性成长 PE。"""
    result = MarketDataProvider._conservative_earnings(
        {"trailingEps": 100.0},
        symbol="SNDK",
        eps_values=[100.0],
        annual_eps_values=[100.0, 90.0, 80.0, 70.0, 60.0],
        forward_eps=527.0,
        current_price=1726.18,
        growth_hint=0.50,
        beta=1.2,
        sector="Technology",
        industry=industry,
        revenue_values=[1000.0, 800.0],
        fcf_values=[250.0],
        operating_income_values=[300.0],
        net_income_values=[200.0],
        da_values=[50.0],
        capex_values=[50.0],
        working_capital_values=[0.0],
        operating_cashflow_values=[250.0],
        market_cap=10_000_000_000.0,
        price_to_sales=12.0,
        target_mean_price=2137.0,
        regime="NORMAL",
        regime_signals={},
    )
    assert result["decision_tree"]["lifecycle"] == "AI存储/硬件周期"
    assert result["value"] == pytest.approx((2137.0 / 35.0) * 0.70 * 25.0)
    assert result["defensive"]["high"] < 1500.0
    assert result["optimistic"]["low"] == pytest.approx(round(2137.0 / 35.0 * 20.0, 2))
    assert result["optimistic"]["high"] == pytest.approx(round(2137.0 / 35.0 * 30.0, 2))
    assert result["optimistic"]["value"] == pytest.approx(round(2137.0 / 35.0 * 25.0, 2))


def test_software_storage_terms_do_not_trigger_storage_hardware_model():
    """cloud/data storage 等软件服务术语不能把 ORCL 误判成存储硬件公司。"""
    result = MarketDataProvider._conservative_earnings(
        {
            "trailingEps": 6.38,
            "longBusinessSummary": (
                "Oracle provides cloud storage, data storage, storage infrastructure and "
                "autonomous database software for enterprise customers."
            ),
        },
        symbol="ORCL",
        eps_values=[6.38],
        annual_eps_values=[6.38, 5.80, 5.20],
        forward_eps=6.80,
        current_price=137.34,
        growth_hint=0.50,
        beta=1.1,
        sector="Technology",
        industry="Software - Infrastructure",
        revenue_values=[67.36, 57.40, 52.96, 49.95],
        fcf_values=[10000.0],
        shares_values=[2.88],
        debt_values=[10000.0],
        cash_values=[11000.0],
        operating_income_values=[15000.0],
        net_income_values=[13000.0],
        da_values=[3000.0],
        capex_values=[8000.0],
        operating_cashflow_values=[18000.0],
        market_cap=417.7e9,
        regime=MarketRegime.NORMAL,
        regime_signals={},
    )
    assert result["decision_tree"]["lifecycle"] != "AI存储/硬件周期"
    assert "AI 存储/硬件周期" not in result["model"]


def test_asset_heavy_transition_defensive_label_matches_branch():
    """重资产转型分支的防守卡片应显示对应模型标签。"""
    result = MarketDataProvider._conservative_earnings(
        {"trailingEps": 6.38, "longBusinessSummary": "Oracle enterprise cloud software and infrastructure."},
        symbol="ORCL",
        eps_values=[6.38],
        annual_eps_values=[6.38, 5.80, 5.20],
        forward_eps=6.80,
        current_price=137.34,
        growth_hint=0.20,
        beta=1.1,
        sector="Technology",
        industry="Software - Infrastructure",
        revenue_values=[67.36, 57.40],
        fcf_values=[-100.0],
        shares_values=[2.88],
        debt_values=[10000.0],
        cash_values=[11000.0],
        operating_income_values=[15000.0],
        net_income_values=[13000.0],
        da_values=[3000.0],
        capex_values=[20000.0],
        operating_cashflow_values=[10000.0],
        market_cap=417.7e9,
        target_mean_price=180.0,
        regime=MarketRegime.NORMAL,
        regime_signals={},
    )
    assert result["decision_tree"]["lifecycle"] == "重资产转型"
    assert result["defensive"]["model"] == "重资产转型：核心业务 PE + 制造/云业务 P/S 压力测试"


def test_defense_tag_enters_order_driven_model_without_forward_eps():
    """国防/航空航天/激光标签配合负 FCF 时，不得掉入普通亏损转型模型。"""
    result = MarketDataProvider._conservative_earnings(
        {"trailingEps": 0.10},
        symbol="LASR",
        eps_values=[0.10],
        annual_eps_values=[0.10, 0.08, 0.05],
        forward_eps=None,
        current_price=38.11,
        growth_hint=0.0,
        beta=1.5,
        sector="Industrials",
        industry="Aerospace & Defense Laser",
        revenue_values=[1000.0, 900.0],
        fcf_values=[-50.0],
        shares_values=[100.0],
        operating_income_values=[10.0],
        net_income_values=[-5.0],
        da_values=[20.0],
        capex_values=[60.0],
        working_capital_values=[0.0],
        target_low_price=75.75,
        target_mean_price=86.70,
        target_high_price=105.0,
        regime="NORMAL",
        regime_signals={},
    )
    assert result["decision_tree"]["lifecycle"] == "国防订单驱动转型"
    assert result["defensive"]["model"] == "国防订单驱动转型：远期 P/S + 分析师共识 压力测试"
    assert result["optimistic"] is not None
    assert result["optimistic"]["low"] >= 75.75
    assert result["optimistic"]["high"] >= 105.0
    assert result["value"] > 20.0


def test_business_summary_overrides_generic_semiconductor_label_for_laser_defense():
    """业务摘要命中激光/国防时，即使 Yahoo 行业为 Semiconductors 也走国防模型。"""
    result = MarketDataProvider._conservative_earnings(
        {
            "trailingEps": 0.10,
            "longBusinessSummary": (
                "nLIGHT designs semiconductor and fiber lasers for aerospace and defense "
                "and high-energy laser systems in directed energy applications."
            ),
        },
        symbol="LASR",
        eps_values=[0.10],
        annual_eps_values=[0.10, 0.08, 0.05],
        forward_eps=None,
        current_price=40.15,
        growth_hint=0.0,
        beta=1.5,
        sector="Technology",
        industry="Semiconductors",
        revenue_values=[1000.0, 900.0],
        fcf_values=[-50.0],
        shares_values=[100.0],
        operating_income_values=[10.0],
        net_income_values=[-5.0],
        da_values=[20.0],
        capex_values=[60.0],
        working_capital_values=[0.0],
        target_low_price=75.75,
        target_mean_price=86.70,
        target_high_price=105.0,
        regime="NORMAL",
        regime_signals={},
    )
    assert result["decision_tree"]["lifecycle"] == "国防订单驱动转型"
    assert result["optimistic"] is not None
    assert result["value"] > 20.0


def test_extreme_model_value_emits_data_source_warning():
    """模型结果超过现价五倍时，输出数据源失真警告并回退到现价锚点。"""
    result = MarketDataProvider._conservative_earnings(
        {"trailingEps": 100.0},
        symbol="SNDK",
        eps_values=[100.0],
        annual_eps_values=[100.0, 90.0, 80.0],
        forward_eps=527.0,
        current_price=100.0,
        growth_hint=0.50,
        beta=1.2,
        sector="Technology",
        industry="Computer Hardware Storage",
        revenue_values=[1000.0, 800.0],
        operating_income_values=[300.0],
        net_income_values=[200.0],
        da_values=[50.0],
        capex_values=[50.0],
        operating_cashflow_values=[250.0],
        market_cap=10_000_000_000.0,
        price_to_sales=12.0,
        target_mean_price=2137.0,
        regime="NORMAL",
        regime_signals={},
    )
    assert result["value"] == pytest.approx(150.0)
    assert any("Yahoo 财务数据口径异常" in warning for warning in result["warnings"])
    assert any("已降级到现价锚定" in warning for warning in result["warnings"])


def test_mobility_technology_growth_keeps_auto_bottom_and_adds_upside_case():
    """超大市值自动驾驶/机器人公司保留汽车底线，同时输出独立成长情景。"""
    result = MarketDataProvider._conservative_earnings(
        {
            "trailingEps": 1.20,
            "longBusinessSummary": (
                "Tesla develops electric vehicles, autonomous self driving robotaxi, "
                "robotics, humanoid Optimus, artificial intelligence and energy storage "
                "for residential, commercial and industrial customers and utilities. "
                "It also provides automotive insurance services."
            ),
        },
        symbol="TSLA",
        eps_values=[1.20],
        annual_eps_values=[1.00, 1.10, 1.20],
        forward_eps=1.26,
        current_price=354.54,
        growth_hint=0.12,
        beta=2.0,
        sector="Consumer Cyclical",
        industry="Auto Manufacturers",
        revenue_values=[100000.0, 85000.0],
        fcf_values=[8000.0],
        shares_values=[3200.0],
        debt_values=[5000.0],
        cash_values=[25000.0],
        operating_income_values=[12000.0],
        net_income_values=[9000.0],
        da_values=[3000.0],
        capex_values=[5000.0],
        working_capital_values=[0.0],
        operating_cashflow_values=[13000.0],
        market_cap=1.13e12,
        target_mean_price=400.0,
        regime=MarketRegime.NORMAL,
        regime_signals={},
    )
    assert result["decision_tree"]["lifecycle"] == "移动科技成长"
    assert result["defensive"]["value"] < 100.0
    assert result["optimistic"] is not None
    assert result["optimistic"]["low"] > result["defensive"]["high"]
    assert "移动科技成长" in result["optimistic"]["model"]
    assert any("保守估值仍按汽车制造底线" in warning for warning in result["warnings"])


def test_enterprise_software_terms_do_not_trigger_mobility_model():
    """Oracle 的 autonomous database/robotic automation 不能误触发移动科技估值。"""
    result = MarketDataProvider._conservative_earnings(
        {
            "trailingEps": 6.38,
            "longBusinessSummary": (
                "Oracle offers autonomous database products and robotic process automation "
                "for enterprise software customers."
            ),
        },
        symbol="ORCL",
        eps_values=[6.38],
        annual_eps_values=[6.38, 5.80, 5.20],
        forward_eps=10.99719,
        current_price=137.34,
        growth_hint=0.50,
        beta=1.1,
        sector="Technology",
        industry="Software - Infrastructure",
        revenue_values=[67.36, 57.40, 52.96, 49.95],
        fcf_values=[10000.0],
        shares_values=[2.88],
        debt_values=[10000.0],
        cash_values=[11000.0],
        operating_income_values=[15000.0],
        net_income_values=[13000.0],
        da_values=[3000.0],
        capex_values=[8000.0],
        operating_cashflow_values=[18000.0],
        market_cap=417.7e9,
        regime=MarketRegime.NORMAL,
        regime_signals={},
    )
    assert result["decision_tree"]["lifecycle"] != "移动科技成长"
    assert "移动科技成长" not in (result["optimistic"] or {}).get("model", "")


def test_mobility_growth_is_disabled_in_ai_winter():
    """寒冬状态关闭移动科技成长估值，回退到压力测试模型。"""
    result = MarketDataProvider._conservative_earnings(
        {
            "trailingEps": 1.20,
            "longBusinessSummary": "autonomous robotaxi robotics artificial intelligence energy storage",
        },
        symbol="TSLA",
        eps_values=[1.20],
        annual_eps_values=[0.80, 1.00, 1.20, 1.40, 1.60],
        forward_eps=1.26,
        current_price=354.54,
        growth_hint=0.12,
        beta=2.0,
        sector="Consumer Cyclical",
        industry="Auto Manufacturers",
        revenue_values=[100000.0, 85000.0],
        fcf_values=[8000.0],
        shares_values=[3200.0],
        debt_values=[5000.0],
        cash_values=[25000.0],
        operating_income_values=[12000.0],
        net_income_values=[9000.0],
        da_values=[3000.0],
        capex_values=[5000.0],
        operating_cashflow_values=[13000.0],
        market_cap=1.13e12,
        regime=MarketRegime.WINTER,
        regime_signals={"extreme_hits": ["vix_extreme"]},
    )
    assert result["regime"] == MarketRegime.WINTER
    assert result["decision_tree"]["lifecycle"] != "移动科技成长"
    assert "AI寒冬" in result["optimistic"]["model"]


def test_large_ai_mobility_company_keeps_upside_when_growth_data_is_weak():
    """TSLA 类超大市值 AI 移动公司不因短期负增长或负 FCF 丢失成长情景。"""
    result = MarketDataProvider._conservative_earnings(
        {
            "trailingEps": 1.20,
            "longBusinessSummary": (
                "Tesla develops electric vehicles, self-driving development and artificial intelligence software."
            ),
        },
        symbol="TSLA",
        eps_values=[1.20],
        annual_eps_values=[1.00, 1.10, 1.20],
        forward_eps=1.26,
        current_price=353.53,
        growth_hint=-0.08,
        beta=2.0,
        sector="Consumer Cyclical",
        industry="Auto Manufacturers",
        revenue_values=[100000.0, 105000.0],
        fcf_values=[-1000.0],
        shares_values=[3200.0],
        debt_values=[5000.0],
        cash_values=[25000.0],
        operating_income_values=[12000.0],
        net_income_values=[9000.0],
        da_values=[3000.0],
        capex_values=[14000.0],
        working_capital_values=[0.0],
        operating_cashflow_values=[13000.0],
        market_cap=1.13e12,
        regime=MarketRegime.NORMAL,
        regime_signals={},
    )
    assert result["decision_tree"]["lifecycle"] == "移动科技成长"
    assert result["optimistic"] is not None
    assert "移动科技成长" in result["optimistic"]["model"]


def test_real_utility_company_still_uses_utility_model():
    """真正的公用事业公司仍应命中公用事业行业过滤。"""
    result = MarketDataProvider._conservative_earnings(
        {"trailingEps": 3.0, "longBusinessSummary": "Operates regulated electric utilities and water utilities."},
        symbol="UTIL",
        eps_values=[3.0],
        annual_eps_values=[2.8, 2.9, 3.0],
        forward_eps=3.2,
        current_price=60.0,
        growth_hint=0.04,
        beta=0.7,
        sector="Utilities",
        industry="Utilities - Regulated Electric",
        revenue_values=[10000.0, 9500.0],
        fcf_values=[1500.0],
        shares_values=[1000.0],
        debt_values=[8000.0],
        cash_values=[500.0],
        operating_income_values=[2500.0],
        net_income_values=[1800.0],
        da_values=[1200.0],
        capex_values=[1800.0],
        operating_cashflow_values=[3300.0],
        market_cap=60_000.0,
        dividend_rate=2.0,
        regime=MarketRegime.NORMAL,
        regime_signals={},
    )
    assert result["decision_tree"]["industry_filter"] == "utility"
    assert result["decision_tree"]["lifecycle"] == "utility"


def test_ai_internet_platform_has_separate_premium_upside_without_inversion():
    """超大市值 AI 广告平台保留基础 P/S 防守值，并单独输出 AI 溢价上沿。"""
    result = MarketDataProvider._conservative_earnings(
        {
            "trailingEps": 25.0,
            "longBusinessSummary": (
                "Meta operates social media and internet content platforms with "
                "artificial intelligence, machine learning, generative AI and recommendation engines."
            ),
        },
        symbol="META",
        eps_values=[25.0],
        annual_eps_values=[20.0, 22.0, 25.0],
        forward_eps=30.0,
        current_price=735.84,
        growth_hint=0.15,
        beta=1.2,
        sector="Communication Services",
        industry="Internet Content & Information",
        revenue_values=[180000.0, 155000.0],
        fcf_values=[60000.0],
        shares_values=[2530.0],
        debt_values=[50000.0],
        cash_values=[65000.0],
        operating_income_values=[70000.0],
        net_income_values=[55000.0],
        da_values=[10000.0],
        capex_values=[30000.0],
        working_capital_values=[0.0],
        operating_cashflow_values=[90000.0],
        market_cap=1.85e12,
        target_mean_price=800.0,
        churn_rate=0.05,
        regime=MarketRegime.NORMAL,
        regime_signals={},
    )
    assert result["decision_tree"]["industry_filter"] == "internet_platform"
    assert result["optimistic"] is not None
    assert "AI 互联网平台" in result["optimistic"]["model"]
    assert result["optimistic"]["high"] > result["defensive"]["high"]
    assert result["optimistic"]["low"] >= result["defensive"]["low"]


def test_regime_benchmark_history_is_cached_for_fifteen_minutes():
    """同一行情提供器内，基准指数在 15 分钟内只读取一次。"""
    provider = MarketDataProvider(ticker_factory=lambda symbol: SimpleNamespace(info={}))
    calls = []
    bars = sample_bars(60)
    provider.benchmark_history = lambda symbol, period="6mo": calls.append((symbol, period)) or bars
    ticker = SimpleNamespace(info={"vix": 15.0})

    provider._detect_regime(ticker)
    provider._detect_regime(ticker)

    assert calls == [("^GSPC", "6mo")]
    assert safe_value(pd.NA) is None
    assert safe_value(pd.NaT) is None
    assert MarketDataProvider.normalize_symbol(" brk.b ") == "BRK.B"
    assert MarketDataProvider.normalize_symbol("BF-B") == "BF-B"
    with pytest.raises(ValueError):
        MarketDataProvider.normalize_symbol("AAPL/")


def test_timeseries_values_preserves_fetch_sorted_newest_first_arrays():
    """时序提取器不再二次反转，直接保留 fetch 层按 timestamp 排好的顺序。"""
    newest_first = {
        "annualTotalRevenue": [
            {"reportedValue": {"raw": 125.0}},
            {"reportedValue": {"raw": 100.0}},
            {"reportedValue": {"raw": 80.0}},
        ]
    }
    assert MarketDataProvider._timeseries_values(newest_first, "annualTotalRevenue") == [125.0, 100.0, 80.0]


def test_valuation_quality_metrics_are_exposed_and_eps_units_are_guarded():
    result = MarketDataProvider._conservative_earnings(
        {"trailingEps": 10.0},
        symbol="QUALITY",
        eps_values=[10.0],
        annual_eps_values=[10.0, 9.0, 8.0],
        net_income_values=[1_000_000.0, 900_000.0],
        shares_values=[100.0],
        current_price=100.0,
        revenue_values=[1_000_000.0, 900_000.0],
        dividend_rate=1.0,
        buyback_values=[-500.0],
        operating_income_values=[200_000.0, 180_000.0],
        da_values=[20_000.0, 18_000.0],
        regime=MarketRegime.NORMAL,
        regime_signals={},
    )
    assert "量纲异常回退" in result["normalized_eps_source"]
    assert result["shareholder_total_return_yield"] == pytest.approx(0.06)


def test_market_cap_reconciliation_preserves_yahoo_value_for_non_statement_shares():
    """info/fast_info 股数推导出更低市值时，避免未经报表确认就向下校正。"""
    result = MarketDataProvider._conservative_earnings(
        {"trailingEps": 1.0},
        symbol="ONDS",
        eps_values=[1.0],
        annual_eps_values=[0.9, 1.0],
        shares_values=[80_000_000.0],
        shares_source="info",
        current_price=35.0,
        market_cap=4.4e9,
        revenue_values=[500e6, 400e6],
        regime=MarketRegime.NORMAL,
        regime_signals={},
    )
    assert result["market_cap_data_quality"] == "market_cap_preferred"
    assert any("保留 Yahoo market_cap" in warning for warning in result["warnings"])


def test_market_cap_reconciliation_allows_balance_sheet_shares_to_correct_value():
    """资产负债表股本是较强证据，市值偏差超过阈值时允许校正。"""
    result = MarketDataProvider._conservative_earnings(
        {"trailingEps": 1.0},
        symbol="BALANCE",
        eps_values=[1.0],
        annual_eps_values=[0.9, 1.0],
        shares_values=[80_000_000.0],
        shares_source="balance_sheet",
        current_price=35.0,
        market_cap=4.4e9,
        revenue_values=[500e6, 400e6],
        regime=MarketRegime.NORMAL,
        regime_signals={},
    )
    assert result["market_cap_data_quality"] == "reconciled"
    assert any("校正市值" in warning for warning in result["warnings"])


def test_market_cap_reconciliation_is_symmetric_for_untrusted_high_implied_value():
    """低 Yahoo 市值不能被滞后/未知股本推导出的高市值反向抬升。"""
    result = MarketDataProvider._conservative_earnings(
        {"trailingEps": 1.0},
        symbol="STALE_SHARES",
        eps_values=[1.0],
        annual_eps_values=[0.9, 1.0],
        shares_values=[80_000_000.0],
        shares_source="fundamentals_timeseries",
        current_price=35.0,
        market_cap=1.0e9,
        revenue_values=[500e6, 400e6],
        regime=MarketRegime.NORMAL,
        regime_signals={},
    )
    assert result["market_cap_data_quality"] == "market_cap_preferred"
    assert any("保留 Yahoo market_cap" in warning for warning in result["warnings"])


def test_industry_key_category_is_typed_without_injecting_free_text_keywords():
    """computer-hardware 映射本身不应制造 storage 关键词命中。"""
    result = MarketDataProvider._conservative_earnings(
        {
            "trailingEps": 2.0,
            "longBusinessSummary": "Designs industrial controllers and automation software.",
        },
        symbol="CTRL",
        eps_values=[2.0],
        annual_eps_values=[1.8, 2.0],
        forward_eps=2.2,
        current_price=40.0,
        growth_hint=0.08,
        sector="Technology",
        industry="Electronic Components",
        industry_key="computer-hardware",
        revenue_values=[1000.0, 950.0],
        fcf_values=[100.0],
        shares_values=[10.0],
        market_cap=400.0,
        regime=MarketRegime.NORMAL,
        regime_signals={},
    )
    assert result["decision_tree"]["lifecycle"] != "AI存储/硬件周期"


def test_valuation_diagnostic_log_records_input_series(caplog):
    """估值诊断日志应暴露输入营收序列和关键锚点，便于定位数据源问题。"""
    previous_debug = market.VALUATION_CONFIG["debug"]
    market.VALUATION_CONFIG["debug"] = True
    try:
        with caplog.at_level("INFO", logger="app.providers.market"):
            MarketDataProvider._conservative_earnings(
                {"trailingEps": 2.16},
                symbol="TSLA",
                eps_values=[2.16],
                annual_eps_values=[2.0, 2.1, 2.16],
                forward_eps=1.8,
                growth_hint=0.05,
                sector="Consumer Cyclical",
                industry="Auto Manufacturers",
                revenue_values=[97.7e9, 96.8e9, 81.5e9],
                shares_values=[3.52e9],
                market_cap=1.23e12,
                regime=MarketRegime.NORMAL,
                regime_signals={},
                )
    finally:
        market.VALUATION_CONFIG["debug"] = previous_debug
    assert any("估值诊断 symbol=TSLA" in record.message for record in caplog.records)
    message = next(record.message for record in caplog.records if "估值诊断 symbol=TSLA" in record.message)
    assert "rev_series(前5)=[97.7, 96.8, 81.5]" in message
    assert "shares=3.520B" in message
    assert "fwd_eps=1.8" in message


def test_fetch_yahoo_timeseries_sorts_each_metric_by_outer_timestamp(monkeypatch):
    """Yahoo timestamp 位于结果外层时，fetch 层应把每项数据排成最新在前。"""
    payload = {
        "timeseries": {
            "result": [
                {
                    "meta": {"type": ["annualTotalRevenue"]},
                    "timestamp": [1262304000, 1735603200],
                    "annualTotalRevenue": [
                        {"reportedValue": {"raw": 80.0}},
                        {"reportedValue": {"raw": 125.0}},
                    ],
                }
            ]
        }
    }

    class Response:
        def __enter__(self):
            return self

        def __exit__(self, *_):
            return False

        def read(self):
            import json
            return json.dumps(payload).encode()

    class Opener:
        def open(self, request, timeout):
            return Response()

    monkeypatch.setattr(market, "build_opener", lambda proxy: Opener())
    provider = MarketDataProvider(ticker_factory=lambda symbol: SimpleNamespace(info={}))
    result = provider._fetch_yahoo_timeseries("TSLA")
    assert MarketDataProvider._timeseries_values(result, "annualTotalRevenue") == [125.0, 80.0]


def test_fetch_yahoo_timeseries_marks_rate_limit_for_retry(monkeypatch):
    """Yahoo 429 不应被当成永久没有财务数据。"""
    from urllib.error import HTTPError

    class Opener:
        def open(self, request, timeout):
            raise HTTPError(request.full_url, 429, "Too Many Requests", {}, None)

    monkeypatch.setattr(market, "build_opener", lambda proxy: Opener())
    provider = MarketDataProvider(ticker_factory=lambda symbol: SimpleNamespace(info={}))
    result = provider._fetch_yahoo_timeseries("ONDS")
    assert result.get("__status__") == "retry"


def test_low_confidence_info_revenue_fallback_produces_value_when_timeseries_is_rate_limited():
    """小盘股时序被限流时，仍用 info 的营收和股数生成可见的低置信度估值。"""
    result = MarketDataProvider._conservative_earnings(
        {"totalRevenue": 1000.0, "priceToSalesTrailing12Months": 3.0},
        symbol="ONDS",
        shares_values=[100.0],
        current_price=10.0,
        revenue_values=[1000.0],
        price_to_sales=3.0,
        upstream_errors=["fundamentals_timeseries_rate_limited"],
        regime=MarketRegime.NORMAL,
        regime_signals={},
    )
    assert result["value"] is not None
    assert result["data_quality"] == "partial_upstream"
    assert any("Yahoo 财务接口" in warning for warning in result["warnings"])


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


def test_api_get_is_not_cached_when_body_is_unchanged():
    """API JSON 不再回 304。重复请求仍然带回正文，并禁止浏览器缓存。"""
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
    second = asyncio.run(run([(b"if-none-match", b'"same"')]))
    assert first[0]["status"] == 200
    assert second[0]["status"] == 200
    assert second[1]["body"] == b'{"ok":true}'
    headers = {key.lower(): value for key, value in second[0]["headers"]}
    assert headers[b"cache-control"] == b"no-store"
    assert b"etag" not in headers
    main = Path("app/main.py").read_text(encoding="utf-8")
    assert "if not settings.low_memory:\n    app.add_middleware(GZipMiddleware, minimum_size=1024)\napp.add_middleware(ETagMiddleware)" in main


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


def test_hybrid_overnight_uses_regular_close_as_change_base():
    """夜盘价格可以来自 Alpaca，但涨跌基准必须使用主行情校准后的最近正式收盘。"""
    eastern = ZoneInfo("America/New_York")

    class Regular:
        def quote(self, symbol):
            return {
                **sample_quote(symbol),
                "price": 132.60,
                "previous_close": 137.04,
                "market_state": "OVERNIGHT",
                "sessions": {"post": {"reference_close": 132.60}},
            }

    class Delayed:
        pass

    class Alpaca:
        def can_request(self):
            return True

        def quote(self, symbol):
            return {
                "price": 133.29,
                "previous_close": 135.04,
                "today_open": 132.60,
                "sessions": {"overnight": {"price": 133.29, "provider": "alpaca-overnight"}},
            }

    provider = HybridMarketDataProvider(
        Regular(), Delayed(),
        now_factory=lambda: datetime(2026, 9, 29, 21, 0, tzinfo=eastern),
        overnight_provider=Alpaca(),
    )
    quote = provider.quote("ORCL")
    assert quote["previous_close"] == pytest.approx(132.60)
    assert quote["change_percent"] == pytest.approx((133.29 - 132.60) / 132.60 * 100)


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


def test_fast_info_snake_case_is_readable():
    """上游 fast_info.get('last_price') 会返回 None，下标访问才能读到蛇形键。"""

    class FastInfo:
        def get(self, key, default=None):
            return default

        def __getitem__(self, key):
            return {"last_price": 110.0, "previous_close": 100.0, "open": 101.0, "currency": "USD"}[key]

    class FakeTicker:
        fast_info = FastInfo()

        def history(self, **kwargs):
            if kwargs.get("interval") == "1m":
                return pd.DataFrame()
            raise AssertionError("现价和昨收已经从 fast_info 读到，不应再回退日线")

    quote = MarketDataProvider(ticker_factory=lambda symbol: FakeTicker()).quote("AAPL")
    assert quote["price"] == 110.0
    assert quote["previous_close"] == 100.0
    assert quote["change_percent"] == pytest.approx(10.0)
    assert quote["today_open"] == pytest.approx(101.0)


def test_regular_change_uses_previous_session_when_daily_bar_is_missing(monkeypatch):
    """日线缺掉最近一个交易日时，盘中涨跌幅仍应相对那天的盘中收盘，而不是更早的收盘。"""
    eastern = ZoneInfo("America/New_York")
    minutes = pd.DataFrame(
        {"Close": [250.25, 234.89, 224.27]},
        index=pd.DatetimeIndex([
            "2026-09-21 15:59",
            "2026-09-22 15:59",
            "2026-09-23 10:30",
        ]).tz_localize(eastern),
    )
    daily = pd.DataFrame(
        {"Open": [250.5, 236.14], "Close": [250.25, 224.27]},
        index=pd.DatetimeIndex(["2026-09-21", "2026-09-23"]).tz_localize(eastern),
    )

    class FakeTicker:
        fast_info = {"last_price": None, "previous_close": None, "currency": "USD"}

        def history(self, **kwargs):
            if kwargs.get("interval") == "1m":
                return minutes
            return daily

    class FrozenDateTime(datetime):
        @classmethod
        def now(cls, tz=None):
            current = datetime(2026, 9, 23, 10, 30, tzinfo=eastern)
            if tz is None:
                return current.replace(tzinfo=None)
            return current.astimezone(tz)

    monkeypatch.setattr(market, "datetime", FrozenDateTime)
    quote = MarketDataProvider(ticker_factory=lambda symbol: FakeTicker()).quote("RCL")
    assert quote["market_state"] == "REGULAR"
    assert quote["price"] == 224.27
    assert quote["previous_close"] == 234.89
    assert quote["change_percent"] == pytest.approx((224.27 - 234.89) / 234.89 * 100)


def test_regular_change_uses_minute_close_when_official_previous_close_differs(monkeypatch):
    """现货卡片与趋势通道统一使用已识别的正常盘收盘，不保留滞后的上游昨收。"""
    eastern = ZoneInfo("America/New_York")
    minutes = pd.DataFrame(
        {"Close": [234.83, 224.27]},
        index=pd.DatetimeIndex([
            "2026-09-22 15:59",
            "2026-09-23 10:30",
        ]).tz_localize(eastern),
    )

    class FakeTicker:
        fast_info = {"last_price": 224.27, "previous_close": 234.89, "currency": "USD"}

        def history(self, **kwargs):
            assert kwargs.get("interval") == "1m"
            return minutes

    class FrozenDateTime(datetime):
        @classmethod
        def now(cls, tz=None):
            current = datetime(2026, 9, 23, 10, 30, tzinfo=eastern)
            if tz is None:
                return current.replace(tzinfo=None)
            return current.astimezone(tz)

    monkeypatch.setattr(market, "datetime", FrozenDateTime)
    quote = MarketDataProvider(ticker_factory=lambda symbol: FakeTicker()).quote("RCL")
    assert quote["market_state"] == "REGULAR"
    assert quote["previous_close"] == pytest.approx(234.83)
    assert quote["change_percent"] == pytest.approx((224.27 - 234.83) / 234.83 * 100)
    # 分钟线只有收盘价时，不能用收盘价冒充今开。
    assert quote["today_open"] is None



def test_regular_session_open_uses_first_regular_bar():
    """今开取最近一个已经开始的盘中交易日的第一根分钟线，不取盘前价，也不用收盘价顶替。"""
    eastern = ZoneInfo("America/New_York")
    frame = pd.DataFrame(
        {
            "Open": [137.32, 137.50, 138.26, 138.27, 138.40],
            "Close": [137.40, 139.54, 138.26, 138.40, 138.50],
        },
        index=pd.DatetimeIndex([
            "2026-09-24 09:30",
            "2026-09-24 09:31",
            "2026-09-25 09:29",
            "2026-09-25 09:30",
            "2026-09-25 09:31",
        ]).tz_localize(eastern),
    )
    opened = datetime(2026, 9, 25, 10, 0, tzinfo=eastern)
    assert regular_session_open(frame, now=opened) == pytest.approx(138.27)
    # 当天盘中还没开始时，回退到前一个已经开过盘的交易日，而不是盘前最后一笔。
    assert regular_session_open(frame, now=datetime(2026, 9, 25, 9, 20, tzinfo=eastern)) == pytest.approx(137.32)
    assert regular_session_open(frame.drop(columns=["Open"]), now=opened) is None


def test_quote_uses_regular_minute_open_after_the_bell(monkeypatch):
    """盘中用第一根盘中分钟线覆盖错误的 fast_info.open；盘前不覆盖。"""
    eastern = ZoneInfo("America/New_York")

    def frame(stamps, opens, closes):
        return pd.DataFrame(
            {"Open": opens, "Close": closes},
            index=pd.DatetimeIndex(stamps).tz_localize(eastern),
        )

    def run(moment, minutes):
        class FakeTicker:
            fast_info = {"last_price": 140.11, "previous_close": 139.54, "open": 136.0, "currency": "USD"}

            def history(self, **kwargs):
                assert kwargs.get("interval") == "1m"
                return minutes

        class FrozenDateTime(datetime):
            @classmethod
            def now(cls, tz=None):
                if tz is None:
                    return moment.replace(tzinfo=None)
                return moment.astimezone(tz)

        monkeypatch.setattr(market, "datetime", FrozenDateTime)
        return MarketDataProvider(ticker_factory=lambda symbol: FakeTicker()).quote("ORCL")

    pre = run(
        datetime(2026, 9, 25, 9, 20, tzinfo=eastern),
        frame(
            ["2026-09-24 09:30", "2026-09-24 15:59", "2026-09-25 09:29"],
            [137.32, 139.20, 138.26],
            [137.40, 139.54, 138.26],
        ),
    )
    assert pre["market_state"] == "PRE"
    assert pre["today_open"] == pytest.approx(136.0)

    regular = run(
        datetime(2026, 9, 25, 10, 0, tzinfo=eastern),
        frame(
            ["2026-09-24 09:30", "2026-09-24 15:59", "2026-09-25 09:29", "2026-09-25 09:30", "2026-09-25 10:00"],
            [137.32, 139.20, 138.26, 138.27, 140.00],
            [137.40, 139.54, 138.26, 138.40, 140.11],
        ),
    )
    assert regular["market_state"] == "REGULAR"
    assert regular["today_open"] == pytest.approx(138.27)
    assert regular["price"] == pytest.approx(140.11)


def test_post_change_uses_prior_session_when_official_close_skips_a_day(monkeypatch):
    """盘后涨跌基准取最近完成的正常盘收盘，不接受落后一个交易日的官方字段。"""
    eastern = ZoneInfo("America/New_York")
    minutes = pd.DataFrame(
        {"Close": [234.89, 224.00, 223.00]},
        index=pd.DatetimeIndex([
            "2026-09-22 15:59",
            "2026-09-23 15:59",
            "2026-09-23 18:00",
        ]).tz_localize(eastern),
    )

    class FakeTicker:
        fast_info = {"last_price": 223.00, "previous_close": 250.25, "currency": "USD"}

        def history(self, **kwargs):
            assert kwargs.get("interval") == "1m"
            return minutes

    class FrozenDateTime(datetime):
        @classmethod
        def now(cls, tz=None):
            current = datetime(2026, 9, 23, 18, 0, tzinfo=eastern)
            if tz is None:
                return current.replace(tzinfo=None)
            return current.astimezone(tz)

    monkeypatch.setattr(market, "datetime", FrozenDateTime)
    quote = MarketDataProvider(ticker_factory=lambda symbol: FakeTicker()).quote("RCL")
    assert quote["market_state"] == "POST"
    assert quote["previous_close"] == pytest.approx(234.89)
    assert quote["change_percent"] == pytest.approx((223.00 - 234.89) / 234.89 * 100)


def test_regular_quote_keeps_latest_completed_close_when_minute_data_lags(monkeypatch):
    """当前盘中分钟线暂缺时，不能把盘后/旧 previous_close 当成昨收。"""
    eastern = ZoneInfo("America/New_York")
    minutes = pd.DataFrame(
        {"Close": [140.00, 137.79, 138.91]},
        index=pd.DatetimeIndex([
            "2026-09-29 15:59",
            "2026-09-30 15:59",
            "2026-09-30 18:00",
        ]).tz_localize(eastern),
    )

    class FakeTicker:
        # 上游 previous_close 错把盘后价返回为 138.91。
        fast_info = {"last_price": 137.53, "previous_close": 138.91, "currency": "USD"}

        def history(self, **kwargs):
            assert kwargs.get("interval") == "1m"
            return minutes

    class FrozenDateTime(datetime):
        @classmethod
        def now(cls, tz=None):
            current = datetime(2026, 10, 1, 10, 0, tzinfo=eastern)
            if tz is None:
                return current.replace(tzinfo=None)
            return current.astimezone(tz)

    monkeypatch.setattr(market, "datetime", FrozenDateTime)
    quote = MarketDataProvider(ticker_factory=lambda symbol: FakeTicker()).quote("ORCL")
    assert quote["market_state"] == "REGULAR"
    assert quote["previous_close"] == pytest.approx(137.79)
    assert quote["change_percent"] == pytest.approx((137.53 - 137.79) / 137.79 * 100)
    assert quote["sessions"]["post"]["reference_close"] == pytest.approx(137.79)


def test_overnight_fallback_uses_previous_trading_day_when_price_is_last_close(monkeypatch):
    """未配置夜盘源时，当前价若是昨天收盘，应相对上一个交易日收盘计算。"""
    eastern = ZoneInfo("America/New_York")
    frame = pd.DataFrame(
        {"Close": [137.085, 132.63, 132.60]},
        index=pd.DatetimeIndex([
            "2026-09-25 15:59",
            "2026-09-28 15:59",
            "2026-09-28 19:59",
        ]).tz_localize(eastern),
    )

    class FakeTicker:
        fast_info = {"last_price": 132.60, "previous_close": 132.63, "currency": "USD"}

        def history(self, **kwargs):
            return frame

    class FrozenDateTime(datetime):
        @classmethod
        def now(cls, tz=None):
            current = datetime(2026, 9, 29, 3, 0, tzinfo=eastern)
            return current if tz is not None else current.replace(tzinfo=None)

    monkeypatch.setattr(market, "datetime", FrozenDateTime)
    quote = MarketDataProvider(ticker_factory=lambda symbol: FakeTicker()).quote("ORCL")
    assert quote["market_state"] == "OVERNIGHT"
    assert quote["previous_close"] == pytest.approx(137.085)
    assert quote["change_percent"] == pytest.approx((132.60 - 137.085) / 137.085 * 100)


def test_pre_session_uses_formal_previous_close_over_1559_minute_bar(monkeypatch):
    """盘前用正式昨收 132.60，不把 15:59 分钟线 132.63 当成昨收。"""
    eastern = ZoneInfo("America/New_York")
    frame = pd.DataFrame(
        {"Close": [137.10, 132.63, 133.06]},
        index=pd.DatetimeIndex([
            "2026-09-25 15:59",
            "2026-09-28 15:59",
            "2026-09-29 04:08",
        ]).tz_localize(eastern),
    )

    class FakeTicker:
        fast_info = {"last_price": 132.60, "previous_close": 132.63, "currency": "USD"}

        def history(self, **kwargs):
            return frame

    class FrozenDateTime(datetime):
        @classmethod
        def now(cls, tz=None):
            current = datetime(2026, 9, 29, 8, 10, tzinfo=eastern)
            return current if tz is not None else current.replace(tzinfo=None)

    monkeypatch.setattr(market, "datetime", FrozenDateTime)
    quote = MarketDataProvider(ticker_factory=lambda symbol: FakeTicker()).quote("ORCL")
    assert quote["market_state"] == "PRE"
    assert quote["previous_close"] == pytest.approx(132.60)
    assert quote["sessions"]["pre"]["reference_close"] == pytest.approx(132.60)
    assert quote["sessions"]["pre"]["change_percent"] == pytest.approx((133.06 - 132.60) / 132.60 * 100)


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


def test_new_quote_snapshot_preserves_previous_fair_value(tmp_path: Path):
    """行情快照缺少后台估值时，不得覆盖上一份有效估值。"""
    database = Database(tmp_path / "options.db")
    first = sample_quote("AAPL") | {
        "fair_value": 210.0,
        "fair_value_low": 180.0,
        "fair_value_high": 240.0,
        "fair_value_source": "valuation_v26",
        "fair_value_defensive": {"low": 180.0, "high": 220.0},
        "fair_value_optimistic": {"low": 200.0, "high": 240.0},
    }
    database.write_snapshot(first, sample_rows(), iso(utc_now() - timedelta(minutes=2)))
    database.write_snapshot(sample_quote("AAPL"), sample_rows(), iso())
    latest = database.latest_quote("AAPL")
    assert latest["fair_value"] == 210.0
    assert latest["fair_value_source"] == "valuation_v26"
    assert latest["fair_value_defensive_json"]
    assert latest["fair_value_optimistic_json"]


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


def test_quote_and_levels_align_cached_previous_close_with_fresh_history(tmp_path: Path, monkeypatch):
    """首屏旧行情快照与新日线并存时，quote 和 levels 必须使用同一昨收。"""
    database = Database(tmp_path / "options.db")
    monkeypatch.setattr(api_module, "market_today", lambda: date(2026, 9, 30))
    quote_fetched_at = iso(utc_now() - timedelta(minutes=2))
    history_fetched_at = iso(utc_now() - timedelta(minutes=1))
    database.write_snapshot(
        {
            **sample_quote("ORCL"),
            "price": 137.30,
            "previous_close": 137.90,
            "change_percent": (137.30 - 137.90) / 137.90 * 100,
        },
        sample_rows("ORCL", "2026-10-02"),
        quote_fetched_at,
    )
    database.write_history(
        "ORCL",
        [
            {"date": "2026-09-29", "open": 132.79, "high": 139.0, "low": 132.0, "close": 137.79, "volume": 1000},
            {"date": "2026-09-30", "open": 136.54, "high": 138.0, "low": 136.0, "close": 137.30, "volume": 1100},
        ],
        history_fetched_at,
    )
    settings = Settings(
        database_path=tmp_path / "options.db",
        proxy_url=None,
        default_symbols=("ORCL",),
        raw_retention_days=30,
        scheduler_enabled=False,
    )
    provider = FakeProvider()
    test_app = FastAPI()
    test_app.include_router(create_router(database, SnapshotService(database, provider), provider, settings))
    with TestClient(test_app) as client:
        quote = client.get("/api/quote/ORCL").json()
        levels = client.get(
            "/api/levels/ORCL",
            params={"expiration": "2026-10-02", "spot": 137.30, "raw": "true"},
        ).json()
    assert quote["previous_close"] == pytest.approx(137.79)
    assert quote["change_percent"] == pytest.approx((137.30 - 137.79) / 137.79 * 100)
    assert levels["candidate_spot"] == pytest.approx(137.79)
    assert levels["trend_market"]["previous_close"] == pytest.approx(137.79)


def test_quote_fetches_missing_history_before_returning_cached_previous_close(tmp_path: Path, monkeypatch):
    """新标的首屏没有日线缓存时，quote 先补齐历史再返回趋势口径的昨收。"""
    database = Database(tmp_path / "options.db")
    monkeypatch.setattr(api_module, "market_today", lambda: date(2026, 9, 30))
    database.write_snapshot(
        {
            **sample_quote("ORCL"),
            "price": 137.30,
            "previous_close": 137.90,
            "change_percent": (137.30 - 137.90) / 137.90 * 100,
        },
        sample_rows("ORCL", "2026-10-02"),
        iso(utc_now() - timedelta(minutes=1)),
    )

    class HistoryProvider(FakeProvider):
        def history(self, symbol: str, period: str = "6mo") -> list[dict]:
            return [
                {"date": "2026-09-29", "open": 132.79, "high": 139.0, "low": 132.0, "close": 137.79, "volume": 1000},
                {"date": "2026-09-30", "open": 136.54, "high": 138.0, "low": 136.0, "close": 137.30, "volume": 1100},
            ]

    settings = Settings(
        database_path=tmp_path / "options.db",
        proxy_url=None,
        default_symbols=("ORCL",),
        raw_retention_days=30,
        scheduler_enabled=False,
    )
    provider = HistoryProvider()
    test_app = FastAPI()
    test_app.include_router(create_router(database, SnapshotService(database, provider), provider, settings))
    with TestClient(test_app) as client:
        quote = client.get("/api/quote/ORCL").json()
    assert quote["previous_close"] == pytest.approx(137.79)
    assert quote["change_percent"] == pytest.approx((137.30 - 137.79) / 137.79 * 100)
    assert database.latest_history("ORCL")["bars"][-1]["close"] == pytest.approx(137.30)


def test_snapshot_stores_extended_hours_and_api_exposes_them(tmp_path: Path):
    """行情快照落库时保存盘前/盘后，读取层把 JSON 解析成 sessions 对象下发。"""
    database = Database(tmp_path / "options.db")
    quote = sample_quote()
    quote["sessions"] = {"pre": {"price": 201.5, "change_percent": 0.5, "as_of": "2026-09-17T08:05:00-04:00"}}
    database.write_snapshot(quote, sample_rows(), iso())
    assert parse_sessions(database.latest_quote("AAPL")["sessions_json"])["pre"]["price"] == 201.5
    settings = Settings(database_path=tmp_path / "options.db", proxy_url=None, default_symbols=("AAPL",), raw_retention_days=30, scheduler_enabled=False)
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
    settings = Settings(database_path=tmp_path / "options.db", proxy_url=None, default_symbols=("AAPL",), raw_retention_days=30, scheduler_enabled=False)
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


def test_quote_triggers_missing_fair_value_without_refreshing_snapshot(tmp_path: Path):
    """已有行情快照但估值为空时，轻量 quote 请求必须启动估值任务并合并结果。"""
    database = Database(tmp_path / "options.db")
    database.write_snapshot(sample_quote("ONDS"), sample_rows("ONDS"), iso())

    class FairValueProvider(FakeProvider):
        def __init__(self):
            self.ensure_calls = 0

        def ensure_fair_value(self, symbol: str) -> dict:
            self.ensure_calls += 1
            return {
                "value": 12.0,
                "low": 10.0,
                "high": 14.0,
                "source": "valuation_v26",
                "status": "ready",
                "defensive": {"low": 10.0, "high": 12.0},
                "optimistic": {"low": 12.0, "high": 14.0},
            }

    provider = FairValueProvider()
    settings = Settings(database_path=tmp_path / "options.db", proxy_url=None, default_symbols=("ONDS",), raw_retention_days=30, scheduler_enabled=False)
    test_app = FastAPI()
    test_app.include_router(create_router(database, SnapshotService(database, provider), provider, settings))
    with TestClient(test_app) as client:
        payload = client.get("/api/quote/ONDS").json()
    assert provider.ensure_calls == 1
    assert payload["fair_value"] == 12.0
    assert payload["fair_value_status"] == "ready"
    assert payload["fair_value_pending"] is False


def test_quote_reports_unavailable_fair_value_instead_of_waiting_forever(tmp_path: Path):
    """没有有效估值候选时，API 应返回终态状态，前端不再无限显示等待。"""
    database = Database(tmp_path / "options.db")
    database.write_snapshot(sample_quote("ONDS"), sample_rows("ONDS"), iso())

    class UnavailableProvider(FakeProvider):
        def ensure_fair_value(self, symbol: str) -> dict:
            return {"value": None, "source": None, "status": "unavailable", "warning": "公开财务数据不足"}

    provider = UnavailableProvider()
    settings = Settings(database_path=tmp_path / "options.db", proxy_url=None, default_symbols=("ONDS",), raw_retention_days=30, scheduler_enabled=False)
    test_app = FastAPI()
    test_app.include_router(create_router(database, SnapshotService(database, provider), provider, settings))
    with TestClient(test_app) as client:
        payload = client.get("/api/quote/ONDS").json()
    assert payload["fair_value_status"] == "unavailable"
    assert payload["fair_value_warning"] == "公开财务数据不足"
    assert payload["fair_value_pending"] is False


def test_quote_keeps_rate_limited_fair_value_pending(tmp_path: Path):
    """临时限流状态要继续让前端轮询，而不是被 API 标成终态。"""
    database = Database(tmp_path / "options.db")
    database.write_snapshot(sample_quote("ONDS"), sample_rows("ONDS"), iso())

    class RetryProvider(FakeProvider):
        def ensure_fair_value(self, symbol: str) -> dict:
            return {"value": None, "source": None, "status": "retry", "warning": "Yahoo 数据源暂时限流"}

    provider = RetryProvider()
    settings = Settings(database_path=tmp_path / "options.db", proxy_url=None, default_symbols=("ONDS",), raw_retention_days=30, scheduler_enabled=False)
    test_app = FastAPI()
    test_app.include_router(create_router(database, SnapshotService(database, provider), provider, settings))
    with TestClient(test_app) as client:
        payload = client.get("/api/quote/ONDS").json()
    assert payload["fair_value_status"] == "retry"
    assert payload["fair_value_pending"] is True


def test_fair_value_process_cache_uses_versioned_shared_key():
    """进程内估值缓存必须和 SQLite 共享缓存使用同一个版本化键。"""
    provider = MarketDataProvider(ticker_factory=lambda symbol: SimpleNamespace(info={}))
    key = f"fair-value:{market.VALUATION_CONFIG['cache_version']}:TSLA"
    provider._fair_value_cache[key] = (time.monotonic(), {"value": 123.0, "source": market.FAIR_VALUE_SOURCE})
    result = provider._fair_value("TSLA", SimpleNamespace(info={}))
    assert result["value"] == 123.0
    assert "TSLA" not in provider._fair_value_cache


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
    assert "排序按触及目标的概率加权，预计盈亏按到达目标价估算" in page
    assert 'key: `${item.kind || "structure"}-${item.direction || "unknown"}-${item.expiration || "unknown"}-${strikes || "unknown"}-${index}`' in source
    assert ".buyer-structure-scenario{font-weight:600}" in styles
    assert "margin:7px 0 14px" in styles
    assert "background:var(--table-head)" in styles


def test_buyer_structure_title_drops_only_trailing_period():
    source = Path("app/static/common/js/app.js").read_text(encoding="utf-8")
    assert 'title: title.replace(/。$/, "")' in source


def test_quote_price_follows_current_session():
    """现货现价按时段动态取值：盘前/盘后取时段价，夜盘取正式收盘价；不再单列时段价格卡。"""
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
    assert 'if (marketState === "POST" && sessions.post?.price != null) return sessions.post;' in source
    assert 'if (marketState === "OVERNIGHT") {' in source
    assert 'sessions.overnight?.provider !== "alpaca-overnight"' in source
    assert "const active = activeSessionQuote(quote);" in source
    assert "const price = active?.price ?? quote?.price;" in source
    assert "const reference = quoteReference(quote);" in source
    assert "const referencePrice = finitePrice(reference.price);" in source
    assert "(Number(price) - referencePrice) / referencePrice * 100" in source
    # 时段标签映射与顶栏时段展示保留。
    assert 'const MARKET_STATE_LABELS = { PRE: "盘前", REGULAR: "正常交易", POST: "盘后", OVERNIGHT: "夜盘", CLOSED: "休市" };' in source
    assert 'state.view.marketState = marketStateLabel(quote?.market_state, "快照数据");' in source
    assert "function quoteMarketLabel(quote)" in source
    assert 'if (quote?.sessions?.overnight?.provider === "alpaca-overnight") return "夜盘价";' in source
    assert 'return state.levelBasisMode === "close" ? "盘后" : "收盘";' in source
    assert "state.view.quoteMarket = quoteMarketLabel(quote);" in source
    assert "if (state.lastQuote) renderQuote(state.lastQuote);" in source


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
    # 正文规则走主题令牌；允许后续主题分组继续声明颜色变量。
    body = "\n".join(line for index, line in enumerate(styles.splitlines(), 1) if index not in (2, 3))
    assert "--flow-body:#" in body
    # 不再禁止所有十六进制值：交互态等局部规则可以直接使用白色，主题颜色仍集中在变量声明中。
    assert "--flow-body:#" in body and "--flow-surface:#" in body


def test_chain_table_shows_bid_ask_without_contract_column():
    """期权链表格：类型并入行权价，并展示当前买价与卖价。"""
    html = Path("app/static/index.html").read_text(encoding="utf-8")
    source = Path("app/static/common/js/app.js").read_text(encoding="utf-8")
    styles = Path("app/static/common/css/styles.css").read_text(encoding="utf-8")
    # 表头去掉「合约」与「类型」（类型改由行权价文字颜色表达），并加入买价/卖价，共 9 列。
    assert "<th>合约</th>" not in html
    assert "<th>类型</th>" not in html
    assert '<th class="num">行权价</th><th class="num">成交量</th><th class="num">未平仓</th><th class="num">买价</th><th class="num">卖价</th>' in html
    assert html.count("<th ") + html.count("<th>") == 9
    # 期权链表格展示买卖价，不额外展示最新价；期权流向区域可以使用「接近买价/卖价」说明。
    table_start = html.index("<table><thead>")
    table_end = html.index("</table>", table_start) + len("</table>")
    chain_template = html[table_start:table_end]
    assert "最新价" not in chain_template and "买价" in chain_template and "卖价" in chain_template
    assert "bid: formatOptionQuote(row.bid)" in source and "ask: formatOptionQuote(row.ask)" in source
    assert "{{ row.bid }}" in source and "{{ row.ask }}" in source
    assert 'function formatOptionQuote(value)' in source
    # 数据行不再渲染合约代码列。
    assert 'key: row.contract_symbol ||' in source
    # 空态 colspan 跟期权链组件模板走，表头仍留在页面里，列数保持 9。
    assert 'colspan="9"' not in html
    assert 'colspan="9"' in source
    assert source.count('colspan="9"') == 1
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
    assert ':style="row.volumeStyle"' in source and ':style="row.interestStyle"' in source
    assert ':style="row.volumeStyle"' not in html and ':style="row.interestStyle"' not in html
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


def test_chain_panel_collapses_by_default():
    """期权链面板：表格与图例默认折叠；标题行（标的 · 到期日）与右侧状态栏始终可见。"""
    page = Path("app/static/index.html").read_text(encoding="utf-8")
    source = Path("app/static/common/js/app.js").read_text(encoding="utf-8")
    styles = Path("app/static/common/css/styles.css").read_text(encoding="utf-8")
    # 折叠按钮挂在标题行左侧（caret + 展开/收起），受控内容是包住工具栏、表格与图例的 chain-fold
    assert 'id="chain-toggle" type="button" aria-expanded="false" aria-controls="chain-fold"' in page
    assert 'id="chain-action"' in page
    assert 'id="chain-fold" hidden' in page
    fold = page[page.index('id="chain-fold"') : page.index('</section>', page.index('id="chain-fold"'))]
    assert 'class="chain-toolbar"' in fold and 'class="table-wrap"' in fold and 'id="chain-heat-note"' in fold
    # 报错提示不跟着折叠：出错时即使面板收起也要看得见
    assert page.index('id="error-box"') < page.index('id="chain-fold"')
    # 默认折叠 + 状态记 sessionStorage（同一标签页换标的、跳 URL 不必重复折叠）
    assert 'const CHAIN_KEY = "option-scope-chain";' in source
    # 折叠逻辑走通用实现：与「分析详情」「图表」共用同一份 bindFoldGroup
    assert "function bindFoldGroup({ headerId, toggleId, bodyId, actionId, storageKey, defaultExpanded, onChange, shouldIgnore })" in source
    assert 'bodyId: "chain-fold",' in source
    assert 'storageKey: CHAIN_KEY,' in source
    # 期权链组默认折叠（注意：图表组仍是 true，这里靠 bodyId 定位到本组）
    chain_group = source[source.index("function initChainGroup()") : source.index("function initChartGroup()")]
    assert "defaultExpanded: false," in chain_group
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

    settings = Settings(database_path=tmp_path / "options.db", proxy_url=None, default_symbols=("AAPL",), raw_retention_days=30, scheduler_enabled=False)
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

    settings = Settings(database_path=tmp_path / "options.db", proxy_url=None, default_symbols=("AAPL",), raw_retention_days=30, scheduler_enabled=False)
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

    settings = Settings(database_path=tmp_path / "options.db", proxy_url=None, default_symbols=("AAPL",), raw_retention_days=30, scheduler_enabled=False)
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

    settings = Settings(database_path=tmp_path / "options.db", proxy_url=None, default_symbols=("AAPL",), raw_retention_days=30, scheduler_enabled=False)
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
    settings = Settings(database_path=tmp_path / "options.db", proxy_url=None, default_symbols=("AAPL",), raw_retention_days=30, scheduler_enabled=False)
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


def test_stale_analysis_job_can_be_reclaimed(tmp_path: Path):
    """running 超过 180 秒后允许重新领取，未过期的任务仍然只有一个 worker。"""
    database = Database(tmp_path / "jobs.db")
    claimed = database.claim_analysis_job("ORCL", 45)
    assert claimed["claimed"] is True
    held = database.claim_analysis_job("ORCL", 45)
    assert held["claimed"] is False
    assert held["status"] == "running"
    stale_at = (utc_now() - timedelta(seconds=181)).isoformat()
    with database.connect() as connection:
        connection.execute(
            "UPDATE analysis_refresh_jobs SET started_at=? WHERE job_id=?",
            (stale_at, claimed["job_id"]),
        )
    reclaimed = database.claim_analysis_job("ORCL", 45)
    assert reclaimed["claimed"] is True
    assert reclaimed["started_at"] != stale_at


def test_orphaned_analysis_job_is_marked_failed(tmp_path: Path):
    """重启清理把遗留的 running 记成失败，旧线程不能再把它改回完成。"""
    database = Database(tmp_path / "jobs.db")
    claimed = database.claim_analysis_job("ORCL", 45)
    assert database.fail_orphaned_analysis_jobs() == 1
    job = database.analysis_job("ORCL", 45)
    assert job["status"] == "failed"
    assert job["error_message"] == "进程重启，后台分析已中断"
    assert job["finished_at"]
    assert database.fail_orphaned_analysis_jobs() == 0
    assert database.finish_analysis_job(claimed["job_id"], "completed", {"ok": True}, None, claimed["started_at"]) is False
    assert database.analysis_job("ORCL", 45)["status"] == "failed"


def test_finish_analysis_job_keeps_the_newer_attempt(tmp_path: Path):
    database = Database(tmp_path / "jobs.db")
    first = database.claim_analysis_job("ORCL", 45)
    stale_at = (utc_now() - timedelta(seconds=181)).isoformat()
    with database.connect() as connection:
        connection.execute(
            "UPDATE analysis_refresh_jobs SET started_at=? WHERE job_id=?",
            (stale_at, first["job_id"]),
        )
    second = database.claim_analysis_job("ORCL", 45)
    assert second["claimed"] is True
    assert database.finish_analysis_job(first["job_id"], "failed", None, "late", stale_at) is False
    current = database.analysis_job("ORCL", 45)
    assert current["status"] == "running"
    assert current["started_at"] == second["started_at"]


def test_gamma_refresh_returns_before_window_and_times_out(tmp_path: Path, monkeypatch: pytest.MonkeyPatch):
    """状态接口不能等窗口算完；超时只失败这一轮，晚到的完成不能翻案。"""
    monkeypatch.setattr(api_module, "GAMMA_JOB_TIMEOUT_SECONDS", 0.05)
    database = Database(tmp_path / "options.db")
    service = SnapshotService(database, FakeProvider())
    entered = threading.Event()
    release = threading.Event()

    def slow_window(symbol: str, horizon_days: int = 45) -> dict:
        entered.set()
        release.wait(2)
        return {"symbol": symbol, "horizon_days": horizon_days, "expirations": [], "results": [], "errors": []}

    monkeypatch.setattr(service, "refresh_window", slow_window)
    settings = Settings(
        database_path=tmp_path / "options.db", proxy_url=None, default_symbols=("AAPL",),
        raw_retention_days=30, scheduler_enabled=False,
    )
    router = create_router(database, service, FakeProvider(), settings)
    test_app = FastAPI()
    test_app.include_router(router)
    with TestClient(test_app) as client:
        started = time.monotonic()
        payload = client.get("/api/gamma/AAPL", params={"horizon_days": 45, "refresh": True, "status_only": True}).json()
        elapsed = time.monotonic() - started
    assert elapsed < 1
    assert payload["status_only"] is True
    assert payload["refresh"]["claimed"] is True
    assert payload["refresh"]["status"] == "running"
    assert entered.wait(1)
    deadline = time.monotonic() + 2
    job = None
    while time.monotonic() < deadline:
        job = database.analysis_job("AAPL", 45)
        if job and job["status"] == "failed":
            break
        time.sleep(0.02)
    assert job is not None
    assert job["status"] == "failed"
    assert job["error_message"] == "分析超时，已停止本轮计算"
    release.set()
    time.sleep(0.1)
    assert database.analysis_job("AAPL", 45)["status"] == "failed"
    assert database.analysis_job("AAPL", 45)["error_message"] == "分析超时，已停止本轮计算"


def test_gamma_status_only_skips_rows_until_display_request(tmp_path: Path):
    """Gamma 轮询只回任务状态；展示请求可以不要合约行，默认响应仍保留完整行。"""
    database = Database(tmp_path / "options.db")
    service = SnapshotService(database, FakeProvider())
    service.refresh("AAPL", "2026-12-18")
    settings = Settings(database_path=tmp_path / "options.db", proxy_url=None, default_symbols=("AAPL",), raw_retention_days=30, scheduler_enabled=False)
    router = create_router(database, service, FakeProvider(), settings)
    test_app = FastAPI()
    test_app.include_router(router)
    with TestClient(test_app) as client:
        status = client.get("/api/gamma/AAPL", params={"horizon_days": 365, "status_only": True}).json()
        assert status["status_only"] is True
        assert status["data"] == []
        assert status["contract_count"] == 0
        assert "iv_model" not in status
        slim = client.get("/api/gamma/AAPL", params={"horizon_days": 365, "include_rows": False}).json()
        assert slim["status_only"] is False
        assert slim["data"] == []
        assert slim["contract_count"] == 2
        assert slim["iv_model"]["2026-12-18"]["iv"] > 0
        assert "zero_gamma" in slim
        full = client.get("/api/gamma/AAPL", params={"horizon_days": 365}).json()
        assert full["status_only"] is False
        assert len(full["data"]) == full["contract_count"] == 2
        assert full["data"][0]["model_iv"] > 0


def test_frontend_skips_repeated_chain_and_gamma_work():
    """前端不再为轮询下载整窗合约，价位键按格子去重，期权链表格独立成组件。"""
    source = Path("app/static/common/js/app.js").read_text(encoding="utf-8")
    html = Path("app/static/index.html").read_text(encoding="utf-8")
    assert "status_only=true" in source
    assert "include_rows=false" in source
    assert "function spotCacheBucket(value)" in source
    assert "function gammaProfileReady(analysis)" in source
    assert 'Vue.component("chain-table-body"' in source
    assert 'is="chain-table-body"' in html
    assert "function gammaAtSpot(spot, row, expiration)" in source
    assert "scheduleScopeCharts" in source


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

    settings = Settings(database_path=tmp_path / "options.db", proxy_url=None, default_symbols=("AAPL",), raw_retention_days=30, scheduler_enabled=False)
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
    settings = Settings(database_path=tmp_path / "options.db", proxy_url=None, default_symbols=("AAPL",), raw_retention_days=30, scheduler_enabled=False)
    service = SnapshotService(database, FakeProvider())
    assert service.recent_snapshot("AAPL", "2026-12-18", 60) is None
    stale = service.refresh("AAPL", "2026-12-18", max_age_seconds=60)
    assert stale.get("skipped") is None
    assert stale["rows"] == len(sample_rows())
    fresh = service.refresh("AAPL", "2026-12-18", max_age_seconds=60)
    assert fresh["skipped"] is True


def test_manual_refresh_forces_provider_when_snapshot_is_fresh(tmp_path: Path):
    """API 仍支持 max_age=0 强制回源，尽管页面不再展示手动刷新按钮。"""
    database = Database(tmp_path / "options.db")
    database.write_snapshot(sample_quote(), sample_rows(), iso())
    calls = {"fetch": 0}

    class CountingProvider(FakeProvider):
        def fetch(self, symbol: str, expiration: str):
            calls["fetch"] += 1
            return super().fetch(symbol, expiration)

    service = SnapshotService(database, CountingProvider())
    result = service.refresh("AAPL", "2026-12-18", max_age_seconds=0)

    assert result.get("skipped") is None
    assert calls["fetch"] == 1


def test_buyer_structures_compare_both_directions_when_signal_is_neutral():
    """中性趋势不应清空首屏结构分析，仍展示看涨和看跌的对比候选。"""
    result = build_buyer_structures(
        sample_option_rows(200.5),
        200.5,
        {"direction": "range", "lower": 195.0, "upper": 205.0},
        {"action": "hold", "label": "继续持有", "reason": "信号未形成共振"},
        [{"price": 195.0}],
        [{"price": 205.0}],
    )

    assert result["available"] is True
    assert result["direction"] == "neutral"
    assert result["primary_direction"] is None
    assert "中性对比" in result["direction_label"]
    assert {item["direction"] for item in result["items"]} == {"call", "put"}
    assert not any(item["is_primary"] for item in result["items"])


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


def test_refresh_different_expirations_are_not_blocked_by_one_symbol_lock(tmp_path: Path):
    """Gamma 窗口正在刷新别的到期日时，当前期限仍要能同时回源。"""
    database = Database(tmp_path / "options.db")
    entered = threading.Barrier(2)
    calls = {"fetch": 0}
    calls_lock = threading.Lock()

    class SlowProvider(FakeProvider):
        def fetch(self, symbol: str, expiration: str):
            entered.wait(timeout=2)
            with calls_lock:
                calls["fetch"] += 1
            time.sleep(0.2)
            return super().fetch(symbol, expiration)

    service = SnapshotService(database, SlowProvider())
    started = time.monotonic()
    with ThreadPoolExecutor(max_workers=2) as executor:
        results = list(executor.map(
            lambda expiration: service.refresh("AAPL", expiration, max_age_seconds=0),
            ("2026-12-18", "2027-01-15"),
        ))
    assert calls["fetch"] == 2
    assert not any(result.get("coalesced") for result in results)
    assert time.monotonic() - started < 0.45


def test_sse_refresh_loads_cached_snapshots_without_manual_refresh_button():
    source = Path("app/static/common/js/app.js").read_text(encoding="utf-8")
    # 快照更新由服务端 SSE 调度；页面不再暴露手动刷新按钮。
    assert 'id="refresh-button"' not in Path("app/static/index.html").read_text(encoding="utf-8")
    assert "refreshNow()" not in source
    assert "function isSnapshotFresh(snapshot)" in source
    assert "function quoteIsReady(quote)" in source
    assert "function hasValidOptionQuotes(rows)" in source
    assert "snapshot?.optionsQuotesReady" in source
    assert "optionsQuotesReady: hasValidOptionQuotes(payload.data)" in source
    assert "snapshot?.shown && snapshot?.quoteReady" in source
    assert "age !== null && age < SNAPSHOT_FRESH_SECONDS" in source
    assert "if (isSnapshotFresh(snapshot)) {" in source
    assert "showFreshStatus(snapshot);" in source
    assert "async function refreshInBackground(loadId)" in source
    assert 'const params = new URLSearchParams({ max_age: String(SNAPSHOT_FRESH_SECONDS) });' in source
    assert "if (refreshResult?.skipped)" in source
    assert "const snapshot = await renderSnapshot(loadId, refreshResult.quote || null);" in source
    assert "let resolvedQuote = quoteIsReady(quote) ? quote : refreshResult?.quote;" in source
    assert "?refresh=true`" in source
    page = Path("app/static/index.html").read_text(encoding="utf-8")
    assert 'id="refresh-button"' not in page
    assert "刷新快照" not in page
    assert "function scheduleAutoRefresh()" in source
    assert 'source.addEventListener("update", onUpdate);' in source
    assert 'symbol === state.symbol && !document.hidden && !state.loading && !state.refreshInFlight' in source
    assert "Automatic snapshot refresh is server-side and active only while this SSE subscription exists." in source
    assert "if (!state.loading && !state.refreshInFlight) loadChain({ loadId: state.loadId });" in source
    assert "state.pushRefreshTimer = setTimeout" in source
    # Auto-refresh is scheduled by the server for the lifetime of the SSE subscription; no browser timer remains.
    assert "SSE 生命周期驱动服务端市场时段调度" in source
    assert "function armRefreshAnchor(fetchedAt)" not in source
    assert "refreshDeadline" not in source
    page = Path("app/static/index.html").read_text(encoding="utf-8")
    assert 'id="refresh-note"' not in page
    assert "分析计算中，刷新稍后开始" not in source
    assert "正在刷新…" not in source
    # 跨期限 Gamma 窗口刷新改为后台任务，表格渲染完成后不再等待窗口。
    assert "function refreshAnalysisWindow(loadId, payload, quote)" in source
    assert "refreshAnalysisWindow(loadId, payload, resolvedQuote);" in source
    assert "async function latestSelectedChain(loadId, symbol, fallbackPayload)" in source
    assert "latestSelectedChain(loadId, symbol, payload)," in source
    assert "?horizon_days=45&refresh=true" in source
    assert "analysisBlockUntil" in source
    assert "ANALYSIS_REFRESH_BLOCK_MS = 8000" in source
    assert "ANALYSIS_POLL_LIMIT = 90" in source
    assert "function analysisRefreshBlocking(" in source
    assert "cache: false" in Path("app/static/common/js/request.js").read_text(encoding="utf-8")
    # 首次拿到选中期限的链后立即请求综合价位，不再等待慢速的跨期限 Gamma 窗口。
    assert "loadFactorLevels(points, levelSpot);" in source
    assert "renderChain(payload, quote, state.lastAnalysis?.analysisPayload || null);" in source
    assert "deferLevels: true" not in source
    assert "if (!snapshot.analysisReady && snapshot.payload?.data?.length && snapshot.quote)" in source
    # 页面加载与 SSE 事件后的读取共用同一条刷新链路与网络互斥。
    assert "if (state.refreshInFlight === symbol) return;" in source
    assert "await loadChain({ loadId });" in source
    assert 'id="refresh-note"' not in Path("app/static/index.html").read_text(encoding="utf-8")


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
    # 没有期权时不能写入空目录，否则会把该标的以后可能出现的到期日缓存抹掉。
    assert database.latest_expiration_catalog("SPCX") == []
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
    settings = Settings(database_path=tmp_path / "options.db", proxy_url=None, default_symbols=("SPCX",), raw_retention_days=30, scheduler_enabled=False)
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
        assert fresh["expirations"] == []
        assert client.get("/api/expirations/SPCX").json()["expirations"] == []
        # 再次刷新命中新鲜期，直接复用本地现货快照。
        skipped = client.post("/api/refresh/SPCX", params={"max_age": 60}).json()
        assert skipped["skipped"] is True
        assert skipped["quote_only"] is True
        assert skipped["expirations"] == []


def test_refresh_publishes_full_expiration_catalog_after_one_chain(tmp_path: Path):
    """一次刷新只写入选中到期日的链时，下拉框仍要立刻拿到全部未过期日期。"""
    database = Database(tmp_path / "options.db")
    today = market_today()
    past = (today - timedelta(days=2)).isoformat()
    near = today.isoformat()
    later = (today + timedelta(days=7)).isoformat()
    far = (today + timedelta(days=40)).isoformat()
    calls = {"expirations": 0, "fetch": 0}

    class CountingProvider(FakeProvider):
        def expirations(self, symbol: str) -> list[str]:
            calls["expirations"] += 1
            return [past, near, later, far]

        def fetch(self, symbol: str, expiration: str):
            calls["fetch"] += 1
            return sample_quote(symbol), sample_rows(symbol, expiration), iso()

    provider = CountingProvider()
    settings = Settings(
        database_path=tmp_path / "options.db",
        proxy_url=None,
        default_symbols=("AAPL",),
        raw_retention_days=30,
        scheduler_enabled=False,
    )
    service = SnapshotService(database, provider)
    router = create_router(database, service, provider, settings)
    test_app = FastAPI()
    test_app.include_router(router)
    with TestClient(test_app) as client:
        fresh = client.post("/api/refresh/AAPL", params={"max_age": 0}).json()
        assert calls == {"expirations": 1, "fetch": 1}
        assert fresh["expiration"] == near
        assert fresh["expirations"] == [near, later, far]
        # 期权链只落了被选中的那一期，不能再把下拉框收成这一个日期。
        assert database.latest_expirations("AAPL") == [near]
        listed = client.get("/api/expirations/AAPL").json()
        assert listed["source"] == "sqlite"
        assert listed["expirations"] == [near, later, far]
        assert calls == {"expirations": 1, "fetch": 1}
        skipped = client.post("/api/refresh/AAPL", params={"expiration": near, "max_age": 60}).json()
        assert skipped["skipped"] is True
        assert skipped["expirations"] == [near, later, far]
        assert calls == {"expirations": 1, "fetch": 1}

    legacy = Database(tmp_path / "legacy.db")
    legacy.write_snapshot(sample_quote(), sample_rows("AAPL", near), iso())

    class BlockingProvider(FakeProvider):
        def expirations(self, symbol: str) -> list[str]:
            raise AssertionError("旧库没有到期日目录时不应回源")

    legacy_settings = Settings(
        database_path=tmp_path / "legacy.db",
        proxy_url=None,
        default_symbols=("AAPL",),
        raw_retention_days=30,
        scheduler_enabled=False,
    )
    legacy_service = SnapshotService(legacy, BlockingProvider())
    legacy_router = create_router(legacy, legacy_service, BlockingProvider(), legacy_settings)
    legacy_app = FastAPI()
    legacy_app.include_router(legacy_router)
    with TestClient(legacy_app) as client:
        payload = client.get("/api/expirations/AAPL").json()
        assert payload["source"] == "sqlite"
        assert payload["expirations"] == [near]
        # 目录里只剩已过期日期时，仍退回已经落库且未过期的链。
        legacy.write_expiration_catalog("AAPL", [past], iso())
        fallback = client.get("/api/expirations/AAPL").json()
        assert fallback["expirations"] == [near]


def test_frontend_loads_symbol_without_cached_expiration():
    source = Path("app/static/common/js/app.js").read_text(encoding="utf-8")
    # 首次载入一个从未抓过的标的时本地没有到期日：必须继续走到后台回源，否则页面永远停在无数据状态。
    assert "if (!state.expiration) return;" not in source
    assert "async function loadExpirations(loadId)" in source
    assert "await refreshInBackground(loadId);" in source
    # 仅现货标的单独一条渲染分支：现货照常画，期权面板统一提示没有期权数据。
    assert "async function loadQuoteOnly(loadId)" in source
    # 仅现货响应也不能直接渲染：等待期间到期日变了要重入，没变才结束，避免落到期权链分支。
    quote_only = source[source.index("if (refreshResult?.quote_only)") : source.index("if (refreshResult?.quote_only)") + 220]
    assert "const restarted = await loadQuoteOnly(loadId);" in quote_only
    assert "if (!restarted && await restartIfExpirationChanged(expiration)) return;" in quote_only
    assert quote_only.index("return;") > quote_only.index("loadQuoteOnly")
    assert 'applyExpirations([], null, "无期权到期日");' in source
    # 本地无缓存（source=pending）与「该标的确实没有期权」必须给出不同占位文案。
    assert 'payload.source === "pending" ? "正在获取到期日…" : "该标的没有期权到期日"' in source
    # 刷新响应里的完整到期日要立刻填进下拉框，不能先只放当前这一期再等下一轮自动刷新。
    assert "applyExpirations(refreshResult.expirations, state.expiration || refreshResult.expiration);" in source


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
    assert "function formatPercentValue(value, decimals = 1)" in source
    assert "factors: [`斐波那契 ${formatPercentValue(ratio * 100)}`]" in source
    assert "const match = text.match(/^斐波那契\\s+([+-]?\\d+(?:\\.\\d+)?)%$/);" in source
    assert "${ratio * 100}%" not in source
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
    assert "斐波那契 23.6%" in by_tag
    assert all("23.599999999999998" not in tag for tag in by_tag)
    assert weights["斐波那契 61.8%"] == 1.0
    assert weights["斐波那契 23.6%"] == 0.6


def test_fibonacci_levels_ignore_single_wick():
    """单根插针不参与摆动端点，回撤仍按收盘趋势的高低点计算。"""
    bars = []
    for index in range(40):
        price = 100 + index * 0.5
        high = 180.0 if index == 20 else price + 0.4
        bars.append({
            "date": f"2026-06-{index + 1:02d}",
            "open": price,
            "high": high,
            "low": price - 0.4,
            "close": price,
            "volume": 1000.0,
        })
    levels = fibonacci_levels(bars, 119.5)
    assert levels
    assert max(price for price, _, _ in levels) < 140


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


def test_absorption_levels_scale_rebound_and_break_with_atr():
    """高波动里大约 1% 的反弹不够确认承接，小于 0.25 ATR 的下探也不算跌破。"""
    def bar(low, high, close):
        return {"date": "2026-06-01", "open": close, "high": high, "low": low, "close": close, "volume": 100.0}

    base = [bar(108.0, 116.0, 112.0) for _ in range(12)]
    dip = bar(100.0, 108.0, 102.0)
    shallow = base + [dip] + [bar(104.0, 112.0, 103.0) for _ in range(6)]
    assert all(price != 100.0 for price, _, _ in absorption_levels(shallow, 112.0))

    confirmed = base + [dip] + [bar(104.0, 112.0, 106.0) for _ in range(6)]
    assert any(price == 100.0 for price, _, _ in absorption_levels(confirmed, 112.0))

    pierced = confirmed[:-1] + [bar(99.0, 112.0, 106.0)]
    assert any(price == 100.0 for price, _, _ in absorption_levels(pierced, 112.0))
    broken = confirmed[:-1] + [bar(97.0, 112.0, 106.0)]
    assert all(price != 100.0 for price, _, _ in absorption_levels(broken, 112.0))


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


def test_option_levels_follow_strike_side_not_contract_type():
    """绝对持仓按行权价相对现价分侧。上方的看跌持仓进压力，下方的看涨持仓进支撑。"""
    def point(strike, call_oi, put_oi):
        return {
            "strike": strike, "callVolume": 0.0, "putVolume": 0.0,
            "callOi": call_oi, "putOi": put_oi, "callGex": call_oi, "putGex": -put_oi,
        }

    above, below, metric = option_levels([
        point(110, 10, 500),
        point(108, 20, 40),
        point(105, 30, 30),
        point(95, 30, 30),
        point(92, 40, 20),
        point(90, 500, 10),
    ], 100.0)
    assert metric == "gex"
    assert any(price == pytest.approx(110) and tag.startswith("看跌") for price, _, tag in above)
    assert all(price > 100 for price, _, _ in above)
    assert any(price == pytest.approx(90) and tag.startswith("看涨") for price, _, tag in below)
    assert all(price < 100 for price, _, _ in below)


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


def test_stop_loss_level_uses_nearest_valid_support_and_volatility_buffer():
    result = stop_loss_level(
        {"lower": 92},
        {"zone_low": 96, "price": 98},
        [{"price": 94}, {"price": 80}],
        100,
        4,
    )
    assert result["source"] == "近期最佳买入点下沿"
    assert result["anchor_price"] == 96
    assert result["buffer"] == 1
    assert result["price"] == 95
    assert stop_loss_level({"lower": 100}, None, [], 100, None) is None
    assert stop_loss_level(None, None, [], None, None) is None


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
    settings = Settings(database_path=tmp_path / "options.db", proxy_url=None, default_symbols=("AAPL",), raw_retention_days=30, scheduler_enabled=False)
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
        assert payload["earnings"]["status"] == "unknown"
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


def test_spot_cache_bucket_groups_nearby_prices():
    """价位缓存格子：200 元附近约 0.2 元，相邻报价共用一格，离开格子后分开。"""
    assert api_module.spot_cache_bucket(200.50) == api_module.spot_cache_bucket(200.55)
    assert api_module.spot_cache_bucket(200.50) != api_module.spot_cache_bucket(199.0)
    assert api_module.spot_cache_bucket(None) is None
    assert api_module.spot_cache_bucket(-1) is None


def test_levels_endpoint_reuses_same_snapshot_analysis(tmp_path: Path, monkeypatch):
    """同一输入快照重复读取时只执行一次价位合成，快照变化后缓存键会自然失效。"""
    database = Database(tmp_path / "options.db")
    database.write_snapshot(sample_quote(), sample_rows(), iso())
    settings = Settings(database_path=tmp_path / "options.db", proxy_url=None, default_symbols=("AAPL",), raw_retention_days=30, scheduler_enabled=False)
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


def test_levels_endpoint_reuses_nearby_spot_bucket(tmp_path: Path, monkeypatch):
    """同一格子内的现价只合成一次价位；跨出格子后用本次精确现价重算。"""
    database = Database(tmp_path / "options.db")
    database.write_snapshot(sample_quote(), sample_rows(), iso())
    settings = Settings(database_path=tmp_path / "options.db", proxy_url=None, default_symbols=("AAPL",), raw_retention_days=30, scheduler_enabled=False)
    service = SnapshotService(database, FakeProvider())
    spots: list[float] = []
    original = api_module.build_levels

    def counted_build_levels(*args, **kwargs):
        spots.append(float(args[2]))
        return original(*args, **kwargs)

    monkeypatch.setattr(api_module, "build_levels", counted_build_levels)
    router = create_router(database, service, FakeProvider(), settings)
    test_app = FastAPI()
    test_app.include_router(router)
    with TestClient(test_app) as client:
        near = client.get("/api/levels/AAPL", params={"expiration": "2026-12-18", "spot": 200.50})
        nearby = client.get("/api/levels/AAPL", params={"expiration": "2026-12-18", "spot": 200.55})
        outside = client.get("/api/levels/AAPL", params={"expiration": "2026-12-18", "spot": 199.0})
    assert near.status_code == nearby.status_code == outside.status_code == 200
    assert spots == pytest.approx([200.50, 199.0])
    assert near.json()["spot"] == pytest.approx(200.50)
    assert nearby.json()["spot"] == pytest.approx(200.50)
    assert outside.json()["spot"] == pytest.approx(199.0)


def test_levels_endpoint_uses_previous_close_as_stable_candidate_anchor(tmp_path: Path):
    """现价变化时，候选池锚点沿用昨收，避免短时价格波动重建价位簇。"""
    database = Database(tmp_path / "options.db")
    quote = sample_quote()
    quote["previous_close"] = 198.0
    database.write_snapshot(quote, sample_rows(), iso())
    settings = Settings(database_path=tmp_path / "options.db", proxy_url=None, default_symbols=("AAPL",), raw_retention_days=30, scheduler_enabled=False)
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
    settings = Settings(database_path=tmp_path / "options.db", proxy_url=None, default_symbols=("AAPL",), raw_retention_days=30, scheduler_enabled=False)
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
    # 单边上涨 RSI 顶到 100 记为超买，单边下跌记为超卖，来回震荡留在中性。
    assert trend_channel(rising)["rsi"] == {"value": 100.0, "period": 14, "state": "overbought", "label": "超买"}
    assert trend_channel(falling)["rsi"] == {"value": 0.0, "period": 14, "state": "oversold", "label": "超卖"}
    neutral = trend_channel(flat)["rsi"]
    assert neutral["state"] == "neutral" and neutral["label"] == "中性"
    assert 40 <= neutral["value"] <= 60


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
    assert current["previous_open"] == 100
    fallback = trend_market_data(bars[:1], {"market_state": "PRE"})
    assert fallback["today_open"] == 100 and fallback["previous_close"] == 105
    assert fallback["previous_open"] == 100


def test_trend_market_data_uses_completed_close_after_regular_session(monkeypatch):
    """盘后/夜盘时，当日日线已经完成，昨收应取当天收盘而不是前一交易日。"""
    monkeypatch.setattr("app.api.market_today", lambda: date(2026, 9, 21))
    bars = [
        {"date": "2026-09-18", "open": 100, "close": 105},
        {"date": "2026-09-21", "open": 110, "close": 120},
    ]
    for market_state in ("POST", "OVERNIGHT", "CLOSED"):
        result = trend_market_data(bars, {"market_state": market_state})
        assert result["today_open"] == 110
        assert result["previous_open"] == 110
        assert result["previous_close"] == 120
        assert result["previous_close_date"] == "2026-09-21"


def test_trend_market_data_prefers_extended_session_reference_close(monkeypatch):
    """日线缓存落后时，非交易时段仍使用分钟线摘要里的正式收盘价。"""
    monkeypatch.setattr("app.api.market_today", lambda: date(2026, 9, 22))
    bars = [
        {"date": "2026-09-18", "open": 100, "close": 147.61},
    ]
    quote = {
        "market_state": "OVERNIGHT",
        "previous_close": 147.61,
        "sessions": {
            "post": {
                "price": 149.08,
                "reference_close": 148.60,
                "as_of": "2026-09-21T19:59:00-04:00",
            }
        },
    }
    result = trend_market_data(bars, quote)
    assert result["previous_close"] == pytest.approx(148.60)
    assert result["previous_close_date"] == "2026-09-21"


def test_trend_market_data_replaces_daily_open_copied_from_previous_day(monkeypatch):
    """未完成日线把昨开抄进今开时，盘中及盘后改用行情里的开盘价。"""
    monkeypatch.setattr("app.api.market_today", lambda: date(2026, 9, 25))
    copied = [
        {"date": "2026-09-24", "open": 137.32, "close": 139.54},
        {"date": "2026-09-25", "open": 137.32000732421875, "close": 140.11},
    ]
    result = trend_market_data(copied, {"market_state": "REGULAR", "today_open": 138.27})
    assert result["today_open"] == pytest.approx(138.27)
    assert result["today_open_date"] == "2026-09-25"
    assert result["previous_close"] == pytest.approx(139.54)

    # 日线开盘价和前一天不同，说明它是可信的今开，不用行情值盖掉。
    distinct = trend_market_data(
        [
            {"date": "2026-09-24", "open": 100, "close": 105},
            {"date": "2026-09-25", "open": 110, "close": 115},
        ],
        {"market_state": "REGULAR", "today_open": 138.27},
    )
    assert distinct["today_open"] == 110
    assert trend_market_data(copied, {"market_state": "POST", "today_open": 138.27})["today_open"] == pytest.approx(138.27)
    assert trend_market_data(copied, {"market_state": "CLOSED", "today_open": 138.27})["today_open"] == pytest.approx(137.32000732421875)
    # 当天日线还没到，盘中用行情今开；盘前仍显示最近一个交易日的开盘价。
    assert trend_market_data(copied[:1], {"market_state": "REGULAR", "today_open": 138.27})["today_open"] == pytest.approx(138.27)
    pre = trend_market_data(copied[:1], {"market_state": "PRE", "today_open": 138.27})
    assert pre["today_open"] == pytest.approx(137.32)
    assert pre["today_open_date"] == "2026-09-24"


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
    assert 'v-for="row in view.trend.rightRows"' in page
    assert "估算评分 {{ row.confidence }}" in page
    assert "{{ row.historySummary }}" in page


def test_levels_analysis_cache_namespace_matches_current_scoring_model():
    """最佳点评分字段变化时必须跳过旧版分析缓存。"""
    source = Path("app/api.py").read_text(encoding="utf-8")
    assert '"levels-v12"' in source
    assert '"levels-v11"' not in source


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
    assert "function renderTrend(trend, extremes, spot, historyMeta, recommendation = null, tradePoints = null, tradePointsHorizon = null, trendMarket = null, beta = null, serverStopLoss = null, stopLossSupports = [])" in source
    assert "function renderPlanRows(levels, spot)" in source
    assert "function renderPlan(plan, spot)" in source
    assert "const PLAN_COUNT = 10;" in source
    assert "const stableTradePoints = stabilizeTradePoints(payload?.trade_points, tradePointContext);" in source
    assert "renderTrend(payload?.trend || null, payload?.extremes || null, spot, payload?.history || null, payload?.recommendation || null, stableTradePoints," in source
    assert '"昨开"' in source and '"昨收"' in source and '["Beta", betaText, betaTitle]' in source
    assert '"Beta（2年）"' not in source
    assert page.index('class="trend-side"') < page.index('class="trend-core"')
    assert "基准指数：标普500" in source
    assert 'trend-beta-sub' not in source
    assert '"近期最佳买入点"' in source and '"近期最佳卖出点"' in source
    assert "未来 5 个交易日" in source
    assert "未来 5 个交易日（约 1 周）" not in source
    assert 'class="trend-layout"' in page and 'class="trend-core"' in page and 'class="trend-side"' in page
    # 左右列显式绑定行数组，避免顺序依赖模板切片。
    assert 'v-for="row in view.trend.leftRows"' in page
    assert 'v-for="row in view.trend.rightRows"' in page
    assert 'const leftRows = ["止损价", "昨开", "昨收", "Beta", "相对强弱", "日均斜率", "样本"]' in source
    assert '"52周最高", "52周最低", "历史最高", "历史最低"' in source
    assert 'label: "止损价"' in source and 'payload?.stop_loss || null' in source
    assert 'class="trend-meta trend-current-price" :title="view.trend.priceTitle"' in page
    assert "formatLevelRange(point)" in source and "formatProbability(point.confidence)" in source
    assert 'const priceLabel = selectedBasis?.label === "夜盘价"' in source
    assert 'const label = quote?.sessions?.overnight?.provider === "alpaca-overnight" ? "夜盘价" : "收盘";' in source
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
    assert ".analysis-levels-grid{align-items:start}" in styles
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
    settings = Settings(database_path=tmp_path / "options.db", proxy_url=None, default_symbols=("AAPL",), raw_retention_days=30, scheduler_enabled=False)
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

    settings = Settings(database_path=tmp_path / "options.db", proxy_url=None, default_symbols=("AAPL",), raw_retention_days=30, scheduler_enabled=False)
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
    assert '["相对强弱", rsiText, rsiTitle, `trend-rsi ${rsiState}`]' in source
    assert "70 及以上为超买，30 及以下为超卖" in source
    trend_rows = source[source.index("const rows = trend"):source.index("].map(([label, value, title, rowClass])")]
    assert trend_rows.index('["相对强弱"') < trend_rows.index('["日均斜率"')
    assert trend_rows.index('["日均斜率"') < trend_rows.index('["样本"')
    assert "每个交易日相对均价的平均涨跌百分比" in source


def test_trend_channel_shows_zero_skeleton_while_loading():
    """新标的加载时趋势通道先铺 0 值版式，不再收成一行空白提示。"""
    source = Path("app/static/common/js/app.js").read_text(encoding="utf-8")
    placeholder = source[source.index("function placeholderTrend"):source.index("function renderTrend")]
    for label in (
        "通道上轨", "通道下轨", "相对强弱", "日均斜率", "样本", "今开", "昨收", "Beta",
        "52周最高", "52周最低", "历史最高", "历史最低", "近期最佳买入点", "近期最佳卖出点",
    ):
        assert label in placeholder
    assert placeholder.count('"0.00"') >= 8
    assert '"0.0 · 中性"' in placeholder
    assert '"+0.000%"' in placeholder
    assert '"0 根日线"' in placeholder
    assert 'range: "0.00"' in placeholder
    assert 'confidence: "0%"' in placeholder
    assert "available: true" in placeholder
    assert "placeholder: true" in placeholder
    assert 'label: "趋势通道"' in placeholder
    assert 'action: ""' in placeholder
    assert 'reason: loading ? "正在加载" : status' in placeholder
    assert 'empty: "历史行情不足，暂无趋势判断"' in placeholder
    assert "trend: placeholderTrend()" in source
    pending = source[source.index("function showPending"):source.index("function applyCachedQuote")]
    assert 'state.view.trend = placeholderTrend(message || "正在加载")' in pending
    assert "available: false, rows: []" not in pending
    trend = source[source.index("function renderTrend"):source.index("function tradePointIdentity")]
    assert 'placeholderTrend("历史行情不足，暂无趋势判断")' in trend
    assert "available: false, rows: []" not in trend
    assert "placeholder: false" in trend
    fallback = source[source.index("function renderFactorFallback"):source.index("function requestFactorLevels")]
    assert "state.view.trend.available && !state.view.trend.placeholder" in fallback


def test_level_tables_show_zero_skeleton_while_loading():
    """加仓、支撑、压力三张表在新数据回来前先铺 10 行 0 值，不再只剩一句暂无数据。"""
    source = Path("app/static/common/js/app.js").read_text(encoding="utf-8")
    placeholder = source[source.index("function placeholderLevelRow"):source.index("function placeholderTrend")]
    assert 'range: "0.00"' in placeholder
    assert 'gap: "+0.00%"' in placeholder
    assert 'probability: "0%"' in placeholder
    assert 'factors: "0"' in placeholder
    assert 'detail: "历史回踩：0 次"' in placeholder
    assert 'title: "正在加载"' in placeholder
    assert 'placeholder: true' in placeholder
    assert 'placeholderLevelRows(LEVEL_COUNT, "resistance")' in placeholder
    assert 'placeholderLevelRows(LEVEL_COUNT, "support")' in placeholder
    assert 'placeholderLevelRows(PLAN_COUNT, "support", true)' in placeholder
    assert "levels: placeholderLevels()" in source
    pending = source[source.index("function showPending"):source.index("function applyCachedQuote")]
    assert 'state.view.levels = placeholderLevels(message || "正在加载")' in pending
    assert "state.view.levels.resistance = []" not in pending
    assert "state.view.levels.support = []" not in pending
    assert "state.view.levels.add = []" not in pending
    fallback = source[source.index("function renderFactorFallback"):source.index("function requestFactorLevels")]
    assert "!state.view.levels.placeholder" in fallback
    factor = source[source.index("function renderFactorLevels"):source.index("function formatStructureDelta")]
    assert "state.view.levels.placeholder = false" in factor


def test_charts_show_zero_skeleton_while_loading():
    """四张分布图在快照回来前先铺 0 值坐标和汇总，不再只在空白区放一句加载提示。"""
    source = Path("app/static/common/js/app.js").read_text(encoding="utf-8")
    charts = Path("app/static/common/js/charts.js").read_text(encoding="utf-8")
    assert "function renderChartSkeleton(targetId, options = {})" in charts
    skeleton_fn = charts[charts.index("function renderChartSkeleton"):charts.index("function showEmpty")]
    assert 'data-skeleton="1"' in skeleton_fn
    assert ">0.00</text>" in skeleton_fn
    assert "看涨 ↑" in skeleton_fn and "看跌 ↓" in skeleton_fn
    assert "压力 ↑" in skeleton_fn and "支撑 ↓" in skeleton_fn
    assert "renderChartSkeleton: renderChartSkeleton," in charts
    show = source[source.index("function showChartSkeletons"):source.index("function showPending")]
    assert 'OptionScopeCharts.renderChartSkeleton("gex-chart");' in show
    assert 'OptionScopeCharts.renderChartSkeleton("volume-chart", { axis: "right" });' in show
    assert 'OptionScopeCharts.renderChartSkeleton("oi-chart", { axis: "right" });' in show
    assert 'OptionScopeCharts.renderChartSkeleton("levels-chart", { zeroLabel: "0.00", corners: "levels" });' in show
    assert '净 Gamma 0 · 估算' in show
    assert '零 Gamma 0.00 · 估算' in show
    assert '看涨墙 0.00' in show and '看跌墙 0.00' in show
    assert '基准 0.00' in show
    assert 'OptionScopeCharts.renderDistributionSummary(byId("volume-summary"), [], null, "volume", "总成交量");' in show
    assert 'OptionScopeCharts.renderDistributionSummary(byId("oi-summary"), [], null, "open_interest", "总持仓量");' in show
    pending = source[source.index("function showPending"):source.index("function applyCachedQuote")]
    assert "showChartSkeletons();" in pending
    assert "state.lastAnalysis = null;" in pending
    assert 'OptionScopeCharts.showEmpty("gex-chart"' not in pending
    assert 'OptionScopeCharts.showEmpty("levels-chart"' not in pending
    assert "OptionScopeCharts.clearSummary" not in pending
    assert "showChartSkeletons();\n  loadSymbol();" in source


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

    settings = Settings(database_path=tmp_path / "options.db", proxy_url=None, default_symbols=("AAPL",), raw_retention_days=30, scheduler_enabled=False)
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
    assert 'id="detail-hint"' not in page and 'id="detail-action"' in page
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
    assert 'class="detail-header analysis-detail-header" id="detail-header"' in page
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
    assert "function overnightCloseQuote(quote)" in source
    assert "rawReportedChange == null || rawReportedChange === \"\"" in source
    assert 'const close = overnightCloseQuote(quote);' in source
    assert "const price = Number(activeSessionQuote(quote)?.price);" in source
    assert 'const priceLabel = selectedBasis?.label === "夜盘价"' in source
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
    # 切换口径先复用已有综合结果，后台补算完成后再替换，避免近期最佳买卖点闪回 --。
    assert "serverLevelsCache: new Map()" in source
    assert "function rememberServerLevels(key, payload)" in source
    assert "const previousServerPayload = sameSymbolExpiration && serverLevelsHasCompleteFields(state.serverLevelsPayload)" in source
    assert "const provisionalTradePoints = hasServerLevels" in source
    assert "state.tradePointStability.stable = { buy: payload.trade_points?.buy || null" in source
    assert 'if (marketState === "POST" && sessions.post?.price != null) return sessions.post;' in source
    assert 'sessions.overnight?.provider !== "alpaca-overnight"' in source
    assert "if (state.lastQuote) renderQuote(state.lastQuote);" in source
    # 开关只在展开时出现；点击开关不会连带折叠，点标题栏其它区域仍然折叠/展开。
    assert 'const modes = byId("detail-modes");' in source
    assert "if (modes) modes.hidden = !expanded;" in source
    assert 'const basisButton = closestElement(event.target, "[data-basis]");' in source
    # 开关容器里的空白点击不改折叠：ignore 回调里用 closest 命中 #detail-modes 就吃掉这次点击
    assert 'return Boolean(closestElement(event.target, "#detail-modes"));' in source
    # 样式：隐藏态生效 + 选中态用 --blue 实心。
    assert ".detail-modes[hidden]{display:none}" in styles
    assert ".detail-segmented{display:inline-flex;" in styles
    assert ".analysis-detail-header>.detail-modes{margin-left:auto;flex:none}" in styles
    assert ".analysis-detail-header>.detail-action{margin-left:0}" in styles
    assert ".analysis-detail-header>.detail-modes[hidden]+.detail-action" in styles
    assert ".option-analysis-group>.detail-header>.detail-modes[hidden]+.detail-action{margin-left:auto}" in styles
    assert ".analysis-detail-header .detail-segmented{border-radius:4px;background:var(--field)}" in styles
    assert "@media(max-width:600px)" in styles
    assert ".analysis-detail-header>.detail-modes{order:3;width:calc(100% - 28px);margin:0 0 0 28px}" in styles
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
    assert "if (!isCurrentLoad(loadId, expiration)) return { shown: false, source: null, discarded: true };" in source
    # 作废的快照不能把新期限清成「正在后台获取」，也不能再为旧期限启动刷新。
    load_chain = source[source.index("async function loadChain"):source.index("async function loadSymbol")]
    assert "if (snapshot.discarded) return;" in load_chain
    assert load_chain.index("if (snapshot.discarded) return;") < load_chain.index('showPending("正在后台获取上游快照…")')
    # 双向防覆盖之二：后台刷新发现期限变了就先还回网络互斥再按新期限重来，避免把用户的选择拽回旧期限。
    assert "if (state.expiration !== currentExpiration) {" in source
    assert "state.refreshInFlight = null;\n      await refreshInBackground(loadId);" in source
    assert source.index("if (state.expiration !== currentExpiration) {") < source.index("applyExpirations(expirations.expirations, currentExpiration);")
    # 自动刷新的 POST 还没回来就换了到期日：先重入新期限，再使用旧 POST 的结果去拉链或渲染。
    refresh_fn = source[source.index("async function refreshInBackground"):source.index("async function loadQuoteOnly")]
    post = refresh_fn.index('method: "POST"')
    restart = refresh_fn.index("if (await restartIfExpirationChanged(expiration)) return;", post)
    current = refresh_fn.index("const currentExpiration = state.expiration || refreshResult?.expiration;")
    render_old = refresh_fn.index("renderChain(payload, resolvedQuote, state.lastAnalysis?.analysisPayload || null);")
    assert post < restart < current < render_old
    assert "async function restartIfExpirationChanged(captured)" in refresh_fn
    assert "if (payload?.expiration && state.expiration && payload.expiration !== state.expiration) return;" in source
    # 下拉框的禁用兜底：网络卡死时不至于永久禁用。
    assert "const EXPIRATION_SWITCH_TIMEOUT_MS = 20000;" in source
    assert "clearTimeout(releaseTimer);" in source
    # 旧的「静默重载」绑定已删除，切换只走 switchExpiration 一条路径。
    assert 'expirationChanged() { return switchExpiration(this.expiration); }' in source
    assert 'loadChain({ silent: true })' not in source
    # Expiry selection reconnects the SSE subscription; initial and change reads remain REST-backed.
    page = Path("app/static/index.html").read_text(encoding="utf-8")
    expiration_select = page[page.index('id="expiration-select"'):page.index('id="expiration-select"') + 500]
    assert '@visible-change="expirationMenuChanged"' in expiration_select
    assert "function setExpirationMenuOpen(open)" in source
    assert "expirationMenuChanged(open) { return setExpirationMenuOpen(open); }" in source
    assert "connectPushStream(state.symbol);" in source
    assert "const skipGamma = snapshotReadInFlight > 0 || state.refreshInFlight === state.symbol;" in source


def test_refresh_cycle_does_not_reread_same_snapshot():
    """已经展示的快照在自动刷新时不要把 quote、chain、gamma、levels 各请求多遍。"""
    source = Path("app/static/common/js/app.js").read_text(encoding="utf-8")
    assert "function displayedSnapshotMatches()" in source
    assert "function displayedSnapshotIsFresh()" in source
    assert "let snapshotReadCache = null;" in source
    assert "function reusableGammaProfile(symbol)" in source
    assert "chainFresh && !analysis" in source
    assert "levelsWindowFetchedAt" not in source
    load_chain = source[source.index("async function loadChain"):source.index("async function loadSymbol")]
    assert load_chain.index("if (displayedSnapshotMatches())") < load_chain.index("const snapshot = await renderSnapshot(loadId);")
    assert "await refreshInBackground(loadId);" in load_chain
    refresh_fn = source[source.index("async function refreshInBackground"):source.index("async function loadQuoteOnly")]
    assert refresh_fn.index("quoteIsReady(refreshResult?.quote)") < refresh_fn.index("request(`/api/quote/${encodedSymbol}`)")
    assert "Promise.resolve(refreshResult.quote)" in refresh_fn
    assert "let resolvedQuote = quoteIsReady(quote) ? quote : refreshResult?.quote;" in refresh_fn
    chain_fn = source[source.index("async function latestSelectedChain"):source.index("function reusableGammaProfile")]
    assert "fallbackPayload.expiration === expiration" in chain_fn
    assert "levelsWindowFetchedAt" not in source
    levels_key = source[source.index("const key = `${state.symbol}|${state.expiration}|"):source.index("const key = `${state.symbol}|${state.expiration}|") + 220]
    assert "levelsWindowFetchedAt" not in levels_key
    assert "state.chainFetchedAt" in levels_key
    # 服务端自动刷新不依赖浏览器倒计时或轮询。
    assert "function displayedSnapshotIsCurrent(fetchedAt)" in source
    assert "if (!displayedSnapshotIsCurrent(refreshResult.fetched_at))" in source
    assert "refreshDeadline" not in source
    assert "setTimeout(() => { refresh(true); }" not in source
    assert "function syncedNow()" in source
    assert "OptionScopeRequest.serverNow" in source
    poll = source[source.index("function pollGammaWindow"):source.index("async function refreshInBackground")]
    assert ".finally(" not in poll
    assert "Promise.resolve(task)" in poll
    assert "continuePolling" in poll
    request_js = Path("app/static/common/js/request.js").read_text(encoding="utf-8")
    assert 'getResponseHeader("Date")' in request_js
    assert "serverNow: function" in request_js
    assert "noteServerNow: noteServerNow" in request_js
    assert "cache: false" in request_js


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
    assert ".option-flow-group .detail-action{color:var(--blue)}" in styles
    # 状态行内部保持中线对齐（间距/分隔线也跟着走同一条中线）
    assert ".top-meta,.panel-status,footer{display:flex;align-items:center;gap:10px" in styles
    # 旧的标题行工具条规则已删除，筛选改由折叠区内的 .chain-toolbar 承载
    assert ".panel-tools" not in styles and ".panel-tools" not in page
    assert ".chain-toolbar{display:flex;justify-content:flex-start;padding:12px 18px 10px}" in styles


def test_default_symbols_remain_page_defaults_only(monkeypatch):
    """DEFAULT_SYMBOLS remains backward compatible for initial page symbol configuration."""
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


def test_auto_refresh_is_market_scheduled_not_env_configured(monkeypatch):
    """AUTO_REFRESH_SECONDS is ignored; refresh cadence is no longer an operator setting."""
    monkeypatch.setenv("AUTO_REFRESH_SECONDS", "2")
    assert not hasattr(Settings.from_env(), "auto_refresh_seconds")


def test_page_auto_refresh_uses_active_sse_market_scheduler():
    page = Path("app/static/index.html").read_text(encoding="utf-8")
    source = Path("app/static/common/js/app.js").read_text(encoding="utf-8")
    main = Path("app/main.py").read_text(encoding="utf-8")
    api = Path("app/api.py").read_text(encoding="utf-8")
    menu = Path("run.sh").read_text(encoding="utf-8")
    assert 'id="refresh-note"' not in page
    assert "分析计算中，刷新稍后开始" not in source
    assert "AUTO_REFRESH_SECONDS" not in source
    assert "refresh_interval_for_now" in Path("app/services/push_events.py").read_text(encoding="utf-8")
    assert "PushEventHub(database, snapshots.refresh)" in api
    assert "__AUTO_REFRESH_SECONDS__" not in main
    assert "AUTO_REFRESH_SECONDS" not in menu
    assert 'printf \' %2d) %s=%s' in menu
    assert 'IFS=\'|\' read -r key default_value type description <<<"${CONFIG_ROWS[index]}"' in menu
    assert '*) ask_env_value "$key" "$description" ;;' in menu


def test_cleanup_interval_setting_removed_and_retention_configured(monkeypatch):
    """历史清理统一按保留天数配置，并且不再读取独立清理间隔。"""
    monkeypatch.setenv("CLEANUP_INTERVAL_SECONDS", "1")
    monkeypatch.setenv("RAW_RETENTION_DAYS", "30")
    settings = Settings.from_env()
    assert settings.raw_retention_days == 30
    assert not hasattr(settings, "cleanup_interval_seconds")
    scheduler = Path("app/services/scheduler.py").read_text(encoding="utf-8")
    assert "DAILY_CLEANUP_INTERVAL_SECONDS = 86400" in scheduler
    assert "timeout=DAILY_CLEANUP_INTERVAL_SECONDS" in scheduler
    assert "settings.raw_retention_days" in scheduler
    config = Path("app/config.py").read_text(encoding="utf-8")
    env_example = Path(".env.example").read_text(encoding="utf-8")
    readme = Path("README.md").read_text(encoding="utf-8")
    menu = Path("run.sh").read_text(encoding="utf-8")
    assert "cleanup_interval_seconds" not in config
    assert "CLEANUP_INTERVAL_SECONDS" not in env_example
    assert "CLEANUP_INTERVAL_SECONDS" not in readme
    assert "CLEANUP_INTERVAL_SECONDS" not in menu


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
    # Tiny databases can gain schema pages during VACUUM; deleted-row counts and the retained latest batch
    # are the stable cleanup guarantees, rather than file size after rebuilding the schema.
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
    settings = Settings(database_path=tmp_path / "options.db", proxy_url=None, default_symbols=("AAPL",), raw_retention_days=30, scheduler_enabled=False)
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
    settings = Settings(database_path=tmp_path / "options.db", proxy_url=None, default_symbols=("QQQ",), raw_retention_days=30, scheduler_enabled=False)
    scheduler = Scheduler(settings, database)
    asyncio.run(scheduler._prune_legacy_raw_json())
    with database.connect() as connection:
        assert connection.execute("SELECT COUNT(*) FROM option_snapshots WHERE raw_json IS NOT NULL").fetchone()[0] == 0
        assert connection.execute("SELECT COUNT(*) FROM quote_snapshots WHERE raw_json IS NOT NULL").fetchone()[0] == 0
        assert connection.execute("SELECT COUNT(*) FROM option_snapshots").fetchone()[0] == 2
    # 再跑一次无事可做，返回 0 且不重复回收。
    assert database.prune_legacy_raw_json() == 0
    assert database.latest_quote("AAPL")["price"] == 200.5


def test_scheduler_only_runs_housekeeping_without_refreshing_default_symbols(tmp_path: Path, monkeypatch):
    """调度器保留数据库清理，但不再为默认标的启动定时行情刷新。"""
    database = Database(tmp_path / "options.db")
    calls: list[tuple[str, ...]] = []

    def boom(*_args, **_kwargs):
        raise RuntimeError("模拟磁盘写满")

    settings = Settings(
        database_path=tmp_path / "options.db",
        proxy_url=None,
        default_symbols=("QQQ",),
        raw_retention_days=30,
        scheduler_enabled=True,
        database_max_mb=1,
    )
    monkeypatch.setattr(database, "prune_legacy_raw_json", boom)
    monkeypatch.setattr(database, "cleanup", boom)
    monkeypatch.setattr(database, "cleanup_by_size", boom)

    async def scenario() -> bool:
        scheduler = Scheduler(settings, database)
        await scheduler.start()
        await asyncio.sleep(0.05)
        alive = scheduler._task is not None and not scheduler._task.done()
        await scheduler.stop()
        return alive

    assert asyncio.run(scenario()) is True
    assert calls == []


def test_access_key_guard_blocks_pages_and_api_without_key(tmp_path: Path):
    """配置访问密钥后，页面和 API 都必须携带正确 key，静态资源和健康检查保持可用。"""
    settings = Settings(
        database_path=tmp_path / "options.db",
        proxy_url=None,
        default_symbols=("QQQ",),
        raw_retention_days=30,
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
        raw_retention_days=30,
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


def test_summarize_earnings_window_includes_holiday_between_sessions():
    """财报日只要落在未来交易日窗口起止之间就算窗口内，即使当天休市。"""
    inside = summarize_earnings(["2026-09-30"], date(2026, 9, 25))
    outside = summarize_earnings(["2026-10-08"], date(2026, 9, 25))
    unknown = summarize_earnings(["2026-09-01"], date(2026, 9, 25))
    weekend = summarize_earnings(["2026-09-29"], date(2026, 9, 26))
    thanksgiving = summarize_earnings(["2026-11-26"], date(2026, 11, 25))
    assert inside["status"] == "inside"
    assert inside["date"] == "2026-09-30"
    assert inside["window_start"] == "2026-09-25"
    assert inside["window_end"] == "2026-10-01"
    assert outside["status"] == "outside"
    assert unknown["status"] == "unknown"
    assert unknown["date"] is None
    assert weekend["status"] == "inside"
    assert weekend["window_start"] == "2026-09-28"
    assert thanksgiving["status"] == "inside"
    assert thanksgiving["window_start"] == "2026-11-25"
    assert thanksgiving["window_end"] == "2026-12-02"


def test_parse_earnings_dates_uses_new_york_calendar_day():
    """尚未公布的 EPS 才保留，并且按美东日期而不是 UTC 日期。"""
    frame = pd.DataFrame(
        {"Reported EPS": [float("nan"), 1.25, "-"]},
        index=pd.to_datetime([
            "2026-10-08 03:30:00+00:00",
            "2026-07-30 20:00:00+00:00",
            "2026-11-05 21:00:00+00:00",
        ]),
    )
    assert parse_earnings_dates(frame) == ["2026-10-07", "2026-11-05"]
    assert parse_earnings_dates(frame.drop(columns=["Reported EPS"])) == []


def test_parse_earnings_dates_keeps_today_after_eps_is_reported():
    """财报当天盘后上游可能已填 EPS，仍必须保留当天日期供页面提示。"""
    today = datetime.now(ZoneInfo("America/New_York")).date().isoformat()
    frame = pd.DataFrame(
        {"Reported EPS": [2.34]},
        index=pd.to_datetime([f"{today} 21:00:00-04:00"]),
    )
    assert parse_earnings_dates(frame) == [today]


def test_hybrid_provider_delegates_earnings_dates():
    """混合行情源不继承主行情类，财报日期必须转给常规适配器。"""
    class Regular(FakeProvider):
        def earnings_dates(self, symbol: str) -> list[str]:
            return ["2026-10-07"]

    provider = HybridMarketDataProvider(Regular(), FakeProvider())
    assert provider.earnings_dates("aapl") == ["2026-10-07"]


def test_history_service_caches_earnings_dates(tmp_path: Path):
    """财报日期在新鲜期内复用 SQLite；上游失败或缺少方法时不抛错。"""
    database = Database(tmp_path / "options.db")

    class CountingProvider(FakeProvider):
        def __init__(self):
            self.calls = 0

        def earnings_dates(self, symbol: str) -> list[str]:
            self.calls += 1
            return ["2026-10-08"]

    provider = CountingProvider()
    service = HistoryService(database, provider)
    first = service.earnings("aapl")
    second = service.earnings("AAPL")
    assert first["source"] == "upstream"
    assert first["dates"] == ["2026-10-08"]
    assert second["source"] == "sqlite"
    assert provider.calls == 1

    class FailingProvider(FakeProvider):
        def earnings_dates(self, symbol: str) -> list[str]:
            raise ProviderError("财报接口不可用")

    stale_at = iso(utc_now() - timedelta(seconds=EARNINGS_MAX_AGE_SECONDS + 60))
    database.write_earnings("AAPL", {"dates": ["2026-01-02"]}, stale_at)
    failed = HistoryService(database, FailingProvider()).earnings("AAPL")
    assert failed["dates"] == ["2026-01-02"]
    assert failed["source"] == "sqlite"
    assert failed["warning"]

    missing = HistoryService(database, FakeProvider()).earnings("MSFT")
    assert missing["source"] == "none"
    assert missing["dates"] == []
    assert database.latest_earnings("MSFT") is None


def test_earnings_refresh_timeout_returns_without_raising(tmp_path: Path, monkeypatch):
    """等待财报刷新租约超时时返回空结果，不能把价位接口打成 500。"""
    database = Database(tmp_path / "options.db")
    assert database.try_acquire_lease("history:earnings:AAPL", "other", 120) is True
    monkeypatch.setattr("app.services.history.HISTORY_WAIT_SECONDS", 0)

    class BlockingProvider(FakeProvider):
        def earnings_dates(self, symbol: str) -> list[str]:
            raise AssertionError("租约未拿到时不应请求上游")

    result = HistoryService(database, BlockingProvider()).earnings("AAPL")
    assert result["source"] == "none"
    assert result["dates"] == []
    assert "超时" in (result["warning"] or "")


def test_levels_endpoint_reports_earnings_inside_window(tmp_path: Path, monkeypatch):
    """财报日期按请求当天判断窗口，并且第二次请求复用日期缓存。"""
    database = Database(tmp_path / "options.db")
    database.write_snapshot(sample_quote(), sample_rows(), iso())
    today = {"value": date(2026, 9, 25)}
    monkeypatch.setattr(api_module, "market_today", lambda: today["value"])

    class EarningsProvider(FakeProvider):
        def __init__(self):
            self.calls = 0

        def earnings_dates(self, symbol: str) -> list[str]:
            self.calls += 1
            return ["2026-09-30"]

    provider = EarningsProvider()
    settings = Settings(database_path=tmp_path / "options.db", proxy_url=None, default_symbols=("AAPL",), raw_retention_days=30, scheduler_enabled=False)
    router = create_router(database, SnapshotService(database, provider), provider, settings)
    test_app = FastAPI()
    test_app.include_router(router)
    with TestClient(test_app) as client:
        first = client.get("/api/levels/AAPL", params={"expiration": "2026-12-18"})
        assert first.status_code == 200
        payload = first.json()
        assert payload["earnings"]["status"] == "inside"
        assert payload["earnings"]["date"] == "2026-09-30"
        assert payload["earnings"]["source"] == "upstream"
        today["value"] = date(2026, 8, 3)
        second = client.get("/api/levels/AAPL", params={"expiration": "2026-12-18"}).json()
        assert second["earnings"]["status"] == "outside"
        assert second["earnings"]["source"] == "sqlite"
        assert provider.calls == 1


def test_quote_endpoint_includes_earnings_summary(tmp_path: Path, monkeypatch):
    """现货接口直接携带财报摘要，前端无需等待 levels 请求。"""
    database = Database(tmp_path / "options.db")
    database.write_snapshot(sample_quote("MU"), [], iso())
    today = {"value": date(2026, 9, 25)}
    monkeypatch.setattr(api_module, "market_today", lambda: today["value"])

    class EarningsProvider(FakeProvider):
        def earnings_dates(self, symbol: str) -> list[str]:
            return ["2026-09-30"]

    provider = EarningsProvider()
    settings = Settings(database_path=tmp_path / "options.db", proxy_url=None, default_symbols=("MU",), raw_retention_days=30, scheduler_enabled=False)
    router = create_router(database, SnapshotService(database, provider), provider, settings)
    test_app = FastAPI()
    test_app.include_router(router)
    with TestClient(test_app) as client:
        response = client.get("/api/quote/MU")
    assert response.status_code == 200
    payload = response.json()
    assert payload["earnings"]["status"] == "inside"
    assert payload["earnings"]["date"] == "2026-09-30"


def test_cleanup_keeps_earnings_snapshots(tmp_path: Path):
    """保留天数清理不删除财报日期缓存。"""
    database = Database(tmp_path / "options.db")
    database.write_earnings("AAPL", {"dates": ["2026-09-30"]}, iso(utc_now() - timedelta(days=40)))
    deleted = database.cleanup(30)
    assert "earnings" not in deleted
    assert database.latest_earnings("AAPL")["dates"] == ["2026-09-30"]


def test_cleanup_keeps_expiration_catalog(tmp_path: Path):
    """到期日目录只服务下拉框，不随期权链的保留天数一起删掉。"""
    database = Database(tmp_path / "options.db")
    database.write_expiration_catalog(
        "AAPL",
        ["2026-12-18", "not-a-date", "2027-01-15", "2026-12-18"],
        iso(utc_now() - timedelta(days=40)),
    )
    assert database.latest_expiration_catalog("AAPL") == ["2026-12-18", "2027-01-15"]
    deleted = database.cleanup(30)
    assert "expirations" not in deleted
    assert database.latest_expiration_catalog("AAPL") == ["2026-12-18", "2027-01-15"]
    # 非法或空列表不能覆盖已经记下的完整目录。
    database.write_expiration_catalog("AAPL", ["still-bad"], iso())
    assert database.latest_expiration_catalog("AAPL") == ["2026-12-18", "2027-01-15"]
    with database.connect() as connection:
        connection.execute(
            "INSERT OR REPLACE INTO expiration_catalog(symbol, payload, fetched_at) VALUES (?, ?, ?)",
            ("MSFT", "{", iso()),
        )
    assert database.latest_expiration_catalog("MSFT") == []


def test_frontend_marks_quote_reference_estimates_and_earnings():
    """现货基准、估算标记和财报提示都要出现在页面上，而且报价刷新不能清掉财报芯片。"""
    source = Path("app/static/common/js/app.js").read_text(encoding="utf-8")
    page = Path("app/static/index.html").read_text(encoding="utf-8")
    styles = Path("app/static/common/css/styles.css").read_text(encoding="utf-8")
    assert "function quoteReference(quote)" in source
    assert "function mergeFairValueSnapshot(quote)" in source
    assert "const keepFairValue = state.lastQuote" in source
    assert "applyEarnings(quote?.earnings)" in source
    assert "昨收" in source
    assert "相对收盘" in source
    assert 'label = "昨收"' in source
    premarket_reference = source[source.index('if (marketState === "PRE")', source.index("function quoteReference")):source.index('} else if (marketState === "POST"', source.index("function quoteReference"))]
    assert "price = previous;" in premarket_reference
    assert "sessions.pre?.reference_close" not in premarket_reference
    assert "function alignedPreviousClose(quote)" in source
    assert "state.trendMarketSymbol === quote?.symbol" in source
    assert "state.trendMarket?.previous_close" in source
    assert "const reference = quoteReference(quote);" in source
    assert "const referencePrice = finitePrice(reference.price);" in source
    assert "(Number(price) - referencePrice) / referencePrice * 100" in source
    assert "state.trendMarket = null;" in source
    assert "state.trendMarketSymbol = \"\";" in source
    render_factor = source[source.index("function renderFactorLevels"):source.index("function formatStructureDelta")]
    assert render_factor.index("payload?.trend_market") < render_factor.index("refreshQuoteWithTrendClose()")
    quote_sync = source[source.index("function refreshQuoteWithTrendClose"):source.index("function earningsMonthDay")]
    assert "state.lastQuote?.symbol === state.symbol" in quote_sync
    render_trend = source[source.index("function renderTrend("):source.index("function tradePointIdentity")]
    assert "const trendClose = finitePrice(trendMarket?.previous_close);" in render_trend
    assert "state.trendMarket = trendMarket;" in render_trend
    assert "state.trendMarketSymbol = state.symbol;" in render_trend
    assert "if (trendClose != null)" in render_trend
    raw_render = source[source.index("function renderClientRaw"):source.index("function renderFactorFallback")]
    assert raw_render.index("payload.trend_market") < raw_render.index("refreshQuoteWithTrendClose()")
    assert "sessions.post?.reference_close" in source
    assert "· 估算" in source
    assert "财报日期未知" in source
    assert "估算评分" in page
    assert "不是胜率" in source
    assert "规则估算，不是下单指令，也不包含财报跳空" in source
    assert 'id="quote-reference"' in page
    assert 'id="quote-earnings"' in page
    assert "估算 {{ item.score }}" in page
    factor = source[source.index("function renderFactorLevels"):source.index("function formatStructureDelta")]
    assert factor.index("applyEarnings(") < factor.index("renderBuyerStructures(")
    assert factor.index("applyEarnings(") < factor.index("renderTrend(")
    pending = source[source.index("function showPending"):source.index("function applyCachedQuote")]
    assert "quoteReference" not in pending
    assert "resetEarningsChip()" in pending
    cached = source[source.index("function applyCachedQuote"):source.index("async function renderSnapshot")]
    assert "quoteReference(null)" in cached
    assert "resetEarningsChip" not in cached
    assert ".quote-earnings.inside{color:var(--amber)}" in styles
    assert ".quote-sub{flex-wrap:wrap;row-gap:4px}" in styles


def test_mobile_option_analysis_tabs_use_compact_two_segment_layout():
    """手机端标签缩短并均分为易点按的两段式按钮，完整名称由 aria-label 保留。"""
    page = Path("app/static/index.html").read_text(encoding="utf-8")
    styles = Path("app/static/common/css/styles.css").read_text(encoding="utf-8")
    assert 'aria-label="期权成交方向与流向分析"' in page
    assert 'aria-label="期权买方结构分析" aria-pressed="false">买方结构</button>' in page
    assert 'aria-label="期权成交方向与流向分析" aria-pressed="true">成交流向</button>' in page
    assert ".option-analysis-tabs{margin-left:auto}" in styles
    assert ".option-analysis-group>.detail-header>.detail-action{margin-left:0}" in styles
    assert "@media(min-width:601px) and (max-width:900px)" in styles
    assert "@media(max-width:600px)" in styles
    assert ".option-analysis-tabs{order:3;width:calc(100% - 28px);margin:0 0 0 28px}" in styles
    assert ".option-analysis-tabs .detail-seg{display:flex;min-width:0;min-height:40px;flex:1 1 50%" in styles
    assert ".option-analysis-tabs .detail-seg[aria-pressed=\"true\"]" in styles


def test_frontend_labels_sqlite_snapshot_as_local_cache():
    """面向用户的来源标签使用「本地缓存」，不暴露存储实现。"""
    source = Path("app/static/common/js/app.js").read_text(encoding="utf-8")
    assert '"本地缓存"' in source
    assert '"SQLite 缓存"' not in source


def test_option_flow_title_uses_professional_analysis_label():
    """期权流向面板标题明确说明成交方向分析口径。"""
    page = Path("app/static/index.html").read_text(encoding="utf-8")
    assert 'aria-label="期权成交方向与流向分析"' in page
    assert "成交方向与流向分析" in page
    assert "<span class=\"detail-title\">期权流向</span>" not in page


def test_empty_option_flow_does_not_overwrite_last_nonempty_result():
    """相邻快照没有新增成交量时保留上次有效流向，避免冷门标的显示空的 0 数据。"""
    source = Path("app/static/common/js/app.js").read_text(encoding="utf-8")
    helper = source[source.index("function hasOptionFlowData"):source.index("function renderOptionFlow")]
    render = source[source.index("function renderOptionFlow"):source.index("// 时段标签", source.index("function renderOptionFlow"))]
    assert "if (!payload?.available) return false;" in helper
    assert "side.buy_volume, side.sell_volume, side.unknown_volume" in helper
    assert "side.concentration, side.top_strikes" in helper
    assert "|| !hasOptionFlowData(payload)) return;" in render
    assert render.index("!hasOptionFlowData(payload)") < render.index("state.view.flow = {")
    snapshot_render = source[source.index("async function loadSnapshotForRender"):source.index("// 跨期限 Gamma 窗口刷新最慢")]
    assert "// 暂时拿不到期权链时不清空已显示的流向" in snapshot_render
    assert 'resetOptionFlow("当前期限暂无可用期权快照")' not in snapshot_render
