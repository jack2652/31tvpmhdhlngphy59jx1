const assert = require("node:assert/strict");
const fs = require("node:fs");
const vm = require("node:vm");

const window = {};
for (const name of ["chan-plan", "chan-vwap", "chan-client"]) {
  vm.runInNewContext(fs.readFileSync(`web/static/common/js/${name}.js`, "utf8"), { window });
}
const bar = (date, close, volume, is_forming = false) => ({
  date, close, volume, is_forming, open: close, high: close + 1, low: close - 1,
});
const candles = [
  bar("2026-10-12 09:30", 10, 100),
  bar("2026-10-12 09:45", 13, 200),
  bar("2026-10-12 10:15", 20, 0),
  bar("2026-10-12 10:30", 19, 100),
  bar("2026-10-13 09:30", 30, 100),
  bar("2026-10-13 09:45", 40, 100, true),
];
const frame = { period: "15m", label: "15分钟", bar_count: 5, as_of: candles[4].date, candles: candles.slice(0, -1),
  strokes: [{ confirmed: true, start: { date: candles[0].date }, end: { date: candles[1].date }, confirmed_at: candles[3].date }],
  segments: [{ end: candles[4].date, confirmed: true, confirmed_at: candles[5].date }] };
const series = window.OptionScopeVwap.calculate(candles, frame);
assert.deepEqual(Array.from(series.session, (item) => item?.value ?? null), [10, 12, null, 13.75, 30, 35]);
assert.notEqual(series.session[1].group, series.session[3].group);
assert.notEqual(series.session[3].group, series.session[4].group);
assert.deepEqual(Array.from(series.anchored, (item) => item?.value ?? null), [null, null, null, 15, 18.75, 23]);
assert.equal(series.anchored[5].anchor.source, "笔");
assert.ok(window.OptionScopeVwap.calculate(candles, { ...frame, period: "1d" }).session.every((item) => item === null));
const payload = { period: "15m", frame, live_bar: candles[5] };
const show = { vwap: true, anchoredVwap: true, supportResistance: false };
const get = (viewport, layers = show) => window.OptionScopeChan.intradayView(payload, false, null, false, 30, viewport, layers).chart;
// 平移和缩放继承完整历史，叠加指标保持原价格范围。
const full = get({ visibleBars: 6 }), zoom = get({ visibleBars: 2 }), past = get({ visibleBars: 2, endOffset: 1 });
assert.equal(full.vwapLines[0][5].value, zoom.vwapLines[0][1].value);
assert.equal(past.vwapLines[0][0].value, full.vwapLines[0][3].value);
assert.equal(full.priceBottom, get({ visibleBars: 6 }, {}).priceBottom);
assert.equal(full.maximum, get({ visibleBars: 6 }, {}).maximum);
assert.match(full.indicatorStatus[0].text, /形成中观察/);
assert.doesNotMatch(past.indicatorStatus[0].text, /形成中观察/);
for (const file of ["index.html", "futures.html"]) {
  const html = fs.readFileSync(`web/static/${file}`, "utf8");
  assert.match(html, /aria-pressed="[^"]+\.vwap/);
  assert.match(html, /aria-pressed="[^"]+\.anchoredVwap/);
  assert.match(html, /chan-vwap\.js[\s\S]*chan-client\.js/);
}
