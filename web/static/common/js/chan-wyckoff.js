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

  function sameChain(left, right) {
    return (Number(left?.chain_id) || 0) === (Number(right?.chain_id) || 0);
  }

  function createState() {
    return {
      phase: "", bias: "", startIndex: -1, rangeHigh: null, rangeLow: null,
      climaxPrice: null, climaxVolume: 0, climaxSpread: 0, climaxIndex: -1,
      arPrice: null, arIndex: -1, arConfirmed: false,
      springPrice: null, springVolume: 0, springSpread: 0, springIndex: -1,
      breakoutIndex: -1, breakoutVolume: 0, outsideBars: 0, failureBars: 0,
      invalidBars: 0, pullbackSeen: false,
    };
  }

  function phaseSnapshot(state, index) {
    if (!state.phase || !Number.isFinite(state.rangeHigh) || !Number.isFinite(state.rangeLow)) return null;
    return { phase: state.phase, bias: state.bias, startIndex: state.startIndex, endIndex: index,
      rangeHigh: state.rangeHigh, rangeLow: state.rangeLow, rangeReady: state.arConfirmed };
  }

  function calculate(candles) {
    const bars = Array.isArray(candles) ? candles : [];
    const snapshots = new Array(bars.length).fill(null), events = [], history = [];
    let trSeedSum = 0, trSeedCount = 0, atr = null, previousClose = null;
    let state = createState();
    const emit = (index, kind, side, price) => events.push({ index, date: String(bars[index].date || ""), kind,
      label: kind === "AR" ? (state.bias === "accumulation" ? "自动反弹" : "自动回落") : eventLabels[kind] || kind,
      side, price, phase: state.phase });

    for (let index = 0; index < bars.length; index += 1) {
      const bar = bars[index], previousBar = bars[index - 1];
      if (!validBar(bar) || (index > 0 && (!validBar(previousBar) || previousBar.is_forming === true || !sameChain(previousBar, bar)))) {
        history.length = 0; trSeedSum = 0; trSeedCount = 0; atr = null; previousClose = null; state = createState();
        if (!validBar(bar)) continue;
      }
      if (bar.is_forming === true) {
        snapshots[index] = phaseSnapshot(state, index);
        continue;
      }
      const high = Number(bar.high), low = Number(bar.low), close = Number(bar.close), open = Number(bar.open);
      const volume = Number(bar.volume), spread = high - low;
      const closePosition = spread > 0 ? (close - low) / spread : 0.5;
      const volumeWindow = history.slice(-VOLUME_PERIOD);
      const volumeReady = volumeWindow.length === VOLUME_PERIOD && volumeWindow.every((item) => Number.isFinite(item.volume) && item.volume > 0);
      const averageVolume = volumeReady ? volumeWindow.reduce((sum, item) => sum + item.volume, 0) / VOLUME_PERIOD : NaN;
      // 所有异常量价阈值都以本根之前的完成行情为基线，并要求完整预热。
      const metricsReady = history.length === RANGE_LOOKBACK && Number.isFinite(atr) && atr > 0;
      if (metricsReady) {
        const priorHigh = Math.max(...history.map((item) => item.high)), priorLow = Math.min(...history.map((item) => item.low));
        const trendClose = history[history.length - TREND_LOOKBACK].close;
        const isAccumulation = state.bias === "accumulation";
        const side = isAccumulation ? "buy" : "sell";
        const edge = isAccumulation ? state.rangeLow : state.rangeHigh;
        const breakoutEdge = isAccumulation ? state.rangeHigh : state.rangeLow;
        const adverseEdge = Number.isFinite(state.springPrice)
          ? (isAccumulation ? Math.min(edge, state.springPrice) : Math.max(edge, state.springPrice)) : edge;
        const adverseDistance = isAccumulation ? adverseEdge - close : close - adverseEdge;
        if (state.phase) {
          state.invalidBars = adverseDistance > atr * 0.5 ? state.invalidBars + 1 : 0;
          const expired = state.phase === "A" ? index - state.climaxIndex > (state.arConfirmed ? 90 : 45)
            : state.phase !== "E" && index - state.startIndex > 260;
          if (expired || adverseDistance > atr * 1.5 || state.invalidBars >= 2) state = createState();
        }

        const hasVolume = Number.isFinite(volume) && volume > 0 && volumeReady;
        const volumeSpike = hasVolume && volume >= averageVolume * 1.4;
        const wideSpread = spread >= atr * 1.1;
        const downTrend = trendClose - close >= atr * 1.5, upTrend = close - trendClose >= atr * 1.5;
        const canStart = !state.phase || state.phase === "E";
        if (canStart && downTrend && low < priorLow && volumeSpike && wideSpread && closePosition >= 0.45) {
          state = { ...createState(), phase: "A", bias: "accumulation", startIndex: index,
            rangeHigh: high, rangeLow: low, climaxPrice: low, climaxVolume: volume, climaxSpread: spread, climaxIndex: index };
          emit(index, "SC", "buy", low);
        } else if (canStart && upTrend && high > priorHigh && volumeSpike && wideSpread && closePosition <= 0.55) {
          state = { ...createState(), phase: "A", bias: "distribution", startIndex: index,
            rangeHigh: high, rangeLow: low, climaxPrice: high, climaxVolume: volume, climaxSpread: spread, climaxIndex: index };
          emit(index, "BC", "sell", high);
        } else if (state.phase === "A") {
          state.rangeHigh = Math.max(state.rangeHigh, high); state.rangeLow = Math.min(state.rangeLow, low);
          const age = index - state.climaxIndex;
          const arMove = isAccumulation ? close - state.climaxPrice : state.climaxPrice - close;
          const reaction = isAccumulation ? close > open && close > previousClose : close < open && close < previousClose;
          if (!state.arConfirmed && age >= 2 && age <= 45 && arMove >= atr * 1.8 && reaction) {
            state.arConfirmed = true; state.arIndex = index; state.arPrice = isAccumulation ? high : low;
            emit(index, "AR", side, state.arPrice);
          } else if (state.arConfirmed && index - state.arIndex >= 2 && hasVolume) {
            const testNear = isAccumulation
              ? low <= state.climaxPrice + atr * 0.8 && low >= state.climaxPrice - atr * 0.2 && closePosition >= 0.5
              : high >= state.climaxPrice - atr * 0.8 && high <= state.climaxPrice + atr * 0.2 && closePosition <= 0.5;
            const lessEffort = volume < state.climaxVolume * 0.8 && volume <= averageVolume * 1.1
              && spread < state.climaxSpread * 0.8 && spread <= atr * 1.2;
            if (testNear && lessEffort) {
              state.phase = "B";
              emit(index, "ST", side, isAccumulation ? low : high);
            }
          }
        } else if (state.phase) {
          const initialPhase = state.phase;
          let emitted = false;
          // 探底/冲高未能守位或久未成功回测时回到区间建设；Spring/UTAD并非必经事件。
          if (state.phase === "C" && (index - state.springIndex > 30
            || (isAccumulation ? close < state.springPrice : close > state.springPrice))) {
            state.phase = "B"; state.springIndex = -1; state.springPrice = null; state.pullbackSeen = false; state.outsideBars = 0;
          }
          const returnedInside = isAccumulation ? close > edge : close < edge;
          const probe = isAccumulation
            ? low < edge - atr * 0.15 && low >= edge - atr * 2.5 && returnedInside && closePosition >= 0.58
            : high > edge + atr * 0.15 && high <= edge + atr * 2.5 && returnedInside && closePosition <= 0.42;
          if (state.phase === "B" && probe && hasVolume && volume <= averageVolume * 1.6) {
            state.phase = "C"; state.springPrice = isAccumulation ? low : high; state.springVolume = volume;
            state.springSpread = spread; state.springIndex = index; state.pullbackSeen = false; state.outsideBars = 0;
            emit(index, isAccumulation ? "Spring" : "UTAD", side, state.springPrice); emitted = true;
          } else if (state.phase === "C" && state.springIndex >= 0 && !state.pullbackSeen
            && index - state.springIndex >= 2 && hasVolume) {
            const testNear = isAccumulation
              ? low > state.springPrice && low <= edge + atr * 0.8 && close > edge && closePosition >= 0.5
              : high < state.springPrice && high >= edge - atr * 0.8 && close < edge && closePosition <= 0.5;
            if (testNear && volume < state.springVolume * 0.85 && volume <= averageVolume
              && spread < state.springSpread && spread <= atr) {
              state.pullbackSeen = true; emit(index, "Test", side, isAccumulation ? low : high); emitted = true;
            }
          }

          const breakDistance = isAccumulation ? close - breakoutEdge : breakoutEdge - close;
          const brokeOut = breakDistance > atr * 0.15;
          const strongClose = isAccumulation ? closePosition >= 0.65 : closePosition <= 0.35;
          const strongEffort = hasVolume && spread >= atr * 0.8 && volume >= averageVolume * 1.1;
          if (state.phase === "B" || state.phase === "C") {
            state.outsideBars = brokeOut ? state.outsideBars + 1 : 0;
            if (!emitted && brokeOut && strongClose && strongEffort
              && (state.outsideBars >= 2 || volume >= averageVolume * 1.25)) {
              state.phase = "D"; state.breakoutIndex = index; state.breakoutVolume = volume;
              state.outsideBars = 0; state.failureBars = 0; state.pullbackSeen = false;
              emit(index, isAccumulation ? "SOS" : "SOW", side, close); emitted = true;
            }
          }
          if (initialPhase === "D" || initialPhase === "E") {
            const failedBreakout = isAccumulation ? close < breakoutEdge - atr * 0.3 : close > breakoutEdge + atr * 0.3;
            state.failureBars = failedBreakout ? state.failureBars + 1 : 0;
            if (state.failureBars >= 2) {
              state.phase = "B"; state.breakoutIndex = -1; state.outsideBars = 0; state.pullbackSeen = false;
            } else if (state.phase === "D") {
              const retest = isAccumulation
                ? low >= breakoutEdge - atr * 0.3 && low <= breakoutEdge + atr * 0.7 && close > breakoutEdge && closePosition >= 0.5
                : high <= breakoutEdge + atr * 0.3 && high >= breakoutEdge - atr * 0.7 && close < breakoutEdge && closePosition <= 0.5;
              if (!emitted && !state.pullbackSeen && index - state.breakoutIndex >= 2 && retest && hasVolume
                && volume < state.breakoutVolume * 0.85 && volume <= averageVolume && spread <= atr) {
                state.pullbackSeen = true; emit(index, isAccumulation ? "LPS" : "LPSY", side, isAccumulation ? low : high); emitted = true;
              }
              state.outsideBars = breakDistance > atr ? state.outsideBars + 1 : 0;
              if (!emitted && state.outsideBars >= 3) {
                state.phase = "E"; emit(index, "E", side, close);
              }
            }
          }
        }
      }
      snapshots[index] = phaseSnapshot(state, index);
      const tr = previousClose === null ? spread : Math.max(spread, Math.abs(high - previousClose), Math.abs(low - previousClose));
      if (atr === null) {
        trSeedSum += tr; trSeedCount += 1;
        if (trSeedCount === ATR_PERIOD) atr = trSeedSum / ATR_PERIOD;
      } else atr += (tr - atr) / ATR_PERIOD;
      previousClose = close;
      history.push({ high, low, close, volume });
      if (history.length > RANGE_LOOKBACK) history.shift();
    }
    return { snapshots, events };
  }

  function cachedCompleted(source, endIndex) {
    let entries = completedCache.get(source);
    if (!entries) { entries = new Map(); completedCache.set(source, entries); }
    if (entries.has(endIndex)) return entries.get(endIndex);
    const result = calculate(endIndex === source.length ? source : source.slice(0, endIndex));
    entries.set(endIndex, result);
    if (entries.size > 2) entries.delete(entries.keys().next().value);
    return result;
  }

  function calculateWithLiveBar(sourceCandles, displayCandles, liveIndex) {
    const source = Array.isArray(sourceCandles) ? sourceCandles : [];
    const display = Array.isArray(displayCandles) ? displayCandles : source;
    const liveBar = liveIndex >= 0 ? display[liveIndex] : null;
    if (liveBar && liveBar.is_forming !== true) return calculate(display);
    const endIndex = liveBar ? Math.max(0, Math.min(liveIndex, source.length)) : source.length;
    const completed = cachedCompleted(source, endIndex);
    if (!liveBar) return completed;
    const snapshots = completed.snapshots.slice();
    // 替换中的完成bar必须从缓存截止剔除；实时bar仅延续同链已确认阶段。
    const prior = display[liveIndex - 1];
    snapshots[liveIndex] = validBar(liveBar) && validBar(prior) && prior.is_forming !== true && sameChain(prior, liveBar)
      && snapshots[liveIndex - 1] ? { ...snapshots[liveIndex - 1], endIndex: liveIndex } : null;
    return { snapshots, events: completed.events };
  }

  window.OptionScopeWyckoff = { calculate, calculateWithLiveBar };
})(window);
