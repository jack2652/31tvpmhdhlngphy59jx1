/* 葛兰碧八大法则：将均线交叉、回踩与乖离回归转为可回溯的收盘信号。 */
(function (window) {
  "use strict";

  const SMA_PERIOD = 200;
  const ATR_PERIOD = 14;
  const SLOPE_PERIOD = 20;
  const SHORT_SLOPE_PERIOD = 5;
  const NEAR_ATR = 0.3;
  const FAR_ATR = 2;
  const BUY_FOUR_REARM_ATR = 0.5;
  const SELL_EIGHT_REARM_ATR = 1;
  const TREND_ATR = 0.5;

  function validBar(bar) {
    const open = Number(bar?.open), high = Number(bar?.high), low = Number(bar?.low), close = Number(bar?.close);
    return [open, high, low, close].every(Number.isFinite)
      && open > 0 && high > 0 && low > 0 && close > 0
      && high >= Math.max(open, close) && low <= Math.min(open, close);
  }

  function calculate(inputBars) {
    const bars = Array.isArray(inputBars) ? inputBars : [];
    const sma = new Array(bars.length).fill(null);
    const atr = new Array(bars.length).fill(null);
    const events = [];
    const closes = [];
    const trueRanges = [];
    const previousRules = new Map();
    let buyFourArmed = true;
    let sellEightArmed = true;
    let closeSum = 0;
    let trueRangeSum = 0;
    let previousClose = null;

    for (let index = 0; index < bars.length; index += 1) {
      const bar = bars[index];
      if (!validBar(bar)) {
        closes.length = 0;
        trueRanges.length = 0;
        closeSum = 0;
        trueRangeSum = 0;
        previousClose = null;
        previousRules.clear();
        continue;
      }

      const open = Number(bar.open), high = Number(bar.high), low = Number(bar.low), close = Number(bar.close);
      closes.push(close);
      closeSum += close;
      if (closes.length > SMA_PERIOD) closeSum -= closes.shift();
      if (closes.length === SMA_PERIOD) sma[index] = closeSum / SMA_PERIOD;

      const trueRange = previousClose == null
        ? high - low
        : Math.max(high - low, Math.abs(high - previousClose), Math.abs(low - previousClose));
      trueRanges.push(trueRange);
      trueRangeSum += trueRange;
      if (trueRanges.length > ATR_PERIOD) trueRangeSum -= trueRanges.shift();
      if (trueRanges.length === ATR_PERIOD) atr[index] = trueRangeSum / ATR_PERIOD;
      previousClose = close;
    }

    for (let index = 25; index < bars.length; index += 1) {
      const bar = bars[index];
      if (!validBar(bar)) {
        previousRules.clear();
        buyFourArmed = true;
        sellEightArmed = true;
        continue;
      }
      if (bar.is_forming === true) continue;

      const average = sma[index], previousAverage = sma[index - 1];
      const shortAverage = sma[index - SHORT_SLOPE_PERIOD];
      const longAverage = sma[index - SLOPE_PERIOD];
      const priorLongAverage = sma[index - SHORT_SLOPE_PERIOD - SLOPE_PERIOD];
      const currentAtr = atr[index], previousAtr = atr[index - 1];
      if (![average, previousAverage, shortAverage, longAverage, priorLongAverage, currentAtr, previousAtr].every(Number.isFinite)
        || currentAtr <= 0 || previousAtr <= 0) {
        previousRules.clear();
        buyFourArmed = true;
        sellEightArmed = true;
        continue;
      }

      const previousBar = bars[index - 1];
      if (!validBar(previousBar)) {
        previousRules.clear();
        buyFourArmed = true;
        sellEightArmed = true;
        continue;
      }
      const open = Number(bar.open), high = Number(bar.high), low = Number(bar.low), close = Number(bar.close);
      const priorClose = Number(previousBar.close), priorHigh = Number(previousBar.high), priorLow = Number(previousBar.low);
      const shortSlope = average - shortAverage;
      const longSlope = average - longAverage;
      const priorLongSlope = shortAverage - priorLongAverage;
      const trendBand = currentAtr * TREND_ATR;
      const nearDistance = currentAtr * NEAR_ATR;
      const longTrend = longSlope > trendBand;
      const shortTrend = longSlope < -trendBand;
      const distance = close - average;
      const priorDistance = priorClose - previousAverage;
      if (distance >= -currentAtr * BUY_FOUR_REARM_ATR) buyFourArmed = true;
      if (distance <= currentAtr * SELL_EIGHT_REARM_ATR) sellEightArmed = true;
      const crossedUp = priorClose <= previousAverage && close > average;
      const crossedDown = priorClose >= previousAverage && close < average;

      const conditions = [
        { rule: 1, side: "buy", active: crossedUp && shortSlope > 0 && longSlope <= trendBand && longSlope > priorLongSlope },
        { rule: 2, side: "buy", active: crossedUp && longSlope > trendBand },
        { rule: 3, side: "buy", active: longTrend && priorClose > previousAverage
          && low <= average + nearDistance && close > average && close > open && close > priorClose },
        { rule: 4, side: "buy", active: buyFourArmed && close < average && average - close >= currentAtr * FAR_ATR
          && priorClose < previousAverage && distance > priorDistance && close > priorClose && close > priorHigh },
        { rule: 5, side: "sell", active: crossedDown && shortSlope < 0 && longSlope >= -trendBand && longSlope < priorLongSlope },
        { rule: 6, side: "sell", active: crossedDown && longSlope < -trendBand },
        { rule: 7, side: "sell", active: shortTrend && priorClose < previousAverage
          && high >= average - nearDistance && close < average && close < open && close < priorClose },
        { rule: 8, side: "sell", active: sellEightArmed && close > average && close - average >= currentAtr * FAR_ATR
          && priorClose > previousAverage && distance < priorDistance && close < priorClose && close < priorLow },
      ];

      for (const condition of conditions) {
        const wasActive = previousRules.get(condition.rule) === true;
        if (condition.active && !wasActive) {
          events.push({
            index,
            date: String(bar.date || ""),
            rule: condition.rule,
            side: condition.side,
            price: close,
          });
          if (condition.rule === 4) buyFourArmed = false;
          if (condition.rule === 8) sellEightArmed = false;
        }
        previousRules.set(condition.rule, condition.active);
      }
    }

    return { sma, events };
  }

  window.OptionScopeGranville = { calculate };
})(window);
