/* AF_StochZ Overlay：按 TradingView Pine v6 的默认参数计算叠加信号。 */
(function (window) {
  "use strict";

  const DEFAULTS = Object.freeze({
    minLen: 8, maxLen: 34, erLen: 10,
    smoothK: 3, smoothD: 3, useKDFilter: false,
    zLen: 20, oscPivotBars: 1, osLevel: -1, obLevel: 1,
    pricePivotL: 3, pricePivotR: 2, maxPriceLag: 5,
    useTrendFilter: false, trendEmaLen: 200,
    showRegularDiv: true, showHiddenDiv: true,
    divLookbackMin: 5, divLookbackMax: 60,
  });

  function finite(value) { return Number.isFinite(Number(value)) ? Number(value) : null; }
  function clamp(value, min, max) { return Math.max(min, Math.min(max, value)); }
  function safeLength(value, fallback, minimum = 1) {
    const number = Math.floor(Number(value));
    return Number.isFinite(number) ? Math.max(minimum, number) : fallback;
  }
  function normaliseOptions(options) {
    const source = options && typeof options === "object" ? options : {};
    const minLen = safeLength(source.minLen, DEFAULTS.minLen);
    const maxLen = safeLength(source.maxLen, DEFAULTS.maxLen);
    return {
      ...DEFAULTS, ...source,
      minLen, maxLen: Math.max(minLen, maxLen), erLen: safeLength(source.erLen, DEFAULTS.erLen),
      smoothK: safeLength(source.smoothK, DEFAULTS.smoothK), smoothD: safeLength(source.smoothD, DEFAULTS.smoothD),
      zLen: safeLength(source.zLen, DEFAULTS.zLen, 2), oscPivotBars: safeLength(source.oscPivotBars, DEFAULTS.oscPivotBars),
      pricePivotL: safeLength(source.pricePivotL, DEFAULTS.pricePivotL), pricePivotR: safeLength(source.pricePivotR, DEFAULTS.pricePivotR),
      maxPriceLag: clamp(safeLength(source.maxPriceLag, DEFAULTS.maxPriceLag), 1, 10),
      trendEmaLen: safeLength(source.trendEmaLen, DEFAULTS.trendEmaLen),
      divLookbackMin: safeLength(source.divLookbackMin, DEFAULTS.divLookbackMin),
      divLookbackMax: Math.max(safeLength(source.divLookbackMin, DEFAULTS.divLookbackMin), safeLength(source.divLookbackMax, DEFAULTS.divLookbackMax)),
    };
  }
  function sma(values, index, length) {
    if (index < length - 1) return null;
    let sum = 0;
    for (let cursor = index - length + 1; cursor <= index; cursor += 1) {
      if (values[cursor] == null) return null;
      sum += values[cursor];
    }
    return sum / length;
  }
  function stdev(values, index, length) {
    const mean = sma(values, index, length);
    if (mean == null) return null;
    let sum = 0;
    for (let cursor = index - length + 1; cursor <= index; cursor += 1) sum += (values[cursor] - mean) ** 2;
    return Math.sqrt(sum / length);
  }
  function rangeExtreme(values, index, length, comparator) {
    if (index < length - 1) return null;
    let result = values[index];
    for (let cursor = index - length + 1; cursor <= index; cursor += 1) {
      if (values[cursor] == null) return null;
      result = comparator(result, values[cursor]);
    }
    return result;
  }
  function pivotValue(values, index, left, right, direction) {
    const center = index - right;
    if (center < left || center < 0 || index >= values.length || values[center] == null) return null;
    const value = values[center];
    for (let cursor = center - left; cursor <= center + right; cursor += 1) {
      if (cursor === center || values[cursor] == null) continue;
      if (direction === "low" ? values[cursor] < value : values[cursor] > value) return null;
    }
    return value;
  }
  function ema(values, length) {
    const result = Array(values.length).fill(null);
    const alpha = 2 / (length + 1);
    let previous = null;
    values.forEach((value, index) => {
      if (value == null) return;
      previous = previous == null ? value : previous + alpha * (value - previous);
      result[index] = previous;
    });
    return result;
  }
  function atr14(bars) {
    const result = Array(bars.length).fill(null);
    const tr = bars.map((bar, index) => {
      const high = finite(bar.high), low = finite(bar.low), previousClose = index ? finite(bars[index - 1].close) : null;
      if (high == null || low == null) return null;
      return Math.max(high - low, previousClose == null ? high - low : Math.abs(high - previousClose), previousClose == null ? high - low : Math.abs(low - previousClose));
    });
    for (let index = 0; index < bars.length; index += 1) result[index] = sma(tr, index, 14);
    return result;
  }

  function calculate(inputBars, inputOptions = null) {
    const bars = Array.isArray(inputBars) ? inputBars : [];
    const options = normaliseOptions(inputOptions);
    const length = bars.length;
    const close = bars.map((bar) => finite(bar.close));
    const high = bars.map((bar) => finite(bar.high));
    const low = bars.map((bar) => finite(bar.low));
    const rawStoch = Array(length).fill(null), stochK = Array(length).fill(null), stochD = Array(length).fill(null);
    const fisher = Array(length).fill(null), z = Array(length).fill(null), trend = ema(close, options.trendEmaLen);
    const adaptiveLengths = Array(length).fill(null), oscillatorLow = Array(length).fill(null), oscillatorHigh = Array(length).fill(null);
    const priceLow = Array(length).fill(null), priceHigh = Array(length).fill(null);
    const atr = atr14(bars);
    for (let index = 0; index < length; index += 1) {
      const changeIndex = index - options.erLen;
      const change = changeIndex >= 0 && close[index] != null && close[changeIndex] != null ? Math.abs(close[index] - close[changeIndex]) : null;
      const stepValues = [];
      for (let cursor = index - options.erLen + 1; cursor <= index; cursor += 1) {
        if (cursor <= 0 || close[cursor] == null || close[cursor - 1] == null) { stepValues.length = 0; break; }
        stepValues.push(Math.abs(close[cursor] - close[cursor - 1]));
      }
      const volatility = stepValues.length === options.erLen ? stepValues.reduce((sum, value) => sum + value, 0) : null;
      const er = volatility != null && volatility !== 0 && change != null ? change / volatility : 0;
      const adaptiveLen = clamp(Math.round(options.maxLen - er * (options.maxLen - options.minLen)), options.minLen, options.maxLen);
      adaptiveLengths[index] = adaptiveLen;
      const highestHigh = rangeExtreme(high, index, adaptiveLen, Math.max);
      const lowestLow = rangeExtreme(low, index, adaptiveLen, Math.min);
      if (highestHigh != null && lowestLow != null && close[index] != null) {
        const range = highestHigh - lowestLow;
        rawStoch[index] = range !== 0 ? ((close[index] - lowestLow) / range) * 100 : 50;
      }
      stochK[index] = sma(rawStoch, index, options.smoothK);
      stochD[index] = sma(stochK, index, options.smoothD);
      if (stochK[index] != null) {
        const scaled = clamp((stochK[index] - 50) / 50, -0.998, 0.998);
        fisher[index] = 0.5 * Math.log((1 + scaled) / (1 - scaled));
      }
      const mean = sma(fisher, index, options.zLen), deviation = stdev(fisher, index, options.zLen);
      z[index] = mean != null && deviation != null && deviation !== 0 ? (fisher[index] - mean) / deviation : deviation === 0 && fisher[index] != null ? 0 : null;
      oscillatorLow[index] = pivotValue(z, index, options.oscPivotBars, options.oscPivotBars, "low");
      oscillatorHigh[index] = pivotValue(z, index, options.oscPivotBars, options.oscPivotBars, "high");
      priceLow[index] = pivotValue(low, index, options.pricePivotL, options.pricePivotR, "low");
      priceHigh[index] = pivotValue(high, index, options.pricePivotL, options.pricePivotR, "high");
    }

    const events = [];
    let lastValidOscLowBar = null, lastValidOscHighBar = null, lastFiredBullBar = null, lastFiredBearBar = null;
    for (let index = 0; index < length; index += 1) {
      const oscLowCenter = index - options.oscPivotBars, oscHighCenter = index - options.oscPivotBars;
      if (oscillatorLow[index] != null && z[oscLowCenter] != null && z[oscLowCenter] < options.osLevel) lastValidOscLowBar = oscLowCenter;
      if (oscillatorHigh[index] != null && z[oscHighCenter] != null && z[oscHighCenter] > options.obLevel) lastValidOscHighBar = oscHighCenter;
      const priceLowCenter = index - options.pricePivotR, priceHighCenter = index - options.pricePivotR;
      const leadBull = priceLow[index] != null && lastValidOscLowBar != null
        && priceLowCenter - lastValidOscLowBar >= 0 && priceLowCenter - lastValidOscLowBar <= options.maxPriceLag
        && lastFiredBullBar !== lastValidOscLowBar
        && (!options.useTrendFilter || (trend[index] != null && close[index] > trend[index]))
        && (!options.useKDFilter || (stochK[index] != null && stochD[index] != null && stochK[index] > stochD[index]));
      const leadBear = priceHigh[index] != null && lastValidOscHighBar != null
        && priceHighCenter - lastValidOscHighBar >= 0 && priceHighCenter - lastValidOscHighBar <= options.maxPriceLag
        && lastFiredBearBar !== lastValidOscHighBar
        && (!options.useTrendFilter || (trend[index] != null && close[index] < trend[index]))
        && (!options.useKDFilter || (stochK[index] != null && stochD[index] != null && stochK[index] < stochD[index]));
      if (leadBull) lastFiredBullBar = lastValidOscLowBar;
      if (leadBear) lastFiredBearBar = lastValidOscHighBar;

      let regularBull = false, hiddenBull = false, regularBear = false, hiddenBear = false;
      let previousBullBar = null, previousBearBar = null;
      if (priceLow[index] != null && z[priceLowCenter] != null) {
        for (let distance = options.divLookbackMin; distance <= options.divLookbackMax; distance += 1) {
          const previousConfirmation = index - distance;
          const previousCenter = priceLowCenter - distance;
          if (previousConfirmation < 0 || previousCenter < 0 || priceLow[previousConfirmation] == null || low[previousCenter] == null || z[previousCenter] == null) continue;
          if (options.showRegularDiv && low[priceLowCenter] < low[previousCenter] && z[priceLowCenter] > z[previousCenter]) { regularBull = true; previousBullBar = previousCenter; break; }
          if (options.showHiddenDiv && low[priceLowCenter] > low[previousCenter] && z[priceLowCenter] < z[previousCenter]) { hiddenBull = true; previousBullBar = previousCenter; break; }
        }
      }
      if (priceHigh[index] != null && z[priceHighCenter] != null) {
        for (let distance = options.divLookbackMin; distance <= options.divLookbackMax; distance += 1) {
          const previousConfirmation = index - distance;
          const previousCenter = priceHighCenter - distance;
          if (previousConfirmation < 0 || previousCenter < 0 || priceHigh[previousConfirmation] == null || high[previousCenter] == null || z[previousCenter] == null) continue;
          if (options.showRegularDiv && high[priceHighCenter] > high[previousCenter] && z[priceHighCenter] < z[previousCenter]) { regularBear = true; previousBearBar = previousCenter; break; }
          if (options.showHiddenDiv && high[priceHighCenter] < high[previousCenter] && z[priceHighCenter] > z[previousCenter]) { hiddenBear = true; previousBearBar = previousCenter; break; }
        }
      }
      const add = (side, center, kinds, price, previousBar) => {
        if (!kinds.length || center < 0 || !bars[center]) return;
        const lead = kinds.includes("lead");
        const divergence = kinds.find((kind) => kind !== "lead") || null;
        const label = lead ? (side === "buy" ? "BUY" : "SELL") : divergence === "regular" ? (side === "buy" ? "R-Bull" : "R-Bear") : (side === "buy" ? "H-Bull" : "H-Bear");
        events.push({
          key: `af-stochz-${side}-${center}-${events.length}`, side, label, kinds, divergence, date: bars[center].date,
          confirmedAt: bars[index].date, confirmedIndex: index, index: center, price, markerPrice: side === "buy" ? price - (atr[center] || 0) * 0.2 : price + (atr[center] || 0) * 0.2,
          oscillatorIndex: side === "buy" ? lastValidOscLowBar : lastValidOscHighBar, previousIndex: previousBar,
          z: z[center], stochK: stochK[center], stochD: stochD[center], status: "confirmed",
        });
      };
      const bullKinds = []; if (leadBull) bullKinds.push("lead"); if (regularBull) bullKinds.push("regular"); if (hiddenBull && !regularBull) bullKinds.push("hidden");
      const bearKinds = []; if (leadBear) bearKinds.push("lead"); if (regularBear) bearKinds.push("regular"); if (hiddenBear && !regularBear) bearKinds.push("hidden");
      add("buy", priceLowCenter, bullKinds, low[priceLowCenter], previousBullBar);
      add("sell", priceHighCenter, bearKinds, high[priceHighCenter], previousBearBar);
    }
    return { options, events, series: { adaptiveLengths, rawStoch, stochK, stochD, fisher, z, trend, priceLow, priceHigh, oscillatorLow, oscillatorHigh } };
  }

  window.OptionScopeStochZ = { DEFAULTS, calculate, normaliseOptions };
}(window));
