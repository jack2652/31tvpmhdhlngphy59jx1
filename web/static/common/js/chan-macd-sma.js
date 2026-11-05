/* MACD + SMA200 策略：保留 Pine 源码的简单均线 MACD 与趋势过滤。 */
(function (window) {
  "use strict";

  const FAST_PERIOD = 12;
  const SLOW_PERIOD = 26;
  const SIGNAL_PERIOD = 9;
  const TREND_PERIOD = 200;
  const completedCache = new WeakMap();

  function validBar(bar) {
    const open = Number(bar?.open), high = Number(bar?.high), low = Number(bar?.low), close = Number(bar?.close);
    return [open, high, low, close].every(Number.isFinite)
      && open > 0 && high > 0 && low > 0 && close > 0
      && high >= Math.max(open, close) && low <= Math.min(open, close);
  }

  function calculate(candles) {
    const bars = Array.isArray(candles) ? candles : [];
    const smaFast = new Array(bars.length).fill(null);
    const smaSlow = new Array(bars.length).fill(null);
    const smaTrend = new Array(bars.length).fill(null);
    const macd = new Array(bars.length).fill(null);
    const signal = new Array(bars.length).fill(null);
    const histogram = new Array(bars.length).fill(null);
    const events = [];
    let validCount = 0;
    let fastSum = 0, slowSum = 0, trendSum = 0;
    let signalCount = 0, signalSum = 0;

    for (let index = 0; index < bars.length; index += 1) {
      const bar = bars[index];
      if (!validBar(bar)) {
        validCount = 0;
        fastSum = 0;
        slowSum = 0;
        trendSum = 0;
        signalCount = 0;
        signalSum = 0;
        continue;
      }

      const close = Number(bar.close);
      validCount += 1;
      fastSum += close;
      slowSum += close;
      trendSum += close;
      if (validCount > FAST_PERIOD) fastSum -= Number(bars[index - FAST_PERIOD].close);
      if (validCount > SLOW_PERIOD) slowSum -= Number(bars[index - SLOW_PERIOD].close);
      if (validCount > TREND_PERIOD) trendSum -= Number(bars[index - TREND_PERIOD].close);
      if (validCount >= FAST_PERIOD) smaFast[index] = fastSum / FAST_PERIOD;
      if (validCount >= SLOW_PERIOD) smaSlow[index] = slowSum / SLOW_PERIOD;
      if (validCount >= TREND_PERIOD) smaTrend[index] = trendSum / TREND_PERIOD;
      if (Number.isFinite(smaFast[index]) && Number.isFinite(smaSlow[index])) {
        macd[index] = smaFast[index] - smaSlow[index];
        signalCount += 1;
        signalSum += macd[index];
        if (signalCount > SIGNAL_PERIOD) signalSum -= macd[index - SIGNAL_PERIOD];
        if (signalCount >= SIGNAL_PERIOD) {
          signal[index] = signalSum / SIGNAL_PERIOD;
          histogram[index] = macd[index] - signal[index];
        }
      } else {
        signalCount = 0;
        signalSum = 0;
      }

      if (bar.is_forming === true || index === 0 || !Number.isFinite(histogram[index - 1])
        || !Number.isFinite(histogram[index]) || !Number.isFinite(smaTrend[index])) continue;
      const comparisonIndex = index - SLOW_PERIOD;
      if (comparisonIndex < 0 || !validBar(bars[comparisonIndex])) continue;
      const previousClose = Number(bars[comparisonIndex].close);
      const crossedUp = histogram[index - 1] <= 0 && histogram[index] > 0;
      const crossedDown = histogram[index - 1] >= 0 && histogram[index] < 0;
      if (crossedUp && macd[index] > 0 && smaFast[index] > smaSlow[index] && previousClose > smaTrend[index]) {
        events.push({ index, date: String(bar.date || ""), side: "buy", price: close });
      } else if (crossedDown && macd[index] < 0 && smaFast[index] < smaSlow[index] && previousClose < smaTrend[index]) {
        events.push({ index, date: String(bar.date || ""), side: "sell", price: close });
      }
    }

    return { smaFast, smaSlow, smaTrend, macd, signal, histogram, events };
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

  function simpleAverageAt(candles, index, period) {
    if (index < period - 1) return null;
    let sum = 0;
    for (let cursor = index - period + 1; cursor <= index; cursor += 1) {
      if (!validBar(candles[cursor])) return null;
      sum += Number(candles[cursor].close);
    }
    return sum / period;
  }

  function liveValuesAt(candles, index) {
    if (!validBar(candles[index])) return null;
    const smaFast = simpleAverageAt(candles, index, FAST_PERIOD);
    const smaSlow = simpleAverageAt(candles, index, SLOW_PERIOD);
    const smaTrend = simpleAverageAt(candles, index, TREND_PERIOD);
    const macd = Number.isFinite(smaFast) && Number.isFinite(smaSlow) ? smaFast - smaSlow : null;
    let signal = null;
    if (index >= SLOW_PERIOD + SIGNAL_PERIOD - 2) {
      let sum = 0;
      let complete = true;
      for (let cursor = index - SIGNAL_PERIOD + 1; cursor <= index; cursor += 1) {
        const fast = simpleAverageAt(candles, cursor, FAST_PERIOD);
        const slow = simpleAverageAt(candles, cursor, SLOW_PERIOD);
        if (!Number.isFinite(fast) || !Number.isFinite(slow)) {
          complete = false;
          break;
        }
        sum += fast - slow;
      }
      if (complete) signal = sum / SIGNAL_PERIOD;
    }
    return {
      smaFast, smaSlow, smaTrend, macd, signal,
      histogram: Number.isFinite(macd) && Number.isFinite(signal) ? macd - signal : null,
    };
  }

  function calculateWithLiveBar(sourceCandles, displayCandles, liveIndex) {
    const liveBar = liveIndex >= 0 ? displayCandles[liveIndex] : null;
    if (liveBar && liveBar.is_forming !== true) return calculate(displayCandles);
    if (!liveBar) {
      return { ...cachedCompletedResult(sourceCandles, sourceCandles.length), liveIndex: -1, liveValues: null };
    }
    const endIndex = Math.max(0, Math.min(liveIndex, sourceCandles.length));
    const completed = cachedCompletedResult(sourceCandles, endIndex);
    return { ...completed, liveIndex, liveValues: liveValuesAt(displayCandles, liveIndex) };
  }

  window.OptionScopeMacdSma200 = { calculate, calculateWithLiveBar };
})(window);
