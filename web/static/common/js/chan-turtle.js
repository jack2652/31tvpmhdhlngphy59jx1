(function (window) {
  const ENTRY_PERIOD = 20;
  const EXIT_PERIOD = 10;
  const TREND_PERIOD = 55;
  const TREND_SLOPE_PERIOD = 5;
  const MIN_ADX = 25;
  const EMA_ALPHA = 2 / (TREND_PERIOD + 1);
  const completedCache = new WeakMap();

  function validBar(bar) {
    const open = Number(bar?.open), high = Number(bar?.high), low = Number(bar?.low), close = Number(bar?.close);
    return [open, high, low, close].every(Number.isFinite)
      && open > 0 && high > 0 && low > 0 && close > 0
      && high >= Math.max(open, close) && low <= Math.min(open, close);
  }

  function priorRange(candles, endIndex, length, field, direction) {
    if (endIndex < length) return null;
    let value = direction === "max" ? -Infinity : Infinity;
    for (let index = endIndex - length; index < endIndex; index += 1) {
      if (!validBar(candles[index])) return null;
      const price = Number(candles[index][field]);
      value = direction === "max" ? Math.max(value, price) : Math.min(value, price);
    }
    return value;
  }

  function trendEma(candles) {
    const values = new Array(candles.length).fill(null);
    let seedCount = 0;
    let seedTotal = 0;
    let previous = null;

    for (let index = 0; index < candles.length; index += 1) {
      if (!validBar(candles[index])) {
        seedCount = 0;
        seedTotal = 0;
        previous = null;
        continue;
      }

      const close = Number(candles[index].close);
      if (previous === null) {
        seedCount += 1;
        seedTotal += close;
        if (seedCount === TREND_PERIOD) {
          previous = seedTotal / TREND_PERIOD;
          values[index] = previous;
        }
        continue;
      }

      previous += (close - previous) * EMA_ALPHA;
      values[index] = previous;
    }

    return values;
  }

  function calculate(candles, dmi = []) {
    if (!Array.isArray(candles)) return { channels: [], events: [] };
    const channels = new Array(candles.length).fill(null);
    const events = [];
    const ema = trendEma(candles);
    let position = "flat";
    let pendingEntry = null;

    // 每根通道只使用当前 K 线之前的历史窗口。
    for (let index = 0; index < candles.length; index += 1) {
      const bar = candles[index];
      if (!validBar(bar)) {
        if (bar?.is_forming !== true) pendingEntry = null;
        continue;
      }
      const entryUpper = priorRange(candles, index, ENTRY_PERIOD, "high", "max");
      const entryLower = priorRange(candles, index, ENTRY_PERIOD, "low", "min");
      const exitUpper = priorRange(candles, index, EXIT_PERIOD, "high", "max");
      const exitLower = priorRange(candles, index, EXIT_PERIOD, "low", "min");
      if ([entryUpper, entryLower, exitUpper, exitLower].some((value) => !Number.isFinite(value))) {
        pendingEntry = null;
        continue;
      }

      channels[index] = { entryUpper, entryLower, exitUpper, exitLower };
      const open = Number(bar.open);
      const high = Number(bar.high);
      const low = Number(bar.low);
      const close = Number(bar.close);
      const currentTrend = ema[index];
      const previousTrend = index >= TREND_SLOPE_PERIOD ? ema[index - TREND_SLOPE_PERIOD] : null;
      const currentDmi = dmi?.[index];
      const adx = Number(currentDmi?.adx);
      const previousAdx = index >= 3 ? Number(dmi?.[index - 3]?.adx) : null;
      const pdi = Number(currentDmi?.pdi);
      const mdi = Number(currentDmi?.mdi);
      const trendReady = Number.isFinite(currentTrend) && Number.isFinite(previousTrend)
        && Number.isFinite(adx) && adx > MIN_ADX
        && Number.isFinite(previousAdx) && adx > previousAdx;
      const date = String(bar.date || "");
      const forming = bar.is_forming === true;
      const trendLong = trendReady && pdi > mdi && close > currentTrend && currentTrend > previousTrend;
      const trendShort = trendReady && mdi > pdi && close < currentTrend && currentTrend < previousTrend;
      const breakoutLong = !forming && trendLong && close > entryUpper;
      const breakoutShort = !forming && trendShort && close < entryLower;

      if (position === "long") {
        if (low < exitLower) {
          events.push({ index, date, kind: "long-exit", price: Number.isFinite(open) && open < exitLower ? open : exitLower, forming });
          position = "flat";
        }
        continue;
      }
      if (position === "short") {
        if (high > exitUpper) {
          events.push({ index, date, kind: "short-exit", price: Number.isFinite(open) && open > exitUpper ? open : exitUpper, forming });
          position = "flat";
        }
        continue;
      }

      if (forming) continue;

      const confirmsLong = pendingEntry?.direction === "long" && trendLong && close > pendingEntry.level;
      const confirmsShort = pendingEntry?.direction === "short" && trendShort && close < pendingEntry.level;
      if (confirmsLong || confirmsShort) {
        const kind = confirmsLong ? "long-entry" : "short-entry";
        events.push({ index, date, kind, price: close, forming: false });
        position = confirmsLong ? "long" : "short";
        pendingEntry = null;
        continue;
      }

      pendingEntry = null;
      if (breakoutLong) {
        pendingEntry = { direction: "long", level: entryUpper };
      } else if (breakoutShort) {
        pendingEntry = { direction: "short", level: entryLower };
      }
    }

    return { channels, events };
  }

  function positionAfter(events) {
    let position = "flat";
    for (const event of events) {
      if (event.kind === "long-entry") position = "long";
      else if (event.kind === "short-entry") position = "short";
      else if (event.kind === "long-exit" || event.kind === "short-exit") position = "flat";
    }
    return position;
  }

  function cachedCompletedResult(sourceCandles, dmi, endIndex) {
    let entries = completedCache.get(sourceCandles);
    if (!entries) {
      entries = new Map();
      completedCache.set(sourceCandles, entries);
    }
    if (entries.has(endIndex)) return entries.get(endIndex);
    const candles = endIndex === sourceCandles.length ? sourceCandles : sourceCandles.slice(0, endIndex);
    const result = calculate(candles, dmi);
    const cached = { ...result, position: positionAfter(result.events) };
    entries.set(endIndex, cached);
    if (entries.size > 2) entries.delete(entries.keys().next().value);
    return cached;
  }

  function calculateWithLiveBar(sourceCandles, displayCandles, dmi, liveIndex) {
    const liveBar = liveIndex >= 0 ? displayCandles[liveIndex] : null;
    if (liveBar && liveBar.is_forming !== true) return calculate(displayCandles, dmi);
    if (!liveBar) {
      return { ...cachedCompletedResult(sourceCandles, dmi, sourceCandles.length), liveIndex: -1, liveChannel: null, liveEvent: null };
    }
    const baseEnd = liveIndex < sourceCandles.length ? liveIndex : sourceCandles.length;
    const completed = cachedCompletedResult(sourceCandles, dmi, baseEnd);
    let liveChannel = null, liveEvent = null;
    if (validBar(liveBar)) {
      const entryUpper = priorRange(displayCandles, liveIndex, ENTRY_PERIOD, "high", "max");
      const entryLower = priorRange(displayCandles, liveIndex, ENTRY_PERIOD, "low", "min");
      const exitUpper = priorRange(displayCandles, liveIndex, EXIT_PERIOD, "high", "max");
      const exitLower = priorRange(displayCandles, liveIndex, EXIT_PERIOD, "low", "min");
      if ([entryUpper, entryLower, exitUpper, exitLower].every(Number.isFinite)) {
        liveChannel = { entryUpper, entryLower, exitUpper, exitLower };
        const open = Number(liveBar.open), high = Number(liveBar.high), low = Number(liveBar.low);
        if (completed.position === "long" && low < exitLower) {
          liveEvent = { index: liveIndex, date: String(liveBar.date || ""), kind: "long-exit",
            price: Number.isFinite(open) && open < exitLower ? open : exitLower, forming: true };
        } else if (completed.position === "short" && high > exitUpper) {
          liveEvent = { index: liveIndex, date: String(liveBar.date || ""), kind: "short-exit",
            price: Number.isFinite(open) && open > exitUpper ? open : exitUpper, forming: true };
        }
      }
    }
    return { channels: completed.channels, events: completed.events, liveIndex, liveChannel, liveEvent };
  }

  window.OptionScopeTurtle = { calculate, calculateWithLiveBar };
})(window);
