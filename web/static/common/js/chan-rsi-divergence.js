/* Wilder RSI 与逐根收盘确认的价格和动量枢轴。 */
(function (window) {
  "use strict";

  const period = 14, radius = 2, pairingRadius = 2, maxPivotDistance = 60;
  const cache = new WeakMap();

  function calculate(candles) {
    if (cache.has(candles)) return cache.get(candles);
    const values = [], events = [];
    const highs = [], lows = [];
    let previous = null, gain = 0, loss = 0, count = 0, chain = null, chainStart = 0;
    let trackers;
    const activeEvents = { top: [], bottom: [] };
    const watches = [];
    function reset(index) {
      previous = null;
      gain = 0;
      loss = 0;
      count = 0;
      chainStart = index;
      trackers = { top: {}, bottom: {} };
    }
    function pivot(series, index, side) {
      if (index - radius < chainStart || !Number.isFinite(series[index])) return false;
      const value = series[index];
      for (let offset = -radius; offset <= radius; offset += 1) {
        if (offset === 0) continue;
        const neighbor = series[index + offset];
        if (!Number.isFinite(neighbor)) return false;
        // 平台只接受最右端的枢轴。
        if (side === "top" ? neighbor > value || (offset > 0 && neighbor === value)
          : neighbor < value || (offset > 0 && neighbor === value)) return false;
      }
      return true;
    }
    function comparePivots(index, side, status) {
      const tracker = trackers[side];
      const prices = side === "top" ? highs : lows;
      const priceIndex = index - radius - pairingRadius;
      if (!pivot(prices, priceIndex, side)) return null;
      let rsiIndex = -1;
      for (let offset = 0; offset <= pairingRadius && rsiIndex === -1; offset += 1) {
        for (const candidate of new Set([priceIndex - offset, priceIndex + offset])) {
          if (candidate > (tracker.previous?.rsiIndex ?? -1) && pivot(values, candidate, side)) {
            rsiIndex = candidate;
            break;
          }
        }
      }
      if (rsiIndex < 0) return null;
      const price = prices[priceIndex], rsi = values[rsiIndex];
      const current = { priceIndex, price, rsiIndex, rsi };
      const prior = tracker.previous;
      if (status === "confirmed") tracker.previous = current;
      const diverges = prior && (side === "top" ? price > prior.price && rsi < prior.rsi : price < prior.price && rsi > prior.rsi);
      if (!diverges || priceIndex - prior.priceIndex > maxPivotDistance) return null;
      for (let after = priceIndex + 1; after <= index; after += 1) {
        if (side === "top" ? prices[after] > price : prices[after] < price) return null;
      }
      return { id: `rsi:${side}:${candles[prior.priceIndex].date}:${candles[priceIndex].date}`, side, status, index,
        date: candles[index].date, pivotDate: candles[priceIndex].date, confirmedAt: status === "confirmed" ? candles[index].date : null,
        invalidationPrice: price, from: prior, to: current, invalidatedAt: null, invalidatedIndex: null };
    }

    reset(0);
    for (const [index, bar] of candles.entries()) {
      const { high, low, close } = bar;
      const live = bar.is_forming === true;
      if (bar.chain_id != null && chain !== bar.chain_id) {
        chain = bar.chain_id;
        reset(index);
      }
      if (![high, low, close].every(Number.isFinite) || low <= 0 || high < close || close < low) {
        values.push(null);
        reset(index + 1);
        continue;
      }
      highs[index] = high;
      lows[index] = low;
      if (previous !== null) {
        const delta = close - previous;
        const up = Math.max(delta, 0), down = Math.max(-delta, 0);
        count += 1;
        if (count <= period) {
          gain += up / period;
          loss += down / period;
        } else {
          gain = (gain * (period - 1) + up) / period;
          loss = (loss * (period - 1) + down) / period;
        }
      }
      previous = close;
      values.push(count < period ? null : loss > 0 ? 100 - 100 / (1 + gain / loss) : gain > 0 ? 100 : 50);
      for (const side of ["top", "bottom"]) {
        // 数据缺口会重置计算窗口，已确认事件仍等待后续行情验证失效价。
        if (!live) activeEvents[side] = activeEvents[side].filter((active) => {
          const invalidated = side === "top" ? high > active.invalidationPrice : low < active.invalidationPrice;
          if (invalidated) {
            active.invalidatedAt = bar.date;
            active.invalidatedIndex = index;
          }
          return !invalidated;
        });
        const event = comparePivots(index, side, live ? "watch" : "confirmed");
        if (!event) continue;
        if (live) watches.push(event);
        else {
          activeEvents[side].push(event);
          events.push(event);
        }
      }
    }
    const result = { values, events, watches };
    cache.set(candles, result);
    return result;
  }

  window.OptionScopeRsiDivergence = { calculate };
})(window);
