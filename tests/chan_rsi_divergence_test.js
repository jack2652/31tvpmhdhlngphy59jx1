const assert = require("node:assert/strict");
const fs = require("node:fs");
const vm = require("node:vm");

const window = {};
for (const name of ["chan-plan", "chan-rsi-divergence", "chan-client"]) {
  vm.runInNewContext(fs.readFileSync(`web/static/common/js/${name}.js`, "utf8"), { window });
}
const at = (index) => new Date(Date.UTC(2026, 0, index + 1)).toISOString().slice(0, 10);
const bar = (close, index) => ({ date: at(index), open: close, high: close + 1, low: close - 1, close, volume: 100 });
const { calculate } = window.OptionScopeRsiDivergence;
assert.equal(calculate(Array.from({ length: 30 }, (_, index) => bar(100, index))).values[14], 50);
assert.equal(calculate(Array.from({ length: 30 }, (_, index) => bar(100 + index, index))).values[14], 100);
assert.equal(calculate(Array.from({ length: 30 }, (_, index) => bar(100 - index, index))).values[14], 0);

let seed = 5, price = 100;
const bars = Array.from({ length: 110 }, (_, index) => {
  seed = (Math.imul(seed, 1664525) + 1013904223) >>> 0;
  price += seed / 2 ** 32 * 8 - 3.9;
  return bar(price, index);
});
const result = calculate(bars);
assert.deepEqual(Array.from(result.events, (event) => [event.side, event.index, event.invalidatedIndex]),
  [["top", 47, null], ["bottom", 76, 109], ["top", 93, 95]]);
assert.equal(new Set(result.events.map((event) => event.id)).size, result.events.length);
assert.equal(calculate(bars.slice(0, 47)).events.length, 0);
const live = calculate([...bars.slice(0, 47), { ...bars[47], is_forming: true }]);
assert.equal(live.events.length, 0);
assert.equal(live.watches[0].status, "watch");
assert.equal(live.watches[0].id, result.events[0].id);
for (let length = 1; length <= bars.length; length += 1) {
  const prefix = calculate(bars.slice(0, length));
  assert.deepEqual(Array.from(prefix.events, (event) => [event.id, event.invalidatedAt]),
    Array.from(result.events.filter((event) => event.index < length), (event) =>
      [event.id, event.invalidatedIndex < length ? event.invalidatedAt : null]));
}
const reset = bars.map((item, index) => ({ ...item, chain_id: index < 50 ? 1 : 2 }));
assert.equal(calculate(reset).values[50], null);
assert.ok(Number.isFinite(calculate(reset).values[64]));
for (const gap of [
  bars.map((item, index) => ({ ...item, chain_id: index < 95 ? 0 : 1 })),
  bars.map((item, index) => index === 94 ? { ...item, close: NaN } : item),
]) {
  const event = calculate(gap).events.find((entry) => entry.index === 93);
  assert.equal(event.invalidatedAt, bars[95].date, "缺口之后的收盘 K 线仍可使已确认事件失效");
}
const frame = (period) => ({ candles: bars, period, label: period, bar_count: bars.length });
const getView = (offset, subcharts) => window.OptionScopeChan.view({ daily: frame("1d"), weekly: frame("1w") },
  false, true, 30, { daily: { visibleBars: 20, endOffset: offset } }, { supportResistance: false }, subcharts);
const view = getView(16, { rsi: true });
assert.equal(view.daily.chart.rsi.events.at(-1).invalidatedAt, null);
assert.equal(view.daily.chart.rsi.events.at(-1).status, "confirmed");
assert.equal(view.daily.chart.priceBottom, getView(16, {}).daily.chart.priceBottom);
assert.ok(Number.isFinite(view.weekly.chart.rsiTop));
const intraday = window.OptionScopeChan.intradayView({ period: "1h", frame: { ...frame("1h"), candles: bars.slice(0, 47) },
  live_bar: { ...bars[47], is_forming: true } }, false, null, false, 30, null, { supportResistance: false }, { rsi: true });
assert.equal(intraday.chart.rsi.events.at(-1).status, "watch");
assert.match(intraday.chart.indicatorStatus.at(-1).text, /形成中观察顶背离/);
