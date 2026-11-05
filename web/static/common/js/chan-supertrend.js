/* 参考 KivancOzbilgic 的 TradingView SuperTrend STRATEGY（MPL-2.0: https://mozilla.org/MPL/2.0/），并按项目价格图适配。 */
(function (window) {
  "use strict";

  const ATR_PERIOD = 10;
  const MULTIPLIER = 3;
  const completedCache = new WeakMap();

  function validBar(bar) {
    const open = Number(bar?.open), high = Number(bar?.high), low = Number(bar?.low), close = Number(bar?.close);
    return [open, high, low, close].every(Number.isFinite)
      && open > 0 && high > 0 && low > 0 && close > 0
      && high >= Math.max(open, close) && low <= Math.min(open, close);
  }

  function trueRange(bar, previousClose) {
    const high = Number(bar.high), low = Number(bar.low);
    if (!Number.isFinite(previousClose)) return high - low;
    return Math.max(high - low, Math.abs(high - previousClose), Math.abs(low - previousClose));
  }

  function calculate(candles) {
    const bars = Array.isArray(candles) ? candles : [];
    const atr = new Array(bars.length).fill(null);
    const upperBand = new Array(bars.length).fill(null);
    const lowerBand = new Array(bars.length).fill(null);
    const upTrend = new Array(bars.length).fill(null);
    const downTrend = new Array(bars.length).fill(null);
    const trend = new Array(bars.length).fill(null);
    const events = [];
    let trSeedCount = 0, trSeedSum = 0, previousAtr = null;
    let previousClose = null, previousUpper = null, previousLower = null;
    let previousTrend = 1;

    for (let index = 0; index < bars.length; index += 1) {
      const bar = bars[index];
      if (!validBar(bar)) {
        trSeedCount = 0;
        trSeedSum = 0;
        previousAtr = null;
        previousClose = null;
        previousUpper = null;
        previousLower = null;
        previousTrend = 1;
        continue;
      }

      const close = Number(bar.close);
      const range = trueRange(bar, previousClose);
      if (Number.isFinite(previousAtr)) {
        previousAtr += (range - previousAtr) / ATR_PERIOD;
      } else {
        trSeedCount += 1;
        trSeedSum += range;
        if (trSeedCount === ATR_PERIOD) previousAtr = trSeedSum / ATR_PERIOD;
      }

      if (Number.isFinite(previousAtr)) {
        const source = (Number(bar.high) + Number(bar.low)) / 2;
        const rawUpper = source - MULTIPLIER * previousAtr;
        const rawLower = source + MULTIPLIER * previousAtr;
        const priorUpper = Number.isFinite(previousUpper) ? previousUpper : rawUpper;
        const priorLower = Number.isFinite(previousLower) ? previousLower : rawLower;
        const currentUpper = Number.isFinite(previousClose) && previousClose > priorUpper
          ? Math.max(rawUpper, priorUpper) : rawUpper;
        const currentLower = Number.isFinite(previousClose) && previousClose < priorLower
          ? Math.min(rawLower, priorLower) : rawLower;
        const currentTrend = previousTrend === -1 && close > priorLower
          ? 1
          : previousTrend === 1 && close < priorUpper ? -1 : previousTrend;

        atr[index] = previousAtr;
        upperBand[index] = currentUpper;
        lowerBand[index] = currentLower;
        trend[index] = currentTrend;
        if (currentTrend === 1) upTrend[index] = currentUpper;
        else downTrend[index] = currentLower;

        if (bar.is_forming !== true && index > 0 && currentTrend !== previousTrend) {
          events.push({
            index,
            date: String(bar.date || ""),
            side: currentTrend === 1 ? "buy" : "sell",
            price: currentTrend === 1 ? currentUpper : currentLower,
          });
        }
        previousUpper = currentUpper;
        previousLower = currentLower;
        previousTrend = currentTrend;
      }
      previousClose = close;
    }

    return { atr, upperBand, lowerBand, upTrend, downTrend, trend, events };
  }

  function cachedCompletedResult(sourceCandles, endIndex) {
    let entries = completedCache.get(sourceCandles);
    if (!entries) {
      entries = new Map();
      completedCache.set(sourceCandles, entries);
    }
    if (entries.has(endIndex)) return entries.get(endIndex);
    const candles = endIndex === sourceCandles.length ? sourceCandles : sourceCandles.slice(0, endIndex);
    const result = calculate(candles);
    entries.set(endIndex, result);
    if (entries.size > 2) entries.delete(entries.keys().next().value);
    return result;
  }

  function calculateWithLiveBar(sourceCandles, displayCandles, liveIndex) {
    const liveBar = liveIndex >= 0 ? displayCandles[liveIndex] : null;
    if (liveBar && liveBar.is_forming !== true) return calculate(displayCandles);
    if (!liveBar) {
      return { ...cachedCompletedResult(sourceCandles, sourceCandles.length), liveIndex: -1, live: null };
    }

    const endIndex = Math.max(0, Math.min(liveIndex, sourceCandles.length));
    const completed = cachedCompletedResult(sourceCandles, endIndex);
    if (!validBar(liveBar)) return { ...completed, liveIndex, live: null };
    const previousIndex = liveIndex - 1;
    const previousAtr = completed.atr?.[previousIndex];
    if (!Number.isFinite(previousAtr)) {
      return { ...calculate(displayCandles), liveIndex, live: null };
    }

    const previousClose = Number(displayCandles[previousIndex]?.close);
    const currentAtr = previousAtr + (trueRange(liveBar, previousClose) - previousAtr) / ATR_PERIOD;
    const source = (Number(liveBar.high) + Number(liveBar.low)) / 2;
    const rawUpper = source - MULTIPLIER * currentAtr;
    const rawLower = source + MULTIPLIER * currentAtr;
    const previousUpper = completed.upperBand[previousIndex];
    const previousLower = completed.lowerBand[previousIndex];
    const priorUpper = Number.isFinite(previousUpper) ? previousUpper : rawUpper;
    const priorLower = Number.isFinite(previousLower) ? previousLower : rawLower;
    const upper = previousClose > priorUpper ? Math.max(rawUpper, priorUpper) : rawUpper;
    const lower = previousClose < priorLower ? Math.min(rawLower, priorLower) : rawLower;
    const previousTrend = completed.trend[previousIndex] || 1;
    const currentTrend = previousTrend === -1 && Number(liveBar.close) > priorLower
      ? 1
      : previousTrend === 1 && Number(liveBar.close) < priorUpper ? -1 : previousTrend;

    return {
      ...completed,
      liveIndex,
      live: {
        atr: currentAtr,
        upperBand: upper,
        lowerBand: lower,
        trend: currentTrend,
        upTrend: currentTrend === 1 ? upper : null,
        downTrend: currentTrend === -1 ? lower : null,
      },
    };
  }

  window.OptionScopeSuperTrend = { calculate, calculateWithLiveBar };
})(window);
