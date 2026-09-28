// 浏览器端买方结构计算；不依赖页面状态、Vue 或 DOM。
(function attachBuyerClient(global) {
  const NEW_YORK_FORMATTER = new Intl.DateTimeFormat("en-US", {
    timeZone: "America/New_York", year: "numeric", month: "2-digit", day: "2-digit",
    hour: "2-digit", minute: "2-digit", second: "2-digit", hourCycle: "h23",
  });
  const EXPIRY_CACHE = new Map();
  const DTE_CACHE = new Map();
  // 与服务端买方结构保持同一套期限和报价门槛；本地计算只负责首屏快速预览。
  const HORIZON_TRADING_DAYS = 5;
  const SWEET_MIN_DTE = 10;
  const SWEET_MAX_DTE = 45;
  const MAX_QUOTE_SPREAD_RATIO = 0.10;
  const MIN_TARGET_TOUCH = 0.15;
// 首屏买方结构回退：只用浏览器已经拿到的报价做少量候选，不扫描历史或触发新的接口。
  // 服务端综合结果返回后会覆盖这份估算；这里的目标是先让新标的有可读内容，而不是复刻全部评分模型。
  function clientNormalCdf(value) {
    const x = Number(value);
    if (!Number.isFinite(x)) return 0.5;
    const sign = x < 0 ? -1 : 1;
    const absolute = Math.abs(x) / Math.sqrt(2);
    const t = 1 / (1 + 0.3275911 * absolute);
    const polynomial = 1 - (((((1.061405429 * t - 1.453152027) * t) + 1.421413741) * t - 0.284496736) * t + 0.254829592) * t * Math.exp(-absolute * absolute);
    return 0.5 * (1 + sign * polynomial);
  }

  function clientImpliedVolatility(price, strike, optionPrice, years, isCall) {
    const target = Number(optionPrice);
    if (![price, strike, target, years].every(Number.isFinite) || price <= 0 || strike <= 0 || target <= 0 || years <= 0) return null;
    const intrinsic = isCall ? Math.max(price - strike, 0) : Math.max(strike - price, 0);
    if (target < intrinsic * 0.98) return null;
    let low = 0.03; let high = 3;
    for (let index = 0; index < 28; index += 1) {
      const mid = (low + high) / 2;
      const value = clientBsValue(price, strike, mid, years, isCall);
      if (!Number.isFinite(value)) return null;
      if (value > target) high = mid; else low = mid;
    }
    return (low + high) / 2;
  }

  function normalizeIv(raw) {
    let value = Number(raw);
    if (!Number.isFinite(value) || value <= 0) return null;
    if (value > 3) value /= 100;
    return value >= 0.03 && value <= 3 ? value : null;
  }

  function clientOptionIv(row, price, strike, mid, years, isCall) {
    // 服务端 _enrich 同样先按当前买卖价反解 IV，只有报价不可反解时才回退到期限模型。
    let value = clientImpliedVolatility(price, strike, mid, years, isCall);
    if (Number.isFinite(value) && value > 0) {
      return { value: Math.max(0.05, Math.min(value, 3)), source: "quote" };
    }
    value = normalizeIv(row?.model_iv);
    if (value != null) {
      const modelSource = String(row?.model_iv_source || "price");
      return { value: Math.max(0.05, Math.min(value, 3)), source: modelSource === "default" ? "default" : "server" };
    }
    return { value: 0.35, source: "default" };
  }

  function clientNewYorkParts(date) {
    const parts = NEW_YORK_FORMATTER.formatToParts(date).reduce((result, item) => { result[item.type] = item.value; return result; }, {});
    return parts;
  }

  function clientOptionExpiry(expiration) {
    const match = String(expiration || "").match(/^(\d{4})-(\d{2})-(\d{2})$/);
    if (!match) return null;
    if (EXPIRY_CACHE.has(expiration)) return EXPIRY_CACHE.get(expiration);
    // 用 America/New_York 的日历日计算 16:00，避免固定 -04:00 在冬令时偏移。
    const guess = new Date(Date.UTC(Number(match[1]), Number(match[2]) - 1, Number(match[3]), 21, 0, 0));
    const offsetParts = clientNewYorkParts(guess);
    const localGuessMinutes = Number(offsetParts.hour) * 60 + Number(offsetParts.minute);
    const expiry = new Date(guess.getTime() + (16 * 60 - localGuessMinutes) * 60000);
    const result = Number.isNaN(expiry.getTime()) ? null : expiry;
    EXPIRY_CACHE.set(expiration, result);
    return result;
  }

  function clientOptionDte(expiration, now = Date.now(), todayOverride = null) {
    const expiry = clientOptionExpiry(expiration);
    if (!expiry) return null;
    const today = todayOverride || clientNewYorkParts(new Date(now));
    const dateKey = `${today.year}-${today.month}-${today.day}`;
    const cacheKey = `${expiration}|${dateKey}|${expiry.getTime() <= now ? "expired" : "active"}`;
    if (DTE_CACHE.has(cacheKey)) return DTE_CACHE.get(cacheKey);
    // 当天 16:00 美东之后，合约已经到期，不能再作为买方结构候选。
    if (expiry.getTime() <= now) {
      DTE_CACHE.set(cacheKey, null);
      return null;
    }
    const expiryParts = clientNewYorkParts(expiry);
    const expiryDay = Date.UTC(Number(expiryParts.year), Number(expiryParts.month) - 1, Number(expiryParts.day));
    const todayDay = Date.UTC(Number(today.year), Number(today.month) - 1, Number(today.day));
    const dte = Math.round((expiryDay - todayDay) / 86400000);
    const result = dte < 0 ? null : Math.max(1, dte);
    DTE_CACHE.set(cacheKey, result);
    return result;
  }

  function clientOptionQuote(row) {
    let bid = Number(row?.bid);
    let ask = Number(row?.ask);
    // 部分券商快照只给最新成交价，没有双边报价；用最新价做零价差预览，
    // 服务端拿到有效 Bid/Ask 后会自动替换这份客户端估算。
    if (!Number.isFinite(bid) || bid <= 0) bid = Number(row?.last_price ?? row?.mark);
    if (!Number.isFinite(ask) || ask <= 0) ask = Number(row?.last_price ?? row?.mark);
    if (!Number.isFinite(bid) || !Number.isFinite(ask) || bid <= 0 || ask < bid) return null;
    const twoSided = Number(row?.bid) > 0 && Number(row?.ask) >= Number(row?.bid);
    return { bid, ask, approximate: !twoSided, twoSided };
  }

  function clientHorizonEnd(now) {
    let cursor = new Date(now);
    let remaining = HORIZON_TRADING_DAYS;
    while (remaining > 0) {
      cursor = new Date(cursor.getTime() + 86400000);
      const parts = clientNewYorkParts(cursor);
      const weekday = new Date(Date.UTC(Number(parts.year), Number(parts.month) - 1, Number(parts.day))).getUTCDay();
      if (weekday >= 1 && weekday <= 5) remaining -= 1;
    }
    return cursor;
  }

  function clientOptionYears(expiration, now) {
    const expiry = clientOptionExpiry(expiration);
    if (!expiry) return null;
    const seconds = (expiry.getTime() - now) / 1000;
    if (!(seconds > 0)) return null;
    return Math.max(seconds / (365 * 24 * 3600), 30 / (365 * 24 * 60));
  }

  function clientTouchProbability(target, spot, volatility, years) {
    if (!(target > 0) || !(spot > 0) || !(volatility > 0) || !(years > 0)) return null;
    const sigmaTime = volatility * Math.sqrt(years);
    if (!(sigmaTime > 0)) return null;
    return Math.min(1, 2 * clientNormalCdf(-Math.abs(Math.log(target / spot)) / sigmaTime));
  }

  function clientReferenceIv(groups, spot) {
    for (const group of groups) {
      const near = group.filter((item) => Math.abs(Math.log(item.strike / spot)) <= 0.03);
      if (!near.length) continue;
      const nearest = near.reduce((best, item) => Math.abs(item.strike - spot) < Math.abs(best.strike - spot) ? item : best, near[0]);
      const values = near.filter((item) => Math.abs(item.strike - nearest.strike) < 0.0001).map((item) => item.iv).filter((value) => Number.isFinite(value));
      if (values.length) return values.reduce((sum, value) => sum + value, 0) / values.length;
    }
    const values = groups.flatMap((group) => group.map((item) => item.iv)).filter((value) => Number.isFinite(value));
    return values.length ? values.sort((a, b) => a - b)[Math.floor(values.length / 2)] : null;
  }

  function clientBsValue(spot, strike, volatility, years, isCall) {
    const s = Number(spot); const k = Number(strike); const sigma = Number(volatility); const t = Number(years);
    if (![s, k, sigma, t].every(Number.isFinite) || s <= 0 || k <= 0 || sigma <= 0 || t <= 0) return null;
    const root = sigma * Math.sqrt(t);
    const d1 = (Math.log(s / k) + (0.005 + 0.5 * sigma ** 2) * t) / root;
    const d2 = d1 - root;
    const discount = Math.exp(-0.005 * t);
    const call = s * clientNormalCdf(d1) - k * discount * clientNormalCdf(d2);
    return isCall ? call : call - s + k * discount;
  }

  function clientBsDelta(spot, strike, volatility, years, isCall) {
    const s = Number(spot); const k = Number(strike); const sigma = Number(volatility); const t = Number(years);
    if (![s, k, sigma, t].every(Number.isFinite) || s <= 0 || k <= 0 || sigma <= 0 || t <= 0) return null;
    const root = sigma * Math.sqrt(t);
    const d1 = (Math.log(s / k) + (0.005 + 0.5 * sigma ** 2) * t) / root;
    return isCall ? clientNormalCdf(d1) : clientNormalCdf(d1) - 1;
  }

  function clientBuyerStructures(rows, spot, support = [], resistance = [], directionHint = null) {
    const price = Number(spot);
    if (!Array.isArray(rows) || !rows.length || !Number.isFinite(price) || price <= 0) return null;
    const now = Date.now();
    const todayParts = clientNewYorkParts(new Date(now));
    const horizonEnd = clientHorizonEnd(now);
    const horizonYears = Math.max((horizonEnd.getTime() - now) / (365 * 24 * 3600 * 1000), 0);
    const candidates = rows.map((source) => {
      const expiration = String(source?.expiration || "");
      const type = source?.contract_type === "put" ? "put" : source?.contract_type === "call" ? "call" : "";
      const strike = Number(source?.strike); const quote = clientOptionQuote(source);
      const bid = quote?.bid; const ask = quote?.ask;
      const expiry = clientOptionExpiry(expiration);
      const dte = clientOptionDte(expiration, now, todayParts);
      if (!type || !Number.isFinite(strike) || strike <= 0 || !quote || !dte || !expiry || expiry.getTime() <= horizonEnd.getTime()) return null;
      const mid = (bid + ask) / 2; const spread = (ask - bid) / Math.max(mid, 0.01);
      if (!Number.isFinite(mid) || mid <= 0 || (quote.twoSided && spread >= MAX_QUOTE_SPREAD_RATIO) || dte > 90) return null;
      const years = clientOptionYears(expiration, now);
      if (!years) return null;
      const ivResult = clientOptionIv(source, price, strike, mid, years, type === "call");
      const iv = ivResult.value;
      const intrinsic = type === "call" ? Math.max(price - strike, 0) : Math.max(strike - price, 0);
      if (mid < intrinsic * 0.98) return null;
      const delta = clientBsDelta(price, strike, iv, years, type === "call");
      if (!Number.isFinite(delta)) return null;
      const activity = Math.min(1, (Math.max(Number(source?.volume) || 0, 0) + Math.max(Number(source?.open_interest) || 0, 0)) / 5000);
      const distance = Math.abs(strike / price - 1);
      const score = Math.max(0, Math.min(1, 0.56 * (1 - Math.min(distance / 0.15, 1)) + 0.24 * activity + 0.20 * (1 - Math.min(spread / 0.65, 1))));
      return { source, expiration, contract_type: type, strike, bid, ask, mid, spread, dte, years, iv, ivSource: ivResult.source, delta, score, approximateQuote: quote.approximate };
    }).filter(Boolean);
    // 与服务端 _quoted_contracts 一致：同一到期日、方向和执行价只保留价差最窄的一条。
    const uniqueCandidates = new Map();
    for (const item of candidates) {
      const key = `${item.expiration}|${item.contract_type}|${item.strike}`;
      const previous = uniqueCandidates.get(key);
      if (!previous || item.spread < previous.spread) uniqueCandidates.set(key, item);
    }
    const normalizedCandidates = [...uniqueCandidates.values()];
    if (!normalizedCandidates.length) return null;
    const grouped = new Map();
    for (const item of normalizedCandidates) { if (!grouped.has(item.expiration)) grouped.set(item.expiration, []); grouped.get(item.expiration).push(item); }
    let groups = [...grouped.values()].sort((a, b) => a[0].dte - b[0].dte || a[0].expiration.localeCompare(b[0].expiration));
    const sweetGroups = groups.filter((group) => group[0].dte >= SWEET_MIN_DTE && group[0].dte <= SWEET_MAX_DTE);
    if (sweetGroups.length) groups = sweetGroups;
    const volatility = clientReferenceIv(groups, price);
    const targetFor = (direction) => {
      const pool = direction === "call" ? resistance : support;
      const levels = (pool || []).map((item) => {
        const value = Number(item?.price);
        if (!Number.isFinite(value) || value <= 0 || (direction === "call" ? value <= price : value >= price)) return null;
        const touch = clientTouchProbability(value, price, volatility, horizonYears);
        const strength = Number.isFinite(Number(item?.score)) ? Math.max(0, Math.min(1, Number(item.score))) : 0.5;
        return { price: value, touch, utility: strength * (touch == null ? 0.5 : touch) };
      }).filter(Boolean);
      const reachable = levels.filter((item) => item.touch != null && item.touch >= MIN_TARGET_TOUCH);
      const chosen = (reachable.length ? reachable : levels).sort((a, b) => b.utility - a.utility || (b.touch || -1) - (a.touch || -1) || Math.abs(a.price - price) - Math.abs(b.price - price))[0];
      return { price: chosen?.price || price * (direction === "call" ? 1.03 : 0.97), source: chosen ? "本地支撑/压力位" : "本地近似目标" };
    };
    const buildSingle = (item, target) => {
      const isCall = item.contract_type === "call";
      const targetValue = clientBsValue(target.price, item.strike, item.iv, Math.max(item.years - horizonYears, 1 / 365), isCall);
      const scenarioReturn = targetValue == null ? null : (targetValue - item.mid) / item.mid;
      const exposure = price * 100 * Math.abs(item.delta);
      return {
        kind: "single", style: item.dte <= 21 ? "短线平值单腿" : "时间容错平值单腿", direction: item.contract_type,
        label: `买入${isCall ? "看涨" : "看跌"}`, expiration: item.expiration, dte: item.dte, strikes: [item.strike],
        legs: [{ action: "buy", contract_type: item.contract_type, strike: item.strike }], iv: item.iv, delta: item.delta,
        delta_exposure: exposure, effective_leverage: exposure / (item.mid * 100), max_loss: item.mid * 100, cost: item.mid * 100,
        breakeven: isCall ? item.strike + item.mid : item.strike - item.mid, target_price: target.price, target_source: target.source,
        scenario_return: scenarioReturn, spread_ratio: item.spread, liquidity: item.spread < 0.15 ? "流动性较好" : "价差偏宽",
        score: item.score, iv_source: item.ivSource, advantage: "本地快速估算，优先展示接近平值且报价有效的合约", risk: "时间价值会随到期临近而衰减",
        quote_source: item.approximateQuote ? "last" : "bid_ask",
        quote_method: item.approximateQuote ? "最新成交价快速估算" : "买卖价中间价",
      };
    };
    const buildVertical = (long, short, target) => {
      const isCall = long.contract_type === "call"; const debit = long.mid - short.mid;
      if (debit <= 0 || debit >= Math.abs(short.strike - long.strike)) return null;
      const remain = Math.max(long.years - horizonYears, 1 / 365);
      const longTarget = clientBsValue(target.price, long.strike, long.iv, remain, isCall);
      const shortTarget = clientBsValue(target.price, short.strike, short.iv, remain, isCall);
      const targetValue = longTarget == null || shortTarget == null ? null : Math.max(0, longTarget - shortTarget);
      return {
        kind: "vertical", style: "客户端目标位价差", direction: long.contract_type, label: isCall ? "看涨价差" : "看跌价差",
        expiration: long.expiration, dte: long.dte, strikes: [long.strike, short.strike],
        legs: [{ action: "buy", contract_type: long.contract_type, strike: long.strike }, { action: "sell", contract_type: short.contract_type, strike: short.strike }],
        iv: (long.iv + short.iv) / 2, delta: long.delta - short.delta, delta_exposure: price * 100 * Math.abs(long.delta - short.delta),
        effective_leverage: price * 100 * Math.abs(long.delta - short.delta) / (debit * 100), max_loss: debit * 100, cost: debit * 100,
        breakeven: isCall ? long.strike + debit : long.strike - debit, target_price: target.price, target_source: target.source,
        scenario_return: targetValue == null ? null : (targetValue - debit) / debit, spread_ratio: Math.max(long.spread, short.spread),
        iv_source: long.ivSource === short.ivSource ? long.ivSource : "mixed",
        quote_source: long.approximateQuote || short.approximateQuote ? "last" : "bid_ask",
        liquidity: long.approximateQuote || short.approximateQuote ? "最新成交价快速估算" : "双腿报价估算", score: Math.min(long.score, short.score), advantage: "卖出腿压低权利金，先展示有限风险结构",
        risk: "收益封顶且两条腿都需要可成交报价", quote_method: long.approximateQuote || short.approximateQuote ? "最新成交价快速估算" : "双腿买卖价中间价",
      };
    };
    const items = [];
    for (const direction of ["call", "put"]) {
      const target = targetFor(direction);
      const group = groups.find((list) => list.some((item) => item.contract_type === direction));
      if (!group) continue;
      const side = group.filter((item) => item.contract_type === direction).sort((a, b) => b.score - a.score || Math.abs(a.strike - price) - Math.abs(b.strike - price));
      if (!side.length) continue;
      items.push(buildSingle(side[0], target));
      const long = side.find((item) => direction === "call" ? item.strike <= price * 1.03 : item.strike >= price * 0.97) || side[0];
      const shorts = side.filter((item) => direction === "call" ? item.strike > long.strike : item.strike < long.strike)
        .sort((a, b) => Math.abs(a.strike - target.price) - Math.abs(b.strike - target.price));
      const vertical = shorts[0] ? buildVertical(long, shorts[0], target) : null;
      if (vertical) items.push(vertical);
    }
    if (!items.length) return null;
    const primary = directionHint === "call" || directionHint === "put" ? directionHint : null;
    items.forEach((item) => { item.is_primary = Boolean(primary && item.direction === primary); item.direction_label = primary ? (item.direction === primary ? "主方向" : "对比方案") : "中性对比"; });
    return {
      available: true, status: "ok", direction: primary || "neutral", primary_direction: primary,
      direction_label: primary ? `本地快速估算：${primary === "call" ? "买入看涨" : "买入看跌"}为主方向` : "本地快速估算：中性对比，看涨/看跌均列出",
      horizon_trading_days: 5, horizon_label: "未来 5 个交易日", targets: { call: targetFor("call"), put: targetFor("put") },
      items: items.slice(0, 5), method: "本地快速估算",
      quote_source: items.some((item) => item.quote_source === "last") ? "last" : "bid_ask",
      iv_source: [...new Set(items.map((item) => item.iv_source).filter(Boolean))].join(",") || "default",
      quote_method: items.some((item) => item.quote_source === "last") ? "最新成交价估算" : "买卖价中间价估算",
      disclaimer: "本地估算仅用于首屏预览，完整综合评分由服务端结果覆盖。",
    };
  }




// 报价暂缺时也保留买方结构骨架，避免面板长期显示“正在计算”。数字字段保持 --，
  // 一旦 Bid/Ask 或服务端综合结果到达，前端会用真实估算覆盖这些占位行。
  function clientBuyerSkeleton(rows, spot, fallbackExpiration = "") {
    const price = Number(spot);
    if (!Array.isArray(rows) || !rows.length || !Number.isFinite(price) || price <= 0) return null;
    const now = Date.now();
    const todayParts = clientNewYorkParts(new Date(now));
    const candidates = rows.map((row) => ({
      row,
      type: row?.contract_type === "put" ? "put" : row?.contract_type === "call" ? "call" : "",
      strike: Number(row?.strike),
      expiration: String(row?.expiration || fallbackExpiration || ""),
    })).filter((item) => item.type && Number.isFinite(item.strike) && item.strike > 0 && clientOptionDte(item.expiration, now, todayParts) !== null);
    if (!candidates.length) return null;
    const items = ["call", "put"].map((direction) => {
      const side = candidates.filter((item) => item.type === direction);
      if (!side.length) return null;
      const selected = side.sort((a, b) => Math.abs(a.strike - price) - Math.abs(b.strike - price))[0];
      const dte = clientOptionDte(selected.expiration, now, todayParts);
      return {
        kind: "single", style: "客户端结构骨架", direction, label: direction === "call" ? "买入看涨" : "买入看跌",
        expiration: selected.expiration, dte: dte || "--", strikes: [selected.strike],
        legs: [{ action: "buy", contract_type: direction, strike: selected.strike }],
        iv: null, delta: null, delta_exposure: null, effective_leverage: null, max_loss: null, cost: null,
        breakeven: null, target_price: price, target_source: "等待支撑/压力位",
        scenario_return: null, spread_ratio: null, score: 0,
        advantage: "等待买卖价后进行本地估算", risk: "报价到达后显示风险和盈亏估算",
        quote_source: "skeleton", quote_method: "等待有效买卖价",
      };
    }).filter(Boolean);
    if (!items.length) return null;
    const primary = null;
    items.forEach((item) => { item.is_primary = false; item.direction_label = "中性对比"; });
    return {
      available: true, status: "preview", direction: "neutral", primary_direction: primary,
      direction_label: "本地结构骨架：报价到达后自动补全",
      horizon_trading_days: 5, horizon_label: "未来 5 个交易日", targets: {}, items,
      method: "等待报价", quote_source: "skeleton", quote_method: "等待有效买卖价",
      disclaimer: "当前仅显示结构骨架，完整报价或服务端综合结果返回后自动更新。",
    };
  }


  global.OptionScopeBuyerClient = {
    normalCdf: clientNormalCdf,
    value: clientBsValue,
    delta: clientBsDelta,
    impliedVolatility: clientImpliedVolatility,
    buildStructures: clientBuyerStructures,
    buildSkeleton: clientBuyerSkeleton,
    optionExpiry: clientOptionExpiry,
    optionDte: clientOptionDte,
  };
}(window));
