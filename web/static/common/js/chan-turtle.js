(function (window) {
  "use strict";

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

  function sameChain(left, right) {
    return (Number(left?.chain_id) || 0) === (Number(right?.chain_id) || 0);
  }

  function priorRange(candles, endIndex, length, field, direction) {
    if (endIndex < length) return null;
    let value = direction === "max" ? -Infinity : Infinity;
    for (let index = endIndex - length; index < endIndex; index += 1) {
      const bar = candles[index];
      if (!validBar(bar) || bar.is_forming === true || !sameChain(bar, candles[endIndex])) return null;
      const price = Number(bar[field]);
      value = direction === "max" ? Math.max(value, price) : Math.min(value, price);
    }
    return value;
  }

  function channelAt(candles, index) {
    const entryUpper = priorRange(candles, index, ENTRY_PERIOD, "high", "max");
    const entryLower = priorRange(candles, index, ENTRY_PERIOD, "low", "min");
    const exitUpper = priorRange(candles, index, EXIT_PERIOD, "high", "max");
    const exitLower = priorRange(candles, index, EXIT_PERIOD, "low", "min");
    return [entryUpper, entryLower, exitUpper, exitLower].every(Number.isFinite)
      ? { entryUpper, entryLower, exitUpper, exitLower } : null;
  }

  function exitEvent(position, channel, bar, index) {
    if (!position || !channel) return null;
    const long = position.direction === "long";
    const level = long ? Math.max(channel.exitLower, position.stopPrice) : Math.min(channel.exitUpper, position.stopPrice);
    if (long ? Number(bar.low) > level : Number(bar.high) < level) return null;
    const open = Number(bar.open);
    return { index, date: String(bar.date || ""), kind: long ? "long-exit" : "short-exit",
      price: long ? Math.min(open, level) : Math.max(open, level), forming: bar.is_forming === true,
      reason: level === position.stopPrice ? "risk-stop" : "channel-exit" };
  }

  function calculate(candles, dmi = []) {
    if (!Array.isArray(candles)) return { channels: [], events: [], position: null };
    const channels = new Array(candles.length).fill(null), events = [];
    const ema = new Array(candles.length).fill(null);
    let position = null, pendingEntry = null, retryDirection = null;
    let seedCount = 0, seedTotal = 0, trend = null;
    let n = null, rangeSum = 0, rangeCount = 0, previousClose = null;

    for (let index = 0; index < candles.length; index += 1) {
      const bar = candles[index], previous = candles[index - 1];
      // 无效行情、中间形成bar或数据链切换后重新预热，不能沿用旧仓位和指标。
      if (!validBar(bar) || (index > 0 && (!validBar(previous) || previous.is_forming === true || !sameChain(previous, bar)))) {
        position = null; pendingEntry = null; retryDirection = null;
        seedCount = 0; seedTotal = 0; trend = null;
        n = null; rangeSum = 0; rangeCount = 0; previousClose = null;
        if (!validBar(bar)) continue;
      }
      const channel = channelAt(candles, index);
      channels[index] = channel;
      const forming = bar.is_forming === true;
      const close = Number(bar.close);
      const riskN = n;
      if (!forming) {
        if (trend === null) {
          seedCount += 1; seedTotal += close;
          if (seedCount === TREND_PERIOD) trend = seedTotal / TREND_PERIOD;
        } else trend += (close - trend) * EMA_ALPHA;
        ema[index] = trend;
      }

      const previousTrend = ema[index - TREND_SLOPE_PERIOD];
      const currentDmi = dmi?.[index];
      const adx = Number(currentDmi?.adx), previousAdx = Number(dmi?.[index - 3]?.adx);
      const pdi = Number(currentDmi?.pdi), mdi = Number(currentDmi?.mdi);
      const ready = channel && Number.isFinite(trend) && Number.isFinite(previousTrend)
        && Number.isFinite(riskN) && riskN > 0 && seedCount >= TREND_PERIOD
        && [adx, pdi, mdi].every(Number.isFinite) && adx > MIN_ADX;
      const trendLong = ready && pdi > mdi && close > trend && trend > previousTrend;
      const trendShort = ready && mdi > pdi && close < trend && trend < previousTrend;
      // 只在2N退出后延续仍有效的同向趋势；趋势变弱或反向即清除许可。
      if (!forming && ((retryDirection === "long" && !trendLong) || (retryDirection === "short" && !trendShort))) retryDirection = null;
      const accelerating = Number.isFinite(previousAdx) && adx > previousAdx;
      const entryLong = trendLong && (accelerating || retryDirection === "long");
      const entryShort = trendShort && (accelerating || retryDirection === "short");

      if (position) {
        const exit = exitEvent(position, channel, bar, index);
        if (exit) {
          events.push(exit);
          // 盘中退出仅为临时提示，不写入已完成历史的仓位状态。
          if (!forming) {
            retryDirection = exit.reason === "risk-stop" && (position.direction === "long" ? trendLong : trendShort) ? position.direction : null;
            position = null;
          }
        }
      } else if (!forming) {
        const confirmsLong = pendingEntry?.direction === "long" && pendingEntry.index === index - 1
          && entryLong && close > pendingEntry.level;
        const confirmsShort = pendingEntry?.direction === "short" && pendingEntry.index === index - 1
          && entryShort && close < pendingEntry.level;
        if (confirmsLong || confirmsShort) {
          const direction = confirmsLong ? "long" : "short";
          const stopPrice = confirmsLong ? Math.max(0, close - 2 * riskN) : close + 2 * riskN;
          position = { direction, stopPrice };
          events.push({ index, date: String(bar.date || ""), kind: `${direction}-entry`, price: close,
            forming: false, riskN, stopPrice, reentry: retryDirection === direction });
          pendingEntry = null; retryDirection = null;
        } else {
          pendingEntry = entryLong && close > channel.entryUpper ? { direction: "long", level: channel.entryUpper, index }
            : entryShort && close < channel.entryLower ? { direction: "short", level: channel.entryLower, index } : null;
        }
      }

      if (forming) continue;
      // N以20根TR均值初始化，再采用原海龟的1/20递推；本根不参与入场风险尺度。
      const tr = previousClose === null ? Number(bar.high) - Number(bar.low)
        : Math.max(Number(bar.high) - Number(bar.low), Math.abs(Number(bar.high) - previousClose), Math.abs(Number(bar.low) - previousClose));
      if (n === null) {
        rangeSum += tr; rangeCount += 1;
        if (rangeCount === ENTRY_PERIOD) n = rangeSum / ENTRY_PERIOD;
      } else n += (tr - n) / ENTRY_PERIOD;
      previousClose = close;
    }
    return { channels, events, position };
  }

  function cachedCompletedResult(sourceCandles, dmi, endIndex) {
    let entries = completedCache.get(sourceCandles);
    if (!entries) { entries = new Map(); completedCache.set(sourceCandles, entries); }
    const cached = entries.get(endIndex);
    if (cached && cached.dmi === dmi) return cached.result;
    const result = calculate(endIndex === sourceCandles.length ? sourceCandles : sourceCandles.slice(0, endIndex), dmi);
    entries.set(endIndex, { dmi, result });
    if (entries.size > 2) entries.delete(entries.keys().next().value);
    return result;
  }

  function calculateWithLiveBar(sourceCandles, displayCandles, dmi, liveIndex) {
    const source = Array.isArray(sourceCandles) ? sourceCandles : [];
    const display = Array.isArray(displayCandles) ? displayCandles : source;
    const liveBar = liveIndex >= 0 ? display[liveIndex] : null;
    if (liveBar && liveBar.is_forming !== true) return calculate(display, dmi);
    const endIndex = liveBar ? Math.max(0, Math.min(liveIndex, source.length)) : source.length;
    const completed = cachedCompletedResult(source, dmi, endIndex);
    const liveChannel = liveBar && validBar(liveBar) ? channelAt(display, liveIndex) : null;
    const liveEvent = liveChannel ? exitEvent(completed.position, liveChannel, liveBar, liveIndex) : null;
    return { channels: completed.channels, events: completed.events, liveIndex: liveBar ? liveIndex : -1, liveChannel, liveEvent };
  }

  window.OptionScopeTurtle = { calculate, calculateWithLiveBar };
})(window);
