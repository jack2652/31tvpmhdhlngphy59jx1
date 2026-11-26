const assert = require("node:assert/strict");
const fs = require("node:fs");
const vm = require("node:vm");
const { test } = require("node:test");

const window = {};
for (const name of ["chan-bollinger", "chan-rsi-divergence", "chan-relative-strength"]) {
  vm.runInNewContext(fs.readFileSync(`web/static/common/js/${name}.js`, "utf8"), { window });
}
const bands = window.OptionScopeBollinger.calculate;
const rsi = window.OptionScopeRsiDivergence.calculate;
const relativeStrength = window.OptionScopeRelativeStrength.calculate;
const history = Object.fromEntries(["QQQ", "ORCL"].map((symbol) => [
  symbol, JSON.parse(fs.readFileSync(`internal/chan/testdata/${symbol.toLowerCase()}_daily_2026.json`, "utf8")),
]));
const cutoff = "2026-10-01";
const horizon = 5;

// 用收盘时已确认的方向做统一观察。下一根开盘进入，五个交易日后收盘退出；
// 回撤按单一、互不重叠的仓位每日收盘估值。
function evaluate(bars, events, start, end) {
  const eligible = events.filter((event) => event.date >= start && event.date < end);
  let assessed = 0, falseSignals = 0, trades = 0, nextEntry = 0;
  let equity = 1, peak = 1, maxDrawdown = 0;
  for (const event of eligible) {
    const entry = event.index + 1, exit = event.index + horizon;
    if (exit >= bars.length || bars[exit].date >= end) continue;
    const direction = ["top", "down"].includes(event.side) ? -1 : 1;
    const changeAt = (index) => direction * (bars[index].close / bars[entry].open - 1);
    assessed += 1;
    if (changeAt(exit) <= 0) falseSignals += 1;
    if (entry < nextEntry) continue;
    trades += 1;
    for (let day = entry; day <= exit; day += 1) {
      const value = equity * (1 + changeAt(day));
      peak = Math.max(peak, value);
      maxDrawdown = Math.max(maxDrawdown, 1 - value / peak);
    }
    equity *= 1 + changeAt(exit);
    nextEntry = exit + 1;
  }
  return {
    signals: eligible.length, assessed, falseSignals, trades,
    drawdownPercent: trades ? Number((maxDrawdown * 100).toFixed(2)) : null,
  };
}

for (const [symbol, bars] of Object.entries(history)) {
  test(`${symbol} 全序列与逐根收盘回放一致`, () => {
    const allBands = bands(bars), allRsi = rsi(bars);
    const benchmark = { history: { bars: history.QQQ } };
    const allRS = relativeStrength(bars, benchmark);
    for (let length = 1; length <= bars.length; length += 1) {
      const prefix = bars.slice(0, length);
      const seenBands = bands(prefix), seenRsi = rsi(prefix);
      assert.deepEqual(seenBands.series.at(-1), allBands.series[length - 1]);
      assert.deepEqual(seenBands.events, allBands.events.filter((event) => event.index < length));
      assert.equal(seenRsi.values.at(-1), allRsi.values[length - 1]);
      assert.deepEqual(Array.from(seenRsi.events, (event) => ({ ...event })),
        Array.from(allRsi.events.filter((event) => event.index < length), (event) => ({
        ...event,
        invalidatedAt: event.invalidatedIndex !== null && event.invalidatedIndex < length ? event.invalidatedAt : null,
        invalidatedIndex: event.invalidatedIndex !== null && event.invalidatedIndex < length ? event.invalidatedIndex : null,
      })));
      const asOf = prefix.at(-1).date;
      const seenBenchmark = { history: { bars: history.QQQ.filter((bar) => bar.date <= asOf) } };
      assert.deepEqual(relativeStrength(prefix, seenBenchmark).at(-1), allRS[length - 1]);
      // 用同根最终 OHLC 构造观察输入，只检查状态，不推断盘中提前多久看到。
      const partial = [...prefix.slice(0, -1), { ...prefix.at(-1), is_forming: true }];
      assert.equal(bands(partial).events.some((event) => event.index === length - 1), false);
      assert.equal(rsi(partial).events.some((event) => event.index === length - 1), false);
    }
    const windows = { beforeCutoff: ["", cutoff], heldOut: [cutoff, "9999-01-01"] };
    for (const [indicator, events] of Object.entries({ bollinger: allBands.events, rsi: allRsi.events })) {
      const results = Object.fromEntries(Object.entries(windows).map(([name, range]) => [name, evaluate(bars, events, ...range)]));
      console.log(JSON.stringify({ symbol, indicator, first: bars[0].date, last: bars.at(-1).date, bars: bars.length,
        pivotToConfirmationBars: indicator === "rsi" ? [...new Set(events.map((event) => event.index - event.to.priceIndex))] : [0],
        ...results }));
    }
    console.log(JSON.stringify({ symbol, indicator: "rs", aligned: allRS.filter(Boolean).length,
      stateSwitches: allRS.filter((point) => point?.changed).length,
      heldOutSwitches: allRS.filter((point, index) => point?.changed && bars[index].date >= cutoff).length }));
  });
}
