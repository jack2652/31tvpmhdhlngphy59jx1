const assert = require("node:assert/strict");
const fs = require("node:fs");
const vm = require("node:vm");

const window = {};
for (const name of ["chan-plan", "chan-bollinger", "chan-client"]) {
  vm.runInNewContext(fs.readFileSync(`web/static/common/js/${name}.js`, "utf8"), { window });
}
const bar = (close, index, is_forming = false) => ({
  date: new Date(Date.UTC(2026, 0, 1 + index)).toISOString().slice(0, 10),
  close, open: close, high: close + 1, low: close - 1, volume: 100, is_forming,
});
const closes = [...Array(65).fill(100), 140, 145, 103, 65];
const bars = closes.map((close, index) => bar(close, index));
const { calculate } = window.OptionScopeBollinger;
const result = calculate(bars);
assert.equal(result.series[18], null);
assert.equal(result.series[19].width, 0);
assert.equal(result.series[58].threshold, null);
assert.equal(result.series[59].threshold, 0);
assert.equal(result.series[59].squeeze, true);
assert.equal(result.series[65].squeeze, false);
assert.equal(result.series[65].middle, 102);
assert.deepEqual(Array.from(result.events, (event) => [event.index, event.side]), [[65, "up"], [68, "down"]]);
assert.equal(result.series[66].event, null);
assert.equal(result.series[67].event, null);
const formed = calculate([...bars.slice(0, 68), bar(65, 68, true)]);
assert.equal(formed.series[68].event.status, "watch");
assert.equal(formed.events.length, 1);
const invalid = bars.map((item, index) => index === 60 ? { ...item, close: null } : item);
assert.equal(calculate(invalid).series[65], null);
const frame = { candles: bars.slice(0, 68), period: "1d", label: "日线", bar_count: 68 };
const payload = { daily: frame, live_daily_bar: bar(65, 68, true) };
const model = (viewport, mobile) => window.OptionScopeChan.view(payload, false, mobile, 30,
  { daily: viewport }, { bollinger: true, supportResistance: false }).daily.chart;
for (const mobile of [false, true]) {
  const full = model({ visibleBars: 69 }, mobile), zoom = model({ visibleBars: 12 }, mobile);
  assert.equal(zoom.bollinger.series.at(-1).lower, full.bollinger.series.at(-1).lower);
  assert.equal(zoom.bollinger.events.at(-1).status, "watch");
  assert.match(zoom.indicatorStatus[0].text, /形成中观察下破/);
  const history = model({ visibleBars: 12, endOffset: 1 }, mobile);
  assert.equal(history.bollinger.events.length, 1);
  assert.match(history.indicatorStatus[0].text, /收盘确认上破/);
}
const quotePreview = window.OptionScopeChan.withSpot({
  symbol: "QQQ", daily: frame, price_coordinate: { basis: "raw" },
}, 65, null, {
  symbol: "QQQ", market_state: "POST", daily_bar: bar(65, 68),
});
const previewChart = window.OptionScopeChan.view(quotePreview, false, false, 30, null,
  { bollinger: true, supportResistance: false }).daily.chart;
assert.equal(previewChart.candles.at(-1).prices.close, 65);
assert.equal(previewChart.bollinger.events.at(-1).status, "watch", "盘后报价快照不能确认日线突破");
const duringRegular = window.OptionScopeChan.withSpot({
  symbol: "QQQ", daily: frame, price_coordinate: { basis: "raw" },
}, 65, null, {
  symbol: "QQQ", market_state: "REGULAR", fetched_at: bars[68].date + "T14:40:00-04:00",
  daily_bar: { ...bar(65, 68, true), as_of: bars[68].date + "T14:36:00-04:00" },
});
assert.equal(duringRegular.live_daily_bar.source_date, bars[68].date + " 14:30");
const confirmedChart = window.OptionScopeChan.view({
  daily: { ...frame, candles: bars, bar_count: bars.length },
  live_daily_bar: bar(90, 68, true),
}, false, false, 30, null, { bollinger: true, supportResistance: false }).daily.chart;
assert.equal(confirmedChart.candles.at(-1).prices.close, 65, "旧报价不能覆盖同日正式收盘");
assert.equal(confirmedChart.bollinger.events.at(-1).status, "confirmed");
const preserveViewport = window.OptionScopeChan.intradayViewportAfterRefresh;
const viewport = { visibleBars: 20, endOffset: 4 };
const before = { frame, live_bar: bars[68] };
const after = { frame: { ...frame, candles: bars } };
assert.equal(preserveViewport(viewport, before, after), viewport, "确认同一根时历史视口不移动");
assert.equal(preserveViewport(viewport, before, { ...after, live_bar: bar(80, 69, true) }).endOffset, 5, "新增一根时历史视口保持同一日期");
assert.equal(preserveViewport(viewport, before, { frame }).endOffset, 3, "暂时移除形成中尾值时历史视口保持同一日期");
