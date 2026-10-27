// 只在当前可见窗口附近，用已确认的缠论转折点筛选结构趋势线。
(function (window) {
  const PIVOT_KINDS = new Set(["top", "bottom"]);
  const MAX_PIVOTS_PER_SIDE = 16;

  function candleIndexForDate(candles, date) {
    let low = 0;
    let high = candles.length;
    while (low < high) {
      const middle = (low + high) >>> 1;
      if (candles[middle].date < date) low = middle + 1;
      else high = middle;
    }
    return low < candles.length && candles[low].date === date ? low : -1;
  }

  function collectPivots(candles, strokes, segments) {
    const pivots = new Map();
    const add = (point, level) => {
      if (!point || !PIVOT_KINDS.has(point.kind) || !point.date) return;
      const price = Number(point.price);
      const index = candleIndexForDate(candles, point.date);
      if (index < 0 || !Number.isFinite(price) || price <= 0) return;
      const key = `${point.date}:${point.kind}`;
      const current = pivots.get(key);
      if (!current || level > current.level) pivots.set(key, { date: point.date, index, kind: point.kind, price, level });
    };

    for (const stroke of strokes || []) {
      if (stroke.confirmed !== true) continue;
      const level = Math.max(0, Number(stroke.level) || 0);
      add(stroke.start, level);
      add(stroke.end, level);
    }
    for (const segment of segments || []) {
      if (segment.confirmed !== true) continue;
      const level = Math.max(0, Number(segment.level) || 0);
      add({ date: segment.start, kind: segment.start_kind, price: segment.start_price }, level + 1);
      add({ date: segment.end, kind: segment.end_kind, price: segment.end_price }, level + 1);
    }

    const ordered = [...pivots.values()].sort((a, b) => a.index - b.index);
    return {
      resistance: ordered.filter((point) => point.kind === "top"),
      support: ordered.filter((point) => point.kind === "bottom"),
    };
  }

  function tolerancesFor(candles) {
    const ranges = candles.map((candle) => Math.max(0, Number(candle.high) - Number(candle.low)));
    const tolerances = new Array(candles.length);
    let rangeSum = 0;
    for (let index = 0; index < candles.length; index += 1) {
      rangeSum += ranges[index];
      if (index >= 14) rangeSum -= ranges[index - 14];
      const sampleCount = Math.min(index + 1, 14);
      const close = Number(candles[index].close) || 0;
      tolerances[index] = Math.max(close * 0.001, rangeSum / sampleCount * 0.35);
    }
    return tolerances;
  }

  function fitLine(points, first, second, side, candles, tolerances, visibleEnd, minimumSpan, maximumSpan, visibleCount, contextBars) {
    const left = points[first];
    const right = points[second];
    const span = right.index - left.index;
    if (span < minimumSpan || span > maximumSpan) return null;
    const slope = (right.price - left.price) / span;
    const scale = Number(candles[visibleEnd]?.close) || left.price;
    const minimumMove = Math.max(scale * 0.004, tolerances[right.index] * 1.25);
    if (Math.abs(right.price - left.price) < minimumMove) return null;
    const priceAt = (index) => left.price + slope * (index - left.index);
    let endIndex = visibleEnd;
    // 收盘价有效穿越后，在破位前一根结束结构线。
    for (let index = left.index + 1; index <= visibleEnd; index += 1) {
      const close = Number(candles[index].close);
      const linePrice = priceAt(index);
      const tolerance = tolerances[index] * 1.4;
      if (side === "support" ? close < linePrice - tolerance : close > linePrice + tolerance) {
        endIndex = index - 1;
        break;
      }
    }
    if (endIndex < right.index) return null;
    if (visibleEnd - endIndex > contextBars) return null;
    const endClose = Number(candles[endIndex].close);
    const endTolerance = Math.max(Math.abs(endClose) * 0.1, tolerances[endIndex] * 7);
    if (Math.abs(priceAt(endIndex) - endClose) > endTolerance) return null;
    const touches = points.filter((point) => point.index >= left.index && point.index <= endIndex
      && Math.abs(point.price - priceAt(point.index)) <= tolerances[point.index] * 1.25);
    if (touches.length < 2) return null;

    const latestTouch = touches[touches.length - 1];
    const levels = touches.reduce((sum, point) => sum + point.level, 0) / touches.length;
    const spanScore = Math.min(span / maximumSpan, 1) * 24;
    const recencyScore = Math.max(0, 1 - (visibleEnd - latestTouch.index) / (visibleCount + contextBars)) * 40;
    const statusScore = endIndex === visibleEnd ? 30 : -((visibleEnd - endIndex) / maximumSpan) * 60;
    const score = touches.length * 100 + spanScore + recencyScore + levels * 12 + statusScore;
    return {
      side,
      startIndex: left.index,
      solidEndIndex: latestTouch.index,
      endIndex,
      startPrice: left.price,
      slope,
      touches: touches.length,
      score,
      priceAt,
    };
  }

  function selectLines(points, side, candles, tolerances, visibleEnd, visibleCount, contextBars) {
    const maximumSpan = Math.max(32, Math.min(120, Math.floor(visibleCount * 0.35)));
    const minimumSpan = Math.max(8, Math.min(18, Math.floor(visibleCount * 0.08)));
    const candidates = [];
    for (let first = 0; first < points.length - 1; first += 1) {
      for (let second = first + 1; second < points.length; second += 1) {
        const candidate = fitLine(points, first, second, side, candles, tolerances, visibleEnd, minimumSpan, maximumSpan, visibleCount, contextBars);
        if (candidate) candidates.push(candidate);
      }
    }
    candidates.sort((a, b) => b.score - a.score);
    return candidates.length ? [candidates[0]] : [];
  }

  function calculate(candles, strokes, segments, visibleStartIndex, visibleEndExclusive) {
    const bars = Array.isArray(candles) ? candles : [];
    if (bars.length < 5) return [];
    const start = Number.isFinite(Number(visibleStartIndex))
      ? Math.max(0, Math.min(bars.length - 1, Math.floor(Number(visibleStartIndex)))) : Math.max(0, bars.length - 32);
    const endExclusive = Number.isFinite(Number(visibleEndExclusive))
      ? Math.max(start + 1, Math.min(bars.length, Math.floor(Number(visibleEndExclusive)))) : bars.length;
    const visibleEnd = endExclusive - 1;
    const visibleCount = endExclusive - start;
    const contextBars = Math.max(24, Math.min(120, Math.floor(visibleCount * 0.25)));
    const contextStart = Math.max(0, start - contextBars);
    const pivots = collectPivots(bars, strokes, segments);
    const tolerances = tolerancesFor(bars);
    const withinWindow = (points) => points.filter((point) => point.index >= contextStart && point.index <= visibleEnd)
      .slice(-MAX_PIVOTS_PER_SIDE);
    return [
      ...selectLines(withinWindow(pivots.resistance), "resistance", bars, tolerances, visibleEnd, visibleCount, contextBars),
      ...selectLines(withinWindow(pivots.support), "support", bars, tolerances, visibleEnd, visibleCount, contextBars),
    ];
  }

  window.OptionScopeTrendlines = { calculate };
}(window));
