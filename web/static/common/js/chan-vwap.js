/* 常规盘成交成本和因果结构锚定成本。 */
(function (window) {
  "use strict";

  const periods = { "15m": 15, "1h": 60, "2h": 120, "4h": 240 };
  function contribution(bar) {
    const { high, low, close, volume } = bar;
    if (![high, low, close, volume].every(Number.isFinite) || low <= 0 || high < close || close < low || volume <= 0) return null;
    return { value: (high + low + close) / 3 * volume, volume };
  }

  function confirmedAnchors(candles, frame) {
    const byDate = new Map(candles.map((bar, index) => [bar.date, index]));
    const segments = frame.recursive_levels?.length
      ? frame.recursive_levels.flatMap((level) => level.segments || [])
      : frame.segments || [];
    const anchors = [];
    const add = (date, confirmedAt, source) => {
      const index = byDate.get(date), confirmedIndex = byDate.get(confirmedAt);
      if (index === undefined || confirmedIndex === undefined || confirmedIndex < index
          || candles[confirmedIndex].is_forming) return;
      anchors.push({ index, confirmedIndex, date, confirmedAt, source });
    };
    for (const stroke of frame.strokes_v4?.length ? frame.strokes_v4 : frame.strokes || []) {
      if (stroke.confirmed === true) add(stroke.end?.date, stroke.confirmed_at || stroke.end?.confirmed_at, "笔");
    }
    for (const segment of segments) {
      if (segment.confirmed === true) add(segment.end, segment.confirmed_at, "线段");
    }
    return anchors.sort((a, b) => a.confirmedIndex - b.confirmedIndex || a.index - b.index);
  }

  function calculate(candles, frame) {
    const interval = periods[frame.period];
    const anchors = confirmedAnchors(candles, frame);
    const session = [], anchored = [], sums = [];
    let day = "", sessionValue = 0, sessionVolume = 0, previousMinute = -1, sessionGroup = 0;
    let active = null, nextAnchor = 0, chain = 0, chainValue = 0, chainVolume = 0;
    for (let index = 0; index < candles.length; index += 1) {
      const bar = candles[index], amount = contribution(bar);
      const match = interval && /^(\d{4}-\d{2}-\d{2}) (\d{2}):(\d{2})$/.exec(bar.date);
      session[index] = null;
      if (match) {
        const minute = Number(match[2]) * 60 + Number(match[3]);
        if (minute >= 570 && minute + interval <= 960) {
          if (match[1] !== day) {
            day = match[1];
            sessionValue = 0;
            sessionVolume = 0;
            previousMinute = -1;
          }
          if (minute !== previousMinute + interval || !amount) sessionGroup += 1;
          previousMinute = minute;
          if (amount) {
            sessionValue += amount.value;
            sessionVolume += amount.volume;
            session[index] = { value: sessionValue / sessionVolume, group: sessionGroup };
          }
        }
      }
      const barChain = bar.chain_id ?? candles[index - 1]?.chain_id ?? 0;
      if (barChain !== chain) {
        active = null;
        chainValue = 0;
        chainVolume = 0;
        chain = barChain;
      }
      sums[index] = { value: chainValue, volume: chainVolume, chain };
      if (amount) {
        chainValue += amount.value;
        chainVolume += amount.volume;
      }
      while (anchors[nextAnchor]?.confirmedIndex === index) {
        const anchor = anchors[nextAnchor++];
        if (sums[anchor.index].chain === chain && (!active || anchor.index >= active.index)) active = anchor;
      }
      const base = active && sums[active.index];
      const volume = base ? chainVolume - base.volume : 0;
      anchored[index] = volume > 0 && amount
        ? { value: (chainValue - base.value) / volume, anchor: active, group: active.confirmedIndex } : null;
    }
    return { session, anchored, intraday: Boolean(interval) };
  }

  window.OptionScopeVwap = { calculate };
})(window);
