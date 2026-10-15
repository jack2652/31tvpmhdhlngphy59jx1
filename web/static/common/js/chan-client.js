/* 缠论展示适配：Go 识别结构，浏览器按现价解读已识别的笔端点并绘制 SVG。 */
(function (window) {
  "use strict";

  const money = (value) => value != null && Number.isFinite(Number(value))
    ? Number(value).toLocaleString("en-US", { minimumFractionDigits: 2, maximumFractionDigits: 2 }) : "--";
  const direction = (value) => ({ up: "向上", down: "向下", unknown: "尚未成笔" }[value] || "尚未成笔");
  // 盘中周期的时间戳形如 2026-10-13 15:45；坐标轴只保留月-日与时间，避免三处标签相互挤压。
  const axisDateLabel = (value) => { const text = String(value || ""); return text.includes(" ") ? text.slice(5) : text; };
  const chartCache = new WeakMap();
  const turningKinds = new Set(["bottom_reversal_candidate", "top_exit_candidate"]);
  const buyKinds = new Set(["third_buy_candidate", "bottom_reversal_candidate"]);
  const sellKinds = new Set(["third_sell_candidate", "top_exit_candidate"]);

  // Keep the protection observed for this continuous pen chain through
  // quote recovery and a history refresh. A lower new low cannot move it down.
  // A later fixed higher low starts a new protection episode; a pen-chain
  // restart discards the old chain rather than joining unrelated structures.
  function withHoldingRisk(frame, spot, observed = null) {
    if (!frame?.strokes?.length) return frame;
    const lows = frame.strokes.filter((stroke, index) => stroke.start.kind === "bottom" && (index > 0 || stroke.confirmed));
    const latest = lows[lows.length - 1];
    if (!latest) return frame;
    const previous = lows[lows.length - 2];
    const reference = previous && latest.start.price < previous.start.price ? previous : latest;
    const chain = frame.strokes[0].start.date;
    const prior = observed?.holding_risk || frame.holding_risk;
    let state = { chain, boundary: reference.start.price, boundary_date: reference.start.date,
      fixed_at: reference.end.confirmed_at, touched: false, touched_at: "" };
    if (prior?.chain === chain && prior.boundary >= state.boundary) state = { ...prior };
    const touchedBar = (frame.candles || []).find((bar) => bar.date > state.fixed_at && bar.low <= state.boundary);
    const price = spot != null && Number.isFinite(Number(spot)) && Number(spot) > 0 ? Number(spot) : frame.last_close;
    if (!state.touched && (touchedBar || price != null && price <= state.boundary)) {
      state.touched = true;
      state.touched_at = touchedBar?.date || "页面行情更新时";
    }
    return { ...frame, holding_risk: state };
  }

  function emptyTurning(label = "当前周期") {
    return [
      { side: "buy", label: "抄底观察", status: "等待信号", title: "暂无底部候选",
        detail: "价格跌得比前一次更低，但 MACD 指标显示下跌推动减弱；随后观察是否出现有效反弹，以及反弹后的回落能否守住低点。",
        phase: "empty", price: "--", boundary: "--", date: "--", observedAt: "--", phaseAt: "--",
        condition: `等待${label}形成可比较的同向笔和已形成中枢。`, nextStep: "信号出现后列出具体价格与确认条件。", ratio: "--", trigger: "--" },
      { side: "sell", label: "逃顶观察", status: "等待信号", title: "暂无顶部候选",
        detail: "价格涨得比前一次更高，但 MACD 指标显示上涨推动减弱；随后观察是否出现有效回落，以及回落后的反弹能否重新涨回高点。",
        phase: "empty", price: "--", boundary: "--", date: "--", observedAt: "--", phaseAt: "--",
        condition: `等待${label}形成可比较的同向笔和已形成中枢。`, nextStep: "信号出现后列出具体价格与确认条件。", ratio: "--", trigger: "--" },
    ];
  }

  function turningView(frame, spot) {
    return emptyTurning(frame?.label).map((emptyCard) => {
      const signal = (frame?.signals || []).find((item) => turningKinds.has(item.kind) && item.side === emptyCard.side);
      if (!signal) {
        if (frame?.bar_count >= 60 && frame?.centers?.length) emptyCard.condition = "最近四根笔未同时满足创新极值、MACD 面积衰减和当时已形成的同一中枢关联条件，或比较笔仍在 MACD 预热区。";
        else if (frame?.bar_count && frame.bar_count < 60) emptyCard.condition = `只有 ${frame.bar_count} 根已收盘${frame.label} K 线；至少需要 60 根，且被比较的笔需避开 MACD 起始预热区。`;
        else if (frame?.bar_count >= 60) emptyCard.condition = "已收盘行情长度足够，尚未形成同一中枢关联的可比较笔；等待结构形成。";
        return emptyCard;
      }
      const invalidated = signal.status === "invalidated";
      const status = invalidated ? "已失效" : signal.status === "watch" ? "形成中 · 会变化"
        : signal.phase === "retest" ? "再次转折已确认 · 仍为候选" : "原高低点已确认 · 仍为候选";
      const closePassed = signal.trigger_price != null && frame.last_close != null && (signal.side === "buy"
        ? frame.last_close > signal.trigger_price : frame.last_close < signal.trigger_price);
      const triggerName = signal.side === "buy" ? "反弹高点" : "回落低点";
      const currentPrice = spot != null ? Number(spot) : frame.last_close;
      const retestDamaged = signal.retest_price != null && currentPrice != null && (signal.side === "buy"
        ? currentPrice <= signal.retest_price : currentPrice >= signal.retest_price);
      const trigger = signal.trigger_price == null ? signal.side === "buy"
        ? "等待下跌后出现有效反弹，确认原低点后列出反弹观察价。"
        : "等待上涨后出现有效回落，确认原高点后列出回落观察价。"
        : `${triggerName} ${money(signal.trigger_price)}${!signal.trigger_confirmed ? "（形成中，会延伸）" : "（已固定）"} · ${frame.last_close == null ? "等待收盘行情" : closePassed ? "最近收盘已越过，继续核对后续结构" : "等待收盘越过"}。`;
      return { ...emptyCard, status, title: signal.label, phase: invalidated ? "invalidated" : signal.phase,
        detail: signal.detail, price: money(signal.price), boundary: money(signal.invalidation),
        date: signal.origin_date || signal.date, observedAt: signal.observed_at || signal.confirmed_at, phaseAt: signal.phase_at || signal.confirmed_at,
        ratio: signal.strength_ratio == null ? "--" : `${(signal.strength_ratio * 100).toFixed(0)}%`,
        condition: invalidated ? `价格已${signal.side === "buy" ? "跌至原低点" : "涨至原高点"}，停止沿用这次候选；等待后续走势提供新的判断依据。` : signal.confirmation,
        nextStep: invalidated ? "等待新结构形成后重新识别，价格恢复不会自动复活这次候选。"
          : retestDamaged ? `现价已${signal.side === "buy" ? "跌至反弹后第一次回落的低点" : "涨至下跌后第一次反弹的高点"} ${money(signal.retest_price)}，之前的改善受到影响；重新观察后续走势，本次候选的失效价仍为 ${money(signal.invalidation)}。` : signal.next_step,
        trigger: invalidated ? `此次价格条件已停用${signal.trigger_price == null ? "。" : `（原${triggerName} ${money(signal.trigger_price)}）。`}` : trigger,
      };
    });
  }

  function emptyGuidance() {
    return { title: "等待已收盘行情", focus: "载入行情后列出近期高低点与确认条件。",
      evidence: [], levels: [], scenarios: [], signalReason: "等待历史行情。",
      note: "近期高低点是已识别笔端点的价格观察位；形成中的端点会更新。价格越过观察位和确认缠论买卖点是两个步骤，后者还需要后续笔结构。" };
  }

  // A transparent structural score, not a calibrated outcome probability.
  // Scenarios may be sequential rather than mutually exclusive, so score each
  // independently instead of forcing the values to add up to 100.
  function scenarioTiltScores(frame, weekly, scenarios) {
    let bias = 0;
    const addPositionBias = (source, weight) => {
      const code = source?.position?.code;
      if (code === "above") bias += weight;
      else if (code === "below") bias -= weight;
    };
    addPositionBias(frame, 0.65);
    addPositionBias(weekly, 0.4);
    if (frame?.position?.code && frame.position.code === weekly?.position?.code) {
      if (frame.position.code === "above") bias += 0.2;
      else if (frame.position.code === "below") bias -= 0.2;
    }
    const lastFixedStroke = [...(frame?.strokes || [])].reverse().find((stroke) => stroke.confirmed);
    if (lastFixedStroke?.direction === "up") bias += 0.25;
    else if (lastFixedStroke?.direction === "down") bias -= 0.25;
    (frame?.signals || []).filter((signal) => signal.status === "candidate" && !turningKinds.has(signal.kind))
      .forEach((signal) => {
        const side = signal.side || (signal.kind === "third_buy_candidate" ? "buy" : signal.kind === "third_sell_candidate" ? "sell" : "");
        bias += side === "buy" ? 0.55 : side === "sell" ? -0.55 : 0;
      });
    (frame?.signals || []).filter((signal) => turningKinds.has(signal.kind) && signal.status !== "invalidated")
      .forEach((signal) => { bias += signal.side === "buy" ? 0.2 : signal.side === "sell" ? -0.2 : 0; });
    bias = Math.max(-1.5, Math.min(1.5, bias));

    const scores = scenarios.map((scenario) => {
      if (scenario.direction === "down") return 50 - bias * 20;
      if (scenario.direction === "repair") return 50 + bias * 14;
      if (scenario.direction === "hold") return 50 + bias * 12;
      if (scenario.direction === "range") return 60 - Math.min(1, Math.abs(bias) / 1.5) * 10;
      return 50 + bias * 20;
    });
    return scenarios.map((scenario, index) => ({ ...scenario,
      tilt_score: Math.round(Math.max(20, Math.min(80, scores[index]))) }));
  }

  function emptyFrame(label) {
    return { label, available: false, position: "等待中枢形成", detail: "载入标的后分析历史行情",
      center: "--", stroke: "尚未成笔", asOf: "--", bars: 0, strokes: 0, confirmed: 0,
      signals: [], turning: emptyTurning(label), plan: window.OptionScopeChanPlan.empty(), scenarios: [], warnings: [], chart: null, boundary: "--", observation: "等待数据", structure: "等待中枢形成", guidance: emptyGuidance() };
  }

  function empty(reason = "正在读取日线与周线结构…") {
    return { available: false, label: "等待日周结构", detail: reason, alignment: "insufficient",
      daily: emptyFrame("日线"), weekly: emptyFrame("周线"), warnings: [], rules: [],
      spot: "--", historyTime: "--", refreshing: false };
  }

  // Recompute the left-axis current-price marker on quote updates even when
  // chart geometry is cached.
  function chartWithSpot(chart, value, minimum, maximum, y, scenarios = []) {
    const badgePrice = Number(value);
    const spot = value != null && Number.isFinite(badgePrice) && badgePrice >= minimum && badgePrice <= maximum
      ? { y: y(badgePrice), label: money(badgePrice), height: 22 } : null;
    const quotePrice = value != null && Number.isFinite(Number(value)) && Number(value) > 0 ? Number(value) : chart.lastClose;
    const startX = chart.historyRight + 8;
    const endX = chart.plotRight - 8;
    const projectionWidth = Math.max(0, endX - startX);
    const projections = (scenarios || []).map((scenario) => {
      if (!Number.isFinite(Number(quotePrice)) || Number(quotePrice) <= 0) return null;
      const levels = [{ price: Number(quotePrice), stage: "现价", showPrice: true }, ...(scenario.projection_levels || []).map((level, index) => ({
        price: Number(level), stage: scenario.projection_labels?.[index] || "结构观察位",
        showPrice: !(scenario.projection_hide_prices || []).includes(index),
      }))]
        .filter((point) => Number.isFinite(point.price) && point.price > 0)
        .filter((point, index, values) => index === 0 || Math.abs(point.price - values[index - 1].price) > 0.000001);
      if (levels.length < 2) return null;
      const points = levels.map((level, index) => ({
        key: `${scenario.direction}-${index}`,
        x: startX + index / Math.max(1, levels.length - 1) * projectionWidth,
        y: y(level.price), price: level.price, label: money(level.price),
        stageLabel: levels[index].stage, showPrice: levels[index].showPrice,
      }));
      return { key: scenario.direction, label: scenario.chart_label || scenario.label,
        tone: scenario.chart_tone || (["up", "repair"].includes(scenario.direction) ? "up" : scenario.direction === "down" ? "down" : "range"),
        condition: scenario.condition, expectation: scenario.expectation, invalidation: scenario.invalidation,
        status: scenario.status, tiltScore: scenario.tilt_score, points,
        endPoint: points[points.length - 1],
        priceLabel: money(points[points.length - 1].price),
        path: points.map((point, index) => `${index ? "L" : "M"}${point.x.toFixed(1)} ${point.y.toFixed(1)}`).join(" "),
      };
    }).filter(Boolean);
    return { ...chart, spot, projections, grid: chart.grid.map((row) => ({ ...row,
      // Keep the nearest tick clear of the HTML current-price marker on the
      // left axis. The mobile layout needs extra clearance for its larger scale.
      labelVisible: !spot || Math.abs(row.y - spot.y) > spot.height / 2 + (chart.mobileChart ? 28 : 8),
    })) };
  }

  function chartModel(frame, spot, scenarios, showScenarios, mobile = false, mobileBars = 30) {
    const cachedLayouts = chartCache.get(frame.candles);
    const cacheKey = `${showScenarios ? "scenarios" : "history"}-${mobile ? `mobile-${mobileBars}` : "desktop"}`;
    const cached = cachedLayouts?.get(cacheKey);
    const currentSpot = Number(spot);
    const scenarioLevels = showScenarios ? (scenarios || []).flatMap((scenario) => scenario.projection_levels || [])
      .map(Number).filter((price) => Number.isFinite(price) && price > 0) : [];
    const newLevelsOutsideCache = scenarioLevels.some((price) => price < (cached?.minimum ?? Infinity) || price > (cached?.maximum ?? -Infinity));
    const quoteOutsideCache = Number.isFinite(currentSpot) && currentSpot > 0
      && (currentSpot < (cached?.minimum ?? -Infinity) || currentSpot > (cached?.maximum ?? Infinity));
    if (cached && !newLevelsOutsideCache && !quoteOutsideCache) {
      return chartWithSpot(cached.chart, spot, cached.minimum, cached.maximum, cached.y, scenarios);
    }
    const maxBars = frame.period === "1wk" ? 120 : 160;
    const visibleBars = mobile ? Math.min(maxBars, Math.max(30, Number(mobileBars) || 60)) : maxBars;
    const bars = (frame.candles || []).slice(-visibleBars);
    if (!bars.length) return null;
    const width = mobile ? 420 : 960, height = mobile ? 520 : 340;
    // Desktop retains room for the left price labels and current-price badge
    // without carrying the oversized axis gutter from the former right axis.
    const left = mobile ? 94 : 72, right = 20, top = mobile ? 24 : 18, bottom = 38;
    const plotWidth = width - left - right, plotHeight = height - top - bottom;
    // Keep the scenario lane separate from historical bars when it is shown.
    const scenarioBoundary = left + plotWidth * (mobile ? 0.78 : 0.88);
    const historyRight = showScenarios ? scenarioBoundary : width - right;
    const historyWidth = historyRight - left;
    const firstDate = bars[0].date, lastDate = bars[bars.length - 1].date;
    const visibleStrokes = (frame.strokes || []).filter((stroke) => stroke.end.date >= firstDate && stroke.start.date <= lastDate);
    let minimum = Math.min(...bars.map((bar) => bar.low));
    let maximum = Math.max(...bars.map((bar) => bar.high));
    visibleStrokes.forEach((stroke) => {
      minimum = Math.min(minimum, stroke.low);
      maximum = Math.max(maximum, stroke.high);
    });
    if (Number.isFinite(currentSpot) && currentSpot > 0) scenarioLevels.push(currentSpot);
    if (scenarioLevels.length) {
      minimum = Math.min(minimum, ...scenarioLevels);
      maximum = Math.max(maximum, ...scenarioLevels);
    }
    const padding = Math.max((maximum - minimum) * 0.08, maximum * 0.004);
    minimum -= padding;
    maximum += padding;
    const y = (price) => top + (maximum - price) / (maximum - minimum) * plotHeight;
    const x = (index) => left + (index + 0.5) / bars.length * historyWidth;
    const dateIndex = (date) => {
      if (date <= firstDate) return 0;
      const index = bars.findIndex((bar) => bar.date >= date);
      return index < 0 ? bars.length - 1 : index;
    };
    const zones = (frame.centers || []).filter((center) => center.end >= firstDate && center.start <= lastDate);
    const latest = frame.centers?.[frame.centers.length - 1];
    if (latest && latest.end < firstDate && latest.lower < maximum && latest.upper > minimum) zones.push(latest);
    // Adjacent pens share a turning point. Label the actual endpoint once;
    // clipped endpoints outside the visible candle range are not labelled.
    const endpoints = new Map();
    const addEndpoint = (point, confirmed) => {
      if (point.date < firstDate || point.date > lastDate) return;
      const key = `${point.date}-${point.kind}-${point.price}`;
      const previous = endpoints.get(key);
      endpoints.set(key, { ...point, key, confirmed: confirmed || Boolean(previous?.confirmed) });
    };
    visibleStrokes.forEach((stroke) => {
      addEndpoint(stroke.start, true);
      addEndpoint(stroke.end, stroke.confirmed);
    });
    const endpointList = [...endpoints.values()];
    const points = endpointList.map((point) => {
      const pointX = x(dateIndex(point.date)), pointY = y(point.price);
      const label = `${money(point.price)}${point.confirmed ? "" : " *"}`;
      // Keep labels inside the plot near the first or last candle.
      const halfWidth = label.length * (mobile ? 4.2 : 3.5) + 3;
      return { key: point.key, date: point.date, kind: point.kind, price: point.price, x: pointX, y: pointY, confirmed: point.confirmed, label,
        labelX: Math.max(left + halfWidth, Math.min(width - right - halfWidth, pointX)),
        labelY: point.kind === "top" ? Math.max(13, pointY - 12) : Math.min(height - bottom + 14, pointY + 18),
        title: `${point.date} · ${point.kind === "top" ? "高点" : "低点"} ${money(point.price)} · ${point.confirmed ? "已确认端点" : "形成中，端点可延伸"}`,
      };
    });
    const chart = {
      width, height, mobileChart: mobile, plotLeft: left, plotTop: top, plotRight: width - right, plotBottom: height - bottom,
      minimum, maximum, periodLabel: frame.label,
      historyRight, scenarioBoundary, lastClose: frame.last_close,
      title: `${frame.label} K 线、笔、中枢与条件情景推演，${firstDate} 至 ${lastDate}`,
      description: "实体 K 线表示收盘后的行情。左侧显示价格刻度与现价；点击现价可查看结构观察。实线为已确认笔，虚线为形成中的笔；阴影表示三根已确认笔的重叠区。笔端点标注价格；底、顶标记是候选观察。图表右侧虚线为根据已知结构边界绘制的条件情景示意，没有未来时间刻度，也不表示目标价或确定预测。移动鼠标或触摸图表可查看 K 线日期和 OHLC；图表获得键盘焦点后，左右键切换 K 线，Home、End 跳到首根、末根，Escape 关闭提示。",
      points,
      keyLevels: (() => {
        const ordered = [...endpointList].sort((a, b) => b.date.localeCompare(a.date));
        const lastHigh = ordered.find((point) => point.kind === "top");
        const lastLow = ordered.find((point) => point.kind === "bottom");
        const candidates = [
          lastHigh && { key: "recent-high", label: lastHigh.confirmed ? "近端高点" : "形成中高点", price: lastHigh.price, date: lastHigh.date, tone: "resistance" },
          lastLow && { key: "recent-low", label: lastLow.confirmed ? "近端低点" : "形成中低点", price: lastLow.price, date: lastLow.date, tone: "support" },
          latest && { key: "center-upper", label: "中枢上沿", price: latest.upper, date: latest.start, tone: "center" },
          latest && { key: "center-lower", label: "中枢下沿", price: latest.lower, date: latest.start, tone: "center" },
        ].filter((level) => level && level.price > minimum && level.price < maximum);
        const seen = new Set();
        const distinct = candidates.filter((level) => {
          const duplicate = [...seen].some((value) => Math.abs(value - level.price) / Math.max(value, level.price) < 0.001);
          seen.add(level.price);
          return !duplicate;
        }).map((level) => ({ ...level, y: y(level.price), x1: x(dateIndex(level.date)), x2: historyRight,
          priceLabel: money(level.price),
          title: `${level.date} · ${level.label} ${money(level.price)}` }));
        return distinct;
      })(),
      candles: bars.map((bar, index) => ({ key: bar.date, date: bar.date, x: x(index),
        prices: { open: bar.open, high: bar.high, low: bar.low, close: bar.close },
        high: y(bar.high), low: y(bar.low), body: Math.min(y(bar.open), y(bar.close)),
        bodyHeight: Math.max(1, Math.abs(y(bar.close) - y(bar.open))), bodyWidth: Math.max(1, historyWidth / bars.length * 0.56),
        direction: bar.close >= bar.open ? "up" : "down",
        title: `${bar.date} · 开 ${money(bar.open)} · 高 ${money(bar.high)} · 低 ${money(bar.low)} · 收 ${money(bar.close)}` })),
      strokes: visibleStrokes.map((stroke, index) => ({ key: `${stroke.start.date}-${index}`,
        x1: x(dateIndex(stroke.start.date)), y1: y(stroke.start.price),
        x2: x(dateIndex(stroke.end.date)), y2: y(stroke.end.price),
        direction: stroke.direction, confirmed: stroke.confirmed,
        title: `${direction(stroke.direction)}笔 · ${stroke.start.date} ${money(stroke.start.price)} → ${stroke.end.date} ${money(stroke.end.price)} · ${stroke.confirmed ? `确认于 ${stroke.confirmed_at}` : "形成中，端点可延伸"}` })),
      centers: zones.filter((center) => center.lower < maximum && center.upper > minimum).map((center, index) => {
        const latestCenter = center === latest;
        const x1 = x(dateIndex(center.start));
        const x2 = latestCenter ? historyRight : x(dateIndex(center.end));
        const upper = y(Math.min(center.upper, maximum)), lower = y(Math.max(center.lower, minimum));
        return { key: `${center.start}-${index}`, x: x1, y: upper, width: Math.max(1, x2 - x1),
          height: Math.max(1, lower - upper), latest: latestCenter,
          title: `笔中枢 ${money(center.lower)}–${money(center.upper)} · 形成于 ${center.confirmed_at} · ${center.stroke_count} 根笔` };
      }),
      grid: Array.from({ length: 5 }, (_, index) => {
        const price = maximum - (maximum - minimum) * index / 4;
        return { key: index, y: y(price), label: money(price) };
      }),
      dates: [...new Set([0, Math.floor((bars.length - 1) / 2), bars.length - 1])]
        .map((index) => ({ x: x(index), y: height - 12, label: mobile ? axisDateLabel(bars[index].date).slice(-5) : axisDateLabel(bars[index].date) })),
    };
    const layouts = cachedLayouts || new Map();
    layouts.set(cacheKey, { chart, minimum, maximum, y });
    if (!cachedLayouts) chartCache.set(frame.candles, layouts);
    return chartWithSpot(chart, spot, minimum, maximum, y, scenarios);
  }

  // Interpret the existing pen endpoints at the displayed price. This does
  // not confirm new pens, buy/sell points or a higher Chan level in-browser.
  function frameGuidance(frame, spot, weekly = null) {
    const guide = emptyGuidance();
    if (!frame?.bar_count) return guide;
    const price = spot != null && Number.isFinite(Number(spot)) && Number(spot) > 0
      ? Number(spot) : frame.last_close;
    const close = frame.last_close;
    const center = frame.position?.center;
    const strokes = frame.strokes || [];
    const latest = strokes[strokes.length - 1];
    const points = new Map();
    const addPoint = (point, confirmed) => {
      const key = `${point.date}-${point.kind}-${point.price}`;
      points.set(key, { ...point, confirmed: confirmed || Boolean(points.get(key)?.confirmed) });
    };
    strokes.forEach((stroke) => { addPoint(stroke.start, true); addPoint(stroke.end, stroke.confirmed); });
    const recent = [...points.values()].reverse();
    const high = recent.find((point) => point.kind === "top");
    const low = recent.find((point) => point.kind === "bottom");
    const lower = low && recent.filter((point) => point.kind === "bottom" && point.confirmed
      && point.price < Math.min(low.price, price)).sort((a, b) => b.price - a.price)[0];
    const higher = high && recent.find((point) => point.kind === "top" && point.confirmed && point.price > high.price);
    const structural = (frame.signals || []).filter((signal) => !turningKinds.has(signal.kind));
    const active = structural.filter((signal) => signal.status === "candidate");
    const invalidated = structural.filter((signal) => signal.status === "invalidated");
    guide.signalReason = active.length ? "已识别笔结构候选，下一步核对后续回试与失效边界；次级别确认尚未完成。"
      : invalidated.length ? "之前的候选已触及失效边界，当前没有有效的第三类位置候选。"
      : !center ? "已确认笔还未形成中枢，暂无法识别第三类位置；可先用近期高低点观察价格变化。"
      : "当前没有符合规则的已确认回试结构，未识别出第三类位置候选；近端高低点仍可作为观察边界。";
    if (close != null) guide.evidence.push(`最近已收盘${frame.label}：${frame.as_of} · ${money(close)}；现价 ${money(price)}。`);
    if (!high || !low || price == null) {
      guide.title = "笔结构尚不足，方向暂待确认";
      guide.focus = `已有 ${frame.bar_count} 根收盘 K 线，尚未识别出可配对的笔高低点；等待有效反向笔形成后再列出观察价位。`;
      return guide;
    }
    const pointName = (point) => point.confirmed ? "已确认笔端点" : "形成中的笔端点，会更新";
    const gate = (boundary, upward) => {
      const livePassed = upward ? price > boundary : price < boundary;
      if (close != null && (upward ? close > boundary : close < boundary)) {
        return livePassed ? "最近收盘已越过（后续结构待核对）" : "最近收盘曾越过，现价已回到另一侧";
      }
      if (livePassed) return "仅现价越过，等待收盘";
      return "收盘价格条件未满足";
    };
    const levels = [];
    const addLevel = (key, label, value, date, source, purpose) => {
      const distance = (value / price - 1) * 100;
      levels.push({ key, label, price: money(value), date, source, purpose,
        distance: `${distance > 0 ? "+" : ""}${distance.toFixed(2)}%` });
    };
    addLevel("recent-low", "近期笔低点", low.price, low.date, pointName(low), price < low.price ? "现价已失守，先核对收盘能否收回" : "失守后重新评估反弹能否延续");
    addLevel("recent-high", "近期笔高点", high.price, high.date, pointName(high), price > high.price ? "现价已越过，再核对收盘和随后回落" : "收盘站上后，再看回落能否形成更高低点");
    if (center) {
      addLevel("center-low", "原中枢下沿", center.lower, center.confirmed_at, "已确认笔中枢", price < center.lower ? "收复后才重新进入原中枢" : "判断是否向下离开中枢");
      addLevel("center-high", "原中枢上沿", center.upper, center.confirmed_at, "已确认笔中枢", "判断能否向上离开中枢");
    }
    if (lower) addLevel("previous-low", "更低的历史笔低点", lower.price, lower.date, "已确认笔端点", "近期低点失守后的下一观察位置");
    (frame.signals || []).filter((signal) => turningKinds.has(signal.kind) && signal.status !== "invalidated" && signal.invalidation != null).forEach((signal) => {
      addLevel(`turning-${signal.side}`, signal.side === "buy" ? "底部观察失效线" : "顶部观察失效线", signal.invalidation, signal.origin_date || signal.date,
        signal.status === "watch" ? "形成中的极值，可延伸" : "原始笔极值", signal.side === "buy" ? "触及后抄底候选失效，等待新结构" : "触及后逃顶候选失效，等待新结构");
      guide.evidence.push(`${signal.label}，MACD 同向柱面积比 ${(signal.strength_ratio * 100).toFixed(0)}%；候选不代表底部或顶部已成立。`);
    });
    guide.levels = levels;
    const below = center && price < center.lower, above = center && price > center.upper;
    if (price < low.price) {
      guide.title = close != null && close < low.price ? "近期低点已被收盘跌破，反弹结构受损" : "现价跌破近期低点，等待收盘核对";
      guide.focus = `先看能否收回 ${money(low.price)}${lower ? `；未收回前，下一观察前低 ${money(lower.price)}` : "；下方暂没有更低的已确认笔端点可参考"}。`;
    } else if (price > high.price) {
      guide.title = below ? "反弹越过近期高点，原中枢尚未收复" : "价格越过近期高点，等待回落结构确认";
      guide.focus = `先核对${frame.label}收盘能否高于 ${money(high.price)}，再看回落低点是否高于 ${money(low.price)}${below ? `；中枢修复边界仍在 ${money(center.lower)}` : ""}。`;
    } else {
      guide.title = below ? (latest?.direction === "up" && price < latest.end.price ? "反弹后回落，中枢下方仍偏弱" : "中枢下方整理，反弹修复未完成")
        : above ? "中枢上方运行，先观察近端回落边界" : center ? "中枢内整理，近端方向尚未选定" : "近期高低点之间整理，趋势待确认";
      guide.focus = `先看 ${money(low.price)} 是否守住，再看 ${money(high.price)} 能否收盘站上${below ? `；收复 ${money(center.lower)} 后，再评估原中枢修复` : ""}。`;
    }
    if (active.length) {
      const signal = active[0];
      guide.evidence.push(`${signal.label}识别于 ${signal.confirmed_at}${signal.invalidation != null ? `，失效边界 ${money(signal.invalidation)}` : ""}；先核对候选的回试和失效条件。`);
    }
    guide.evidence.push(`现价${price > high.price ? "高于" : price < low.price ? "低于" : "仍在"}近期笔端点区间 ${money(low.price)}–${money(high.price)}${!high.confirmed || !low.confirmed ? "，其中形成中的端点会随结构更新" : ""}。`);
    if (center) guide.evidence.push(`现价${below ? "低于中枢下沿" : above ? "高于中枢上沿" : "处于中枢内部"}；最近中枢为 ${money(center.lower)}–${money(center.upper)}。`);
    if (latest && !latest.confirmed) guide.evidence.push(`图中虚线是从 ${money(latest.start.price)} 到 ${money(latest.end.price)} 的形成中笔，表示已识别的摆动方向；端点后的回落或反弹还可能尚未成笔。`);
    const nextUp = [below ? center.lower : null, higher?.price].filter((value) => value != null && value > high.price).sort((a, b) => a - b)[0];
    guide.scenarios = [
      ...(Math.abs(price - high.price) > 0.000001 ? [{ direction: "up", label: "第一步：反弹改善", status: gate(high.price, true),
        condition: `${frame.label}收盘高于 ${money(high.price)}，随后回落的已确认笔低点高于 ${money(low.price)}。`,
        expectation: nextUp != null ? `形成更高低点后，再观察 ${money(nextUp)} 附近能否继续改善。` : "等待形成更高低点和新的中枢，再评估上涨结构。",
        invalidation: `收盘重新跌回 ${money(high.price)} 以下，突破改善未能保持；跌破 ${money(low.price)} 则反弹结构进一步受损。`,
        chart_label: price > high.price ? "回踩近期高点" : "收盘站上高点",
        chart_tone: price > high.price ? "range" : "up",
        // The midpoint is only a visual placeholder inside the valid
        // higher-low zone; its price is intentionally hidden.
        projection_levels: price > high.price ? [high.price]
          : price < low.price ? [low.price, high.price, low.price + (high.price - low.price) * 0.45, high.price]
            : [high.price, low.price + (high.price - low.price) * 0.45, high.price],
        projection_labels: price > high.price ? ["回踩边界"]
          : price < low.price ? ["收回近期低点", "突破观察", "更高低点区·示意", "再次确认"]
            : ["突破观察", "更高低点区·示意", "再次确认"],
        projection_hide_prices: price > high.price ? [] : price < low.price ? [2] : [1] }] : []),
      ...(price > low.price || (lower && price > lower.price) ? [{ direction: "down", label: "风险路径：近期低点失守", status: gate(low.price, false),
        condition: `${frame.label}收盘低于 ${money(low.price)}，随后反抽仍未收回该位置。`,
        expectation: lower ? `再观察前低 ${money(lower.price)} 附近是否形成止跌结构。` : "下方没有更低的已确认笔端点，等待新低及反抽结构形成。",
        invalidation: `收盘重新站回 ${money(low.price)} 上方，并形成止跌的反向笔，才重新评估这一风险路径。`,
        chart_label: price > low.price ? "低点失守 → 前低" : "继续观察前低",
        // Below the recent low, continue only to a lower confirmed endpoint.
        // Without one there is no defensible numeric path to draw.
        projection_levels: price > low.price ? [low.price, ...(lower ? [low.price - (low.price - lower.price) * 0.3, lower.price] : [])]
          : [lower.price],
        projection_labels: price > low.price ? ["近期低点失守", ...(lower ? ["反抽未收回·示意", "前低观察"] : [])]
          : ["前低观察"],
        projection_hide_prices: price > low.price && lower ? [1] : [] }] : []),
    ];
    if (center) guide.scenarios.push(below ? { direction: "repair", label: "第二步：收复原中枢", status: gate(center.lower, true),
      condition: `${frame.label}收盘高于中枢下沿 ${money(center.lower)}，随后回落不再跌出中枢。`,
      expectation: `重新评估中枢内修复，再观察上沿 ${money(center.upper)}；近期高点突破只是更早的一步。`,
      invalidation: `收盘再次低于 ${money(center.lower)}，中枢修复条件失效。`,
      chart_label: "中枢修复", projection_levels: [center.lower, center.upper, center.lower + (center.upper - center.lower) * 0.58, center.upper, ...(high.price > center.upper ? [high.price] : [])],
      projection_labels: ["收复下沿", "观察上沿", "中枢回踩·示意", "重新测试上沿", "近期高点"],
      projection_hide_prices: [2] }
      : above ? { direction: "hold", label: "结构边界：中枢上沿回试", status: gate(center.upper, true),
        condition: `回落的已确认笔低点仍高于中枢上沿 ${money(center.upper)}。`,
        expectation: "核对离开中枢后的回试结构，再评估第三类位置候选。",
        invalidation: `回落触及 ${money(center.upper)}，中枢上方回试不进入的条件失效。`,
        chart_label: "回试中枢上沿", chart_tone: "range",
        projection_levels: [center.upper, price],
        projection_labels: ["回试边界", "返回现价区间·示意"],
        projection_hide_prices: [] }
      : { direction: "range", label: "结构边界：中枢内整理", status: "现价位于中枢内",
        condition: `价格继续在 ${money(center.lower)}–${money(center.upper)} 内运行。`,
        expectation: "先用近端高低点观察摆动变化；突破中枢后再核对回试结构。",
        invalidation: "离开中枢后，形成不重新进入区间的已确认回试笔。",
        chart_label: "中枢内整理",
        projection_levels: [price <= center.lower || price >= center.upper ? (price < center.lower ? center.lower : center.upper) : price,
          price <= center.lower || price >= center.upper ? (center.lower + center.upper) / 2 : (price > (center.lower + center.upper) / 2 ? center.lower : center.upper),
          price <= center.lower || price >= center.upper ? (center.upper + center.lower) / 2 : price > (center.lower + center.upper) / 2 ? center.upper : center.lower],
        projection_labels: ["回到中枢", "区间内摆动·示意", "另一侧边界"],
        projection_hide_prices: [1, 2] });
    guide.scenarios = scenarioTiltScores(frame, weekly, guide.scenarios);
    // A downside path may be intentionally omitted when price is already
    // below the recent low and no lower confirmed endpoint exists. Keep its
    // rule-based tilt available for the live risk summary even without a path.
    guide.downsideTilt = scenarioTiltScores(frame, weekly, [{ direction: "down" }])[0]?.tilt_score;
    return guide;
  }

  // Summarize how the live quote sits against the closed-bar structure. This
  // describes price location only; it never promotes an intraday move into a
  // confirmed pen or close-based condition.
  function liveSpotGuide(frame, spot, guidance) {
    const price = spot != null && Number.isFinite(Number(spot)) && Number(spot) > 0
      ? Number(spot) : frame?.last_close;
    if (price == null || !Number.isFinite(Number(price)) || Number(price) <= 0) return null;
    const close = frame?.last_close != null && Number.isFinite(Number(frame.last_close)) ? Number(frame.last_close) : null;
    const center = frame?.position?.center;
    const endpoints = new Map();
    (frame?.strokes || []).forEach((stroke) => {
      [stroke.start, stroke.end].forEach((point) => {
        const key = point.date + "-" + point.kind + "-" + point.price;
        endpoints.set(key, { ...point, confirmed: point.confirmed || Boolean(endpoints.get(key)?.confirmed) });
      });
    });
    const ordered = [...endpoints.values()].sort((a, b) => a.date.localeCompare(b.date));
    const high = [...ordered].reverse().find((point) => point.kind === "top");
    const low = [...ordered].reverse().find((point) => point.kind === "bottom");
    let position = "历史结构不足，先观察现价与最近收盘价的变化。";
    if (center) {
      const location = price > center.upper ? "高于中枢上沿 " + money(center.upper)
        : price < center.lower ? "低于中枢下沿 " + money(center.lower)
          : "位于中枢 " + money(center.lower) + "–" + money(center.upper) + " 内";
      position = "现价 " + money(price) + "，" + location + (close != null ? "；最近收盘 " + money(close) : "") + "。";
    } else if (high && low) {
      position = "现价 " + money(price) + "；近期笔端点区间为 " + money(low.price) + "–" + money(high.price)
        + (close != null ? "，最近收盘 " + money(close) : "") + "。";
    } else if (close != null) {
      position = "现价 " + money(price) + "；最近收盘 " + money(close) + "。";
    }
    const scenarios = guidance?.scenarios || [];
    const bullish = scenarios.find((item) => item.direction === "up")
      || scenarios.find((item) => item.direction === "hold")
      || scenarios.find((item) => item.direction === "range");
    const bullishNext = center && price < center.lower
      ? scenarios.find((item) => item.direction === "repair") : null;
    const bearish = scenarios.find((item) => item.direction === "down");
    const bullishCondition = bullish?.condition
      || (high ? "等待" + frame.label + "收盘站上近期笔高点 " + money(high.price) + "，再核对随后回落结构。" : "等待可确认的笔高低点和后续结构。");
    const bearishCondition = bearish?.condition
      || (low ? (price <= low.price ? "现价已到近期笔低点 " + money(low.price) + " 下方；等待反抽能否收回，再核对收盘。"
        : "观察" + frame.label + "收盘是否跌破近期笔低点 " + money(low.price) + "；跌破后等待反抽确认。") : "等待下方结构边界形成后再评估风险。");
    const change = close != null && close > 0 ? (price / close - 1) * 100 : null;
    const freshness = frame.label + " K 线截至 " + (frame.as_of || "未知日期")
      + (change == null ? "" : " · 现价较最近收盘 " + (change > 0 ? "+" : "") + change.toFixed(2) + "%")
      + "。现价越过观察位不等于收盘确认。";
    return { position,
      bullishLabel: bullish?.label || "向上观察", bullishStatus: bullish?.status || "等待结构确认",
      bullish: bullishCondition, bullishExpectation: bullish?.expectation || "观察高点突破后的回落能否形成更高低点。",
      bullishTilt: bullish?.tilt_score == null ? "" : "倾向 " + bullish.tilt_score + "/100",
      bullishInvalidation: bullish?.invalidation || "条件未满足前，不把现价波动当作结构确认。",
      bullishNextLabel: bullishNext?.label || "",
      bullishNextStatus: bullishNext?.status || "",
      bullishNextPrerequisite: bullishNext ? "前置：完成第一步后，再进入中枢修复观察。" : "",
      bullishNextCondition: bullishNext?.condition || "",
      bullishNextExpectation: bullishNext?.expectation || "",
      bullishNextTilt: bullishNext?.tilt_score == null ? "" : "倾向 " + bullishNext.tilt_score + "/100",
      bullishNextInvalidation: bullishNext?.invalidation || "",
      bearishLabel: bearish?.label || "向下风险", bearishStatus: bearish?.status || "等待结构确认",
      bearish: bearishCondition, bearishExpectation: bearish?.expectation || "观察失守后的反抽能否收回边界。",
      bearishTilt: (bearish?.tilt_score ?? guidance?.downsideTilt) == null ? "" : "倾向 " + (bearish?.tilt_score ?? guidance.downsideTilt) + "/100",
      bearishInvalidation: bearish?.invalidation || "条件未满足前，不把现价波动当作结构确认。",
      freshness };
  }

  function frameView(frame, spot, weekly = null, showScenarios = true, mobileChart = false, mobileBars = 30) {
    if (!frame) return emptyFrame("--");
    const center = frame.position?.center;
    const last = frame.strokes?.[frame.strokes.length - 1];
    const fixed = (frame.strokes || []).filter((stroke) => stroke.confirmed);
    const guidance = frameGuidance(frame, spot, weekly);
    const chart = chartModel(frame, spot, guidance.scenarios, showScenarios, mobileChart, mobileBars);
    const markers = chart ? (frame.signals || []).filter((signal) => turningKinds.has(signal.kind) && signal.status !== "invalidated").map((signal) => {
      const point = chart.points.find((item) => item.date === (signal.origin_date || signal.date) && item.price === signal.price);
      if (!point) return null;
      const buy = signal.side === "buy", watch = signal.status === "watch";
      return { key: `${signal.kind}-${signal.origin_date || signal.date}`, x: point.x, y: point.y,
          labelX: Math.max(48, Math.min(chart.plotRight - 35, point.x)),
        labelY: buy ? Math.min(chart.plotBottom - 4, point.labelY + 13) : Math.max(10, point.labelY - 13),
        label: `${buy ? "底" : "顶"}${watch ? "观察" : "候选"}`, watch,
        title: `${signal.label} · 极值 ${signal.origin_date || signal.date} ${money(signal.price)} · 首次识别 ${signal.observed_at || signal.confirmed_at} · 当前阶段 ${signal.phase_at || signal.confirmed_at}` };
    }).filter(Boolean) : [];
    const plan = window.OptionScopeChanPlan.view(frame, spot, weekly);
    if (plan.metrics.length) {
      guidance.title = plan.headline;
      guidance.focus = plan.focus;
    }
    return {
      label: frame.label, available: frame.bar_count > 0, asOf: frame.as_of || "--", bars: frame.bar_count,
      position: frame.position?.label || "等待中枢形成", detail: frame.position?.detail || "历史数据不足",
      center: center ? `${money(center.lower)}–${money(center.upper)}` : "尚未形成",
      boundary: center ? `下沿 ${money(center.lower)} · 上沿 ${money(center.upper)}` : "等待三根已确认笔重叠",
      stroke: last ? `${direction(last.direction)} · ${last.confirmed ? "已确认" : "形成中"}` : "尚未成笔",
      observation: last ? `最近转折价 ${last.end.date} · ${money(last.end.price)}；${last.confirmed ? `确认于 ${last.confirmed_at}` : `等待随后出现有效${last.direction === "up" ? "回落来确认高点" : "反弹来确认低点"}`}` : "尚未识别出满足规则的完整上涨或下跌波段",
      strokes: (frame.strokes || []).length, confirmed: fixed.length, warnings: frame.warnings || [], structure: frame.structure || "等待中枢形成",
      turning: turningView(frame, spot),
      plan,
      spotGuide: liveSpotGuide(frame, spot, guidance),
      signals: (frame.signals || []).filter((signal) => !turningKinds.has(signal.kind)).map((signal, index) => ({ ...signal, key: `${signal.kind}-${index}`,
        dateLabel: `结构位置 ${signal.date} · 识别于 ${signal.confirmed_at}`,
        statusLabel: signal.status === "invalidated" ? "已失效" : (signal.status === "candidate" ? "候选，待次级别确认" : "力度观察"),
        invalidationLabel: signal.invalidation == null ? "" : `参考边界 ${money(signal.invalidation)}`,
        ratioLabel: signal.strength_ratio == null ? "" : `同向 MACD 面积比 ${(signal.strength_ratio * 100).toFixed(0)}%`,
      })),
      guidance, chart: chart ? { ...chart, markers } : null,
    };
  }

  function view(payload, showScenarios = true, mobileChart = false, mobileBars = 30) {
    if (!payload) return empty();
    let historyTime = "--";
    if (payload.history?.fetched_at) {
      const date = new Date(payload.history.fetched_at);
      if (!Number.isNaN(date.getTime())) historyTime = date.toLocaleString("zh-CN", { hour12: false });
    }
    const daily = frameView(payload.daily, payload.spot, payload.weekly, showScenarios, mobileChart, mobileBars), weekly = frameView(payload.weekly, payload.spot, payload.daily, showScenarios, mobileChart, mobileBars);
    const weeklyReady = Boolean(payload.weekly?.position?.center);
    const weeklyContext = weeklyReady ? `周线：${weekly.position}。` : `周线已有 ${weekly.bars} 根收盘 K 线、${weekly.confirmed} 根已确认笔，尚未形成中枢，不能给日线方向增加确认。`;
    return {
      available: Boolean(payload.daily?.bar_count || payload.weekly?.bar_count),
      label: daily.available ? `日线判断：${daily.guidance.title}` : (payload.summary?.label || "等待结构形成"),
      detail: daily.available ? `${daily.guidance.focus} ${weeklyContext}` : (payload.summary?.detail || "历史数据不足"),
      alignment: payload.summary?.alignment || "insufficient",
      daily, weekly,
      warnings: payload.warnings || [], rules: payload.rules || [],
      spot: money(payload.spot), historyTime, refreshing: Boolean(payload.history?.refreshing),
    };
  }

  // Closed-bar intraday view. The server sends one frame for the requested
  // 15-minute or 4-hour period; reuse the same frame renderer as daily/weekly.
  function intradayView(payload, showScenarios = true, spotOverride = null, mobileChart = false, mobileBars = 30) {
    const label = payload?.period === "4h" ? "4小时" : "15分钟";
    if (!payload || !payload.frame) return emptyFrame(label);
    const spot = spotOverride != null && Number.isFinite(Number(spotOverride)) && Number(spotOverride) > 0
      ? Number(spotOverride) : payload.spot;
    return frameView(payload.frame, spot, null, showScenarios, mobileChart, mobileBars);
  }

  // Refresh the current price position without rebuilding historical pens.
  // The daily/weekly structure remains tied to completed candles.
  function withSpot(payload, spot, observed = null) {
    if (!payload) return payload;
    // Keep an observed intraday boundary break across a history refetch.
    // A later price recovery cannot reactivate the same candidate.
    if (observed?.symbol === payload.symbol) {
      payload = { ...payload };
      ["daily", "weekly"].forEach((period) => {
        const frame = payload[period];
        if (!frame) return;
        const invalidated = (observed[period]?.signals || []).filter((signal) => signal.status === "invalidated");
        const signals = (frame.signals || []).map((signal) => {
          const previous = invalidated.find((old) => old.kind === signal.kind && old.date === signal.date
            && (turningKinds.has(signal.kind) || old.confirmed_at === signal.confirmed_at) && old.invalidation === signal.invalidation);
          return previous && signal.status !== "invalidated"
            ? { ...signal, status: "invalidated", detail: previous.detail } : signal;
        });
        payload[period] = { ...frame, signals };
      });
    }
    const price = spot != null && Number.isFinite(Number(spot)) && Number(spot) > 0 ? Number(spot) : payload.spot;
    payload = { ...payload, daily: withHoldingRisk(payload.daily, price, observed?.symbol === payload.symbol ? observed.daily : null),
      weekly: withHoldingRisk(payload.weekly, price, observed?.symbol === payload.symbol ? observed.weekly : null) };
    if (price == null || !Number.isFinite(Number(price)) || Number(price) <= 0) return payload;
    spot = price;
    const locate = (frame) => {
      if (!frame?.position?.center) return frame;
      const center = frame.position.center;
      let code = "inside", label = "位于最近中枢内部", detail = "现价处于中枢重叠区，优先观察边界突破及随后的回试";
      let support = center.lower, resistance = center.upper;
      if (spot > center.upper) {
        code = "above"; label = "位于最近中枢上方";
        detail = "现价向上离开中枢；需观察回试是否守住中枢上沿";
        support = center.upper; resistance = null;
      } else if (spot < center.lower) {
        code = "below"; label = "位于最近中枢下方";
        detail = "现价向下离开中枢；需观察反抽能否重新进入中枢";
        support = null; resistance = center.lower;
      }
      const signals = (frame.signals || []).map((signal) => {
        const invalidated = signal.invalidation != null && (
          buyKinds.has(signal.kind) && spot <= signal.invalidation
          || sellKinds.has(signal.kind) && spot >= signal.invalidation
        );
        return invalidated && signal.status !== "invalidated"
          ? { ...signal, status: "invalidated", detail: `${signal.detail}；现价已触及失效边界` } : signal;
      });
      return { ...frame, signals, position: { ...frame.position, code, label, detail, support, resistance } };
    };
    const daily = locate(payload.daily), weekly = locate(payload.weekly);
    let summary = payload.summary;
    if (daily?.position?.center && weekly?.position?.center) {
      const d = daily.position.code, w = weekly.position.code;
      summary = { alignment: "mixed", label: "日周位置尚未同向", detail: "日线与周线处于不同结构位置，结合周线边界观察日线离开与回试" };
      if (d === "above" && w === "above") summary = { alignment: "up", label: "日周均处于中枢上方", detail: "两个周期的位置偏强，继续观察日线回试能否守住中枢上沿" };
      if (d === "below" && w === "below") summary = { alignment: "down", label: "日周均处于中枢下方", detail: "两个周期的位置偏弱，关注日线反抽能否重新进入中枢" };
      if (d === "inside" && w === "inside") summary = { alignment: "range", label: "日周均处于中枢内部", detail: "两个周期都在重叠区内，等待突破和回试结构" };
    }
    return { ...payload, spot, daily, weekly, summary };
  }

  window.OptionScopeChan = { empty, view, withSpot, intradayView, emptyFrame };
}(window));
