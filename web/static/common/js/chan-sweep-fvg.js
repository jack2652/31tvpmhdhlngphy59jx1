// This Pine Script(R) code is subject to the terms of the Mozilla Public License 2.0 at https://mozilla.org/MPL/2.0/
// Pine source: Displacement Sweep Reversal with FVG, LuxAlgo/Vela, converted for OptionScope.
(function (window) {
  "use strict";

  const DEFAULTS = Object.freeze({
    pivotLength: 3,
    maxReversalBars: 10,
    entryWindowBars: 20,
    entryWindowOpacity: 75,
    atrLength: 14,
    bodyAtrMultiple: 0.8,
    minFvgAtrMultiple: 0.15,
  });
  const completedCache = new WeakMap();

  function configFor(overrides) {
    const source = overrides && typeof overrides === "object" ? overrides : {};
    const number = (key, minimum) => {
      const value = Number(source[key]);
      return Number.isFinite(value) && value >= minimum ? value : DEFAULTS[key];
    };
    return {
      pivotLength: Math.min(20, Math.floor(number("pivotLength", 1))),
      maxReversalBars: Math.min(20, Math.floor(number("maxReversalBars", 5))),
      entryWindowBars: Math.min(500, Math.floor(number("entryWindowBars", 1))),
      entryWindowOpacity: Math.min(100, Math.floor(number("entryWindowOpacity", 0))),
      atrLength: Math.floor(number("atrLength", 1)),
      bodyAtrMultiple: number("bodyAtrMultiple", 0.1),
      minFvgAtrMultiple: number("minFvgAtrMultiple", 0),
    };
  }

  function validBar(bar) {
    const values = [bar?.open, bar?.high, bar?.low, bar?.close].map(Number);
    return values.every(Number.isFinite) && values.every((value) => value > 0)
      && values[1] >= Math.max(values[0], values[3]) && values[2] <= Math.min(values[0], values[3]);
  }

  function sameChain(left, right) {
    return (Number(left?.chain_id) || 0) === (Number(right?.chain_id) || 0);
  }

  function trueRange(bar, previousClose) {
    const high = Number(bar.high), low = Number(bar.low);
    return previousClose == null ? high - low : Math.max(high - low, Math.abs(high - previousClose), Math.abs(low - previousClose));
  }

  function pivotValue(candles, pivotIndex, length, kind) {
    if (pivotIndex < length || pivotIndex + length >= candles.length) return null;
    const pivot = candles[pivotIndex];
    if (!validBar(pivot) || pivot.is_forming === true) return null;
    const values = [];
    for (let index = pivotIndex - length; index <= pivotIndex + length; index += 1) {
      const bar = candles[index];
      if (!validBar(bar) || bar.is_forming === true || !sameChain(bar, pivot)) return null;
      values.push(Number(bar[kind]));
    }
    const value = Number(pivot[kind]);
    const extreme = kind === "high" ? Math.max(...values) : Math.min(...values);
    return value === extreme ? value : null;
  }

  function initialState() {
    return {
      bearish: { active: false, sweepIndex: -1, breakLevel: null, breakLevelIndex: -1, fvg: null },
      bullish: { active: false, sweepIndex: -1, breakLevel: null, breakLevelIndex: -1, fvg: null },
      entryWindow: null,
      lastPivotHigh: null, priorPivotHigh: null, lastPivotHighIndex: -1,
      lastPivotLow: null, priorPivotLow: null, lastPivotLowIndex: -1,
    };
  }

  function copyFvg(fvg) {
    return fvg ? { ...fvg } : null;
  }

  function copyWindow(windowState) {
    return windowState ? { ...windowState, fvg: copyFvg(windowState.fvg) } : null;
  }

  function sourceFingerprint(candles, endIndex) {
    // 调用方可能复用数组并原地写入最新收盘 K 线；缓存键必须反映实际行情内容。
    let hash = 2166136261;
    const limit = Math.max(0, Math.min(Number(endIndex) || 0, candles.length));
    // 完成历史通常以新数组替换；原地刷新只会改动尾部收盘区。保留首根和尾部
    // 八根即可识别实时刷新，同时避免每次报价都为缓存重扫整段历史。
    const indexes = limit > 9 ? [0, ...Array.from({ length: 8 }, (_, offset) => limit - 8 + offset)]
      : Array.from({ length: limit }, (_, index) => index);
    for (const index of indexes) {
      const bar = candles[index] || {};
      for (const value of [bar.date, bar.open, bar.high, bar.low, bar.close, bar.chain_id, bar.is_forming]) {
        const text = String(value == null ? "" : value);
        for (let cursor = 0; cursor < text.length; cursor += 1) {
          hash ^= text.charCodeAt(cursor);
          hash = Math.imul(hash, 16777619);
        }
        hash ^= 31;
        hash = Math.imul(hash, 16777619);
      }
    }
    return (hash >>> 0).toString(16);
  }

  function calculate(candles, overrides = {}) {
    const bars = Array.isArray(candles) ? candles : [];
    const config = configFor(overrides);
    const state = initialState();
    const sweeps = [], structureBreaks = [], fvgZones = [], entryWindows = [], events = [];
    let atr = null, atrSeed = 0, atrCount = 0, previousClose = null;
    let chain = null;

    const resetForChain = () => {
      if (state.entryWindow) state.entryWindow.active = false;
      state.bearish = { active: false, sweepIndex: -1, breakLevel: null, breakLevelIndex: -1, fvg: null };
      state.bullish = { active: false, sweepIndex: -1, breakLevel: null, breakLevelIndex: -1, fvg: null };
      state.entryWindow = null;
      state.lastPivotHigh = null; state.priorPivotHigh = null; state.lastPivotHighIndex = -1;
      state.lastPivotLow = null; state.priorPivotLow = null; state.lastPivotLowIndex = -1;
      atr = null; atrSeed = 0; atrCount = 0; previousClose = null;
    };
    const addEntryWindow = (index, direction, boundary, high, low, fvg, bar) => {
      if (state.entryWindow) state.entryWindow.active = false;
      const windowState = { startIndex: index, endIndex: index, high, low, boundary, direction, active: true,
        date: String(bar.date || ""), fvg: { ...fvg, endIndex: index } };
      state.entryWindow = windowState;
      entryWindows.push(windowState);
      fvgZones.push(windowState.fvg);
    };
    const updateEntryWindow = (index, bar) => {
      const windowState = state.entryWindow;
      if (!windowState) return;
      const barsIntoWindow = index - windowState.startIndex;
      if (barsIntoWindow < config.entryWindowBars) {
        if (windowState.direction === "bearish") {
          windowState.high = Math.min(Math.max(windowState.high, Number(bar.high)), windowState.boundary);
          windowState.low = Math.min(windowState.low, Number(bar.low));
        } else {
          windowState.high = Math.max(windowState.high, Number(bar.high));
          windowState.low = Math.max(Math.min(windowState.low, Number(bar.low)), windowState.boundary);
        }
        windowState.endIndex = Math.min(index + 5, windowState.startIndex + config.entryWindowBars);
        windowState.fvg.endIndex = windowState.endIndex;
      } else {
        windowState.active = false;
      }
    };

    for (let index = 0; index < bars.length; index += 1) {
      const bar = bars[index], forming = bar?.is_forming === true;
      if (!validBar(bar)) {
        resetForChain(); chain = null;
        continue;
      }
      // 中间形成中的 K 线切断已完成行情链，后续收盘 bar 必须重新预热。
      if (chain !== null && (!sameChain(bars[index - 1], bar)
        || (bars[index - 1] && (!validBar(bars[index - 1]) || bars[index - 1].is_forming === true)))) resetForChain();
      chain = Number(bar.chain_id) || 0;
      if (forming) continue;

      const close = Number(bar.close), priorClose = previousClose;
      const tr = trueRange(bar, previousClose);
      atrCount += 1; atrSeed += tr;
      if (atr === null) { if (atrCount >= config.atrLength) atr = atrSeed / config.atrLength; }
      else atr += (tr - atr) / config.atrLength;

      const pivotHighIndex = index - config.pivotLength, pivotLowIndex = index - config.pivotLength;
      const confirmedHigh = pivotValue(bars, pivotHighIndex, config.pivotLength, "high");
      const confirmedLow = pivotValue(bars, pivotLowIndex, config.pivotLength, "low");
      if (confirmedHigh != null) {
        state.priorPivotHigh = state.lastPivotHigh;
        state.lastPivotHigh = confirmedHigh;
        state.lastPivotHighIndex = pivotHighIndex;
      }
      if (confirmedLow != null) {
        state.priorPivotLow = state.lastPivotLow;
        state.lastPivotLow = confirmedLow;
        state.lastPivotLowIndex = pivotLowIndex;
      }

      const body = Math.abs(close - Number(bar.open));
      const significantBull = close > Number(bar.open) && atr != null && body >= atr * config.bodyAtrMultiple;
      const significantBear = close < Number(bar.open) && atr != null && body >= atr * config.bodyAtrMultiple;
      const highTwoBack = Number(bars[index - 2]?.high), lowTwoBack = Number(bars[index - 2]?.low);
      const bearishGapSize = lowTwoBack - Number(bar.high), bullishGapSize = Number(bar.low) - highTwoBack;
      const bearishFvg = index >= 2 && Number.isFinite(bearishGapSize) && Number(bar.high) < lowTwoBack
        && atr != null && bearishGapSize >= atr * config.minFvgAtrMultiple;
      const bullishFvg = index >= 2 && Number.isFinite(bullishGapSize) && Number(bar.low) > highTwoBack
        && atr != null && bullishGapSize >= atr * config.minFvgAtrMultiple;
      const bearishSweep = state.lastPivotHigh != null && state.lastPivotLow != null && state.priorPivotLow != null
        && state.lastPivotLow > state.priorPivotLow && state.lastPivotLowIndex < index
        && priorClose != null && priorClose <= state.lastPivotHigh && close > state.lastPivotHigh && significantBull;
      const bullishSweep = state.lastPivotLow != null && state.lastPivotHigh != null && state.priorPivotHigh != null
        && state.lastPivotHigh < state.priorPivotHigh && state.lastPivotHighIndex < index
        && priorClose != null && priorClose >= state.lastPivotLow && close < state.lastPivotLow && significantBear;

      if (bearishSweep) {
        const fvg = bearishFvg ? { top: lowTwoBack, bottom: Number(bar.high), index } : null;
        state.bearish = { active: true, sweepIndex: index, breakLevel: state.lastPivotLow, breakLevelIndex: state.lastPivotLowIndex, fvg };
        state.bullish.active = false;
        sweeps.push({ index, date: String(bar.date || ""), direction: "bearish", level: state.lastPivotHigh, pivotIndex: state.lastPivotHighIndex, price: close });
      }
      if (bullishSweep) {
        const fvg = bullishFvg ? { top: Number(bar.low), bottom: highTwoBack, index } : null;
        state.bullish = { active: true, sweepIndex: index, breakLevel: state.lastPivotHigh, breakLevelIndex: state.lastPivotHighIndex, fvg };
        state.bearish.active = false;
        sweeps.push({ index, date: String(bar.date || ""), direction: "bullish", level: state.lastPivotLow, pivotIndex: state.lastPivotLowIndex, price: close });
      }

      const processSetup = (setup, direction, fvg, significant, crossed) => {
        if (!setup.active) return null;
        const barsFromSweep = index - setup.sweepIndex;
        if (fvg && barsFromSweep <= config.maxReversalBars) setup.fvg = direction === "bearish"
          ? { top: lowTwoBack, bottom: Number(bar.high), index } : { top: Number(bar.low), bottom: highTwoBack, index };
        if (barsFromSweep > config.maxReversalBars) { setup.active = false; return null; }
        if (barsFromSweep > 0 && crossed && significant && setup.fvg) {
          setup.active = false;
          const signal = { index, date: String(bar.date || ""), side: direction === "bearish" ? "sell" : "buy",
            direction, price: close, sweepIndex: setup.sweepIndex, breakLevel: setup.breakLevel, fvg: copyFvg(setup.fvg) };
          structureBreaks.push({ index, date: signal.date, direction, level: setup.breakLevel, fromIndex: setup.breakLevelIndex });
          addEntryWindow(index, direction, direction === "bearish" ? setup.fvg.bottom : setup.fvg.top,
            direction === "bearish" ? Math.min(Number(bar.high), setup.fvg.bottom) : Math.max(Number(bar.high), setup.fvg.top),
            direction === "bearish" ? Math.min(Number(bar.low), setup.fvg.bottom) : Math.max(Number(bar.low), setup.fvg.top), setup.fvg, bar);
          events.push(signal);
          return signal;
        }
        return null;
      };
      if (!bearishSweep) processSetup(state.bearish, "bearish", bearishFvg, significantBear,
        priorClose != null && priorClose >= Number(state.bearish.breakLevel) && close < Number(state.bearish.breakLevel));
      if (!bullishSweep) processSetup(state.bullish, "bullish", bullishFvg, significantBull,
        priorClose != null && priorClose <= Number(state.bullish.breakLevel) && close > Number(state.bullish.breakLevel));
      updateEntryWindow(index, bar);
      previousClose = close;
    }

    return {
      config, sweeps: sweeps.slice(-100), structureBreaks: structureBreaks.slice(-100),
      fvgZones: fvgZones.slice(-100), entryWindows: entryWindows.slice(-100), events: events.slice(-100),
      activeWindow: copyWindow(state.entryWindow),
    };
  }

  function cachedCompleted(sourceCandles, overrides, endIndex) {
    let entries = completedCache.get(sourceCandles);
    if (!entries) { entries = new Map(); completedCache.set(sourceCandles, entries); }
    const key = `${endIndex}:${JSON.stringify(configFor(overrides))}:${sourceFingerprint(sourceCandles, endIndex)}`;
    if (entries.has(key)) return entries.get(key);
    const result = calculate(endIndex === sourceCandles.length ? sourceCandles : sourceCandles.slice(0, endIndex), overrides);
    entries.set(key, result);
    if (entries.size > 4) entries.delete(entries.keys().next().value);
    return result;
  }

  function calculateWithLiveBar(sourceCandles, displayCandles, liveIndex, overrides = {}) {
    const source = Array.isArray(sourceCandles) ? sourceCandles : [];
    const display = Array.isArray(displayCandles) ? displayCandles : source;
    const liveBar = liveIndex >= 0 ? display[liveIndex] : null;
    if (liveBar && liveBar.is_forming !== true) return calculate(display, overrides);
    const endIndex = liveBar ? Math.max(0, Math.min(liveIndex, source.length)) : source.length;
    const completed = cachedCompleted(source, overrides, endIndex);
    if (!liveBar || !validBar(liveBar) || !completed.activeWindow) return { ...completed, liveIndex: liveBar ? liveIndex : -1, live: null };
    const liveWindow = copyWindow(completed.activeWindow);
    const barsIntoWindow = liveIndex - liveWindow.startIndex;
    if (barsIntoWindow < completed.config.entryWindowBars) {
      if (liveWindow.direction === "bearish") {
        liveWindow.high = Math.min(Math.max(liveWindow.high, Number(liveBar.high)), liveWindow.boundary);
        liveWindow.low = Math.min(liveWindow.low, Number(liveBar.low));
      } else {
        liveWindow.high = Math.max(liveWindow.high, Number(liveBar.high));
        liveWindow.low = Math.max(Math.min(liveWindow.low, Number(liveBar.low)), liveWindow.boundary);
      }
      liveWindow.endIndex = Math.min(liveIndex + 5, liveWindow.startIndex + completed.config.entryWindowBars);
      liveWindow.fvg.endIndex = liveWindow.endIndex;
    }
    return { ...completed, liveIndex, live: { window: liveWindow, confirmed: false } };
  }

  window.OptionScopeSweepFvg = { calculate, calculateWithLiveBar, defaults: { ...DEFAULTS } };
})(window);
