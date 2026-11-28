// 趋势线以已收盘行情为准，同一份行情在平移和缩放时共用锚点。
(function (window) {
  const LOOKBACK = 160;
  const ATR_PERIOD = 14;
  const CONFIRMATION_BARS = 2;
  const MAX_PIVOTS_PER_SIDE = 28;
  const MIN_ANCHOR_SPAN = 6;
  const MAX_UNTESTED_SPAN = 40;
  const MAX_EXTENSION = 40;
  const MAX_PROJECTED_ATR = 2;
  const cache = new WeakMap();
  const empty = Object.freeze([]);

  function completedWindow(candles, asOf) {
    let end = candles.length - 1;
    while (end >= 0 && (candles[end].is_forming === true || (asOf && candles[end].date > asOf))) end -= 1;
    if (end < 0) return null;
    const chain = Number(candles[end].chain_id) || 0;
    const bars = [];
    // 断链或无效行情处结束回看。额外读取 ATR 预热所需的行情。
    for (let index = end; index >= Math.max(0, end - LOOKBACK - ATR_PERIOD + 1); index -= 1) {
      const bar = candles[index];
      const open = Number(bar.open), high = Number(bar.high), low = Number(bar.low), close = Number(bar.close);
      if (!bar.date || (index < end && bar.date >= candles[index + 1].date) || bar.is_forming === true
          || (Number(bar.chain_id) || 0) !== chain || ![open, high, low, close].every(Number.isFinite)
          || low <= 0 || high < Math.max(open, close) || low > Math.min(open, close)) break;
      bars.push({ date: bar.date, high, low, close });
    }
    if (bars.length < MIN_ANCHOR_SPAN + CONFIRMATION_BARS + 2) return null;
    bars.reverse();
    let average = 0;
    for (let index = 0; index < bars.length; index += 1) {
      const bar = bars[index], prior = bars[index - 1];
      const range = prior ? Math.max(bar.high - bar.low, Math.abs(bar.high - prior.close), Math.abs(bar.low - prior.close)) : bar.high - bar.low;
      average += (range - average) / Math.min(index + 1, ATR_PERIOD);
      bar.atr = Math.max(average, bar.close * 0.001);
    }
    return { bars, offset: end - bars.length + 1 };
  }

  function confirmedWeights(frame, bars) {
    const firstDate = bars[0].date, lastDate = bars.at(-1).date;
    const weights = new Map();
    const add = (date, kind, weight) => {
      if (!date || date < firstDate || date > lastDate) return;
      const key = kind + ":" + date;
      weights.set(key, Math.max(weights.get(key) || 0, weight));
    };
    const levels = frame.recursive_levels || [];
    const base = levels.find((level) => Number(level.level) === 0);
    const strokes = frame.strokes_v4?.length ? frame.strokes_v4 : base?.strokes?.length ? base.strokes : frame.strokes;
    for (const stroke of strokes || []) {
      const confirmed = stroke.confirmed_at || stroke.end?.confirmed_at;
      if (stroke.confirmed !== true || !confirmed || confirmed < stroke.end?.date || confirmed > lastDate
          || !stroke.start || stroke.start.date > stroke.end?.date) continue;
      add(stroke.start.date, stroke.start.kind, 1);
      add(stroke.end?.date, stroke.end?.kind, 1);
    }
    const segments = levels.length ? levels.flatMap((level) => level.segments || []) : frame.segments;
    for (const segment of segments || []) {
      if (segment.confirmed !== true || segment.forming === true || !segment.confirmed_at
          || segment.confirmed_at < segment.end || segment.confirmed_at > lastDate || segment.start > segment.end) continue;
      add(segment.start, segment.start_kind, 2);
      add(segment.end, segment.end_kind, 2);
    }
    return weights;
  }

  function swingPivots(bars, weights, support) {
    const extreme = support ? "low" : "high";
    const opposite = support ? "high" : "low";
    const kind = support ? "bottom" : "top";
    const direction = support ? -1 : 1;
    const points = [];
    for (let index = Math.max(1, bars.length - LOOKBACK); index < bars.length - CONFIRMATION_BARS; index += 1) {
      const bar = bars[index], prior = bars[index - 1];
      const next = bars[index + 1], confirmation = bars[index + CONFIRMATION_BARS];
      const price = bar[extreme];
      if (direction * (price - prior[extreme]) <= 0 || direction * (price - next[extreme]) < 0
          || direction * (price - confirmation[extreme]) < 0) continue;
      const before = direction * (price - prior[opposite]);
      const after = Math.max(direction * (price - next[opposite]), direction * (price - confirmation[opposite]));
      const swing = Math.min(before, after) / bar.atr;
      if (swing < 0.75) continue;
      points.push({ index, price, weight: (weights.get(kind + ":" + bar.date) || 0) + Math.min(swing, 2) * 0.5 });
    }
    return points.slice(-MAX_PIVOTS_PER_SIDE);
  }

  function fitLine(first, second, points, bars, support) {
    const span = second.index - first.index;
    if (span < MIN_ANCHOR_SPAN) return null;
    const move = second.price - first.price;
    if (Math.abs(move) < Math.max(second.price * 0.002, bars[second.index].atr * 0.85)) return null;
    const slope = move / span, latest = bars.length - 1;
    const futureBars = Math.floor(bars[latest].atr * MAX_PROJECTED_ATR / Math.abs(slope));
    const horizon = Math.min(span, MAX_EXTENSION, latest - second.index + futureBars);
    const endIndex = second.index + (slope < 0 ? Math.min(horizon, Math.ceil(second.price / -slope) - 1) : horizon);
    if (endIndex < latest) return null;
    const priceAt = (index) => first.price + slope * (index - first.index);
    const distance = Math.abs(priceAt(latest) - bars[latest].close) / bars[latest].atr;
    if (distance > 3.5) return null;

    const direction = support ? 1 : -1;
    let violations = 0, priorCloseThrough = false;
    for (let index = first.index + 1; index <= latest; index += 1) {
      const bar = bars[index], value = priceAt(index);
      const closeThrough = direction * (value - bar.close) / bar.atr;
      const wickThrough = direction * (value - (support ? bar.low : bar.high)) / bar.atr;
      // 收盘明显穿越或连续穿越即失效，影线允许小幅扫过。
      if (closeThrough > 0.65 || (priorCloseThrough && closeThrough > 0.25) || wickThrough > 1.1) return null;
      priorCloseThrough = closeThrough > 0.25;
      if (wickThrough > 0.25) violations += 1;
    }
    if (violations / (latest - first.index) > 0.16) return null;

    let touches = 2, lastTouch = second.index, priorTouch = first.index, lastSolidTouch = first.index;
    for (const point of points) {
      if (point.index - priorTouch < 4 || Math.abs(point.index - second.index) < 4
          || Math.abs(point.price - priceAt(point.index)) > bars[point.index].atr * 0.35) continue;
      priorTouch = point.index;
      lastTouch = Math.max(lastTouch, point.index);
      touches += 1;
      if (point.index < second.index) {
        if (point.index - lastSolidTouch > MAX_UNTESTED_SPAN) return null;
        lastSolidTouch = point.index;
      }
    }
    // 长线的触及点也要贯穿锚点之间，避免把相隔很久的波段连在一起。
    if (second.index - lastSolidTouch > MAX_UNTESTED_SPAN) return null;
    const score = Math.min(touches - 2, 3) * 18 + Math.min(span / 40, 1) * 16
      + Math.max(0, 1 - (latest - lastTouch) / MAX_EXTENSION) * 24
      + (first.weight + second.weight) * 6 - distance * 4 - violations * 2;
    return { startIndex: first.index, solidEndIndex: second.index, endIndex,
      confirmedIndex: second.index + CONFIRMATION_BARS, startPrice: first.price, endPrice: second.price, slope, span, score };
  }

  function selectLines(bars, weights, offset, support) {
    const points = swingPivots(bars, weights, support);
    const candidates = [];
    for (let first = 0; first < points.length - 1; first += 1) {
      for (let second = first + 1; second < points.length; second += 1) {
        const line = fitLine(points[first], points[second], points, bars, support);
        if (line) candidates.push(line);
      }
    }
    candidates.sort((a, b) => b.score - a.score || b.solidEndIndex - a.solidEndIndex || b.span - a.span);
    if (!candidates.length) return empty;
    const main = candidates[0], latest = bars.length - 1, atr = bars[latest].atr;
    const valueNow = (line) => line.endPrice + line.slope * (latest - line.solidEndIndex);
    // 只补一条独立的近期线，避免近似锚点、角度或价位重复叠画。
    const recent = candidates.find((line) => line.startIndex > main.startIndex && line.span <= main.span * 0.65
      && line.solidEndIndex >= main.solidEndIndex && latest - line.solidEndIndex <= 12
      && Math.abs(line.slope - main.slope) * line.span >= atr * 0.65
      && Math.abs(valueNow(line) - valueNow(main)) >= atr * 0.65);
    const side = support ? "support" : "resistance";
    return (recent ? [main, recent] : [main]).map((line, rank) => ({
      key: side + ":" + bars[line.startIndex].date + ":" + bars[line.solidEndIndex].date,
      side, primary: rank === 0,
      startIndex: line.startIndex + offset, solidEndIndex: line.solidEndIndex + offset,
      endIndex: line.endIndex + offset, confirmedIndex: line.confirmedIndex + offset,
      startPrice: line.startPrice, endPrice: line.endPrice, slope: line.slope,
    }));
  }

  function calculate(frame) {
    const candles = frame.candles;
    if (!candles?.length) return empty;
    const inputs = [frame.as_of, frame.strokes_v4, frame.strokes, frame.recursive_levels, frame.segments];
    const previous = cache.get(candles);
    if (previous && inputs.every((input, index) => previous.inputs[index] === input)) return previous.lines;
    const history = completedWindow(candles, frame.as_of);
    let lines = empty;
    if (history) {
      const { bars, offset } = history;
      const weights = confirmedWeights(frame, bars);
      const selected = [...selectLines(bars, weights, offset, false), ...selectLines(bars, weights, offset, true)];
      if (selected.length) lines = selected;
    }
    cache.set(candles, { inputs, lines });
    return lines;
  }

  window.OptionScopeTrendlines = { calculate };
}(window));
