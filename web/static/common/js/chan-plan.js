/* 把已识别的笔结构转成当前价下的入场与持仓条件，不新增缠论买卖点。 */
(function (window) {
  "use strict";

  // These are this project's entry filters, not Chan theorems or win rates.
  const policy = Object.freeze({ maxRiskATR: 2, minRoomRisk: 2 });
  const number = (value) => value != null && Number.isFinite(Number(value)) && Number(value) > 0 ? Number(value) : null;
  const money = (value) => number(value) == null ? "--" : Number(value).toLocaleString("en-US", { minimumFractionDigits: 2, maximumFractionDigits: 2 });
  const percent = (value) => `${(value * 100).toFixed(2)}%`;
  const card = (side, action, reason, next, protection, tone = "wait") => ({
    side, label: side === "buy" ? "未持仓 · 入场判断" : "已持仓 · 持有与退出",
    action, reason, next, protection, tone,
  });
  const terms = [
    { name: "笔与反向笔", detail: "笔是连接有效高点和低点的一段上涨或下跌。前一段下跌，随后出现符合识别规则的上涨，这段上涨就叫向上反向笔；上涨后出现有效下跌，则叫向下反向笔。一两根涨跌 K 线可能还不够，出现反向笔后仍需观察走势能否持续。" },
    { name: "回踩与反抽", detail: "回踩是上涨后再次回落，用来观察能否守住前面的低点或突破位置。反抽是下跌后再次反弹，用来观察能否重新涨回跌破的位置。回踩守住可增加上涨依据，反抽未收回则提示下跌风险仍在。" },
    { name: "已固定与形成中", detail: "已固定表示这个高点或低点已经由后续满足规则的反向走势确认，作为历史转折点记录。形成中表示本段走势的结束位置还会更新，图上用虚线和星号标注。已固定的低点仍可能被未来价格跌破。" },
    { name: "保护位", detail: "用于检查买入或持有计划是否仍成立的风险边界。价格触及时先减少风险，收盘失守且反弹未能收回时优先退出。它是处理风险的参考价，跳空时实际成交价可能不同。" },
    { name: "ATR：近期通常波动多大", detail: "这里用最近 14 根已收盘 K 线的真实波幅均值，真实波幅包含当根最高、最低价和与上一根收盘的跳空距离。日线对应最近 14 个交易日，周线对应最近 14 个完整交易周。距离为 0.75 ATR，表示约为近期平均单根波幅的四分之三。" },
    { name: "观察空间／风险距离", detail: "观察空间是现价到首个上方观察价的距离；风险距离是现价到保护位的距离。比如上方空间为 10 元、保护距离为 5 元，比例就是 2 倍。上方观察价用于检查后续变化，尚不能保证上涨到那里或兑现收益。" },
    { name: "中枢", detail: "本项目取多段已确认上涨、下跌的重叠价格区，帮助观察价格是否离开整理区，以及离开后是否又回到区间。上沿是区间最高边界，下沿是最低边界。" },
  ];

  function movementExplanation(strokes, price) {
    const last = strokes[strokes.length - 1], previous = strokes[strokes.length - 2];
    if (!last) return null;
    const upward = last.direction === "up";
    let text = `图上最近识别的一段走势，从 ${money(last.start.price)} ${upward ? "反弹到" : "回落到"} ${money(last.end.price)}。`;
    if (previous && previous.direction !== last.direction) {
      text += upward ? "它接在上一段下跌之后，缠论称为“向上反向笔”，表示下跌后出现了一段有效反弹。"
        : "它接在上一段上涨之后，缠论称为“向下反向笔”，表示上涨后出现了一段有效回落。";
    }
    text += last.confirmed ? "该段结束位置已由后续走势确认。"
      : `这段${upward ? "反弹的高点" : "回落的低点"}仍在形成，后续可能更新。`;
    if (price != null && (upward && price < last.end.price || !upward && price > last.end.price)) {
      text += `现价 ${money(price)} 已从这个${upward ? "高点回落" : "低点反弹"}，端点之后的波动可能还未满足新的完整波段识别条件。`;
    }
    return { title: "这段走势怎么理解", text };
  }

  function empty() {
    return {
      headline: "等待已收盘行情与有效波段", focus: "载入后分别列出入场条件、持仓保护位和退出条件。",
      cards: [card("buy", "等待走势确认", "尚无经后续走势确认的低点可作参考。", "等待下跌后出现符合识别规则的一段反弹，确认前面的低点。", "未形成可用保护位，暂不判断买入。"),
        card("hold", "等待结构", "尚无可用的已固定低点。", "已有持仓请先沿用自己的风险计划。", "本周期尚未形成可用保护位。")],
      metrics: [], context: "", movement: null, terms,
      note: `入场筛选：现价到保护位的距离，最多为近期平均单根波幅（ATR）的 ${policy.maxRiskATR} 倍；到上方观察价的空间，至少为保护距离的 ${policy.minRoomRisk} 倍。形成可用反弹或回落确认后，还要同时满足这两项筛选。这些阈值是本项目规则，上方空间尚不能保证兑现为收益。`,
    };
  }

  function endpoints(strokes) {
    const points = new Map();
    const add = (point, fixed, fixedAt) => {
      const key = `${point.date}-${point.kind}-${point.price}`;
      const old = points.get(key);
      points.set(key, { ...point, fixed: fixed || Boolean(old?.fixed),
        fixedAt: old?.fixedAt || (fixed ? fixedAt : "") });
    };
    strokes.forEach((stroke, index) => {
      add(stroke.start, index > 0 || stroke.confirmed, stroke.end.confirmed_at);
      add(stroke.end, stroke.confirmed, stroke.confirmed_at);
    });
    return [...points.values()].sort((a, b) => a.date.localeCompare(b.date));
  }

  // A simple 14-period mean of true range; only the completed candles in
  // the frame participate. This normalizes distance without using future bars.
  function atr14(candles) {
    if (candles.length < 15) return null;
    const last = candles.length - 1;
    let sum = 0;
    for (let i = last - 13; i <= last; i++) {
      const bar = candles[i], previous = candles[i - 1].close;
      sum += Math.max(bar.high - bar.low, Math.abs(bar.high - previous), Math.abs(bar.low - previous));
    }
    return sum > 0 ? sum / 14 : null;
  }

  function view(frame, spot, weekly = null) {
    const plan = empty();
    const price = number(spot) ?? number(frame?.last_close);
    const close = number(frame?.last_close);
    const strokes = frame?.strokes || [];
    plan.movement = movementExplanation(strokes, price);
    const points = endpoints(strokes);
    const lows = points.filter((point) => point.fixed && point.kind === "bottom");
    const low = lows[lows.length - 1], previousLow = lows[lows.length - 2];
    if (!price || !low || !frame?.bar_count) return plan;

    const fixedHighs = points.filter((point) => point.fixed && point.kind === "top");
    const highsBeforeLow = fixedHighs.filter((point) => point.date < low.date);
    const priorHigh = highsBeforeLow[highsBeforeLow.length - 1];
    const olderHigh = highsBeforeLow[highsBeforeLow.length - 2];
    const higherLow = previousLow && low.price > previousLow.price;
    const higherHigh = priorHigh && olderHigh && priorHigh.price > olderHigh.price;
    const center = frame.position?.center;
    const active = (frame.signals || []).filter((signal) => signal.status === "candidate");
    const bottom = active.find((signal) => signal.kind === "bottom_reversal_candidate");
    const top = active.find((signal) => signal.kind === "top_exit_candidate");
    const thirdBuy = active.find((signal) => signal.kind === "third_buy_candidate");
    const thirdSell = active.find((signal) => signal.kind === "third_sell_candidate");
    const topWatch = (frame.signals || []).find((signal) => signal.kind === "top_exit_candidate" && signal.status === "watch");
    const atr = atr14(frame.candles || []);
    const stop = low.price;
    // A later lower low must not silently move an existing holding's
    // protection downward. It may supply a new-entry reference separately.
    const structuralHoldingLow = previousLow && low.price < previousLow.price ? previousLow : low;
    const holdingLow = frame.holding_risk?.boundary > structuralHoldingLow.price
      ? { price: frame.holding_risk.boundary, date: frame.holding_risk.boundary_date, fixedAt: frame.holding_risk.fixed_at } : structuralHoldingLow;
    const holdingStop = holdingLow.price;
    const holdingTouched = Boolean(frame.holding_risk?.touched && frame.holding_risk.boundary === holdingStop);
    const risk = price - stop;
    const highPoints = points.filter((point) => point.kind === "top");
    const latestHigh = highPoints[highPoints.length - 1];
    // An older high already surpassed by the latest swing is not revived
    // as the first upside target on every small pullback. Use the latest
    // swing high or older fixed highs beyond it, plus current center edges.
    const hurdles = highPoints.filter((point) => point.price > price
      && (point === latestHigh || point.fixed && point.price > latestHigh.price)).map((point) => ({
      price: point.price, source: point.fixed ? "已固定笔高点" : "形成中笔高点，会延伸", date: point.date,
    }));
    if (center) {
      if (center.lower > price) hurdles.push({ price: center.lower, source: "中枢下沿", date: center.confirmed_at });
      if (center.upper > price) hurdles.push({ price: center.upper, source: "中枢上沿", date: center.confirmed_at });
    }
    const hurdle = hurdles.sort((a, b) => a.price - b.price)[0];
    const room = hurdle ? hurdle.price - price : null;
    const ratio = risk > 0 && room != null ? room / risk : null;
    const riskATR = risk > 0 && atr ? risk / atr : null;
    const weeklyWeak = weekly?.position?.code === "below";
    plan.context = weekly ? weekly.position?.center
      ? `周线背景：${weekly.position.label}，中枢 ${money(weekly.position.center.lower)}–${money(weekly.position.center.upper)}${weeklyWeak ? "；日线入场属于逆势尝试，不升级为加仓判断。" : "；位置背景不等于买点共振。"}`
      : "周线中枢尚未形成，不能为本次日线入场增加确认。" : "按当前周期独立判断，形成中的端点不用于下移持仓保护位。";
    plan.metrics = [
      { label: "新仓参考保护位", value: money(stop), note: `已固定低点 · ${low.date}` },
      { label: "现价到新仓保护位", value: risk > 0 ? percent(risk / price) : "已触及", note: risk > 0 ? `每股距离 ${money(risk)}${riskATR != null ? ` · 约 ${riskATR.toFixed(2)} 倍平均波幅（ATR）` : " · 平均波幅数据不足"}` : "先处理风险，停止沿用入场计划" },
      { label: "首个上方观察位", value: hurdle ? money(hurdle.price) : "暂无", note: hurdle ? `${hurdle.source} · ${hurdle.date}` : "不虚构上涨目标，等待新结构" },
      { label: "观察空间 / 风险距离", value: ratio != null ? `${ratio.toFixed(2)} 倍` : "--", note: ratio != null ? `入场筛选至少 ${policy.minRoomRisk} 倍，未扣费用` : "缺少空间或有效保护距离" },
    ];

    const protection = `现价跌至 ${money(stop)} 时先减少风险；${frame.label}收盘低于此价，随后反弹也未能涨回此价上方时，停止沿用本次买入计划。`;
    const holdingProtection = `持仓保护位 ${money(holdingStop)}${holdingStop > stop ? `（此前确认的低点；出现新低 ${money(stop)} 后，原保护位继续保留）` : "（已由后续走势确认的低点）"}；跌至此价时先控风险，收盘跌破且反弹未能涨回时优先退出。`;
    const distance = risk > 0 ? `现价比已固定低点 ${money(stop)} 高 ${percent(price / stop - 1)}。` : `现价已触及已固定低点 ${money(stop)}。`;
    const brokeOnClose = close != null && close <= stop && frame.as_of >= low.fixedAt;
    const holdingBrokeOnClose = close != null && close <= holdingStop && frame.as_of >= holdingLow.fixedAt;
    const breakoutClose = priorHigh && close != null && close > priorHigh.price && frame.as_of >= low.fixedAt;
    const continuedHigh = points.some((point) => point.kind === "top" && point.date > low.date && priorHigh && point.price > priorHigh.price);
    const trendEntry = higherLow && (higherHigh || breakoutClose || continuedHigh);
    // A fixed downward pen followed by a fixed upward pen that cannot
    // reclaim the earlier low is a failed rebound, independently of MACD.
    const lastFixed = strokes[strokes.length - 2];
    const brokenLow = lastFixed?.direction === "up" && lastFixed.confirmed
      ? lows.filter((point) => point.date < lastFixed.start.date).slice(-1)[0] : null;
    const failedReclaim = brokenLow && lastFixed.start.price < brokenLow.price
      && lastFixed.end.price < brokenLow.price && price < brokenLow.price;
    const ready = Boolean(bottom || thirdBuy || trendEntry);
    const retestBreakout = bottom?.phase === "retest" && bottom.trigger_confirmed && close != null
      && close > bottom.trigger_price && price > bottom.trigger_price && frame.as_of >= bottom.phase_at;
    const strongerEntry = Boolean(retestBreakout || thirdBuy || (trendEntry && breakoutClose && price > priorHigh.price));
    const safeDistance = riskATR != null && riskATR <= policy.maxRiskATR;
    const enoughRoom = ratio != null && ratio >= policy.minRoomRisk;
    const basis = bottom ? bottom.phase === "retest" ? "反弹后的第一次回落已经结束，最低价仍高于原来的低点。" : "下跌后已出现一段有效反弹，原低点已确认；还要观察反弹后的第一次回落能否守住它。"
      : thirdBuy ? "价格向上离开整理区后再次回落，最低价仍在整理区上方，已形成买入观察候选。"
      : trendEntry ? `已确认的低点从 ${money(previousLow.price)} 抬高到 ${money(stop)}，可继续评估上涨后的回落机会。`
      : "买入所需的止跌或回落确认尚未齐备，需要后续走势提供更多依据。";
    let buy;
    if (risk <= 0 || brokeOnClose) {
      buy = card("buy", "暂停买入", `${distance}${brokeOnClose ? "最近收盘已触及或跌破保护位。" : "盘中已触及，仍需核对收盘；入场条件已受损。"}`, `等价格重新涨回 ${money(stop)} 上方，并出现一段符合规则的止跌反弹，再评估买入。`, protection, "risk");
    } else if (holdingTouched && holdingStop === stop) {
      buy = card("buy", "保护位曾触及，等待重新企稳", `价格曾跌至保护位 ${money(stop)}；重新涨回之后，还需要后续止跌依据。`, "等待下一次回落的低点抬高并得到确认，或出现新的底部观察候选，再评估买入。", protection);
    } else if (top || thirdSell || failedReclaim) {
      buy = card("buy", "先等待下跌风险缓解", "上涨后已出现有效回落，或下跌后的反弹未能收回失守的位置，当前仍有减仓风险提示。", "等下跌后出现有效反弹，再看后续回落能否守住低点，重新计算保护位和上方空间。", protection);
    } else if (!ready) {
      const rebounding = strokes[strokes.length - 1]?.direction === "up";
      buy = card("buy", rebounding ? "已有反弹，等待回落企稳" : "等待下跌后出现有效反弹", `${distance}${basis}`, rebounding
        ? `先观察反弹后的回落能否停在 ${money(stop)} 上方，并重新上涨；回落低点由后续走势确认后，再计算是否适合买入。`
        : `先等待下跌后形成一段有效反弹，再观察随后的回落能否守住 ${money(stop)}，确认新的低点。`, protection);
    } else if (!atr) {
      buy = card("buy", "等待波动数据", `${basis}已收盘行情不足以计算近期平均波幅（ATR），暂不能判断现价是否离保护位太远。`, "历史行情更新后，结合保护距离与上方空间重新评估。", protection);
    } else if (!safeDistance) {
      buy = card("buy", "离保护位偏远，等待回落企稳", `${distance}从现价跌至保护位的距离，约为近期平均单根波幅的 ${riskATR.toFixed(2)} 倍，超过 ${policy.maxRiskATR} 倍筛选值。`, `等待价格回落并守住 ${money(stop)}，或后续走势确认一个更高低点，再评估买入。`, protection);
    } else if (!enoughRoom) {
      buy = card("buy", hurdle ? "上方空间不足，等待回落企稳" : "突破后等待回落企稳", hurdle
        ? `${basis}上方先观察 ${money(hurdle.price)}，距离为 ${money(room)}；下方跌至保护位的距离为 ${money(risk)}。上方空间是下方保护距离的 ${ratio.toFixed(2)} 倍，未达到 ${policy.minRoomRisk} 倍筛选值。`
        : `${basis}当前上方没有可配对的结构观察位，无法计算空间与风险的比例。`, hurdle
        ? `等价格回落并守住低点，重新比较上方空间和保护距离；收盘涨过 ${money(hurdle.price)} 后，也要观察再次回落能否站稳。`
        : "等再次回落后重新上涨，确认回落低点，再计算入场距离及上方观察价。", protection);
    } else {
      buy = card("buy", strongerEntry && !weeklyWeak ? "可考虑分批介入" : "可考虑小仓试探", `${distance}${basis}距离和空间筛选均满足${weeklyWeak ? "，但周线位置偏弱" : ""}。`, strongerEntry && !weeklyWeak
        ? "已有回落守住或收盘突破的依据，可分次介入；买入后继续观察上涨能否延续。"
        : bottom?.trigger_price != null ? `先确认反弹后的第一次回落守住低点，再看收盘能否涨过 ${money(bottom.trigger_price)}；满足后再评估加仓。`
        : priorHigh ? `先看收盘能否涨过 ${money(priorHigh.price)}，再看后续回落能否站稳，之后评估加仓。` : "等待反弹后的第一次回落守住低点，并出现收盘突破，再评估加仓。", protection, "act");
    }

    const topClose = top?.trigger_confirmed && close != null && close < top.trigger_price
      && price < top.trigger_price && frame.as_of >= top.phase_at;
    const recentHigh = latestHigh;
    const retreat = recentHigh && price < recentHigh.price
      ? `现价较${recentHigh.fixed ? "已固定" : "形成中"}高点 ${money(recentHigh.price)} 回落 ${percent(1 - price / recentHigh.price)}。` : "现价尚未低于近期笔高点。";
    let hold;
    if (thirdSell || failedReclaim || topClose) {
      hold = card("hold", "退出优先", thirdSell ? "价格向下离开整理区后再次反弹，仍未涨回区间内，已形成卖出观察候选。"
        : failedReclaim ? `下跌后的反弹已经结束，最高价 ${money(lastFixed.end.price)} 仍低于此前跌破的低点 ${money(brokenLow.price)}。`
        : `从高位回落后的低点 ${money(top.trigger_price)} 已被收盘跌破，现价也在其下方。`, "优先处理剩余仓位；重新买入前，等待下跌后的有效反弹，以及后续回落守住低点。", holdingProtection, "risk");
    } else if (price <= holdingStop || holdingBrokeOnClose) {
      hold = card("hold", holdingBrokeOnClose ? "先减仓，反弹未收回再退出" : "现价触及保护位，先控风险", `${retreat}${holdingBrokeOnClose ? `最近收盘已触及或跌破持仓保护位 ${money(holdingStop)}；重新涨回后仍需等待止跌确认。` : `现价触及 ${money(holdingStop)}，这是盘中风险提示，收盘结果仍需核对。`}`, `先减少风险；随后反弹仍未能涨回 ${money(holdingStop)}，或反弹最高价比前一次更低时，退出剩余仓位。`, holdingProtection, "risk");
    } else if (holdingTouched) {
      hold = card("hold", "保护位曾触及，等待重新企稳", `价格在 ${frame.holding_risk.touched_at} 曾跌至保护位 ${money(holdingStop)}；现价已涨回，之前的风险提示继续保留。`, "继续沿用减仓后的风险计划；等待下一次回落的低点抬高并得到确认，再重新评估持有与加仓。", holdingProtection);
    } else if (top) {
      hold = card("hold", "可先减仓，剩余守保护位", `${retreat}上涨后已出现一段符合规则的下跌，原高点已确认，顶部观察候选仍有效。`, top.trigger_price != null
        ? `${top.trigger_confirmed ? "" : "这段下跌的低点由后续走势确认后，"}收盘再跌破 ${money(top.trigger_price)} 且现价仍在下方时，退出优先${top.trigger_confirmed ? "。" : "；目前低点还会更新。"}`
        : "观察下跌后的第一次反弹能否涨回高点；未能涨回时优先退出。", holdingProtection, "risk");
    } else if (topWatch) {
      hold = card("hold", "守住保护位，暂不全部退出", `${retreat}MACD 指标显示上涨推动减弱，目前尚未出现足以确认前面高点的一段有效下跌。`, `守住 ${money(holdingStop)} 可继续观察；上涨后出现有效下跌时先评估减仓，跌破保护位后处理剩余仓位。`, holdingProtection);
    } else if (holdingStop > stop) {
      hold = card("hold", "反弹中控制仓位，等待修复", `新低 ${money(stop)} 低于此前低点 ${money(holdingStop)}；重新涨回旧低点后，仍要观察下次回落能否停在更高位置。`, "等待下一次回落的低点抬高并得到确认，原持仓保护位继续保留。", holdingProtection);
    } else {
      hold = card("hold", "可继续持有，守住保护位", `${retreat}现价仍高于已确认低点 ${money(holdingStop)}，当前尚未识别出需要减仓的高位下跌或反弹未能收回失守价位的形态。`, buy.tone === "act" && strongerEntry && !weeklyWeak
        ? "持有优先；入场距离、上方空间和回落守住的条件继续满足时，再评估分批加仓。"
        : `守住 ${money(holdingStop)} 时继续观察；收盘跌破后减仓，随后反弹仍未能涨回此价时退出。`, holdingProtection, "act");
    }
    plan.cards = [buy, hold];
    plan.headline = `新仓：${buy.action}；持仓：${hold.action}`;
    plan.focus = `当前价 ${money(price)}，持仓保护位 ${money(holdingStop)}${holdingStop !== stop ? `，新仓参考低点 ${money(stop)}` : ""}${hurdle ? `，首个上方观察位 ${money(hurdle.price)}` : "，上方暂缺可配对的观察位"}。入场与退出分别判断，旧极值候选失效后继续跟踪最新结构。`;
    return plan;
  }

  window.OptionScopeChanPlan = { empty, view };
}(window));
