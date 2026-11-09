/* 优化版 MFI：资金流量、变色均线和个股/大盘强弱对比。 */
(function (window) {
  "use strict";

  const PERIOD = 14;
  const AVERAGE_PERIOD = 9;
  const completedCache = new WeakMap();

  function cachedResult(sourceCandles, benchmarkCandles) {
    const cached = completedCache.get(sourceCandles);
    return cached && cached.benchmarkCandles === benchmarkCandles ? cached.result : null;
  }

  function storeCachedResult(sourceCandles, benchmarkCandles, result) {
    completedCache.set(sourceCandles, { benchmarkCandles, result });
    return result;
  }

  function validBar(bar) {
    const high = Number(bar?.high), low = Number(bar?.low), close = Number(bar?.close);
    const volume = Number(bar?.volume);
    return [high, low, close, volume].every(Number.isFinite)
      && high > 0 && low > 0 && close > 0 && high >= low && volume >= 0;
  }

  function dateKey(bar) {
    return String(bar?.date || "").slice(0, 10);
  }

  function calculateSeries(candles) {
    const bars = Array.isArray(candles) ? candles : [];
    const values = new Array(bars.length).fill(null);
    const average = new Array(bars.length).fill(null);
    const trend = new Array(bars.length).fill(null);
    const moneyFlows = [];
    let previousTypical = null;
    let averageWindow = [];

    for (let index = 0; index < bars.length; index += 1) {
      const bar = bars[index];
      if (!validBar(bar)) {
        previousTypical = null;
        moneyFlows.length = 0;
        averageWindow = [];
        continue;
      }
      const high = Number(bar.high), low = Number(bar.low), close = Number(bar.close);
      const typical = (high + low + close) / 3;
      const rawFlow = typical * Math.max(0, Number(bar.volume) || 0);
      let positive = 0, negative = 0;
      if (previousTypical != null) {
        if (typical > previousTypical) positive = rawFlow;
        else if (typical < previousTypical) negative = rawFlow;
      }
      previousTypical = typical;
      moneyFlows.push({ positive, negative });
      if (moneyFlows.length > PERIOD) moneyFlows.shift();
      if (moneyFlows.length === PERIOD) {
        let positiveSum = 0, negativeSum = 0;
        for (const flow of moneyFlows) {
          positiveSum += flow.positive;
          negativeSum += flow.negative;
        }
        const value = negativeSum === 0
          ? (positiveSum === 0 ? 50 : 100)
          : Math.max(0, Math.min(100, 100 - 100 / (1 + positiveSum / negativeSum)));
        values[index] = value;
        averageWindow.push(value);
        if (averageWindow.length > AVERAGE_PERIOD) averageWindow.shift();
        if (averageWindow.length === AVERAGE_PERIOD) {
          const sum = averageWindow.reduce((total, item) => total + item, 0);
          average[index] = sum / AVERAGE_PERIOD;
          const previousAverage = average[index - 1];
          trend[index] = Number.isFinite(previousAverage)
            ? average[index] > previousAverage ? "up" : average[index] < previousAverage ? "down" : "flat"
            : null;
        }
      }
    }
    return { values, average, trend };
  }

  // 指数没有成交量时不能生成可比较的资金流。把整段基准标记为缺失，
  // 避免把无效数据误画成 0.00 或一条看似正常的对比线。
  function hasUsableMoneyFlow(candles) {
    let volumeBars = 0;
    for (const bar of Array.isArray(candles) ? candles : []) {
      if (validBar(bar) && Number(bar.volume) > 0) volumeBars += 1;
    }
    return volumeBars >= PERIOD + 1;
  }

  function crossingEvents(series, benchmarkSeries, candles) {
    const events = [];
    for (let index = 1; index < series.values.length; index += 1) {
      const bar = candles[index];
      if (bar?.is_forming === true) continue;
      const value = series.values[index], previous = series.values[index - 1];
      const average = series.average[index], previousAverage = series.average[index - 1];
      if (![value, previous, average, previousAverage].every(Number.isFinite)) continue;
      const goldenCross = previous <= previousAverage && value > average && value <= 35;
      const deathCross = previous >= previousAverage && value < average && value >= 65;
      if (!goldenCross && !deathCross) continue;
      const benchmark = Number(benchmarkSeries?.[index]);
      const relative = Number.isFinite(benchmark)
        ? value > benchmark + 0.01 ? "strong" : value < benchmark - 0.01 ? "weak" : "neutral"
        : "unknown";
      events.push({
        index,
        date: String(bar?.date || ""),
        kind: goldenCross ? "golden-cross" : "death-cross",
        side: goldenCross ? "buy" : "sell",
        price: Number(bar?.close),
        mfi: value,
        average,
        benchmarkMfi: Number.isFinite(benchmark) ? benchmark : null,
        relative,
      });
    }
    return events;
  }

  function calculate(candles, benchmarkCandles = []) {
    const bars = Array.isArray(candles) ? candles : [];
    const benchmarkBars = Array.isArray(benchmarkCandles) ? benchmarkCandles : [];
    const source = calculateSeries(bars);
    const benchmark = calculateSeries(benchmarkBars);
    const benchmarkByDate = new Map();
    if (hasUsableMoneyFlow(benchmarkBars)) {
      benchmarkBars.forEach((bar, index) => {
        const key = dateKey(bar);
        if (key && Number.isFinite(benchmark.values[index])) {
          benchmarkByDate.set(key, benchmark.values[index]);
        }
      });
    }
    const benchmarkMfi = bars.map((bar) => benchmarkByDate.get(dateKey(bar)) ?? null);
    return {
      mfi: source.values,
      benchmarkMfi,
      average: source.average,
      trend: source.trend,
      zones: { overbought: 80, oversold: 20 },
      events: crossingEvents(source, benchmarkMfi, bars),
    };
  }

  function calculateWithLiveBar(sourceCandles, displayCandles, benchmarkCandles = [], liveIndex = -1) {
    const source = Array.isArray(sourceCandles) ? sourceCandles : [];
    const display = Array.isArray(displayCandles) ? displayCandles : source;
    const cached = cachedResult(source, benchmarkCandles);
    if (liveIndex < 0 || !display[liveIndex] || display[liveIndex].is_forming !== true) {
      if (cached && liveIndex < 0) return cached;
      return storeCachedResult(source, benchmarkCandles, calculate(display, benchmarkCandles));
    }
    const completed = cached || storeCachedResult(source, benchmarkCandles, calculate(source, benchmarkCandles));
    const live = calculate(display, benchmarkCandles);
    return { ...completed, liveIndex, liveValues: {
      mfi: live.mfi[liveIndex] ?? null,
      benchmarkMfi: live.benchmarkMfi[liveIndex] ?? null,
      average: live.average[liveIndex] ?? null,
      trend: live.trend[liveIndex] ?? null,
    } };
  }

  window.OptionScopeMfi = { calculate, calculateWithLiveBar };
})(window);
