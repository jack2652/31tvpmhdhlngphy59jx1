"""保守盈利估值策略；由估值协调器混入以保持原 provider 方法 API。"""
from __future__ import annotations
import logging
import math
from typing import Any
from app.providers.market_common import FAIR_VALUE_SOURCE, MarketRegime, VALUATION_CONFIG, _valuation_debug
from app.providers.valuation_classification import classify_company
logger = logging.getLogger("app.providers.market")

class EarningsValuationMixin:
    @classmethod
    def _conservative_earnings(
        cls,
        info: dict[str, Any],
        *,
        symbol: str | None = None,
        eps_values: list[float] | None = None,
        annual_eps_values: list[float] | None = None,
        fcf_values: list[float] | None = None,
        shares_values: list[float] | None = None,
        shares_source: str | None = None,
        debt_values: list[float] | None = None,
        cash_values: list[float] | None = None,
        timeseries: dict[str, list[dict[str, Any]]] | None = None,
        forward_eps: float | None = None,
        current_price: float | None = None,
        growth_hint: float | None = None,
        beta: float | None = None,
        sector: str = "",
        industry: str = "",
        industry_key: str = "",
        revenue_values: list[float] | None = None,
        quarterly_revenue_values: list[float] | None = None,
        quarterly_eps_values: list[float] | None = None,
        operating_income_values: list[float] | None = None,
        net_income_values: list[float] | None = None,
        da_values: list[float] | None = None,
        capex_values: list[float] | None = None,
        working_capital_values: list[float] | None = None,
        buyback_values: list[float] | None = None,
        historical_price_values: list[float] | None = None,
        equity_values: list[float] | None = None,
        operating_cashflow_values: list[float] | None = None,
        market_cap: float | None = None,
        dividend_rate: float | None = None,
        book_value: float | None = None,
        defense_revenue_growth: float | None = None,
        target_mean_price: float | None = None,
        target_low_price: float | None = None,
        target_high_price: float | None = None,
        price_to_sales: float | None = None,
        enterprise_to_ebitda: float | None = None,
        forward_ebitda: float | None = None,
        interest_expense: float | None = None,
        revenue_growth: float | None = None,
        roe: float | None = None,
        ffo_per_share: float | None = None,
        affo_per_share: float | None = None,
        nav_per_share: float | None = None,
        rab_per_share: float | None = None,
        embedded_value_per_share: float | None = None,
        new_business_value_per_share: float | None = None,
        pipeline_rnpv: float | None = None,
        approved_drug_value: float | None = None,
        platform_users: float | None = None,
        arpu: float | None = None,
        churn_rate: float | None = None,
        ai_revenue_share: float | None = None,
        analyst_count: float | None = None,
        regime: str = MarketRegime.NORMAL,
        regime_signals: dict[str, Any] | None = None,
        upstream_errors: list[str] | None = None,
    ) -> dict[str, Any]:
        """按公司生命周期选择估值模型，并同时生成防守与进攻两个区间。"""
        ttm_eps = eps_values[0] if eps_values and eps_values[0] > 0 else cls._info_number(info, "trailingEps")
        annual = [value for value in (annual_eps_values or []) if value > 0 and math.isfinite(value)]
        historical_eps = ([ttm_eps] if ttm_eps else []) + annual[:5]
        normalized_eps = sorted(historical_eps)[len(historical_eps) // 2] if historical_eps else None
        normalized_eps_source = "历史 EPS 中位数" if normalized_eps is not None else None
        current_shares_for_normalization = next(
            (value for value in (shares_values or []) if value and value > 0 and math.isfinite(value)),
            None,
        )
        normalized_net_income = [
            value / current_shares_for_normalization
            for value in (net_income_values or [])
            if current_shares_for_normalization and value and value > 0 and math.isfinite(value)
        ]
        if len(normalized_net_income) >= 2:
            normalized_net_income_eps = sorted(normalized_net_income)[len(normalized_net_income) // 2]
            # 报表接口偶尔混用美元、千美元和百万美元；在切换到净利润/当前股本
            # 前先与历史 EPS 中位数做量纲检查，避免一个单位错误把保守估值放大数十倍。
            eps_ratio_ok = True
            if normalized_eps and normalized_eps > 0:
                eps_ratio = normalized_net_income_eps / normalized_eps
                eps_cfg = VALUATION_CONFIG.get("normalized_eps", {})
                eps_ratio_ok = (
                    float(eps_cfg.get("min_ratio", 0.25))
                    <= eps_ratio
                    <= float(eps_cfg.get("max_ratio", 4.0))
                )
            if eps_ratio_ok:
                normalized_eps = normalized_net_income_eps
                normalized_eps_source = "净利润 / 当前股本"
            else:
                normalized_eps_source = "历史 EPS 中位数（净利润股本量纲异常回退）"
        forward_eps_observed = forward_eps is not None and math.isfinite(forward_eps) and forward_eps > 0
        if forward_eps is not None and forward_eps <= 0:
            forward_eps = None
        # 没有分析师远期 EPS 时，用最近盈利和可观察增长率做模型估计，并在结果中标注来源。
        growth = growth_hint
        if growth is None and len(annual) >= 2 and annual[-1] > 0:
            growth = (annual[0] / annual[-1]) ** (1 / (len(annual) - 1)) - 1
        if growth is None and revenue_values and len(revenue_values) >= 2 and revenue_values[-1] > 0:
            growth = (revenue_values[0] / revenue_values[-1]) ** (1 / (len(revenue_values) - 1)) - 1
        growth = min(0.50, max(-0.15, growth or 0.0))
        # Yahoo 的 marketCap 可能滞后于现价或使用了不同股本口径；先校正市值，
        # 再参与行业门槛判断，避免 ONDS 这类标的在错误市值下走错分支。
        input_shares = next(
            (value for value in (shares_values or []) if value and value > 0 and math.isfinite(value)),
            None,
        )
        if shares_source is None and input_shares is not None:
            # 直接调用估值函数时无法知道上游来源；将其标成 provided，按保守口径
            # 处理“推导市值更低”的情况，避免未知股本未经确认就压低市值。
            shares_source = "provided"
        implied_market_cap = input_shares * current_price if input_shares and current_price and current_price > 0 else None
        market_cap_quality = "unknown"
        market_cap_warning = None
        reconciliation_cfg = VALUATION_CONFIG.get("market_cap_reconciliation", {})
        mismatch_threshold = float(
            reconciliation_cfg.get(
                "mismatch_threshold",
                VALUATION_CONFIG.get("market_cap_mismatch_threshold", 0.20),
            )
        )
        trusted_shares_sources = set(reconciliation_cfg.get("trusted_shares_sources", ("balance_sheet", "info", "fast_info")))
        shares_source_trusted = shares_source in trusted_shares_sources
        if market_cap and market_cap > 0 and implied_market_cap and implied_market_cap > 0:
            mismatch_ratio = abs(market_cap - implied_market_cap) / market_cap
            if mismatch_ratio > mismatch_threshold:
                scale_ratio = market_cap / implied_market_cap
                # 测试/上游有时把 shares 以“十亿股”返回，而 marketCap 仍是原始美元；
                # 这种百万倍偏差是单位问题，不能把市值改成几百万美元。
                if scale_ratio > 100.0 or scale_ratio < 0.01:
                    market_cap_quality = "unit_mismatch"
                    market_cap_warning = "market_cap 与股本×现价存在数量级差异，已保留市值并降低数据质量评分"
                elif not shares_source_trusted:
                    # 方向必须对称：年度时序股数、provided 或未知来源都可能滞后/稀释；
                    # 无法证明当前口径时，无论推导市值更高还是更低，都保留 Yahoo market_cap，
                    # 避免把错误股本带入大市值门槛或把真实市值压低。
                    market_cap_quality = "market_cap_preferred"
                    market_cap_warning = (
                        f"股本来源 {shares_source or 'unknown'} 未通过当前口径核验，"
                        "已保留 Yahoo market_cap"
                    )
                else:
                    market_cap = implied_market_cap
                    market_cap_quality = "reconciled"
                    market_cap_warning = (
                        f"market_cap 与股本×现价偏差超过 {mismatch_threshold:.0%}，"
                        f"已使用{shares_source or '可用股本'}×现价校正市值"
                    )
            else:
                market_cap_quality = "consistent"
        elif market_cap is None and implied_market_cap:
            market_cap = implied_market_cap
            market_cap_quality = "derived"
        classification = classify_company(
            info=info,
            symbol=symbol,
            sector=sector,
            industry=industry,
            industry_key=industry_key,
            market_cap=market_cap,
            market_cap_quality=market_cap_quality,
            growth=growth,
            revenue_growth=revenue_growth,
            defense_revenue_growth=defense_revenue_growth,
            fcf_values=fcf_values,
            operating_cashflow_values=operating_cashflow_values,
            capex_values=capex_values,
            forward_eps=forward_eps,
            normalized_eps=normalized_eps,
            forward_eps_observed=forward_eps_observed,
            price_to_sales=price_to_sales,
        )
        ai_storage_cycle = classification['ai_storage_cycle']
        asset_heavy_transition = classification['asset_heavy_transition']
        banking_model = classification['banking_model']
        biotech_model = classification['biotech_model']
        business_summary = classification['business_summary']
        category_is_bank = classification['category_is_bank']
        category_is_biotech = classification['category_is_biotech']
        category_is_computer_hardware = classification['category_is_computer_hardware']
        category_is_defense = classification['category_is_defense']
        category_is_insurance = classification['category_is_insurance']
        category_is_internet_platform = classification['category_is_internet_platform']
        category_is_memory_chip = classification['category_is_memory_chip']
        category_is_reit = classification['category_is_reit']
        category_is_utility = classification['category_is_utility']
        classification_text = classification['classification_text']
        cruise_operator = classification['cruise_operator']
        defense_industry_tag = classification['defense_industry_tag']
        defense_transition = classification['defense_transition']
        electric_mobility_keywords = classification['electric_mobility_keywords']
        electric_mobility_tag = classification['electric_mobility_tag']
        energy_mining_model = classification['energy_mining_model']
        financial_or_real_estate = classification['financial_or_real_estate']
        high_growth_chip = classification['high_growth_chip']
        high_growth_chip_signal = classification['high_growth_chip_signal']
        industry_key_category = classification['industry_key_category']
        industry_key_normalized = classification['industry_key_normalized']
        industry_key_text = classification['industry_key_text']
        industry_text = classification['industry_text']
        insurance_model = classification['insurance_model']
        internet_ai_keywords = classification['internet_ai_keywords']
        internet_ai_platform = classification['internet_ai_platform']
        internet_platform_model = classification['internet_platform_model']
        memory_chip = classification['memory_chip']
        mobility_cfg = classification['mobility_cfg']
        mobility_industry_context = classification['mobility_industry_context']
        mobility_signal_count = classification['mobility_signal_count']
        mobility_signal_market_cap = classification['mobility_signal_market_cap']
        mobility_signal_min_count = classification['mobility_signal_min_count']
        mobility_strong_signals = classification['mobility_strong_signals']
        mobility_technology_keywords = classification['mobility_technology_keywords']
        mobility_technology_tag = classification['mobility_technology_tag']
        reit_model = classification['reit_model']
        sector_text = classification['sector_text']
        specialized_industry = classification['specialized_industry']
        storage_hardware = classification['storage_hardware']
        storage_hardware_strong_signals = classification['storage_hardware_strong_signals']
        utility_model = classification['utility_model']
        if financial_or_real_estate:
            # 行业过滤优先级最高，避免后面的负 FCF/资本开支信号把银行、REIT
            # 误判成转型公司。后续只允许 DDM/P/B 候选。
            defense_transition = False
            asset_heavy_transition = False
            high_growth_chip = False
            cruise_operator = False

        if forward_eps is not None and normalized_eps:
            # 普通公司仍保留 3 倍上限；高增长芯片和 AI 存储周期股放宽至 5 倍，
            # 使分析师对景气周期的远期 EPS 不会被 GAAP 正常化利润过早截断。
            growth_forward_class = high_growth_chip or ai_storage_cycle
            forward_cap = max(normalized_eps * (5.0 if growth_forward_class else 3.0), 12.0 if growth_forward_class else 0.0)
            # 分析师共识硬锚定：剥离/转型公司的 Yahoo EPS 可能出现单位或口径异常，
            # 用目标均价反推一个不超过 35 倍 PE 的远期 EPS 上限，避免把模型放大数倍。
            if growth_forward_class and target_mean_price and target_mean_price > 0:
                forward_cap = min(forward_cap, target_mean_price / 35.0)
            forward_eps = min(forward_eps, forward_cap)
        model_forward_eps = forward_eps or (normalized_eps * (1 + growth) if normalized_eps else None)
        forward_source = "Yahoo远期EPS" if forward_eps is not None else ("模型估计远期EPS" if model_forward_eps else None)
        forward_eps_missing_warning = bool(
            not forward_eps_observed
            and (high_growth_chip_signal or ai_storage_cycle or defense_transition)
        )
        if forward_eps_missing_warning and (high_growth_chip_signal or ai_storage_cycle):
            # 高增长/存储模型不得把历史 EPS × 增长率伪装成分析师远期 EPS。
            # 保留行业分类以便走周期或现金流交叉验证，但禁用远期 PEG 主路径。
            model_forward_eps = None
            forward_source = None
            ai_storage_cycle = False
            high_growth_chip = False

        latest_fcf = fcf_values[0] if fcf_values else None
        shares = shares_values[0] if shares_values and shares_values[0] > 0 else None
        debt = debt_values[0] if debt_values else 0.0
        cash = cash_values[0] if cash_values else 0.0
        latest_revenue = revenue_values[0] if revenue_values else None
        op_income = operating_income_values[0] if operating_income_values else None
        da = da_values[0] if da_values else 0.0
        capex = capex_values[0] if capex_values else 0.0
        working_capital = working_capital_values[0] if working_capital_values else 0.0
        quarterly_momentum = None
        if quarterly_revenue_values and len(quarterly_revenue_values) >= 4:
            latest_quarter = quarterly_revenue_values[0]
            year_ago_quarter = quarterly_revenue_values[3]
            yoy_growth = latest_quarter / year_ago_quarter - 1 if year_ago_quarter > 0 else None
            qoq_growth = latest_quarter / quarterly_revenue_values[1] - 1 if quarterly_revenue_values[1] > 0 else None
            quarterly_momentum = {
                "revenue_yoy": round(yoy_growth, 4) if yoy_growth is not None else None,
                "revenue_qoq": round(qoq_growth, 4) if qoq_growth is not None else None,
                "eps_yoy": (
                    round(quarterly_eps_values[0] / quarterly_eps_values[3] - 1, 4)
                    if quarterly_eps_values and len(quarterly_eps_values) >= 4 and quarterly_eps_values[3] > 0
                    else None
                ),
            }
        historical_valuation_percentiles = cls._historical_valuation_percentiles(
            current_price=current_price,
            historical_price_values=historical_price_values,
            annual_eps_values=annual_eps_values,
            ttm_eps=ttm_eps,
            debt=debt,
            cash=cash,
            shares=shares,
            debt_values=debt_values,
            cash_values=cash_values,
            shares_values=shares_values,
            equity_values=equity_values,
            book_value=book_value,
            operating_income_values=operating_income_values,
            da_values=da_values,
        )
        shareholder_total_return_yield = None
        if current_price and current_price > 0 and shares and shares > 0:
            dividend_per_share = dividend_rate if dividend_rate and dividend_rate > 0 else 0.0
            latest_buyback = buyback_values[0] if buyback_values else None
            buyback_per_share = abs(latest_buyback) / shares if latest_buyback and math.isfinite(latest_buyback) else 0.0
            total_return = (dividend_per_share + buyback_per_share) / current_price
            if math.isfinite(total_return) and 0 <= total_return <= 1.0:
                shareholder_total_return_yield = round(total_return, 4)
        _valuation_debug(
            "估值诊断 symbol=%s | sector=%s industry=%s | rev_series(前5)=%s | shares=%s(%s) | fwd_eps=%s | ttm_eps=%s | growth=%s | market_cap=%s | market_cap_quality=%s",
            symbol or "未知",
            sector,
            industry,
            [round(value / 1e9, 2) for value in (revenue_values or [])[:5]],
            f"{shares / 1e9:.3f}B" if shares is not None else None,
            shares_source,
            forward_eps,
            ttm_eps,
            round(growth, 4) if growth is not None else None,
            f"{market_cap / 1e9:.1f}B" if market_cap is not None else None,
            market_cap_quality,
        )
        ebitda = (op_income + abs(da)) if op_income is not None else None
        interest_amount = abs(interest_expense) if interest_expense is not None else None
        if interest_amount is not None and interest_amount <= 1.0 and latest_revenue and latest_revenue > 0:
            # Yahoo 某些字段返回利息/营收比例；按比例换算成金额后再计算覆盖率。
            interest_amount *= latest_revenue
        interest_coverage = None
        if op_income is not None and interest_amount and interest_amount > 0:
            interest_coverage = op_income / interest_amount
        low_interest_coverage = interest_coverage is not None and interest_coverage < float(VALUATION_CONFIG["interest_coverage_warning"])
        owner_earnings_ratio, owner_earnings_ratio_source = cls._owner_earnings_maintenance_ratio(
            sector,
            industry,
            industry_key,
            asset_heavy_transition,
        )
        owner_earnings = None
        if net_income_values:
            owner_earnings = net_income_values[0] + abs(da) - abs(capex) * owner_earnings_ratio - max(working_capital, 0.0)
        if owner_earnings is None and latest_fcf is not None:
            owner_earnings = latest_fcf
        data_quality_score = 100.0
        if market_cap and market_cap < float(VALUATION_CONFIG.get("small_cap_threshold", 5e9)):
            data_quality_score -= 10.0
        if market_cap_quality in ("reconciled", "unit_mismatch", "market_cap_preferred"):
            data_quality_score -= 15.0
        if not revenue_values or any(value <= 0 or not math.isfinite(value) for value in revenue_values):
            data_quality_score -= 15.0
        if analyst_count is not None and analyst_count < 3:
            data_quality_score -= 20.0
        elif analyst_count is None and target_mean_price is None:
            data_quality_score -= 10.0
        data_quality_score = round(min(100.0, max(0.0, data_quality_score)), 1)

        text = f"{sector} {industry} {industry_key}".lower().replace("_", " ")
        # 只把内存、能源、航运等强周期行业归入周期模型；普通半导体（如 AMD/NVDA）
        # 仍允许使用远期盈利模型，避免把结构性成长误判为存储器周期。
        cyclical = ai_storage_cycle or any(word in text for word in ("semiconductor memory", "memory chip", "memory", "dram", "nand", "energy", "oil", "shipping", "airline"))
        stable = any(word in text for word in ("consumer defensive", "consumer staples", "restaurant", "retail", "food", "beverage", "discount stores"))
        digital_retail = bool(
            stable
            and market_cap
            and market_cap >= 1e11
            and any(word in text for word in ("retail", "discount stores", "consumer defensive", "consumer staples"))
        )
        cycle_eps = normalized_eps
        if cyclical and len(historical_eps) >= 3:
            # 去掉一个异常高峰后再取中位数，避免只有少数高景气年度时仍把周期顶点当常态。
            trimmed = sorted(historical_eps)[:-1]
            cycle_eps = trimmed[len(trimmed) // 2] if trimmed else normalized_eps
        ai_revenue_share_value = ai_revenue_share
        if ai_revenue_share_value is not None and ai_revenue_share_value > 1:
            ai_revenue_share_value /= 100.0
        ai_revenue_share_value = min(1.0, max(0.0, ai_revenue_share_value or 0.0))
        if ai_revenue_share_value <= 0 and not defense_industry_tag:
            defaults = VALUATION_CONFIG.get("ai_revenue_share_defaults", {})
            if "ai chip" in classification_text or high_growth_chip_signal:
                ai_revenue_share_value = float(defaults.get("ai_chip", 0.70))
            elif "semiconductor" in classification_text:
                ai_revenue_share_value = float(defaults.get("semiconductor", 0.60))
            elif "storage" in classification_text:
                ai_revenue_share_value = float(defaults.get("storage", 0.30))
            ai_revenue_share_value = min(1.0, max(0.0, ai_revenue_share_value))
        ai_exposed = bool(
            not specialized_industry
            and (
                high_growth_chip_signal
                or ai_storage_cycle
                or storage_hardware
                or any(word in classification_text for word in ("semiconductor", "ai chip", "graphics processor"))
                or mobility_technology_tag
                or ai_revenue_share_value > 0
            )
        )
        ai_winter = regime == MarketRegime.WINTER and ai_exposed
        if ai_winter:
            # 寒冬模式使用 5–7 年盈利中枢，先剔除最近两个高景气峰值，
            # 再取最低若干年均值，避免 AI 高景气利润继续抬高压力测试底线；
            # 若历史盈利不足，则保留可用的正常化 EPS，不伪造长期数据。
            winter_history = [value for value in (annual_eps_values or [])[:7] if value > 0 and math.isfinite(value)]
            if len(winter_history) >= 5:
                winter_history = sorted(winter_history)[:-2]
            winter_cfg = VALUATION_CONFIG.get("winter_model", {})
            try:
                floor_years = max(1, int(winter_cfg.get("eps_floor_years", 3)))
            except (TypeError, ValueError):
                floor_years = 3
            sorted_winter_history = sorted(winter_history)
            if len(sorted_winter_history) >= floor_years:
                winter_cycle_eps = sum(sorted_winter_history[:floor_years]) / floor_years
            else:
                winter_cycle_eps = (sum(sorted_winter_history) / len(sorted_winter_history)) if sorted_winter_history else cycle_eps
            cycle_eps = winter_cycle_eps
            cyclical = True
        if regime == MarketRegime.SLOWDOWN and ai_exposed:
            # 放缓阶段把远期盈利打七折，仍保留行业模型但收紧其盈利输入。
            if model_forward_eps is not None and model_forward_eps > 0:
                model_forward_eps *= 0.70
                forward_source = f"{forward_source or '远期EPS'}（放缓调整70%）"
        high_growth = (
            growth >= 0.18
            and model_forward_eps is not None
            and model_forward_eps > 0
            and (forward_eps_observed or not high_growth_chip_signal)
        )
        if high_growth_chip and model_forward_eps is not None and model_forward_eps > 0:
            high_growth = True
        # 周期顶点识别：一年远期 EPS 相对历史正常化 EPS 翻倍以上时，
        # 视为周期利润峰值，禁用 PEG，强制回到周期中值 PE。
        cyclical_peak = cyclical and normalized_eps and model_forward_eps and model_forward_eps > normalized_eps * 2.0
        if cyclical_peak:
            high_growth = False
        negative_fcf = latest_fcf is not None and latest_fcf < 0
        beta_value = beta if beta is not None and 0.2 <= beta <= 3.0 else 1.0
        fcf_margin = (latest_fcf / latest_revenue) if latest_fcf is not None and latest_revenue and latest_revenue > 0 else None
        # 部分超大市值汽车公司同时经营自动驾驶、机器人和能源存储，Yahoo
        # 往往仍只返回 Auto Manufacturers。该分类只用于进攻情景，保守卡片
        # 继续保留重资产汽车制造的下行压力测试，且要求可观察的规模/经营信号。
        mobility_cfg = VALUATION_CONFIG.get("mobility_technology", {})
        try:
            mobility_min_market_cap = float(mobility_cfg.get("min_market_cap", 1e11))
            mobility_min_growth = float(mobility_cfg.get("min_growth", 0.05))
        except (TypeError, ValueError):
            mobility_min_market_cap, mobility_min_growth = 1e11, 0.05
        mobility_technology_growth = bool(
            not ai_winter
            and not financial_or_real_estate
            and not specialized_industry
            and not defense_transition
            and market_cap is not None
            and market_cap >= mobility_min_market_cap
            and (
                mobility_technology_tag
                or (
                    electric_mobility_tag
                    and market_cap >= max(mobility_min_market_cap, float(mobility_cfg.get("large_ev_market_cap", 5e11)))
                )
            )
            and (
                growth >= mobility_min_growth
                or (revenue_growth is not None and revenue_growth >= mobility_min_growth)
                or (fcf_margin is not None and fcf_margin > 0)
                or (target_mean_price is not None and target_mean_price > 0)
                # 超大市值且业务摘要明确包含自动驾驶/AI 的公司，即使当前收入增速
                # 放缓、自由现金流为负或缺少分析师目标价，也保留成长型期权估值。
                # 保守卡片仍走汽车制造底线，成长分支只影响乐观卡片。
                or (
                    market_cap >= float(mobility_cfg.get("large_ev_market_cap", 5e11))
                    and mobility_technology_tag
                    and fcf_values
                    and latest_revenue is not None
                    and latest_revenue > 0
                )
            )
        )
        if mobility_technology_growth:
            _valuation_debug(
                "移动科技成长分类：symbol=%s signal_count=%s/%s market_cap=%s threshold=%s quality=%s classification_text=%s",
                symbol or "未知",
                mobility_signal_count,
                mobility_signal_min_count,
                market_cap,
                mobility_signal_market_cap,
                market_cap_quality,
                classification_text[:200],
            )
        # 只有明确属于硬盘、磁盘驱动器或企业级存储的公司，才允许进入
        # “结构性增长”子类。NAND/DRAM/Flash 等内存制造商即使当前 FCF
        # 和增速很高，本质仍是强周期，不能因为景气高点而使用 35–55 倍 PE。
        structural_storage = any(
            word in classification_text
            for word in ("hard disk", "disk drive", "enterprise storage")
        )
        ai_storage_growth = bool(
            ai_storage_cycle
            and storage_hardware
            and not memory_chip
            and structural_storage
            and fcf_margin is not None
            and fcf_margin >= 0.15
            and (growth >= 0.30 or (revenue_growth is not None and revenue_growth >= 0.30))
        )
        stable_cashflow = bool(
            not financial_or_real_estate
            and any(word in classification_text for word in ("consumer defensive", "consumer staples", "restaurant", "retail", "food", "beverage"))
            and growth < 0.15
            and fcf_margin is not None
            and fcf_margin > 0.15
        )
        operating_cashflow = operating_cashflow_values[0] if operating_cashflow_values else None
        capex_latest = abs(capex_values[0]) if capex_values else 0.0
        # 先识别公司生命周期，再决定估值锚点。这样微软等成熟增长型巨头不会被
        # 当成普通 PEG 成长股，Oracle 这类重资产转型期公司也不会被强行套单一 PE。
        mature_growth = bool(
            market_cap and market_cap >= 5e11
            and latest_fcf is not None and latest_fcf > 0
            and fcf_margin is not None and fcf_margin >= 0.20
            and (
                (revenue_growth if revenue_growth is not None else growth) >= 0.06
                # AAPL/MSFT 已经是超大市值、正自由现金流的成熟增长型巨头。
                # 收入增速偶尔因财年或一次性因素缺失时，不能因此退回低估的
                # 普通 DCF/PEG 分支；现金流质量是这里更稳定的分类依据。
                or (market_cap >= 1e12 and revenue_growth is None and growth_hint is None)
            )
        )
        premium_mature_growth = bool(
            mature_growth
            and market_cap
            and market_cap >= 1e12
            and (revenue_growth if revenue_growth is not None else growth) >= 0.08
        )
        if ai_winter:
            # 市场状态只改写 AI 相关生命周期模型；行业专用模型仍保持优先级。
            high_growth_chip = False
            ai_storage_cycle = False
            ai_storage_growth = False
            mobility_technology_growth = False
            mature_growth = False
            premium_mature_growth = False
            asset_heavy_transition = False
            transition = False
        if specialized_industry:
            # 专用行业模型优先于生命周期模型，避免银行/REIT 的负 FCF、药企的
            # 研发投入或平台公司的早期亏损触发普通重资产/PEG 分支。
            defense_transition = False
            asset_heavy_transition = False
            high_growth_chip = False
            cruise_operator = False
            ai_storage_cycle = False
            ai_storage_growth = False
            mature_growth = False
            premium_mature_growth = False
            digital_retail = False
        industry_model = (
            "banking" if banking_model else
            "insurance" if insurance_model else
            "reit" if reit_model else
            "biotech" if biotech_model else
            "energy_mining" if energy_mining_model else
            "utility" if utility_model else
            "internet_platform" if internet_platform_model else
            None
        )
        transition = bool(
            not cyclical
            and (
                negative_fcf
                or (
                    operating_cashflow is not None
                    and capex_latest > 0
                    and capex_latest >= operating_cashflow * 0.85
                )
            )
        )
        # 重资产转型优先级高于高增长芯片和普通周期模型。
        transition = bool(asset_heavy_transition or transition)
        if ai_winter:
            # 寒冬状态下不让负 FCF/资本开支再次把估值送入普通转型分支。
            transition = False
        candidates: dict[str, float] = {}
        model = ""
        pe_low = pe_high = None
        industry_low = industry_high = None
        winter_value = None
        if defense_transition:
            # 国防订单驱动模型优先于通用重资产、芯片和普通转型分支。
            asset_heavy_transition = False
            high_growth_chip = False
            mature_growth = False
            digital_retail = False
            transition = False
        if ai_storage_cycle:
            # AI 存储/硬件周期优先于通用重资产和高增长芯片模型；它们使用
            # “远期 EPS 打七折 + 20–30 倍 PE”的专用估值带。
            asset_heavy_transition = False
            high_growth_chip = False
            mature_growth = False
            digital_retail = False
            transition = False
        if mobility_technology_growth:
            # 保留 asset_heavy_transition 供模型 A 计算汽车制造底线；这里只
            # 禁止它抢占模型 B 的移动科技成长分支。
            high_growth_chip = False
            mature_growth = False
            digital_retail = False
        if asset_heavy_transition:
            high_growth_chip = False
            mature_growth = False
        if digital_retail:
            # 大型数字化零售优先于普通稳定消费模型，避免低净利率把广告、会员
            # 和电商履约带来的平台溢价完全抹掉。
            mature_growth = False
            transition = False
        if cruise_operator:
            # 邮轮是已经投入运营、持续产生现金流的消费服务资产，不能和
            # INTC/ORCL 的失败型重资产转型混在一起估值。
            asset_heavy_transition = False
            high_growth_chip = False
            digital_retail = False
            mature_growth = False
            transition = False
        if mature_growth:
            high_growth = False
        # CAPM 锚定要求回报率；上游若提供国债收益率则使用，否则使用长期 10 年期近似值。
        risk_free_rate = cls._first_info_number(info, ("riskFreeRate", "treasuryYield", "us10y")) or 0.04
        if risk_free_rate > 1:
            risk_free_rate /= 100
        risk_free_rate = min(0.08, max(0.02, risk_free_rate))
        required_return = min(0.18, max(0.085, risk_free_rate + 0.055 * beta_value))
        if stable:
            # 特许经营/稳定消费公司使用 8%–9% 的窄折现率区间，避免单一 Beta
            # 让 MCD/WMT 被不必要地压低。
            required_return = min(0.09, max(0.08, required_return))
        terminal_growth = min(0.035, max(0.015, growth * 0.25))
        if ai_winter:
            winter_eps = cycle_eps or normalized_eps
            winter_cfg = VALUATION_CONFIG.get("winter_model", {})
            if ai_revenue_share_value >= 0.50:
                winter_discount = float(winter_cfg.get("ai_share_discount_high", 0.40))
            elif ai_revenue_share_value >= 0.10:
                winter_discount = float(winter_cfg.get("ai_share_discount_medium", 0.80))
            else:
                winter_discount = float(winter_cfg.get("ai_share_discount_low", 1.0))
            if winter_eps and winter_eps > 0:
                pe_low = float(winter_cfg.get("pe_low", 8.0))
                pe_high = float(winter_cfg.get("pe_high", 12.0))
                candidates["AI寒冬周期中值 PE"] = winter_eps * (pe_low + pe_high) / 2.0 * winter_discount
            if book_value and book_value > 0:
                candidates["寒冬 P/B 底线"] = book_value * winter_discount
            if shares and cash - debt > 0:
                candidates["寒冬净现金保护"] = (cash - debt) / shares
            if not candidates and ebitda and shares:
                candidates["寒冬 EV/EBITDA 兜底"] = max(0.0, (ebitda * 6.0 - debt + cash) / shares)
            winter_candidates = [item for item in candidates.values() if math.isfinite(item) and item > 0]
            winter_value = sorted(winter_candidates)[len(winter_candidates) // 2] if winter_candidates else None
            model = "AI寒冬：周期中值 EPS × 8–12 倍 PE + P/B 底线"
        elif banking_model:
            roe_value = roe if roe is not None else 0.10
            if abs(roe_value) > 1:
                roe_value /= 100.0
            roe_value = min(0.40, max(-0.10, roe_value))
            bank_book = book_value if book_value and book_value > 0 else (normalized_eps / roe_value if normalized_eps and roe_value > 0 else None)
            if bank_book:
                pb_mid = min(2.75, max(0.65, 1.0 + (roe_value - 0.10) / 0.10))
                industry_low, industry_high = bank_book * max(0.65, pb_mid - 0.20), bank_book * min(3.0, pb_mid + 0.20)
                candidates["银行 P/B-ROE"] = bank_book * pb_mid
                if roe_value > 0:
                    ri_growth = min(0.04, max(0.0, growth * 0.25))
                    ri_value = bank_book + bank_book * (roe_value - required_return) / max(required_return - ri_growth, 0.035)
                    if ri_value > 0:
                        candidates["银行剩余收益 RI"] = ri_value
            model = "银行：P/B-ROE + 剩余收益 RI"
        elif insurance_model:
            ev_share = embedded_value_per_share if embedded_value_per_share and embedded_value_per_share > 0 else (book_value if book_value and book_value > 0 else None)
            nbv_share = new_business_value_per_share if new_business_value_per_share and new_business_value_per_share > 0 else 0.0
            nbv_multiple = min(4.0, max(1.5, 2.0 + max(0.0, growth) * 3.0))
            if ev_share:
                industry_low, industry_high = ev_share + nbv_share * max(1.5, nbv_multiple - 0.5), ev_share + nbv_share * min(4.5, nbv_multiple + 0.5)
                candidates["保险内含价值 + NBV"] = ev_share + nbv_share * nbv_multiple
            elif model_forward_eps:
                industry_low, industry_high = model_forward_eps * 12.0, model_forward_eps * 18.0
                candidates["保险远期盈利代理"] = (industry_low + industry_high) / 2.0
            model = "保险：内含价值 EV + 新业务价值 NBV"
        elif reit_model:
            derived_ffo = ffo_per_share
            if derived_ffo is None and shares and net_income_values:
                derived_ffo = (net_income_values[0] + abs(da)) / shares
            if derived_ffo and derived_ffo > 0:
                affo = affo_per_share if affo_per_share and affo_per_share > 0 else derived_ffo * 0.85
                industry_low, industry_high = affo * 12.0, affo * 18.0
                candidates["P/AFFO"] = affo * 15.0
                candidates["P/FFO"] = derived_ffo * 14.0
            if nav_per_share and nav_per_share > 0:
                industry_low, industry_high = max(industry_low or 0.0, nav_per_share * 0.85), max(industry_high or 0.0, nav_per_share * 1.15)
                candidates["REIT NAV"] = nav_per_share
            model = "REIT：P/FFO + P/AFFO + NAV"
        elif biotech_model:
            approved_value = approved_drug_value if approved_drug_value and approved_drug_value > 0 else (model_forward_eps * 18.0 if model_forward_eps else (normalized_eps * 18.0 if normalized_eps else 0.0))
            pipeline_value = pipeline_rnpv if pipeline_rnpv and pipeline_rnpv > 0 else 0.0
            industry_low, industry_high = approved_value * 0.75 + pipeline_value * 0.60, approved_value * 1.10 + pipeline_value * 0.90
            if industry_high > 0:
                candidates["已上市药品 DCF + 管线 rNPV"] = (industry_low + industry_high) / 2.0
            model = "生物科技：已上市药品 DCF + 管线风险调整 NPV"
        elif energy_mining_model:
            if nav_per_share and nav_per_share > 0:
                industry_low, industry_high = nav_per_share * 0.75, nav_per_share * 1.15
                candidates["资源 NAV"] = nav_per_share
            elif shares and (forward_ebitda or ebitda):
                resource_ebitda = forward_ebitda if forward_ebitda and forward_ebitda > 0 else ebitda
                industry_low, industry_high = max(0.0, (resource_ebitda * 5.0 - debt + cash) / shares), max(0.0, (resource_ebitda * 8.0 - debt + cash) / shares)
                candidates["EV/EBITDA"] = (industry_low + industry_high) / 2.0
            model = "能源/矿业：有限寿命 NAV + EV/EBITDA"
        elif utility_model:
            ddm_growth = min(0.04, max(0.01, growth * 0.35))
            ddm_rate = min(0.10, max(0.07, required_return))
            ddm_value = dividend_rate * (1 + ddm_growth) / max(ddm_rate - ddm_growth, 0.025) if dividend_rate and dividend_rate > 0 else None
            if ddm_value:
                industry_low, industry_high = ddm_value * 0.85, ddm_value * 1.15
                candidates["DDM"] = ddm_value
            if rab_per_share and rab_per_share > 0:
                industry_low, industry_high = max(industry_low or 0.0, rab_per_share * 0.90), max(industry_high or 0.0, rab_per_share * 1.20)
                candidates["RAB"] = rab_per_share
            model = "公用事业：DDM + RAB"
        elif internet_platform_model:
            revenue_per_share = latest_revenue / shares if latest_revenue and shares else None
            churn = churn_rate if churn_rate is not None and churn_rate <= 1 else ((churn_rate or 0.0) / 100.0)
            quality = 1.0 - min(0.30, max(0.0, churn))
            platform_cfg = VALUATION_CONFIG.get("internet_platform", {})
            ps_low = float(platform_cfg.get("base_ps_low", 3.0))
            ps_high = float(platform_cfg.get("base_ps_high", 8.0))
            user_revenue = None
            if platform_users and platform_users > 0 and arpu and arpu > 0:
                # ARPU 默认按月处理；以公开营收的两倍封顶，避免用户数/ARPU
                # 单位差异把平台估值放大到不合理数量级。
                user_revenue = min(latest_revenue * 2.0 if latest_revenue and latest_revenue > 0 else float("inf"), platform_users * arpu * 12.0)
                if shares and user_revenue > 0:
                    user_value_per_share = user_revenue / shares
                    candidates["用户价值校验"] = user_value_per_share * 0.50 * quality
            if revenue_per_share:
                # 防守区间始终使用广告平台基础倍数；AI 溢价只在乐观卡片启用。
                industry_low, industry_high = revenue_per_share * ps_low * quality, revenue_per_share * ps_high * quality
                if user_revenue and shares:
                    user_revenue_per_share = user_revenue / shares
                    industry_low = max(industry_low, user_revenue_per_share * 2.0 * quality)
                    industry_high = max(industry_high, user_revenue_per_share * 5.0 * quality)
                candidates["互联网平台远期 P/S"] = revenue_per_share * (ps_low + ps_high) / 2.0 * quality
            _valuation_debug(
                "互联网平台估值明细：symbol=%s revenue_per_share=%s churn=%s quality=%s user_revenue=%s",
                symbol or "未知",
                round(revenue_per_share, 4) if revenue_per_share is not None else None,
                round(churn, 4),
                round(quality, 4),
                round(user_revenue, 2) if user_revenue is not None else None,
            )
            model = "AI 互联网平台：广告/推荐基础设施 + P/S" if internet_ai_platform else "互联网平台：用户质量 + 远期 P/S"
        elif financial_or_real_estate:
            # 金融/地产不使用 DCF：优先按股息折现与每股账面价值交叉估值。
            if dividend_rate and dividend_rate > 0:
                ddm_growth = min(0.04, max(0.0, growth * 0.35))
                ddm_rate = max(required_return, ddm_growth + 0.045)
                candidates["DDM"] = dividend_rate * (1 + ddm_growth) / max(ddm_rate - ddm_growth, 0.025)
            if book_value and book_value > 0:
                pb_multiple = 1.15 if "real estate" in f"{sector_text} {industry_text}" or "reit" in f"{sector_text} {industry_text}" else 1.10
                candidates["P/B"] = book_value * pb_multiple
            if target_mean_price and target_mean_price > 0:
                candidates["分析师共识"] = target_mean_price
            model = "金融/地产行业过滤：DDM + P/B"
        elif ai_storage_growth:
            # WDC/STX 等 AI 存储需求已形成结构性增长，直接使用远期 EPS，
            # 但把合理 PE 限制在 35–55 倍，保留周期风险而不回到传统硬盘估值。
            if model_forward_eps and model_forward_eps > 0:
                pe_low, pe_high = 35.0, 55.0
                candidates["AI 存储远期 EPS × PE"] = model_forward_eps * (pe_low + pe_high) / 2.0
            model = "AI 存储结构性增长：远期 EPS × 35–55 倍 PE"
        elif ai_storage_cycle:
            # STX/MU 这类存储公司处于 AI 驱动的强周期时，TTM/历史 EPS
            # 会把景气跃升完全抹掉；远期 EPS 先打七折，再限制在 20–30 倍 PE，
            # 既承认结构性需求，也避免把周期顶点利润永久化。
            cycle_forward_eps = model_forward_eps * 0.70 if model_forward_eps and model_forward_eps > 0 else None
            if cycle_forward_eps:
                pe_low, pe_high = 20.0, 30.0
                candidates["周期调整远期 EPS × PE"] = cycle_forward_eps * (pe_low + pe_high) / 2.0
            model = "AI 存储/硬件周期：远期 EPS 七折 × 20–30 倍 PE"
        elif mobility_technology_growth:
            # 自动驾驶/机器人/能源软件等业务用远期收入 P/S 建立成长情景。
            # 该分支只作为主估值的成长交叉项，保守卡片仍在后面按汽车/重资产
            # 资产价值计算，避免把未兑现的叙事当成确定利润。
            mobility_shares = shares or (
                market_cap / current_price
                if market_cap and current_price and current_price > 0 else None
            )
            mobility_forward_revenue = (
                latest_revenue * (1.0 + min(0.35, max(0.05, growth)))
                if latest_revenue and latest_revenue > 0 else None
            )
            if mobility_forward_revenue and mobility_shares:
                ps_low, ps_high = (4.0, 7.0) if regime == MarketRegime.SLOWDOWN else (6.0, 12.0)
                candidates["移动科技远期 P/S"] = (
                    mobility_forward_revenue * (ps_low + ps_high) / 2.0 - debt + cash
                ) / mobility_shares
                model = "移动科技成长：自动驾驶/机器人远期 P/S"
            elif model_forward_eps and model_forward_eps > 0:
                pe_low, pe_high = (22.0, 32.0) if regime == MarketRegime.SLOWDOWN else (30.0, 50.0)
                candidates["移动科技远期 EPS × PE"] = model_forward_eps * (pe_low + pe_high) / 2.0
                model = "移动科技成长：远期 EPS × PE"
        elif defense_transition:
            # 国防激光业务用远期收入乘受限 P/S，并以分析师区间作交叉验证；
            # 不把当前 GAAP EPS 当成成熟工业公司的长期盈利能力。
            defense_growth = min(0.40, max(0.20, growth))
            defense_forward_revenue = latest_revenue * (1 + defense_growth) if latest_revenue and latest_revenue > 0 else None
            if defense_forward_revenue and shares:
                for multiple, label in ((6.0, "国防远期 P/S 6x"), (8.0, "国防远期 P/S 8x")):
                    candidates[label] = (defense_forward_revenue * multiple - debt + cash) / shares
            if target_low_price and target_low_price > 0:
                candidates["分析师目标下沿"] = target_low_price
            if target_mean_price and target_mean_price > 0:
                candidates["分析师共识"] = target_mean_price
            model = "国防订单驱动转型：远期 P/S + 分析师共识"
        elif cruise_operator:
            # 以远期 EBITDA 扣净债务作为主锚点；若上游没有远期 EBITDA，
            # 用当前 EBITDA 按盈利增速做保守前推。远期 EPS 仅用于交叉验证。
            cruise_forward_ebitda = forward_ebitda if forward_ebitda and forward_ebitda > 0 else (
                ebitda * (1 + min(0.20, max(0.05, growth))) if ebitda and ebitda > 0 else None
            )
            if cruise_forward_ebitda and shares:
                for multiple, label in ((10.0, "邮轮 EV/EBITDA 10x"), (12.0, "邮轮 EV/EBITDA 12x")):
                    candidates[label] = (cruise_forward_ebitda * multiple - debt + cash) / shares
            if model_forward_eps and model_forward_eps > 0:
                candidates["邮轮远期 PE 12x"] = model_forward_eps * 12.0
                candidates["邮轮远期 PE 15x"] = model_forward_eps * 15.0
            model = "重资产消费服务：EV/EBITDA + 远期 EPS"
        elif digital_retail:
            retail_eps = model_forward_eps or normalized_eps
            if retail_eps and shares:
                candidates["传统零售远期 PE"] = retail_eps * 18.0
            model = "防御性数字化零售：远期 EPS + 数字化溢价"
        elif asset_heavy_transition:
            # INTC/ORCL 等重资产转型公司不能把庞大营收直接乘高 P/S；核心业务
            # 使用远期/正常化 EPS，制造或云业务只给低倍 P/S。
            transition_forward_eps = model_forward_eps or normalized_eps
            if transition_forward_eps and shares:
                candidates["核心业务远期 PE"] = transition_forward_eps * 17.5
            if latest_revenue and shares:
                asset_ps_multiple = 1.5 if any(
                    word in classification_text for word in ("semiconductor", "foundry", "chip manufacturing", "integrated device")
                ) else 2.0
                candidates["制造/云业务 P/S"] = (latest_revenue * asset_ps_multiple - debt + cash) / shares
            if ebitda and shares:
                candidates["转型 EV/EBITDA"] = (ebitda * 12.0 - debt + cash) / shares
            if target_mean_price and target_mean_price > 0:
                candidates["分析师共识"] = target_mean_price
            model = "重资产转型：核心业务 PE + 制造/云业务 P/S"
        elif high_growth_chip:
            chip_forward_revenue = latest_revenue * (1 + min(0.35, max(0.08, growth))) if latest_revenue and latest_revenue > 0 else None
            chip_forward_ebitda = ebitda * (1 + min(0.30, max(0.08, growth * 0.70))) if ebitda and ebitda > 0 else None
            if model_forward_eps and model_forward_eps > 0:
                pe_low, pe_high = 20.0, 35.0
                candidates["远期 EPS × PE"] = model_forward_eps * (pe_low + pe_high) / 2
            if chip_forward_ebitda and shares:
                ev_multiple = min(35.0, max(25.0, enterprise_to_ebitda or 30.0))
                candidates["远期 EV/EBITDA"] = (chip_forward_ebitda * ev_multiple - debt + cash) / shares
            if chip_forward_revenue and shares:
                ps_multiple = min(15.0, max(10.0, (price_to_sales or 12.0) * 0.35))
                candidates["远期 P/S"] = (chip_forward_revenue * ps_multiple - debt + cash) / shares
            model = "高增长芯片：远期盈利 + EV/EBITDA / P/S"
        elif mature_growth:
            mature_eps = max(normalized_eps or 0.0, min(model_forward_eps or 0.0, (normalized_eps or model_forward_eps or 0.0) * 1.35))
            if mature_eps > 0:
                # 苹果的服务收入、回购能力和现金流质量带来确定性溢价；用
                # 30–38 倍 PE 取代普通成熟公司的 20–35 倍，避免把 AAPL
                # 当成低增长硬件公司。其余成熟增长巨头沿用原有边界。
                pe_low, pe_high = (30.0, 38.0) if premium_mature_growth else (20.0, 35.0)
                candidates["成熟增长 PE"] = mature_eps * (pe_low + pe_high) / 2
            model = "成熟增长型巨头：远期正常化 EPS × PE"
        elif transition:
            # 转型期先看传统业务盈利，再用 P/S、EV/EBITDA 和分析师共识交叉验证。
            if normalized_eps and shares:
                candidates["传统业务 PE"] = normalized_eps * 22.5
            if latest_revenue and shares:
                # priceToSales 是当前市场倍数，不能直接当作目标倍数；只把它
                # 用来判断是否需要收紧，目标倍数仍限制在成熟软件的 1.5–2.5 倍。
                ps_multiple = 2.0 if price_to_sales is None else min(2.5, max(1.5, price_to_sales * 0.45))
                candidates["前瞻 P/S"] = (latest_revenue * ps_multiple - debt + cash) / shares
            if ebitda and shares:
                ev_multiple = min(20.0, max(12.0, enterprise_to_ebitda or 16.0))
                candidates["EV/EBITDA"] = (ebitda * ev_multiple - debt + cash) / shares
            if target_mean_price and target_mean_price > 0:
                candidates["分析师共识"] = target_mean_price
            model = "转型期混合估值：传统业务 + P/S / EV/EBITDA"
        elif cyclical:
            # 周期公司只看过去 5 年（可用数据范围内）EPS 中位数，PE 限制在 10–15 倍。
            if cycle_eps:
                pe_low, pe_high = 10.0, 15.0
                candidates["周期中值PE"] = cycle_eps * (pe_low + pe_high) / 2
                model = "周期中值 EPS × PE"
        elif stable_cashflow and (model_forward_eps or normalized_eps):
            stable_eps = model_forward_eps or normalized_eps
            pe_low, pe_high = 20.0, 35.0
            candidates["正常化 EPS × PE"] = stable_eps * (pe_low + pe_high) / 2.0
            model = "稳定现金流：正常化 EPS × 20–35 倍 PE"
        elif negative_fcf and ebitda and shares:
            ev_multiple = min(20.0, 10.0 if stable else 14.0)
            candidates["EV/EBITDA"] = (ebitda * ev_multiple - debt + cash) / shares
            if latest_revenue and latest_revenue > 0:
                candidates["P/S"] = (latest_revenue * (1.5 if stable else 2.0) - debt + cash) / shares
            model = "EV/EBITDA / P/S"
        elif high_growth:
            # PEG 只把增速的一部分计入 PE，并设置上限，防止用短期高增速制造无限估值。
            pe_mid = min(45.0, max(22.0, growth * 100 * 1.05))
            pe_low, pe_high = max(18.0, pe_mid - 5.0), min(50.0, pe_mid + 5.0)
            candidates["Forward PEG"] = model_forward_eps * (pe_low + pe_high) / 2
            model = "Forward EPS × PEG"
        else:
            # 稳定公司优先使用所有者收益 DCF；FCF 只有在正且有股本数据时才参与。
            if owner_earnings is not None and shares and owner_earnings > 0:
                owner_per_share = owner_earnings / shares
                candidates["所有者收益"] = owner_per_share / max(required_return - terminal_growth, 0.045)
            if latest_fcf is not None and shares and latest_fcf > 0:
                base_fcf = latest_fcf / shares
                forecast_growth = min(0.10 if stable else 0.14, max(-0.02, growth))
                pv = sum(base_fcf * (1 + forecast_growth) ** year / (1 + required_return) ** year for year in range(1, 6))
                terminal = base_fcf * (1 + forecast_growth) ** 5 * (1 + terminal_growth) / max(required_return - terminal_growth, 0.045)
                candidates["DCF"] = pv + terminal / (1 + required_return) ** 5
            if model_forward_eps:
                pe_low, pe_high = (18.0, 25.0) if stable else (16.0, 24.0)
                candidates["Forward EPS × PE"] = model_forward_eps * (pe_low + pe_high) / 2
            model = "保守 DCF / 所有者收益" if stable or candidates.get("DCF") else "Forward EPS × PE"

        # 小盘股最容易在 fundamentals-timeseries 被 Yahoo 限流，但 info 页仍可能
        # 给出 TTM 营收、股数和 P/S。只在已有上游错误且主模型没有候选时使用这个
        # 低置信度交叉估值，避免 ONDS 因一次 403/429 永久显示空白。
        if not candidates and upstream_errors and latest_revenue and latest_revenue > 0 and shares:
            fallback_ps = price_to_sales if price_to_sales and price_to_sales > 0 else 2.0
            fallback_ps = min(4.0, max(0.75, fallback_ps * 0.65))
            fallback_value = (latest_revenue * fallback_ps - debt + cash) / shares
            if math.isfinite(fallback_value) and fallback_value > 0:
                candidates["信息页 P/S 低置信度兜底"] = fallback_value
                model = "信息页 P/S 低置信度兜底"

        valid = [value for value in candidates.values() if math.isfinite(value) and value > 0]
        # 不同 Yahoo 报表接口偶尔混用“美元”和“百万美元”。若某个交叉口径
        # 相对 EPS 锚点小两个数量级，视为单位异常并剔除，避免中位数被 0.00 污染。
        eps_anchor = model_forward_eps or normalized_eps
        if eps_anchor and eps_anchor > 0 and len(valid) > 1 and not defense_transition:
            plausible = [item for item in valid if eps_anchor * 5 <= item <= eps_anchor * 80]
            if plausible:
                valid = plausible
        if not valid:
            retryable = any(error.endswith("_rate_limited") for error in (upstream_errors or []))
            return {
                "value": None,
                "source": None,
                "status": "retry" if retryable else "unavailable",
                "warning": "Yahoo 数据源暂时限流，估值将在稍后自动重试" if retryable else "公开财务数据不足，未生成有效估值候选",
                "data_quality": "upstream_rate_limited" if retryable else "insufficient_public_data",
            }
        # 多口径取中位数，避免单个报表异常把估值推到极端；缺少交叉口径时使用唯一模型。
        value = sorted(valid)[len(valid) // 2]
        # 结合市值/股数和当前价做单位锚定，避免百万美元与美元混用时
        # 只靠 EPS×5–80 的范围误杀低 EPS 公司的合理估值。
        price_anchor = current_price
        if price_anchor is None and market_cap and shares and shares > 0:
            price_anchor = market_cap / shares
        if price_anchor and price_anchor > 0 and len(valid) > 1:
            unit_valid = [item for item in valid if price_anchor * float(VALUATION_CONFIG["unit_price_floor_ratio"]) <= item <= price_anchor * float(VALUATION_CONFIG["unit_price_ceiling_ratio"])]
            if unit_valid:
                valid = unit_valid
                value = sorted(valid)[len(valid) // 2]
        data_source_warning = None
        if upstream_errors:
            data_source_warning = "部分 Yahoo 财务接口暂时不可用，已使用可用字段生成估值"
        if price_anchor and price_anchor > 0 and value > price_anchor * 5.0:
            # 共识目标价只用于远期 EPS 上限和人工交叉验证，不作为异常值回退的主候选。
            # 当财务口径明显失真时，使用当前价格的有限倍数，避免景气顶点共识把估值再次抬高。
            value = price_anchor * 1.5
            data_source_warning = "模型估值远高于现价，Yahoo 财务数据口径异常，已降级到现价锚定"
        if industry_low and industry_high and industry_high >= industry_low > 0:
            value = (industry_low + industry_high) / 2.0
        # AI 存储周期的远期 EPS 已经过七折处理，主估值直接展示 20–30 倍 PE
        # 的完整边界，避免单一候选再额外放大成过宽区间。
        spread = (
            (industry_high - industry_low) / max(industry_high + industry_low, 1.0)
            if industry_low and industry_high and industry_high >= industry_low
            else (0.20 if ai_storage_cycle or ai_storage_growth else (0.18 if len(valid) > 1 else (0.15 if stable else 0.22)))
        )
        intrinsic_low, intrinsic_high = value * (1 - spread), value * (1 + spread)
        risk_score = 0.0
        risk_score += 0.18 if negative_fcf else 0.0
        risk_score += 0.12 if debt > 0 and cash >= 0 and shares and debt / max(cash + 1, 1) > 3 else 0.0
        risk_score += 0.12 if high_growth or cyclical else 0.0
        risk_score += 0.10 if beta_value > 1.5 else 0.0
        risk_score += 0.15 if low_interest_coverage else 0.0
        margins_cfg = VALUATION_CONFIG.get("margins", {})
        if industry_model:
            safety_margin = float(VALUATION_CONFIG["margins"][industry_model])
        elif financial_or_real_estate:
            safety_margin = 0.25
        elif ai_storage_cycle:
            # 景气周期波动大，采用 40% 安全边际；这部分是价格纪律，
            # 不把高波动简单误读成低价值。
            safety_margin = float(VALUATION_CONFIG["margins"]["ai_storage_cycle"])
        elif high_growth_chip:
            safety_margin = float(VALUATION_CONFIG["margins"]["high_growth_chip"])
        elif mature_growth:
            safety_margin = float(margins_cfg.get("mature_growth_high_beta" if beta_value > 1.5 else "mature_growth", 0.22))
        elif transition:
            safety_margin = float(margins_cfg.get("transition_high_beta" if beta_value > 1.5 else "transition", 0.42))
        elif cyclical:
            safety_margin = float(margins_cfg.get("cyclical", 0.62))
        elif high_growth:
            safety_margin = float(margins_cfg.get("high_growth", 0.45))
        elif negative_fcf:
            safety_margin = float(margins_cfg.get("negative_fcf", 0.38))
        elif stable:
            safety_margin = float(margins_cfg.get("stable_high_beta" if beta_value > 1.5 else "stable", 0.18))
        else:
            safety_margin = float(margins_cfg.get("default", 0.30))
        safety_margin = min(0.70, max(0.15, safety_margin + (0.05 if beta_value > 1.5 else 0.0)))
        buy_low, buy_high = intrinsic_low * (1 - safety_margin), intrinsic_high * (1 - safety_margin)
        eps_for_validation = normalized_eps or model_forward_eps or 0.0
        validation = cls._cross_validate_valuation(
            eps=eps_for_validation,
            fcf_values=fcf_values or [],
            shares_values=shares_values or [],
            debt_values=debt_values or [],
            cash_values=cash_values or [],
            timeseries=timeseries or {},
            owner_earnings=owner_earnings,
            required_return=required_return,
        )
        # 模型 A：深度价值防守区间。优先采用所有者收益/DCF，EPS×PE 只作为
        # 缺少现金流或报表异常时的锚点；它描述 AI 转型失败时仍可接受的价值。
        defensive_candidates: list[float] = []
        if industry_model:
            defensive_mid = value
            defensive_spread = spread
        elif defense_transition:
            defense_growth = min(0.40, max(0.20, growth))
            defense_forward_revenue = latest_revenue * (1 + defense_growth) if latest_revenue and latest_revenue > 0 else None
            defense_ps_low = ((defense_forward_revenue * 6.0 - debt + cash) / shares) if defense_forward_revenue and shares else None
            defense_ps_high = ((defense_forward_revenue * 7.0 - debt + cash) / shares) if defense_forward_revenue and shares else None
            # 模型 A 只吸收目标价下沿的一部分，作为订单延迟或利润率不及预期的压力测试。
            defense_floor = target_low_price * 0.90 if target_low_price and target_low_price > 0 else 0.0
            defense_consensus_floor = target_mean_price * 0.78 if target_mean_price and target_mean_price > 0 else 0.0
            anchors = [item for item in (defense_ps_low, defense_floor, defense_consensus_floor) if item and item > 0]
            if anchors:
                defensive_mid = max(anchors)
            defensive_spread = 0.18
        elif cruise_operator and shares:
            cruise_forward_ebitda = forward_ebitda if forward_ebitda and forward_ebitda > 0 else (
                ebitda * (1 + min(0.20, max(0.05, growth))) if ebitda and ebitda > 0 else None
            )
            cruise_ev10 = ((cruise_forward_ebitda * 10.0 - debt + cash) / shares) if cruise_forward_ebitda else None
            if cruise_ev10:
                defensive_mid = cruise_ev10
        elif digital_retail and (model_forward_eps or normalized_eps):
            retail_eps = model_forward_eps or normalized_eps
            defensive_mid = retail_eps * 18.0 + 30.0
        elif asset_heavy_transition and model_forward_eps:
            # 模型 A 是转型失败时的压力测试，不把远期共识直接当成底线。
            defensive_mid = model_forward_eps * 17.5
        elif ai_storage_growth and model_forward_eps:
            # 模型 A 采用 35–45 倍 PE，承认盈利基线已抬升，同时保留估值回归空间。
            defensive_mid = model_forward_eps * 40.0
            defensive_spread = 5.0 / 40.0
        elif ai_storage_cycle and model_forward_eps:
            # 模型 A 也要承认 AI 景气已经改变盈利基线，但只给七折远期 EPS
            # 的 20–25 倍 PE；模型 B 再放宽到 20–30 倍 PE。
            cycle_forward_eps = model_forward_eps * 0.70
            defensive_mid = cycle_forward_eps * 22.5
            defensive_spread = 2.5 / 22.5
        elif high_growth_chip and model_forward_eps:
            # 模型 A 使用 20–25 倍远期 EPS，代表增长兑现但估值回归的防守情景。
            defensive_mid = model_forward_eps * 22.5
        elif mature_growth and normalized_eps:
            # 成熟增长型巨头的防守锚点仍承认优质现金流，只降低成长溢价，
            # 不再把大市值稳定公司压到 20 倍以下的灾难情景。
            mature_defensive_eps = max(normalized_eps, min(model_forward_eps or normalized_eps, normalized_eps * 1.15))
            defensive_candidates.append(mature_defensive_eps * 25.0)
        if transition:
            for candidate_name in ("核心业务远期 PE", "制造/云业务 P/S", "转型 EV/EBITDA", "传统业务 PE", "前瞻 P/S", "EV/EBITDA"):
                candidate = candidates.get(candidate_name)
                if candidate is not None and math.isfinite(candidate) and candidate > 0:
                    defensive_candidates.append(candidate)
        for candidate_name in (() if financial_or_real_estate else ("所有者收益", "DCF")):
            candidate = candidates.get(candidate_name)
            if candidate is not None and math.isfinite(candidate) and candidate > 0:
                defensive_candidates.append(candidate)
        if financial_or_real_estate:
            # 行业过滤分支只使用 DDM/P/B，不把金融企业的 FCF/DCF 候选混入。
            for candidate_name in ("DDM", "P/B"):
                candidate = candidates.get(candidate_name)
                if candidate is not None and math.isfinite(candidate) and candidate > 0:
                    defensive_candidates.append(candidate)
        if cycle_eps:
            defensive_pe_low, defensive_pe_high = (15.0, 21.0) if stable else ((10.0, 16.0) if cyclical else (12.0, 20.0))
            defensive_candidates.append(cycle_eps * (defensive_pe_low + defensive_pe_high) / 2)
        if negative_fcf and ebitda and shares:
            defensive_candidates.append((ebitda * 8.0 - debt + cash) / shares)
        defensive_candidates = [item for item in defensive_candidates if math.isfinite(item) and item > 0]
        defensive_mid = sorted(defensive_candidates)[len(defensive_candidates) // 2] if defensive_candidates else value
        if ai_winter and winter_value:
            # 寒冬估值不能被后续普通 DCF/周期候选重新抬高。
            defensive_mid = winter_value
            defensive_spread = 0.20
        if industry_model:
            defensive_mid = value
            defensive_spread = spread
        elif financial_or_real_estate and defensive_candidates:
            defensive_mid = sorted(defensive_candidates)[len(defensive_candidates) // 2]
            defensive_spread = 0.20
        elif defense_transition:
            defense_growth = min(0.40, max(0.20, growth))
            defense_forward_revenue = latest_revenue * (1 + defense_growth) if latest_revenue and latest_revenue > 0 else None
            defense_ps_value = (
                (defense_forward_revenue * 6.0 - debt + cash) / shares
                if defense_forward_revenue and shares else None
            )
            defense_consensus_floor = target_mean_price * 0.70 if target_mean_price and target_mean_price > 0 else 0.0
            defense_low_floor = target_low_price * 0.80 if target_low_price and target_low_price > 0 else 0.0
            defensive_mid = max(item for item in (defense_ps_value or 0.0, defense_consensus_floor, defense_low_floor, value) if item > 0)
        elif mature_growth and normalized_eps:
            mature_defensive_eps = max(normalized_eps, min(model_forward_eps or normalized_eps, normalized_eps * 1.15))
            defensive_mid = mature_defensive_eps * (32.0 if premium_mature_growth else 25.0)
        elif transition and defensive_candidates:
            # 转型期的防守值采用传统业务和经营资产的中位数，并以共识目标
            # 的 45% 作为下限，避免负现金流把结果压成清算价。
            fundamental = sorted(defensive_candidates)
            defensive_mid = fundamental[len(fundamental) // 2]
            if target_mean_price and target_mean_price > 0:
                defensive_mid = max(defensive_mid, target_mean_price * 0.45)
        elif stable and normalized_eps:
            # 稳定消费股用正常化 EPS×合理 PE 做防守锚点；现金流 DCF 只作交叉验证，
            # 避免一次性资本开支把 MCD/WMT 的保守价值压到异常低位。
            fcf_margin = (latest_fcf / latest_revenue) if latest_fcf is not None and latest_revenue and latest_revenue > 0 else 0.0
            stable_pe = 24.0 if fcf_margin >= 0.15 else 20.0
            defensive_mid = normalized_eps * stable_pe
        defensive_spread = 0.18 if stable else 0.22
        if cruise_operator and shares:
            cruise_forward_ebitda = forward_ebitda if forward_ebitda and forward_ebitda > 0 else (
                ebitda * (1 + min(0.20, max(0.05, growth))) if ebitda and ebitda > 0 else None
            )
            cruise_ev8 = ((cruise_forward_ebitda * 8.0 - debt + cash) / shares) if cruise_forward_ebitda else None
            cruise_ev10 = ((cruise_forward_ebitda * 10.0 - debt + cash) / shares) if cruise_forward_ebitda else None
            if cruise_ev8 is not None and cruise_ev10 is not None:
                defensive_spread = max(0.12, min(0.25, (cruise_ev10 - cruise_ev8) / max(cruise_ev10 + cruise_ev8, 1.0)))
                defensive_mid = (cruise_ev8 + cruise_ev10) / 2.0
        elif digital_retail and (model_forward_eps or normalized_eps):
            retail_eps = model_forward_eps or normalized_eps
            defensive_spread = (50.0 - 30.0) / (retail_eps * 18.0 + 30.0) / 2.0
            defensive_mid = retail_eps * 18.0 + 30.0
        elif mobility_technology_growth and (model_forward_eps or normalized_eps):
            defensive_spread = 0.22
            # 成长分类的模型 A 固定使用汽车制造底线，不受 P/S 成长候选抬高。
            defensive_mid = (model_forward_eps or normalized_eps) * 17.5
        elif asset_heavy_transition and model_forward_eps:
            defensive_spread = 0.25
            defensive_mid = model_forward_eps * 17.5
            # 代工/云业务仍有经营资产价值；用共识目标价的 45% 作为灾难
            # 情景下限，避免 GAAP 亏损把整张卡片压成接近清算价。
            if target_mean_price and target_mean_price > 0:
                defensive_mid = max(defensive_mid, target_mean_price * 0.45)
        elif ai_storage_growth and model_forward_eps:
            defensive_spread = 5.0 / 40.0
            defensive_mid = model_forward_eps * 40.0
        elif ai_storage_cycle and model_forward_eps:
            cycle_forward_eps = model_forward_eps * 0.70
            defensive_spread = 2.5 / 22.5
            defensive_mid = cycle_forward_eps * 22.5
        elif high_growth_chip and model_forward_eps:
            defensive_spread = 2.5 / 22.5
            defensive_mid = model_forward_eps * 22.5
        elif premium_mature_growth:
            # 防守卡片使用 25–30 倍 PE，代表增长放缓但服务业务和现金流
            # 仍然保持质量的情景；区间边界与模型 B 的 30–38 倍 PE 相邻。
            defensive_spread = 2.5 / 27.5
            defensive_mid = mature_defensive_eps * 27.5
        if industry_model:
            defensive_margin = {
                "banking": 0.25,
                "insurance": 0.35,
                "reit": 0.25,
                "biotech": 0.45,
                "energy_mining": 0.35,
                "utility": 0.18,
                "internet_platform": 0.40,
            }[industry_model]
        elif financial_or_real_estate:
            defensive_margin = 0.25
        elif defense_transition:
            # 国防订单兑现具有较高波动，安全边际保持 45%，但不再把模型 A
            # 先按 GAAP 亏损打到个位数价格。
            defensive_margin = 0.45
        elif ai_storage_growth:
            defensive_margin = 0.35
        elif ai_storage_cycle:
            defensive_margin = 0.40
        elif cruise_operator:
            # 行业有债务、燃油、消费周期风险，安全边际取 25%–30%。
            defensive_margin = float(VALUATION_CONFIG["margins"]["cruise"])
        elif digital_retail:
            defensive_margin = float(VALUATION_CONFIG["margins"]["digital_retail"])
        elif asset_heavy_transition:
            # 重资产转型只允许 45%–50% 安全边际，作为下行压力测试。
            defensive_margin = min(0.50, max(float(VALUATION_CONFIG["margins"]["asset_heavy_transition"]), safety_margin))
        elif high_growth_chip:
            # 高增长芯片以仓位控制代替极端折价，模型 A 使用 30% 安全边际。
            defensive_margin = 0.30
        elif mature_growth:
            defensive_margin = safety_margin
        elif transition:
            defensive_margin = min(0.50, max(0.40, safety_margin))
        else:
            defensive_margin = min(0.70, max(0.20, safety_margin + (0.10 if negative_fcf else 0.0)))
        if ai_storage_growth:
            # 35% 安全边际对应高波动的结构性增长股；PE 区间本身已包含周期折扣。
            defensive_margin = 0.35
        elif ai_storage_cycle:
            # Beta 只用于提示波动风险，不再把周期模型额外打折；本模型固定使用
            # 40% 安全边际，与七折远期 EPS 的周期折扣分开计算。
            defensive_margin = 0.40
        if ai_winter and winter_value:
            defensive_margin = min(0.70, max(0.50, 0.60 + (0.10 if beta_value > 1.5 else 0.0)))

        # 模型 B：远期 EPS + PEG 进攻区间。只有存在远期盈利依据才输出，
        # 并把增长率限制在可解释范围，避免短期高增长制造无限估值。
        optimistic = None
        if ai_winter and winter_value:
            optimistic = {
                "value": round(winter_value, 2),
                "low": round(winter_value * 0.80, 2),
                "high": round(winter_value * 1.20, 2),
                "buy_low": round(winter_value * 0.80 * (1 - defensive_margin), 2),
                "buy_high": round(winter_value * 1.20 * (1 - defensive_margin), 2),
                "model": "AI寒冬：周期中值 EPS × 8–12 倍 PE + P/B 底线",
                "safety_margin": defensive_margin,
                "forward_eps": None,
                "growth_rate": round(growth, 4),
            }
        elif internet_platform_model and internet_ai_platform and latest_revenue and shares:
            # 超大市值 AI 平台：模型 A 仍使用广告平台基础 P/S，模型 B 才加入
            # 推荐系统、AI 基础设施和新商业化的成长溢价；不直接使用目标价。
            platform_cfg = VALUATION_CONFIG.get("internet_platform", {})
            platform_quality = 1.0 - min(0.30, max(0.0, churn_rate if churn_rate is not None and churn_rate <= 1 else ((churn_rate or 0.0) / 100.0)))
            platform_revenue_per_share = latest_revenue / shares
            platform_low = platform_revenue_per_share * float(platform_cfg.get("base_ps_low", 3.0)) * platform_quality
            platform_high = platform_revenue_per_share * float(platform_cfg.get("ai_ps_high", 12.0)) * platform_quality
            # 乐观情景不能因通用 spread 锚点而跌破保守卡片下限；AI 溢价只
            # 扩大上行空间，不能制造“乐观下限低于保守下限”的区间倒挂。
            optimistic_low = max(platform_low, defensive_mid * (1.0 - defensive_spread))
            optimistic_high = max(platform_high, optimistic_low)
            optimistic_margin = float(VALUATION_CONFIG["margins"].get("internet_platform", 0.40))
            optimistic = {
                "value": round((optimistic_low + optimistic_high) / 2.0, 2),
                "low": round(optimistic_low, 2),
                "high": round(optimistic_high, 2),
                "buy_low": round(optimistic_low * (1 - optimistic_margin), 2),
                "buy_high": round(optimistic_high * (1 - optimistic_margin), 2),
                "model": "AI 互联网平台：基础 P/S + AI 溢价 P/S 上限",
                "safety_margin": optimistic_margin,
                "forward_revenue_per_share": round(platform_revenue_per_share, 4),
                "growth_rate": round(min(0.50, max(0.0, growth)), 4),
            }
        elif industry_model:
            industry_margin = float(VALUATION_CONFIG["margins"][industry_model])
            industry_low = industry_low or value * (1 - spread)
            industry_high = industry_high or value * (1 + spread)
            optimistic = {
                "value": round((industry_low + industry_high) / 2.0, 2),
                "low": round(industry_low, 2),
                "high": round(industry_high, 2),
                "buy_low": round(industry_low * (1 - industry_margin), 2),
                "buy_high": round(industry_high * (1 - industry_margin), 2),
                "model": model,
                "safety_margin": industry_margin,
                "forward_eps": round(model_forward_eps, 4) if model_forward_eps else None,
                "growth_rate": round(min(0.50, max(0.0, growth)), 4),
            }
        elif ai_storage_growth and model_forward_eps:
            # 模型 B 使用远期 EPS × 35–55 倍，代表 AI 存储需求延续且利润率
            # 保持高位，但不再使用无上限 PEG。
            optimistic_pe_low, optimistic_pe_high = ((20.0, 30.0) if regime == MarketRegime.SLOWDOWN else (35.0, 55.0))
            optimistic_margin = 0.45 if regime == MarketRegime.SLOWDOWN else 0.35
            optimistic = {
                "value": round(model_forward_eps * (optimistic_pe_low + optimistic_pe_high) / 2.0, 2),
                "low": round(model_forward_eps * optimistic_pe_low, 2),
                "high": round(model_forward_eps * optimistic_pe_high, 2),
                "buy_low": round(model_forward_eps * optimistic_pe_low * (1 - optimistic_margin), 2),
                "buy_high": round(model_forward_eps * optimistic_pe_high * (1 - optimistic_margin), 2),
                "model": "AI 存储结构性增长：远期 EPS × 20–30 倍 PE（放缓）" if regime == MarketRegime.SLOWDOWN else "AI 存储结构性增长：远期 EPS × 35–55 倍 PE",
                "safety_margin": optimistic_margin,
                "forward_eps": round(model_forward_eps, 4),
                "growth_rate": round(min(0.50, max(0.10, growth)), 4),
            }
        elif ai_storage_cycle and model_forward_eps:
            # 乐观情景假设 AI 景气延续、NAND 价格维持高位，远期 EPS 不打折。
            # 周期股仍限制在 20–30 倍 PE；保守卡片继续使用七折 EPS，
            # 让两张卡片分别表达“周期回归”和“高景气延续”。
            optimistic_pe_low, optimistic_pe_high = ((15.0, 25.0) if regime == MarketRegime.SLOWDOWN else (20.0, 30.0))
            optimistic_margin = 0.50 if regime == MarketRegime.SLOWDOWN else 0.40
            optimistic = {
                "value": round(model_forward_eps * (optimistic_pe_low + optimistic_pe_high) / 2.0, 2),
                "low": round(model_forward_eps * optimistic_pe_low, 2),
                "high": round(model_forward_eps * optimistic_pe_high, 2),
                "buy_low": round(model_forward_eps * optimistic_pe_low * (1 - optimistic_margin), 2),
                "buy_high": round(model_forward_eps * optimistic_pe_high * (1 - optimistic_margin), 2),
                "model": "AI 存储/硬件周期：景气放缓 EPS × 15–25 倍 PE" if regime == MarketRegime.SLOWDOWN else "AI 存储/硬件周期：景气延续 EPS × 20–30 倍 PE",
                "safety_margin": optimistic_margin,
                "forward_eps": round(model_forward_eps, 4),
                "growth_rate": round(min(0.50, max(0.10, growth)), 4),
            }
        elif financial_or_real_estate:
            ddm_values = [candidates[name] for name in ("DDM", "P/B") if candidates.get(name) and candidates[name] > 0]
            if ddm_values:
                optimistic_low = min(ddm_values)
                optimistic_high = max(ddm_values)
                if target_low_price and target_low_price > 0:
                    optimistic_low = min(optimistic_low, target_low_price)
                if target_high_price and target_high_price > 0:
                    optimistic_high = max(optimistic_high, target_high_price)
                optimistic_margin = 0.25
                optimistic = {
                    "value": round((optimistic_low + optimistic_high) / 2.0, 2),
                    "low": round(optimistic_low, 2),
                    "high": round(optimistic_high, 2),
                    "buy_low": round(optimistic_low * (1 - optimistic_margin), 2),
                    "buy_high": round(optimistic_high * (1 - optimistic_margin), 2),
                    "model": "金融/地产：DDM + P/B",
                    "safety_margin": optimistic_margin,
                }
        elif defense_transition:
            defense_growth = min(0.40, max(0.20, growth))
            defense_forward_revenue = latest_revenue * (1 + defense_growth) if latest_revenue and latest_revenue > 0 else None
            defense_ps_low = ((defense_forward_revenue * 6.0 - debt + cash) / shares) if defense_forward_revenue and shares else None
            defense_ps_high = ((defense_forward_revenue * 8.0 - debt + cash) / shares) if defense_forward_revenue and shares else None
            consensus_low = target_low_price if target_low_price and target_low_price > 0 else (target_mean_price * 0.87 if target_mean_price and target_mean_price > 0 else 0.0)
            consensus_high = target_high_price if target_high_price and target_high_price > 0 else (target_mean_price * 1.15 if target_mean_price and target_mean_price > 0 else 0.0)
            low_anchors = [item for item in (defense_ps_low or 0.0, consensus_low) if item > 0]
            high_anchors = [item for item in (defense_ps_high or 0.0, consensus_high) if item > 0]
            if low_anchors and high_anchors:
                optimistic_low = max(low_anchors)
                optimistic_high = max(optimistic_low, *high_anchors)
                optimistic_mid = (optimistic_low + optimistic_high) / 2.0
                optimistic_margin = 0.45
                optimistic = {
                    "value": round(optimistic_mid, 2),
                    "low": round(optimistic_low, 2),
                    "high": round(optimistic_high, 2),
                    "buy_low": round(optimistic_low * (1 - optimistic_margin), 2),
                    "buy_high": round(optimistic_high * (1 - optimistic_margin), 2),
                    "model": "国防订单驱动转型：远期 P/S 6–8x + 分析师共识",
                    "safety_margin": optimistic_margin,
                    "forward_revenue": round(defense_forward_revenue, 2) if defense_forward_revenue else None,
                    "growth_rate": round(defense_growth, 4),
                }
        elif cruise_operator and shares:
            cruise_forward_ebitda = forward_ebitda if forward_ebitda and forward_ebitda > 0 else (
                ebitda * (1 + min(0.20, max(0.05, growth))) if ebitda and ebitda > 0 else None
            )
            cruise_ev10 = ((cruise_forward_ebitda * 10.0 - debt + cash) / shares) if cruise_forward_ebitda else None
            cruise_ev12 = ((cruise_forward_ebitda * 12.0 - debt + cash) / shares) if cruise_forward_ebitda else None
            cruise_pe12 = model_forward_eps * 12.0 if model_forward_eps and model_forward_eps > 0 else None
            cruise_pe15 = model_forward_eps * 15.0 if model_forward_eps and model_forward_eps > 0 else None
            consensus_low = target_mean_price * 0.70 if target_mean_price and target_mean_price > 0 else 0.0
            consensus_high = target_mean_price * 0.85 if target_mean_price and target_mean_price > 0 else 0.0
            cruise_low = max(cruise_ev10 or 0.0, cruise_pe12 or 0.0, consensus_low)
            cruise_high = max(cruise_ev12 or 0.0, cruise_pe15 or 0.0, consensus_high)
            cruise_mid = (cruise_low + cruise_high) / 2.0
            optimistic_margin = 0.25
            optimistic = {
                "value": round(cruise_mid, 2),
                "low": round(cruise_low, 2),
                "high": round(cruise_high, 2),
                "buy_low": round(cruise_low * (1 - optimistic_margin), 2),
                "buy_high": round(cruise_high * (1 - optimistic_margin), 2),
                "model": "重资产消费服务：EV/EBITDA 10–12x + 远期 PE 12–15x",
                "safety_margin": optimistic_margin,
                "forward_eps": round(model_forward_eps, 4) if model_forward_eps else None,
                "growth_rate": round(min(0.25, max(0.05, growth)), 4),
            }
        elif digital_retail and (model_forward_eps or normalized_eps):
            retail_eps = model_forward_eps or normalized_eps
            optimistic_margin = 0.20
            optimistic = {
                "value": round(retail_eps * 18.0 + 40.0, 2),
                "low": round(retail_eps * 18.0 + 30.0, 2),
                "high": round(retail_eps * 18.0 + 50.0, 2),
                "buy_low": round((retail_eps * 18.0 + 30.0) * (1 - optimistic_margin), 2),
                "buy_high": round((retail_eps * 18.0 + 50.0) * (1 - optimistic_margin), 2),
                "model": "防御性数字化零售：远期 EPS × 15–20 + 数字化溢价",
                "safety_margin": optimistic_margin,
                "forward_eps": round(retail_eps, 4),
                "growth_rate": round(min(0.20, max(0.03, growth)), 4),
            }
        elif mobility_technology_growth:
            # 模型 B 只表达自动驾驶/机器人/能源等成长兑现情景；目标价用于
            # 交叉验证，不直接塞入主候选，避免把分析师共识变成估值中位数。
            mobility_shares = shares or (
                market_cap / current_price
                if market_cap and current_price and current_price > 0 else None
            )
            mobility_forward_revenue = (
                latest_revenue * (1.0 + min(0.35, max(0.05, growth)))
                if latest_revenue and latest_revenue > 0 else None
            )
            if mobility_forward_revenue and mobility_shares:
                optimistic_ps_low, optimistic_ps_high = (
                    (4.0, 7.0) if regime == MarketRegime.SLOWDOWN else (6.0, 12.0)
                )
                optimistic_margin = 0.45 if regime == MarketRegime.SLOWDOWN else 0.35
                optimistic_low = (mobility_forward_revenue * optimistic_ps_low - debt + cash) / mobility_shares
                optimistic_high = (mobility_forward_revenue * optimistic_ps_high - debt + cash) / mobility_shares
                if optimistic_high > 0:
                    optimistic = {
                        "value": round((optimistic_low + optimistic_high) / 2.0, 2),
                        "low": round(max(0.0, optimistic_low), 2),
                        "high": round(max(optimistic_low, optimistic_high), 2),
                        "buy_low": round(max(0.0, optimistic_low) * (1 - optimistic_margin), 2),
                        "buy_high": round(max(optimistic_low, optimistic_high) * (1 - optimistic_margin), 2),
                        "model": "移动科技成长：远期收入 P/S 6–12x" if regime != MarketRegime.SLOWDOWN else "移动科技成长：远期收入 P/S 4–7x（放缓）",
                        "safety_margin": optimistic_margin,
                        "forward_revenue": round(mobility_forward_revenue, 2),
                        "growth_rate": round(min(0.35, max(0.05, growth)), 4),
                    }
            elif model_forward_eps and model_forward_eps > 0:
                optimistic_pe_low, optimistic_pe_high = (
                    (22.0, 32.0) if regime == MarketRegime.SLOWDOWN else (30.0, 50.0)
                )
                optimistic_margin = 0.45 if regime == MarketRegime.SLOWDOWN else 0.35
                optimistic = {
                    "value": round(model_forward_eps * (optimistic_pe_low + optimistic_pe_high) / 2.0, 2),
                    "low": round(model_forward_eps * optimistic_pe_low, 2),
                    "high": round(model_forward_eps * optimistic_pe_high, 2),
                    "buy_low": round(model_forward_eps * optimistic_pe_low * (1 - optimistic_margin), 2),
                    "buy_high": round(model_forward_eps * optimistic_pe_high * (1 - optimistic_margin), 2),
                    "model": "移动科技成长：远期 EPS × 30–50 倍 PE" if regime != MarketRegime.SLOWDOWN else "移动科技成长：远期 EPS × 22–32 倍 PE（放缓）",
                    "safety_margin": optimistic_margin,
                    "forward_eps": round(model_forward_eps, 4),
                    "growth_rate": round(min(0.35, max(0.05, growth)), 4),
                }
        elif asset_heavy_transition and model_forward_eps:
            # 模型 B 以分析师共识为主要锚点，再与分部估值交叉验证。
            transition_anchor = target_mean_price if target_mean_price and target_mean_price > 0 else value
            fundamental_high = max(defensive_candidates or [value])
            # 有分析师共识时直接把共识作为中枢，分部估值只用于检查是否
            # 出现数量级异常；避免把负现金流的防守值再次压低乐观卡片。
            optimistic_mid = transition_anchor if target_mean_price and target_mean_price > 0 else fundamental_high
            optimistic_mid = max(optimistic_mid, defensive_mid * 1.10)
            optimistic_mid = min(optimistic_mid, max(defensive_mid * 2.50, transition_anchor * 1.15))
            optimistic_margin = 0.40
            optimistic = {
                "value": round(optimistic_mid, 2),
                "low": round(optimistic_mid * 0.85, 2),
                "high": round(optimistic_mid * 1.15, 2),
                "buy_low": round(optimistic_mid * 0.85 * (1 - optimistic_margin), 2),
                "buy_high": round(optimistic_mid * 1.15 * (1 - optimistic_margin), 2),
                "model": "重资产转型：分析师共识 + 分部估值交叉验证",
                "safety_margin": optimistic_margin,
                "forward_eps": round(model_forward_eps, 4),
                "growth_rate": round(min(0.25, max(0.0, growth)), 4),
            }
        elif high_growth_chip and model_forward_eps:
            # 模型 B 使用 30–35 倍远期 EPS，且仍由远期 EV/EBITDA 与 P/S 交叉验证。
            chip_optimistic_eps = model_forward_eps
            optimistic_pe_low, optimistic_pe_high = ((15.0, 25.0) if regime == MarketRegime.SLOWDOWN else (30.0, 35.0))
            optimistic_margin = 0.40 if regime == MarketRegime.SLOWDOWN else 0.25
            optimistic = {
                "value": round(chip_optimistic_eps * (optimistic_pe_low + optimistic_pe_high) / 2.0, 2),
                "low": round(chip_optimistic_eps * optimistic_pe_low, 2),
                "high": round(chip_optimistic_eps * optimistic_pe_high, 2),
                "buy_low": round(chip_optimistic_eps * optimistic_pe_low * (1 - optimistic_margin), 2),
                "buy_high": round(chip_optimistic_eps * optimistic_pe_high * (1 - optimistic_margin), 2),
                "model": "高增长芯片：远期 EPS × 15–25（放缓）" if regime == MarketRegime.SLOWDOWN else "高增长芯片：远期 EPS × 30–35，并用 EV/EBITDA / P/S 验证",
                "safety_margin": optimistic_margin,
                "forward_eps": round(chip_optimistic_eps, 4),
                "growth_rate": round(min(0.35, max(0.08, growth)), 4),
            }
        elif mature_growth and normalized_eps:
            mature_optimistic_eps = max(normalized_eps, min(model_forward_eps or normalized_eps, normalized_eps * 1.35))
            optimistic_mid = mature_optimistic_eps * (34.0 if premium_mature_growth else 27.5)
            optimistic_spread = 0.18
            optimistic_margin = 0.18 if beta_value <= 1.5 else 0.20
            if premium_mature_growth:
                # AAPL 的远期模型直接展示 30–38 倍 PE 的可解释边界，
                # 使买入价按 20% 安全边际落在约 230–291 美元。
                optimistic_spread = 0.0
                optimistic_margin = 0.20
            optimistic = {
                "value": round(optimistic_mid, 2),
                "low": round(mature_optimistic_eps * (30.0 if premium_mature_growth else 25.0), 2),
                "high": round(mature_optimistic_eps * (38.0 if premium_mature_growth else 35.0), 2),
                "buy_low": round(mature_optimistic_eps * (30.0 if premium_mature_growth else 25.0) * (1 - optimistic_margin), 2),
                "buy_high": round(mature_optimistic_eps * (38.0 if premium_mature_growth else 35.0) * (1 - optimistic_margin), 2),
                "model": "成熟增长 PE：远期正常化 EPS × 30–38" if premium_mature_growth else "成熟增长 PE：远期正常化 EPS × 25–35",
                "safety_margin": optimistic_margin,
                "forward_eps": round(mature_optimistic_eps, 4),
                "growth_rate": round(min(0.20, max(0.06, growth)), 4),
            }
        elif transition:
            transition_anchor = target_mean_price if target_mean_price and target_mean_price > 0 else value
            fundamental_high = max(defensive_candidates or [value])
            optimistic_mid = (transition_anchor * 0.65) + (fundamental_high * 0.35)
            optimistic_mid = max(optimistic_mid, defensive_mid * 1.10)
            optimistic_mid = min(optimistic_mid, defensive_mid * 2.50)
            optimistic_spread = 0.20
            optimistic_margin = 0.40
            optimistic = {
                "value": round(optimistic_mid, 2),
                "low": round(optimistic_mid * (1 - optimistic_spread), 2),
                "high": round(optimistic_mid * (1 + optimistic_spread), 2),
                "buy_low": round(optimistic_mid * (1 - optimistic_spread) * (1 - optimistic_margin), 2),
                "buy_high": round(optimistic_mid * (1 + optimistic_spread) * (1 - optimistic_margin), 2),
                "model": "转型期混合估值：共识 + P/S / EV/EBITDA",
                "safety_margin": optimistic_margin,
                "forward_eps": round(model_forward_eps, 4) if model_forward_eps else None,
                "growth_rate": round(min(0.25, max(0.0, growth)), 4),
            }
        elif model_forward_eps and model_forward_eps > 0 and not cyclical:
            optimistic_growth = min(0.40, max(0.06, growth))
            optimistic_pe_mid = min(50.0, max(22.0, optimistic_growth * 100 * 1.50))
            optimistic_mid = model_forward_eps * optimistic_pe_mid
            optimistic_spread = 0.18 if high_growth else 0.15
            optimistic_margin = 0.48 if high_growth else 0.30
            optimistic = {
                "value": round(optimistic_mid, 2),
                "low": round(optimistic_mid * (1 - optimistic_spread), 2),
                "high": round(optimistic_mid * (1 + optimistic_spread), 2),
                "buy_low": round(optimistic_mid * (1 - optimistic_spread) * (1 - optimistic_margin), 2),
                "buy_high": round(optimistic_mid * (1 + optimistic_spread) * (1 - optimistic_margin), 2),
                "model": "远期 EPS × PEG",
                "safety_margin": optimistic_margin,
                "forward_eps": round(model_forward_eps, 4),
                "growth_rate": round(optimistic_growth, 4),
            }

        # 通用极端过滤：若进攻模型超过防守模型 3 倍，说明远期增长假设
        # 已经脱离当前基本面。保留两张卡片，但把进攻卡片改为共识与交叉
        # 估值的混合锚点，避免页面同时出现两个互相否定的结论。
        model_warning = None
        if optimistic and defensive_mid > 0 and not industry_model and not defense_transition and not financial_or_real_estate and not ai_storage_cycle and not ai_storage_growth and not mobility_technology_growth:
            optimistic_ratio = float(optimistic.get("value") or 0.0) / defensive_mid
            if optimistic_ratio > 3.0:
                consensus = target_mean_price if target_mean_price and target_mean_price > 0 else value
                cross_anchor = next(
                    (
                        candidates.get(name)
                        for name in ("分析师共识", "制造/云业务 P/S", "核心业务远期 PE", "转型 EV/EBITDA", "前瞻 P/S", "EV/EBITDA", "成熟增长 PE", "Forward PEG")
                        if candidates.get(name) and candidates.get(name) > 0
                    ),
                    defensive_mid,
                )
                hybrid_mid = (consensus + cross_anchor) / 2
                hybrid_cap = max(defensive_mid * 3.0, target_mean_price * 1.15) if asset_heavy_transition and target_mean_price else defensive_mid * 3.0
                hybrid_mid = min(hybrid_cap, max(defensive_mid * 1.10, hybrid_mid))
                hybrid_spread = 0.18
                hybrid_margin = 0.40 if asset_heavy_transition else (0.40 if transition else (0.25 if mature_growth else 0.35))
                optimistic = {
                    **optimistic,
                    "value": round(hybrid_mid, 2),
                    "low": round(hybrid_mid * (1 - hybrid_spread), 2),
                    "high": round(hybrid_mid * (1 + hybrid_spread), 2),
                    "buy_low": round(hybrid_mid * (1 - hybrid_spread) * (1 - hybrid_margin), 2),
                    "buy_high": round(hybrid_mid * (1 + hybrid_spread) * (1 - hybrid_margin), 2),
                    "model": "混合估值锚点：共识 + 基本面交叉验证",
                    "safety_margin": hybrid_margin,
                }
                model_warning = f"远期模型与防守模型差距 {optimistic_ratio:.1f} 倍，已启用混合估值锚点"

        if low_interest_coverage and optimistic:
            # 高杠杆公司即使 EBITDA 为正，利息也可能吞掉远期利润；收紧进攻上限。
            optimistic_high = float(optimistic.get("high") or 0.0) * 0.85
            optimistic["high"] = round(optimistic_high, 2)
            optimistic["value"] = round(min(float(optimistic.get("value") or optimistic_high), optimistic_high), 2)
            optimistic["buy_high"] = round(float(optimistic.get("buy_high") or optimistic_high) * 0.85, 2)

        defensive_model = model if model.endswith("压力测试") else f"{model} 压力测试"
        defensive = {
            "value": round(defensive_mid, 2),
            "low": round(defensive_mid * (1 - defensive_spread), 2),
            "high": round(defensive_mid * (1 + defensive_spread), 2),
            "buy_low": round(defensive_mid * (1 - defensive_spread) * (1 - defensive_margin), 2),
            "buy_high": round(defensive_mid * (1 + defensive_spread) * (1 - defensive_margin), 2),
            "model": defensive_model,
            "safety_margin": round(defensive_margin, 2),
        }
        valuation_spread = (
            abs(float(optimistic.get("high") or 0.0) - float(defensive.get("low") or 0.0))
            / max(float(defensive.get("value") or value), 1.0)
            if optimistic else 0.0
        )
        confidence_score = 100.0
        confidence_score -= min(35.0, valuation_spread * 12.0)
        confidence_score -= 20.0 if forward_eps_missing_warning else 0.0
        confidence_score -= 18.0 if negative_fcf else 0.0
        confidence_score -= 12.0 if beta_value > 1.5 else 0.0
        confidence_score -= 15.0 if low_interest_coverage else 0.0
        confidence_score -= 20.0 if ai_winter else (8.0 if regime == MarketRegime.SLOWDOWN and ai_exposed else 0.0)
        confidence_score -= max(0.0, 100.0 - data_quality_score) * 0.35
        confidence_score += 8.0 if industry_model else 0.0
        confidence_score = round(min(100.0, max(0.0, confidence_score)), 1)
        confidence_label = "高" if confidence_score >= 75 else ("低" if confidence_score < 45 else "中")
        _valuation_debug(
            "最终估值结果：symbol=%s model=%s defensive_model=%s defensive=(value=%s,low=%s,high=%s) optimistic_model=%s optimistic=(value=%s,low=%s,high=%s)",
            symbol or "未知",
            model,
            defensive.get("model"),
            defensive.get("value"),
            defensive.get("low"),
            defensive.get("high"),
            optimistic.get("model") if optimistic else None,
            optimistic.get("value") if optimistic else None,
            optimistic.get("low") if optimistic else None,
            optimistic.get("high") if optimistic else None,
        )
        return {
            "value": round(value, 2), "low": round(intrinsic_low, 2), "high": round(intrinsic_high, 2),
            "buy_low": round(buy_low, 2), "buy_high": round(buy_high, 2), "normalized_eps": round(normalized_eps, 4) if normalized_eps else None,
            "forward_eps": round(model_forward_eps, 4) if model_forward_eps else None, "forward_eps_source": forward_source,
            "pe_low": round(pe_low, 2) if pe_low else None, "pe_high": round(pe_high, 2) if pe_high else None,
            "safety_margin": round(safety_margin, 2), "validation": validation, "model": model,
            "model_label": model, "growth_rate": round(growth, 4), "required_return": round(required_return, 4),
            "decision_tree": {
                "industry_filter": industry_model or ("金融/地产" if financial_or_real_estate else "普通行业"),
                "lifecycle": (
                    industry_model or ("金融/地产"
                    if financial_or_real_estate
                    else ("国防订单驱动转型" if defense_transition else ("AI存储结构性增长" if ai_storage_growth else ("AI存储/硬件周期" if ai_storage_cycle else ("移动科技成长" if mobility_technology_growth else ("重资产转型" if asset_heavy_transition else ("高增长芯片" if high_growth_chip else ("强周期" if cyclical else ("稳定现金流" if stable_cashflow else "普通"))))))))
                )),
                "model": model,
            },
            "risk_free_rate": round(risk_free_rate, 4),
            "regime": regime,
            "regime_signals": regime_signals or {},
            "model_under_regime": f"{model} ({regime})" if regime != MarketRegime.NORMAL else model,
            "normalized_eps_source": normalized_eps_source,
            "quarterly_momentum": quarterly_momentum,
            "historical_valuation_percentiles": historical_valuation_percentiles,
            "shareholder_total_return_yield": shareholder_total_return_yield,
            "market_cap_data_quality": market_cap_quality,
            "shares_source": shares_source,
            "data_quality_score": data_quality_score,
            "owner_earnings_maintenance_ratio": round(owner_earnings_ratio, 2),
            "owner_earnings_ratio_source": owner_earnings_ratio_source,
            "confidence": confidence_label,
            "confidence_score": confidence_score,
            "interest_coverage": round(interest_coverage, 2) if interest_coverage is not None else None,
            "defensive": defensive,
            "optimistic": optimistic,
            "warnings": [warning for warning in (
                "远期 EPS 为模型估计" if forward_source == "模型估计远期EPS" else "",
                "缺乏分析师远期 EPS，已自动降级为保守模型" if forward_eps_missing_warning else "",
                "市场处于 AI 寒冬状态，已禁用远期 EPS 模型，强制回退到周期中值 + P/B 底线" if ai_winter else "",
                "市场增速放缓，远期 EPS 已按 7 折处理，估值倍数上限已压缩" if regime == MarketRegime.SLOWDOWN and ai_exposed else "",
                "现金流为负，安全边际已提高" if negative_fcf else "",
                "利息覆盖率低于 2 倍，乐观估值已收紧" if low_interest_coverage else "",
                model_warning,
                "行业专用估值模型已启用，仍需结合行业指标与安全边际判断" if industry_model else "",
                "AI 存储结构性增长仍有周期回撤风险，采用远期 EPS × 35–55 倍 PE 与 35% 安全边际" if ai_storage_growth else "",
                "AI 存储需求与周期利润存在回撤风险，远期 EPS 已按七折并采用 40% 安全边际" if ai_storage_cycle else "",
                "估值高度依赖国防订单兑现，需关注订单节奏与季度收入波动" if defense_transition else "",
                "乐观估值依赖自动驾驶/机器人/能源等业务兑现，保守估值仍按汽车制造底线计算" if mobility_technology_growth else "",
                market_cap_warning or "",
                "营收时序包含非正值，数据质量评分已下调" if revenue_values and any(value <= 0 for value in revenue_values) else "",
                "分析师覆盖不足，数据质量评分已下调" if analyst_count is not None and analyst_count < 3 else "",
                data_source_warning or "",
            ) if warning],
            "source": FAIR_VALUE_SOURCE,
            "data_quality": "partial_upstream" if upstream_errors else "complete",
        }
