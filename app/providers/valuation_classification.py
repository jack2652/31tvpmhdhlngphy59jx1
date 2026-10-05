"""Industry and company-lifecycle classification for valuation models."""
from __future__ import annotations
import logging
from typing import Any
from app.providers.market_common import VALUATION_CONFIG, _valuation_debug

logger = logging.getLogger("app.providers.market")

def classify_company(
    info: Any,
    symbol: Any,
    sector: Any,
    industry: Any,
    industry_key: Any,
    market_cap: Any,
    market_cap_quality: Any,
    growth: Any,
    revenue_growth: Any,
    defense_revenue_growth: Any,
    fcf_values: Any,
    operating_cashflow_values: Any,
    capex_values: Any,
    forward_eps: Any,
    normalized_eps: Any,
    forward_eps_observed: Any,
    price_to_sales: Any,
) -> dict[str, Any]:
    """Derive industry/model flags from company metadata and operating signals."""
    # Yahoo 的 industry 字段可能把国防/激光公司归为 Semiconductors 或
    # Electronic Components；业务摘要通常包含 aerospace、defense、laser、
    # directed energy 等更准确的业务线索，因此将其一并用于行业识别。
    business_summary = str(info.get("longBusinessSummary") or "").lower()
    industry_key_normalized = str(industry_key or "").strip().lower().replace("_", "-")
    industry_key_text = industry_key_normalized.replace("-", " ")
    industry_key_category = str(
        VALUATION_CONFIG.get("industry_key_map", {}).get(industry_key_normalized) or ""
    ).lower()
    # 映射类别是结构化标签，不拼进自由文本，避免新类别值凭空制造关键词命中。
    classification_text = f"{sector} {industry} {industry_key_text} {business_summary}".lower().replace("_", " ")
    mobility_technology_keywords = (
        "autonomous",
        "self-driving",
        "self driving",
        "full self-driving",
        "full self driving",
        "robotaxi",
        "robotics",
        "optimus",
        "humanoid",
        "robot",
        "autopilot",
        "fsd",
    )
    mobility_strong_signals = (
        "self-driving",
        "self driving",
        "full self-driving",
        "full self driving",
        "robotaxi",
        "autopilot",
        "fsd",
        "humanoid",
        "optimus",
    )
    electric_mobility_keywords = ("electric vehicle", "electric vehicles", "ev maker", "battery electric")
    sector_text = str(sector or "").lower().replace("_", " ")
    industry_text = f"{industry} {industry_key_text}".lower().replace("_", " ")
    category_is_memory_chip = industry_key_category == "memory_chip"
    category_is_computer_hardware = industry_key_category == "computer_hardware"
    category_is_defense = industry_key_category == "defense"
    category_is_bank = industry_key_category == "banking"
    category_is_insurance = industry_key_category == "insurance"
    category_is_reit = industry_key_category == "reit"
    category_is_biotech = industry_key_category == "biotech"
    category_is_utility = industry_key_category == "utility"
    category_is_internet_platform = industry_key_category == "internet_platform"
    mobility_industry_context = any(
        word in f"{sector_text} {industry_text}"
        for word in ("auto", "automotive", "electric vehicle", "mobility", "transportation", "ride-hailing")
    )
    mobility_signal_count = sum(word in classification_text for word in mobility_strong_signals)
    mobility_cfg = VALUATION_CONFIG.get("mobility_technology", {})
    try:
        mobility_signal_min_count = int(mobility_cfg.get("strong_signal_min_count", 2))
        mobility_signal_market_cap = float(mobility_cfg.get("strong_signal_market_cap", 1e11))
    except (TypeError, ValueError):
        mobility_signal_min_count, mobility_signal_market_cap = 2, 1e11
    # “autonomous database”“robotic process automation”等企业软件术语不能单独
    # 触发移动科技模型。汽车/移动行业需要行业上下文；没有上下文时，至少要有
    # 两个明确指向自动驾驶、Robotaxi 或人形机器人的强信号。
    mobility_technology_tag = bool(
        (
            mobility_industry_context
            and any(word in classification_text for word in mobility_technology_keywords)
        )
        or (
            mobility_signal_count >= mobility_signal_min_count
            and market_cap is not None
            and market_cap_quality not in {"unit_mismatch", "market_cap_preferred"}
            and market_cap >= mobility_signal_market_cap
        )
    )
    electric_mobility_tag = any(word in classification_text for word in electric_mobility_keywords)
    # 第一级：行业过滤。Yahoo 不一定返回标准 GICS 名称，因此同时接受
    # Financial Services/Real Estate 等常见别名；金融和地产禁止进入 DCF。
    financial_or_real_estate = (
        category_is_bank
        or category_is_insurance
        or category_is_reit
        or any(
            word in f"{sector_text} {industry_text}"
            for word in ("financial", "bank", "insurance", "capital markets", "credit", "real estate", "reit", "property")
        )
    )
    cruise_operator = bool(
        any(word in classification_text for word in ("cruise", "cruise lines", "hotels resorts cruise", "travel services", "resorts"))
    )
    # LASR 是国防订单驱动的激光技术转型公司。它可能仍有 GAAP 微利、
    # 研发投入或阶段性负现金流，但已有高增长订单时不能落入 INTC/ORCL
    # 的失败型重资产压力测试；该分类必须在通用重资产判断之前抢占。
    defense_industry_tag = category_is_defense or any(
        word in classification_text for word in ("defense", "aerospace", "laser", "military", "directed energy")
    )
    defense_transition = bool(
        (defense_revenue_growth is not None and defense_revenue_growth >= 0.30)
        or (
            defense_industry_tag
            and (
                growth >= 0.10
                or (revenue_growth is not None and revenue_growth >= 0.10)
                or (defense_revenue_growth is not None and defense_revenue_growth >= 0.30)
                or bool(fcf_values and fcf_values[0] < 0)
            )
        )
    )
    if defense_transition:
        _valuation_debug(
            "国防订单驱动转型分类：symbol=%s defense_industry_tag=%s classification_text=%s",
            symbol or "未知",
            defense_industry_tag,
            classification_text[:200],
        )
    # AMD/NVDA 等芯片公司的 GAAP EPS 常被并购摊销和研发投入压低，
    # 先识别这类公司，再决定远期 EPS 的上限，避免把增长预期截断成普通周期股。
    memory_chip = category_is_memory_chip or any(word in classification_text for word in ("memory", "dram", "nand", "flash"))
    # 软件公司的业务摘要常出现 cloud/data storage/storage infrastructure；
    # 这些是服务对象或功能，不代表公司制造存储硬件。只有明确的物理存储
    # 产品信号，或“Computer Hardware”行业同时出现 storage/disk，才命中。
    storage_hardware_strong_signals = (
        "hard disk",
        "disk drive",
        "hdd",
        "nand",
        "flash memory",
        "solid state drive",
        "ssd",
        "memory chip",
        "storage chip",
        "storage device",
        "storage hardware",
        "storage products",
        "storage systems",
        "enterprise storage",
        "computer storage",
    )
    storage_hardware = bool(
        any(word in classification_text for word in storage_hardware_strong_signals)
        or (
            ("computer hardware" in classification_text or category_is_computer_hardware)
            and any(word in classification_text for word in ("storage", "disk"))
        )
    )
    # 存储硬件既有周期属性，又可能处在 AI 需求带来的景气上行阶段；
    # 普通软件公司仅提到 cloud/data storage 时不会进入该模型。
    ai_storage_cycle = bool(
        not financial_or_real_estate
        and (
            memory_chip
            or (
                storage_hardware
                and (
                    growth >= 0.20
                    or (revenue_growth is not None and revenue_growth >= 0.20)
                    or (
                        forward_eps is not None
                        and normalized_eps is not None
                        and forward_eps >= normalized_eps * 1.25
                    )
                )
            )
        )
    )
    asset_heavy_transition = bool(
        not defense_transition
        and not ai_storage_cycle
        and (
            not memory_chip
            and not cruise_operator
            and (
                (operating_cashflow_values and capex_values and abs(capex_values[0]) >= abs(operating_cashflow_values[0]) * 0.70)
                or (fcf_values and fcf_values[0] < 0)
            )
        )
    )
    high_growth_chip_signal = bool(
        not asset_heavy_transition
        and not ai_storage_cycle
        and not cruise_operator
        and not defense_industry_tag
        and (
            not memory_chip
            and any(word in classification_text for word in ("semiconductor", "ai chip", "graphics processor"))
            and growth >= 0.25
            and (price_to_sales is None or price_to_sales > 10.0)
        )
    )
    # 没有分析师远期 EPS 时不启用芯片 PEG/高增长估值，后续回退到
    # DCF、EV/EBITDA 或历史盈利模型，并把降级原因写入 warnings。
    high_growth_chip = high_growth_chip_signal and forward_eps_observed
    # 行业专用模型优先于普通 DCF/PE。字段命名同时兼容 Yahoo 行业名和
    # GICS/供应商自定义标签，缺少专用指标时再回退到可观察的公开字段。
    banking_model = category_is_bank or bool(any(word in classification_text for word in ("bank", "banking", "regional bank", "diversified banks")))
    insurance_model = category_is_insurance or bool(any(word in classification_text for word in ("insurance", "life insurance", "property & casualty", "reinsurance", "insurer")))
    reit_model = category_is_reit or bool(any(word in classification_text for word in ("reit", "real estate investment trust")))
    biotech_model = category_is_biotech or bool(any(word in classification_text for word in ("biotechnology", "biotech", "drug manufacturer", "pharmaceutical", "drug manufacturers")))
    # “directed energy”是国防激光业务的术语，不能因为业务摘要出现 energy
    # 就误判为能源/矿业公司；国防标签命中时保留 defense_transition 优先级。
    energy_mining_model = bool(
        not defense_industry_tag
        and not mobility_technology_tag
        and any(word in classification_text for word in ("energy", "oil", "gas", "coal", "uranium", "mining", "gold", "silver", "copper"))
    )
    # 业务摘要中的 “utilities” 可能只是客户群体（例如“为公用事业公司提供
    # 储能产品”），不能据此把电动车/自动驾驶公司本身归类为公用事业。
    utility_model = bool(
        category_is_utility
        or (
            not mobility_technology_tag
            and any(word in classification_text for word in ("utilities", "utility", "regulated electric", "electric utilities", "water utilities", "gas utilities"))
        )
    )
    internet_platform_model = bool(
        category_is_internet_platform
        or any(word in classification_text for word in ("internet content", "internet retail", "interactive media", "social media", "e commerce", "e-commerce", "online platform", "internet services"))
        or ("communication services" in sector_text and any(word in classification_text for word in ("internet", "media", "interactive", "social")))
    )
    internet_ai_keywords = (
        "artificial intelligence",
        "generative ai",
        "large language model",
        "machine learning",
        "ai infrastructure",
        "ai recommendation",
        "recommendation engine",
        "ad ranking",
        "personalized ads",
        "ai-powered",
        "ai powered",
        "ai-driven",
        "ai driven",
        "ai models",
        "llama",
    )
    internet_ai_platform = bool(
        internet_platform_model
        and any(word in classification_text for word in internet_ai_keywords)
        and market_cap is not None
        and market_cap >= float(VALUATION_CONFIG.get("internet_platform", {}).get("ai_premium_market_cap", 1e12))
    )
    if internet_platform_model:
        _valuation_debug(
            "互联网平台估值输入：symbol=%s ai_platform=%s market_cap=%s summary=%s",
            symbol or "未知",
            internet_ai_platform,
            market_cap,
            classification_text[:200],
        )
    # 移动科技公司的业务摘要经常同时提到汽车保险、储能客户、公用事业或
    # 能源业务。这些是附属业务/客户，不应把 TSLA、RIVN 等锁进金融、能源、
    # 公用事业或互联网平台专用估值模型；移动科技识别优先于关键词分类。
    if mobility_technology_tag:
        banking_model = False
        insurance_model = False
        reit_model = False
        biotech_model = False
        energy_mining_model = False
        utility_model = False
        internet_platform_model = False
        # TSLA 等移动科技公司的摘要也会提到 storage/battery/energy，
        # 这些是附属业务或产品线，不应把公司误判为 AI 存储周期股。
        ai_storage_cycle = False
        storage_hardware = False
    specialized_industry = any((banking_model, insurance_model, reit_model, biotech_model, energy_mining_model, utility_model, internet_platform_model))
    if specialized_industry:
        _valuation_debug(
            "专用行业模型命中：symbol=%s banking=%s insurance=%s reit=%s biotech=%s energy=%s utility=%s internet=%s",
            symbol or "未知",
            banking_model,
            insurance_model,
            reit_model,
            biotech_model,
            energy_mining_model,
            utility_model,
            internet_platform_model,
        )

    return {
        'ai_storage_cycle': ai_storage_cycle,
        'asset_heavy_transition': asset_heavy_transition,
        'banking_model': banking_model,
        'biotech_model': biotech_model,
        'business_summary': business_summary,
        'category_is_bank': category_is_bank,
        'category_is_biotech': category_is_biotech,
        'category_is_computer_hardware': category_is_computer_hardware,
        'category_is_defense': category_is_defense,
        'category_is_insurance': category_is_insurance,
        'category_is_internet_platform': category_is_internet_platform,
        'category_is_memory_chip': category_is_memory_chip,
        'category_is_reit': category_is_reit,
        'category_is_utility': category_is_utility,
        'classification_text': classification_text,
        'cruise_operator': cruise_operator,
        'defense_industry_tag': defense_industry_tag,
        'defense_transition': defense_transition,
        'electric_mobility_keywords': electric_mobility_keywords,
        'electric_mobility_tag': electric_mobility_tag,
        'energy_mining_model': energy_mining_model,
        'financial_or_real_estate': financial_or_real_estate,
        'high_growth_chip': high_growth_chip,
        'high_growth_chip_signal': high_growth_chip_signal,
        'industry_key_category': industry_key_category,
        'industry_key_normalized': industry_key_normalized,
        'industry_key_text': industry_key_text,
        'industry_text': industry_text,
        'insurance_model': insurance_model,
        'internet_ai_keywords': internet_ai_keywords,
        'internet_ai_platform': internet_ai_platform,
        'internet_platform_model': internet_platform_model,
        'memory_chip': memory_chip,
        'mobility_cfg': mobility_cfg,
        'mobility_industry_context': mobility_industry_context,
        'mobility_signal_count': mobility_signal_count,
        'mobility_signal_market_cap': mobility_signal_market_cap,
        'mobility_signal_min_count': mobility_signal_min_count,
        'mobility_strong_signals': mobility_strong_signals,
        'mobility_technology_keywords': mobility_technology_keywords,
        'mobility_technology_tag': mobility_technology_tag,
        'reit_model': reit_model,
        'sector_text': sector_text,
        'specialized_industry': specialized_industry,
        'storage_hardware': storage_hardware,
        'storage_hardware_strong_signals': storage_hardware_strong_signals,
        'utility_model': utility_model,
    }
