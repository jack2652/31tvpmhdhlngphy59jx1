/* 布林带与收盘突破，按当前周期完整行情计算。 */
(function (window) {
  "use strict";

  const period = 20;
  const widthLookback = 120;
  const minimumWidths = 40;
  const squeezePercentile = 0.2;
  const cache = new WeakMap();

  function calculate(candles) {
    if (cache.has(candles)) return cache.get(candles);
    const series = [], events = [];
    let closes = [], widths = [], chain = null, priorSide = null;
    for (const [index, bar] of candles.entries()) {
      const close = bar.close;
      if (chain !== (bar.chain_id ?? chain)) {
        closes = [];
        widths = [];
        priorSide = null;
        chain = bar.chain_id;
      }
      if (!Number.isFinite(close) || close <= 0) {
        closes = [];
        widths = [];
        priorSide = null;
        series.push(null);
        continue;
      }
      closes.push(close);
      if (closes.length > period) closes.shift();
      if (closes.length < period) {
        series.push(null);
        continue;
      }
      const middle = closes.reduce((sum, value) => sum + value, 0) / period;
      const deviation = Math.sqrt(closes.reduce((sum, value) => sum + (value - middle) ** 2, 0) / period);
      const upper = middle + 2 * deviation, lower = middle - 2 * deviation;
      const width = (upper - lower) / middle;
      const threshold = widths.length < minimumWidths ? null
        : [...widths].sort((a, b) => a - b)[Math.ceil(widths.length * squeezePercentile) - 1];
      const side = close > upper ? "up" : close < lower ? "down" : "inside";
      const live = bar.is_forming === true;
      const event = priorSide && side !== "inside" && side !== priorSide
        ? { id: `bollinger:${side}:${bar.date}`, index, date: bar.date, side, price: close, status: live ? "watch" : "confirmed" }
        : null;
      series.push({ upper, middle, lower, width, threshold, squeeze: threshold !== null && width <= threshold, live, event });
      if (live) continue;
      if (event) events.push(event);
      priorSide = side;
      widths.push(width);
      if (widths.length > widthLookback) widths.shift();
    }
    const result = { series, events };
    cache.set(candles, result);
    return result;
  }
  window.OptionScopeBollinger = { calculate };
})(window);
