(function (window) {
  "use strict";

  const ATR_PERIOD = 14;
  const VOLUME_PERIOD = 20;
  const RANGE_LOOKBACK = 30;
  const TREND_LOOKBACK = 20;
  const completedCache = new WeakMap();
  const eventLabels = {
    SC: "抛售高潮", BC: "买入高潮", ST: "二次测试", Spring: "弹簧探底", UTAD: "派发冲高",
    Test: "回测", SOS: "强势信号", SOW: "弱势信号", LPS: "最后支撑", LPSY: "最后供给", E: "趋势延伸",
  };

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

  function createState() {
    return {
      phase: "", bias: "", startIndex: -1, rangeHigh: null, rangeLow: null,
      climaxPrice: null, climaxVolume: 0, climaxIndex: -1, arPrice: null,
      arConfirmed: false, springPrice: null, springVolume: 0, springIndex: -1,
      breakoutIndex: -1, breakoutPrice: null, outsideBars: 0, pullbackSeen: false,
    };
  }

  function phaseSnapshot(state, index) {
    if (!state.phase || !Number.isFinite(state.rangeHigh) || !Number.isFinite(state.rangeLow)) return null;
    return {
      phase: state.phase,
      bias: state.bias,
      startIndex: state.startIndex,
      endIndex: index,
      rangeHigh: state.rangeHigh,
      rangeLow: state.rangeLow,
      rangeReady: state.arConfirmed,
    };
  }

  function calculate(candles) {
    const bars = Array.isArray(candles) ? candles : [];
    const snapshots = new Array(bars.length).fill(null);
    const events = [];
    const volumeHistory = [];
    let volumeSum = 0, trSeedSum = 0, trSeedCount = 0, atr = null, previousClose = null;
    let state = createState();

    const emit = (index, kind, side, price) => {
      const bar = bars[index];
      if (bar?.is_forming === true) return;
      events.push({
        index,
        date: String(bar?.date || ""),
        kind,
        label: kind === "AR"
          ? (state.bias === "accumulation" ? "自动反弹" : "自动回落")
          : eventLabels[kind] || kind,
        side,
        price,
        phase: state.phase,
      });
    };

    for (let index = 0; index < bars.length; index += 1) {
      const bar = bars[index];
      if (bar?.is_forming === true) {
        snapshots[index] = phaseSnapshot(state, index);
        continue;
      }
      if (!validBar(bar)) {
        snapshots[index] = phaseSnapshot(state, index);
        continue;
      }

      const high = Number(bar.high), low = Number(bar.low), close = Number(bar.close);
      const open = Number(bar.open), volume = Math.max(0, Number(bar.volume) || 0);
      const range = high - low;
      const tr = trueRange(bar, previousClose);
      if (Number.isFinite(atr)) atr += (tr - atr) / ATR_PERIOD;
      else {
        trSeedSum += tr;
        trSeedCount += 1;
        if (trSeedCount >= ATR_PERIOD) atr = trSeedSum / ATR_PERIOD;
      }

      const averageVolume = volumeHistory.length ? volumeSum / volumeHistory.length : NaN;
      const start = Math.max(0, index - RANGE_LOOKBACK);
      let priorHigh = -Infinity, priorLow = Infinity;
      for (let cursor = start; cursor < index; cursor += 1) {
        if (!validBar(bars[cursor])) continue;
        priorHigh = Math.max(priorHigh, Number(bars[cursor].high));
        priorLow = Math.min(priorLow, Number(bars[cursor].low));
      }
      const priorTrendClose = Number(bars[Math.max(0, index - TREND_LOOKBACK)]?.close);
      const closePosition = range > 0 ? (close - low) / range : 0.5;
      const metricsReady = Number.isFinite(atr) && atr > 0 && Number.isFinite(averageVolume) && averageVolume > 0;

      if (metricsReady) {
        if (state.phase && state.phase !== "E" && index - state.startIndex > 260) state = createState();

        const volumeSpike = volume >= averageVolume * 1.4;
        const wideSpread = range >= atr * 1.1;
        const downTrend = Number.isFinite(priorTrendClose) && priorTrendClose - close >= atr * 1.5;
        const upTrend = Number.isFinite(priorTrendClose) && close - priorTrendClose >= atr * 1.5;
        const newLow = low < priorLow;
        const newHigh = high > priorHigh;
        const canStart = !state.phase || state.phase === "E";

        if (canStart && downTrend && newLow && volumeSpike && wideSpread && closePosition >= 0.45) {
          state = {
            ...createState(), phase: "A", bias: "accumulation", startIndex: index,
            rangeHigh: high, rangeLow: low, climaxPrice: low, climaxVolume: volume, climaxIndex: index,
          };
          emit(index, "SC", "buy", low);
        } else if (canStart && upTrend && newHigh && volumeSpike && wideSpread && closePosition <= 0.55) {
          state = {
            ...createState(), phase: "A", bias: "distribution", startIndex: index,
            rangeHigh: high, rangeLow: low, climaxPrice: high, climaxVolume: volume, climaxIndex: index,
          };
          emit(index, "BC", "sell", high);
        } else if (state.phase === "A") {
          state.rangeHigh = Math.max(state.rangeHigh, high);
          state.rangeLow = Math.min(state.rangeLow, low);
          const age = index - state.climaxIndex;
          const isAccumulation = state.bias === "accumulation";
          const arMove = isAccumulation ? high - state.climaxPrice : state.climaxPrice - low;
          if (!state.arConfirmed && age >= 2 && age <= 45 && arMove >= atr * 1.8) {
            state.arConfirmed = true;
            state.arPrice = isAccumulation ? high : low;
            if (isAccumulation) state.rangeHigh = Math.max(state.rangeHigh, high);
            else state.rangeLow = Math.min(state.rangeLow, low);
            emit(index, "AR", isAccumulation ? "buy" : "sell", state.arPrice);
          }
          const testNear = isAccumulation
            ? low <= state.climaxPrice + atr * 0.8 && low >= state.climaxPrice - atr * 0.35 && closePosition >= 0.42
            : high >= state.climaxPrice - atr * 0.8 && high <= state.climaxPrice + atr * 0.35 && closePosition <= 0.58;
          if (state.arConfirmed && age >= 3 && testNear && volume <= Math.max(state.climaxVolume, averageVolume) * 1.05) {
            state.phase = "B";
            state.rangeHigh = Math.max(state.rangeHigh, state.arPrice);
            state.rangeLow = Math.min(state.rangeLow, state.climaxPrice);
            emit(index, "ST", isAccumulation ? "buy" : "sell", isAccumulation ? low : high);
          }
        } else if (state.phase === "B" || state.phase === "C" || state.phase === "D" || state.phase === "E") {
          const isAccumulation = state.bias === "accumulation";
          const edge = isAccumulation ? state.rangeLow : state.rangeHigh;
          const breakoutEdge = isAccumulation ? state.rangeHigh : state.rangeLow;
          const returnedInside = isAccumulation ? close > edge : close < edge;
          const probeCandle = isAccumulation
            ? low < edge - atr * 0.15 && low >= edge - atr * 2.5 && returnedInside && closePosition >= 0.58
            : high > edge + atr * 0.15 && high <= edge + atr * 2.5 && returnedInside && closePosition <= 0.42;

          if (state.phase === "B" && probeCandle && volume <= averageVolume * 1.6) {
            state.phase = "C";
            state.springPrice = isAccumulation ? low : high;
            state.springVolume = volume;
            state.springIndex = index;
            emit(index, isAccumulation ? "Spring" : "UTAD", isAccumulation ? "buy" : "sell", state.springPrice);
          } else if (state.phase === "C" && state.springIndex >= 0 && !state.pullbackSeen
            && index - state.springIndex >= 2 && index - state.springIndex <= 30) {
            const nearSpring = isAccumulation
              ? low <= edge + atr * 0.8 && low >= state.springPrice - atr * 0.25 && close > edge && closePosition >= 0.5
              : high >= edge - atr * 0.8 && high <= state.springPrice + atr * 0.25 && close < edge && closePosition <= 0.5;
            if (nearSpring && volume <= Math.max(state.springVolume, averageVolume) * 0.95) {
              state.pullbackSeen = true;
              emit(index, "Test", isAccumulation ? "buy" : "sell", isAccumulation ? low : high);
            }
          }

          const breakDistance = isAccumulation ? close - breakoutEdge : breakoutEdge - close;
          const brokeOut = breakDistance > atr * 0.15;
          if (state.phase !== "D" && state.phase !== "E") state.outsideBars = brokeOut ? state.outsideBars + 1 : 0;
          const strongClose = isAccumulation ? closePosition >= 0.65 : closePosition <= 0.35;
          const strongEffort = range >= atr * 0.8 || volume >= averageVolume * 1.1;
          const breakoutConfirmed = state.phase !== "D" && state.phase !== "E"
            && brokeOut && strongClose && strongEffort
            && (state.outsideBars >= 2 || volume >= averageVolume * 1.25);
          if (breakoutConfirmed) {
            state.phase = "D";
            state.breakoutIndex = index;
            state.breakoutPrice = breakoutEdge;
            state.outsideBars = 0;
            state.pullbackSeen = false;
            emit(index, isAccumulation ? "SOS" : "SOW", isAccumulation ? "buy" : "sell", close);
          } else if (state.phase === "D" && !state.pullbackSeen && index - state.breakoutIndex >= 2) {
            const retest = isAccumulation
              ? low <= breakoutEdge + atr * 0.7 && close > breakoutEdge && closePosition >= 0.45
              : high >= breakoutEdge - atr * 0.7 && close < breakoutEdge && closePosition <= 0.55;
            if (retest && volume <= averageVolume * 1.15) {
              state.pullbackSeen = true;
              emit(index, isAccumulation ? "LPS" : "LPSY", isAccumulation ? "buy" : "sell", isAccumulation ? low : high);
            }
          }

          const acceptedOutside = isAccumulation ? close > breakoutEdge + atr : close < breakoutEdge - atr;
          if (state.phase === "D") state.outsideBars = acceptedOutside ? state.outsideBars + 1 : 0;
          if (state.phase === "D" && state.outsideBars >= 3) {
            state.phase = "E";
            emit(index, "E", isAccumulation ? "buy" : "sell", close);
          }
        }
      }

      snapshots[index] = phaseSnapshot(state, index);
      previousClose = close;
      volumeHistory.push(volume);
      volumeSum += volume;
      if (volumeHistory.length > VOLUME_PERIOD) volumeSum -= volumeHistory.shift();
    }

    return { snapshots, events };
  }

  function cachedCompleted(sourceCandles) {
    const cached = completedCache.get(sourceCandles);
    if (cached) return cached;
    const result = calculate(sourceCandles);
    completedCache.set(sourceCandles, result);
    return result;
  }

  function calculateWithLiveBar(sourceCandles, displayCandles, liveIndex) {
    const source = Array.isArray(sourceCandles) ? sourceCandles : [];
    return cachedCompleted(source);
  }

  window.OptionScopeWyckoff = { calculate, calculateWithLiveBar };
})(window);
