const assert = require("node:assert/strict");
const fs = require("node:fs");
const vm = require("node:vm");

const window = {};
vm.runInNewContext(fs.readFileSync("web/static/common/js/chan-plan.js", "utf8"), { window });
vm.runInNewContext(fs.readFileSync("web/static/common/js/chan-client.js", "utf8"), { window });
const plain = (value) => JSON.parse(JSON.stringify(value));
const day = (index) => `2026-10-${String(index + 1).padStart(2, "0")}`;
function stroke(price, start, end, options = {}) {
  return { confirmed: true, confirmed_at: day(end), chain: 0,
    start: { kind: "bottom", date: day(start), price, chain_id: 0 },
    end: { kind: "top", date: day(end), price: 120, chain_id: 0 }, low: price, high: 120, ...options };
}
function frame(options = {}) {
  return { period: "1d", label: "日线", as_of: day(19), last_close: 110, bar_count: 20,
    candles: Array.from({ length: 20 }, (_, index) => ({ date: day(index), open: 110, high: 125, low: 85, close: 110, chain_id: 0 })),
    strokes: [stroke(90, 1, 3), stroke(100, 5, 7), stroke(100.2, 8, 10)],
    centers: [], segments: [], recursive_centers: [], recursive_levels: [], signals: [], position: {}, ...options };
}
function chart(source, layers = {}, viewport = null, spot = 110, mobile = false) {
  return window.OptionScopeChan.view({ symbol: "TEST", spot, daily: source, weekly: source, summary: {} }, false, mobile, 30,
    viewport ? { daily: viewport } : null, { pens: false, penCenters: false, signals: false, structureSupports: true, ...layers }).daily.chart;
}

const source = frame();
const merged = chart(source);
assert.deepEqual(plain(merged.structureSupportLevels.map((level) => level.price)), [100, 90], "nearby confirmed lows merge into one support zone");
assert.equal(merged.structureSupportLevels[0].zoneHigh, 100.2);
assert.ok(merged.structureSupports.every((level) => level.status === "active"));
assert.match(merged.description, /结构支撑自动取/);
assert.deepEqual(plain(source), plain(frame()), "support generation does not mutate upstream structures");

const unconfirmed = frame({ strokes: [stroke(88, 1, 3, { confirmed: false }), stroke(92, 4, 7, { confirmed_at: day(25) }), stroke(94, 5, 8, { confirmed_at: "" })] });
assert.equal(chart(unconfirmed).structureSupportLevels.length, 0, "forming, future-confirmed and undated structures cannot create support");

const brokenSource = frame();
brokenSource.candles[12].low = 80;
assert.equal(chart(brokenSource).structureSupportLevels.find((level) => level.price === 100).status, "active", "an intrabar low alone cannot break support");
const closedBelow = frame();
closedBelow.candles[12].close = 99;
const broken = chart(closedBelow).structureSupportLevels.find((level) => level.price === 100);
assert.equal(broken.status, "broken");
assert.equal(broken.brokenAt, day(12), "first close below the zone invalidates it even after a later rebound");
assert.equal(chart(frame({ candles: [...source.candles, { date: day(20), open: 110, high: 115, low: 70, close: 75, is_forming: true }] })).structureSupportLevels[0].status, "active", "live candles never confirm a break");

const centers = frame({ strokes: [], candles: source.candles.map((bar, index) => ({ ...bar, close: index < 11 ? 100 : 110 })),
  centers: [{ lower: 96, upper: 105, start: day(2), end: day(10), confirmed_at: day(8), chain: 0 }] });
const centerChart = chart(centers);
assert.deepEqual(plain(centerChart.structureSupportLevels.map((level) => level.price)), [105, 96], "confirmed center upper boundary becomes support only after a close above it");
const upperSupport = centerChart.structureSupportLevels.find((level) => level.price === 105);
assert.equal(upperSupport.pointDate, day(11), "center upper support point anchors to the first close above its boundary after confirmation");
assert.equal(centerChart.structureSupports.find((level) => level.price === 105).pointX,
  centerChart.candles.find((bar) => bar.date === day(11)).x, "center upper support marker aligns with the breakout candle");
const belowUpper = frame({ strokes: [], centers: centers.centers, candles: source.candles.map((bar) => ({ ...bar, close: 102 })) });
assert.deepEqual(plain(chart(belowUpper).structureSupportLevels.map((level) => level.price)), [96]);
assert.equal(chart(frame({ strokes: [], centers: [{ ...centers.centers[0], forming: true }] })).structureSupportLevels.length, 0);

const separated = frame({ strokes_v4: [stroke(90, 1, 3), stroke(101, 11, 13, { chain: 4, start: { kind: "bottom", date: day(11), price: 101, chain_id: 4 } })],
  structure_chains_v4: [{ chain_id: 0, data_chain_id: 0, start: day(0), end: day(9) }, { chain_id: 4, data_chain_id: 0, start: day(10), end: day(19) }] });
assert.deepEqual(plain(chart(separated).structureSupportLevels.map((level) => level.price)), [101], "older disconnected structure chains must not leak into current supports");
const noRecursiveFallback = frame({ strokes_v4: [], recursive_centers: [] });
assert.equal(chart(noRecursiveFallback).structureSupportLevels.length, 0, "empty recursive structures must not silently borrow legacy coordinates");

const segmentSource = frame({ strokes_v4: [], segments: [{ level: 0, confirmed: true, forming: false, confirmed_at: day(14), chain_id: 0, start: day(2), end: day(10), start_kind: "bottom", start_price: 95 }] });
assert.equal(chart(segmentSource).structureSupportLevels[0].sources, "已确认线段低点");

const moved = chart(source, {}, { visibleBars: 12, endOffset: 0 }, 88);
assert.deepEqual(plain(moved.structureSupportLevels), plain(merged.structureSupportLevels), "pan, zoom and live price do not change automatic selection or close status");
assert.equal(moved.structureSupports.find((level) => level.price === 90).x1, moved.plotLeft, "old confirmed support extends through a viewport without its source candle");
const off = chart(source, { structureSupports: false });
assert.equal(off.structureSupports.length, 0);
assert.equal(off.layers.structureSupports, false);
assert.equal(chart(source).structureSupports.length, 2, "cached layout restores supports when toggled back on");

const updatedStructures = frame({ candles: source.candles, strokes: [stroke(98, 5, 7)] });
assert.equal(chart(updatedStructures).structureSupportLevels[0].price, 98, "changed structures invalidate support cache even when candles are shared");
const crowded = frame({ strokes: [99, 99.4, 99.8, 100.2, 100.6, 101].map((price, index) => stroke(price, index + 1, index + 3)) });
const mobileChart = chart(crowded, {}, null, 110, true);
for (let index = 1; index < mobileChart.structureSupports.length; index += 1) {
  assert.ok(mobileChart.structureSupports[index].labelY - mobileChart.structureSupports[index - 1].labelY >= 21, "nearby support labels remain distinct on mobile");
}
assert.ok(mobileChart.structureSupports.every((level) => level.labelY >= mobileChart.plotTop && level.labelY <= mobileChart.priceBottom));

const adjusted = window.OptionScopeChan.withSpot({ symbol: "TEST", spot: 110, daily: source, weekly: source, summary: {},
  price_coordinate: { basis: "split_dividend_adjusted", status: "available", adjustment_factor: 0.5 } }, 180);
const adjustedChart = window.OptionScopeChan.view(adjusted, false).daily.chart;
assert.equal(adjustedChart.spot.label, "90.00");
assert.deepEqual(plain(adjustedChart.structureSupportLevels.map((level) => level.price)), [100, 90], "support stays in adjusted candle coordinates rather than being adjusted twice");

const intraday = frame({ period: "15m", label: "15分钟" });
const intradayView = window.OptionScopeChan.intradayView({ period: "15m", symbol: "TEST", spot: 110, frame: intraday }, false);
assert.deepEqual(plain(intradayView.chart.structureSupportLevels.map((level) => level.price)), [100, 90]);

console.log("Automatic structure support selection, close breaks, chains, coordinates and layout checks passed");
