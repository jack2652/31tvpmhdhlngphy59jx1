"""估值决策树的集中参数配置。

把行业模型的关键倍数、安全边际和运行资源限制集中在这里，避免在行情适配器
里散落同一组业务参数。数值仍由公开财务数据驱动，配置只定义合理边界。
"""

VALUATION_CONFIG = {
    "cache_version": "v39",
    "debug": False,
    "fair_value_workers": 8,
    "interest_coverage_warning": 2.0,
    "market_cap_mismatch_threshold": 0.20,
    "market_cap_reconciliation": {
        # 市值与股本×现价偏差超过该比例才进入口径核对。
        "mismatch_threshold": 0.20,
        # 只有可视为当前口径的股本来源才允许双向校正；年度时序股本和未知来源
        # 可能滞后或是稀释后口径，不能用来把已有 Yahoo market_cap 向任一方向覆盖。
        "trusted_shares_sources": ("balance_sheet",),
    },
    "normalized_eps": {
        "min_ratio": 0.25,
        "max_ratio": 4.0,
    },
    "small_cap_threshold": 5e9,
    "unit_price_floor_ratio": 0.05,
    "unit_price_ceiling_ratio": 20.0,
    "regime_thresholds": {
        "eps_revision_slowdown": -0.10,
        "eps_revision_winter": -0.25,
        "revenue_growth_slowdown": 0.05,
        "revenue_growth_winter": -0.10,
        "tnx_yield_slowdown": 0.045,
        "tnx_yield_winter": 0.055,
        "vix_slowdown": 25.0,
        "vix_winter": 35.0,
        "inventory_days_winter": 120.0,
        "capex_growth_winter": -0.15,
        "benchmark_momentum_slowdown": -0.08,
        "benchmark_momentum_winter": -0.15,
        # 任一极端信号都足以切换寒冬：避免 VIX 熔断或指数断崖时等待第二个信号。
        "vix_extreme": 45.0,
        "benchmark_momentum_extreme": -0.20,
        "eps_revision_extreme": -0.35,
    },
    # Yahoo 通常没有逐公司 AI 营收拆分；行业默认值用于寒冬折扣的保守兜底。
    "ai_revenue_share_defaults": {
        "semiconductor": 0.60,
        "ai_chip": 0.70,
        "storage": 0.30,
    },
    # 超大市值移动科技公司的成长分支仅影响乐观情景；保守卡片仍按汽车/重资产底线。
    "mobility_technology": {
        "min_market_cap": 1e11,
        "large_ev_market_cap": 5e11,
        "min_growth": 0.05,
        # 自动驾驶/机器人强信号的数量与市值门槛必须显式配置，避免在代码中散落硬编码。
        "strong_signal_min_count": 2,
        "strong_signal_market_cap": 1e11,
    },
    # 超大市值互联网平台的 AI 推荐、广告基础设施和大模型业务只进入乐观情景。
    "internet_platform": {
        "ai_premium_market_cap": 1e12,
        "base_ps_low": 3.0,
        "base_ps_high": 8.0,
        "ai_ps_high": 12.0,
    },
    "winter_model": {
        # 寒冬盈利基准取去峰值后最低若干年均值，而不是高景气中位数。
        "eps_floor_years": 3,
        "pe_low": 8.0,
        "pe_high": 12.0,
        "ai_share_discount_high": 0.40,
        "ai_share_discount_medium": 0.80,
        "ai_share_discount_low": 1.0,
    },
    "margins": {
        "banking": 0.25,
        "insurance": 0.35,
        "reit": 0.25,
        "biotech": 0.45,
        "energy_mining": 0.35,
        "utility": 0.18,
        "internet_platform": 0.40,
        "ai_storage_cycle": 0.40,
        "ai_storage_growth": 0.35,
        "cruise": 0.25,
        "digital_retail": 0.20,
        "asset_heavy_transition": 0.45,
        "high_growth_chip": 0.30,
        "transition": 0.42,
        "transition_high_beta": 0.47,
        "cyclical": 0.62,
        "high_growth": 0.45,
        "negative_fcf": 0.38,
        "stable": 0.18,
        "stable_high_beta": 0.20,
        "mature_growth": 0.20,
        "mature_growth_high_beta": 0.25,
        "default": 0.30,
    },
    "owner_earnings": {
        "default_maintenance_capex_ratio": 0.70,
        "mature_manufacturing_maintenance_capex_ratio": 0.85,
        "technology_maintenance_capex_ratio": 0.50,
        "asset_heavy_transition_maintenance_capex_ratio": 0.30,
    },
    # Yahoo industryKey 比展示名称稳定；映射结果作为结构化类别信号使用，
    # 不直接拼入自由文本，也不覆盖业务摘要中的明确事实。
    "industry_key_map": {
        "semiconductor-memory": "memory_chip",
        "semiconductors": "semiconductor",
        "software-infrastructure": "software",
        "software-application": "software",
        "auto-manufacturers": "automotive",
        "computer-hardware": "computer_hardware",
        "aerospace-defense": "defense",
        "internet-content-information": "internet_platform",
        "internet-retail": "internet_platform",
        "reit-residential": "reit",
        "reit-retail": "reit",
        "reit-office": "reit",
        "reit-diversified": "reit",
        "banks-diversified": "banking",
        "insurance-diversified": "insurance",
        "biotechnology": "biotech",
        "utilities-regulated-electric": "utility",
    },
}

