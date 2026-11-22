/* 葛兰碧八大法则：区分趋势转向、短暂穿越恢复、未破回踩和乖离反应。 */
(function (window) {
  "use strict";

  const SMA_PERIOD = 200;
  const ATR_PERIOD = 14;
  const SLOPE_PERIOD = 20;
  const SHORT_SLOPE_PERIOD = 5;
  const NEAR_ATR = 0.3;
  const FAR_ATR = 2;
  const TREND_ATR = 0.5;
  const RECOVERY_BARS = 10;
  const completedCache = new WeakMap();

  function validBar(bar) {
    const open = Number(bar?.open), high = Number(bar?.high), low = Number(bar?.low), close = Number(bar?.close);
    return [open, high, low, close].every(Number.isFinite)
      && open > 0 && high > 0 && low > 0 && close > 0
      && high >= Math.max(open, close) && low <= Math.min(open, close);
  }

  function sameChain(left, right) {
    return (Number(left?.chain_id) || 0) === (Number(right?.chain_id) || 0);
  }

  function calculate(inputBars) {
    const bars = Array.isArray(inputBars) ? inputBars : [];
    const sma = new Array(bars.length).fill(null), events = [];
    const closes = [];
    let closeSum = 0, atr = null, trSeedSum = 0, trSeedCount = 0, previousClose = null;
    let recovery = null, buyPullbackArmed = false, sellPullbackArmed = false;
    let buyFourArmed = true, sellEightArmed = true, lowerExcursion = false, upperExcursion = false;

    for (let index = 0; index < bars.length; index += 1) {
      const bar = bars[index], previousBar = bars[index - 1];
      if (!validBar(bar) || (index > 0 && (!validBar(previousBar) || previousBar.is_forming === true || !sameChain(previousBar, bar)))) {
        closes.length = 0; closeSum = 0; atr = null; trSeedSum = 0; trSeedCount = 0; previousClose = null;
        recovery = null; buyPullbackArmed = false; sellPullbackArmed = false;
        buyFourArmed = true; sellEightArmed = true; lowerExcursion = false; upperExcursion = false;
        if (!validBar(bar)) continue;
      }
      const high = Number(bar.high), low = Number(bar.low), close = Number(bar.close), open = Number(bar.open);
      // 未收盘bar只显示临时均线，不更新波动、事件武装或已确认历史。
      if (bar.is_forming === true) {
        sma[index] = liveAverage(bars, index);
        continue;
      }
      closes.push(close); closeSum += close;
      if (closes.length > SMA_PERIOD) closeSum -= closes.shift();
      if (closes.length === SMA_PERIOD) sma[index] = closeSum / SMA_PERIOD;

      const average = sma[index], previousAverage = sma[index - 1];
      const shortAverage = sma[index - SHORT_SLOPE_PERIOD], longAverage = sma[index - SLOPE_PERIOD];
      const priorLongAverage = sma[index - SHORT_SLOPE_PERIOD - SLOPE_PERIOD];
      // 阈值取前一根完成后的Wilder ATR，避免本根异常波幅把自身归一化掉。
      const ready = closes.length === SMA_PERIOD && [average, previousAverage, shortAverage, longAverage, priorLongAverage, atr].every(Number.isFinite)
        && atr > 0 && validBar(previousBar) && previousBar.is_forming !== true;
      if (ready) {
        const shortSlope = average - shortAverage, longSlope = average - longAverage;
        const priorLongSlope = shortAverage - priorLongAverage;
        const trendBand = atr * TREND_ATR, nearDistance = atr * NEAR_ATR;
        const longTrend = longSlope > trendBand && shortSlope > 0;
        const shortTrend = longSlope < -trendBand && shortSlope < 0;
        const distance = close - average, priorClose = Number(previousBar.close);
        const crossedUp = priorClose <= previousAverage && close > average;
        const crossedDown = priorClose >= previousAverage && close < average;

        if (distance >= -atr * 0.5) { buyFourArmed = true; lowerExcursion = false; }
        if (distance <= atr) { sellEightArmed = true; upperExcursion = false; }
        if (distance <= -atr * FAR_ATR && longSlope <= trendBand && shortSlope <= 0) lowerExcursion = true;
        if (distance >= atr * FAR_ATR && longSlope >= -trendBand && shortSlope >= 0) upperExcursion = true;

        if (recovery && (index - recovery.index > RECOVERY_BARS
          || (recovery.side === "buy" ? !longTrend || low < average - atr : !shortTrend || high > average + atr))) recovery = null;
        // 2/6只在既有趋势的短暂收盘越界后恢复；首次交叉不能冒充回踩。
        if (!recovery && crossedDown && longTrend && low >= average - atr) recovery = { side: "buy", index };
        else if (!recovery && crossedUp && shortTrend && high <= average + atr) recovery = { side: "sell", index };

        const buyRecovery = recovery?.side === "buy" && index > recovery.index && crossedUp && longTrend;
        const sellRecovery = recovery?.side === "sell" && index > recovery.index && crossedDown && shortTrend;
        const buyRetest = buyPullbackArmed && longTrend && priorClose > previousAverage
          && low >= average && low <= average + nearDistance && close > average && close > open && close > priorClose;
        const sellRetest = sellPullbackArmed && shortTrend && priorClose < previousAverage
          && high <= average && high >= average - nearDistance && close < average && close < open && close < priorClose;

        let rule = null;
        if (buyRecovery) rule = 2;
        else if (sellRecovery) rule = 6;
        else if (crossedUp && shortSlope > 0 && longSlope >= -trendBand && priorLongSlope <= trendBand && longSlope > priorLongSlope) rule = 1;
        else if (crossedDown && shortSlope < 0 && longSlope <= trendBand && priorLongSlope >= -trendBand && longSlope < priorLongSlope) rule = 5;
        else if (buyRetest) rule = 3;
        else if (sellRetest) rule = 7;
        else if (buyFourArmed && lowerExcursion && close < average && longSlope <= trendBand
          && close > open && close > priorClose && close > Number(previousBar.high)) rule = 4;
        else if (sellEightArmed && upperExcursion && close > average && longSlope >= -trendBand
          && close < open && close < priorClose && close < Number(previousBar.low)) rule = 8;

        if (rule !== null) {
          events.push({ index, date: String(bar.date || ""), rule, side: rule <= 4 ? "buy" : "sell", price: close });
          if (rule === 2 || rule === 6) recovery = null;
          if (rule === 2 || rule === 3) buyPullbackArmed = false;
          if (rule === 6 || rule === 7) sellPullbackArmed = false;
          if (rule === 4) { buyFourArmed = false; lowerExcursion = false; }
          if (rule === 8) { sellEightArmed = false; upperExcursion = false; }
        }
        // 回踩提示后必须先离开均线带，再重新接近；近均线反复抖动只算同一轮。
        if (longTrend && low > average + atr * 0.8) buyPullbackArmed = true;
        else if (!longTrend || close <= average) buyPullbackArmed = false;
        if (shortTrend && high < average - atr * 0.8) sellPullbackArmed = true;
        else if (!shortTrend || close >= average) sellPullbackArmed = false;
      }

      const tr = previousClose === null ? high - low
        : Math.max(high - low, Math.abs(high - previousClose), Math.abs(low - previousClose));
      if (atr === null) {
        trSeedSum += tr; trSeedCount += 1;
        if (trSeedCount === ATR_PERIOD) atr = trSeedSum / ATR_PERIOD;
      } else atr += (tr - atr) / ATR_PERIOD;
      previousClose = close;
    }
    return { sma, events };
  }

  function cachedCompletedResult(sourceCandles, endIndex) {
    let entries = completedCache.get(sourceCandles);
    if (!entries) { entries = new Map(); completedCache.set(sourceCandles, entries); }
    if (entries.has(endIndex)) return entries.get(endIndex);
    const result = calculate(endIndex === sourceCandles.length ? sourceCandles : sourceCandles.slice(0, endIndex));
    entries.set(endIndex, result);
    if (entries.size > 2) entries.delete(entries.keys().next().value);
    return result;
  }

  function liveAverage(bars, liveIndex) {
    const liveBar = bars[liveIndex];
    if (!validBar(liveBar)) return null;
    let sum = Number(liveBar.close), count = 1;
    for (let index = liveIndex - 1; index >= 0 && count < SMA_PERIOD; index -= 1) {
      const bar = bars[index];
      if (!validBar(bar) || bar.is_forming === true || !sameChain(bar, liveBar)) break;
      sum += Number(bar.close); count += 1;
    }
    return count === SMA_PERIOD ? sum / SMA_PERIOD : null;
  }

  function calculateWithLiveBar(sourceCandles, displayCandles, liveIndex) {
    const source = Array.isArray(sourceCandles) ? sourceCandles : [];
    const display = Array.isArray(displayCandles) ? displayCandles : source;
    const liveBar = liveIndex >= 0 ? display[liveIndex] : null;
    if (liveBar && liveBar.is_forming !== true) return calculate(display);
    const endIndex = liveBar ? Math.max(0, Math.min(liveIndex, source.length)) : source.length;
    return { ...cachedCompletedResult(source, endIndex), liveIndex: liveBar ? liveIndex : -1,
      liveSma: liveBar ? liveAverage(display, liveIndex) : null };
  }

  window.OptionScopeGranville = { calculate, calculateWithLiveBar };
})(window);
