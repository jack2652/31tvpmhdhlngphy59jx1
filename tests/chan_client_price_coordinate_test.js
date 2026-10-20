const assert = require("node:assert/strict");
const fs = require("node:fs");
const vm = require("node:vm");

const window = {};
vm.runInNewContext(fs.readFileSync("web/static/common/js/chan-plan.js", "utf8"), { window });
vm.runInNewContext(fs.readFileSync("web/static/common/js/chan-client.js", "utf8"), { window });

function frame() {
  return {
    period: "1d",
    label: "日线",
    as_of: "2026-10-16",
    bar_count: 1,
    last_close: 54,
    candles: [{ date: "2026-10-16", open: 53, high: 55, low: 52, close: 54 }],
    strokes: [],
    centers: [],
    recursive_centers: [],
    recursive_levels: [],
    signals: [],
    position: { code: "inside", center: { lower: 50, upper: 60 } },
  };
}

const available = window.OptionScopeChan.withSpot({
  symbol: "TEST",
  spot: 54,
  price_coordinate: { basis: "split_dividend_adjusted", status: "available", adjustment_factor: 0.5 },
  daily: frame(),
  weekly: frame(),
  summary: {},
}, 110);
assert.equal(available.spot, 55, "raw live quote must be converted to adjusted coordinates");
assert.equal(available.daily.position.code, "inside", "position must compare adjusted spot with adjusted structure");
const availableView = window.OptionScopeChan.view(available);
assert.equal(availableView.daily.chart.spot.label, "55.00", "chart marker must use the adjusted live quote");
assert.equal(availableView.daily.chart.candles[0].prices.close, 54, "hover OHLC must stay in the adjusted candle coordinate");

const mobileChart = window.OptionScopeChan.view(available, true, true, 30).daily.chart;
assert.equal(mobileChart.plotLeft, 72, "mobile charts must keep the price-label gutter compact");
assert.equal(mobileChart.historyRight, mobileChart.plotRight, "mobile candles must use the full chart width");
assert.ok(mobileChart.historyRight - mobileChart.plotLeft >= 330, "mobile candles must use the available chart width");

function scenarioFrame() {
  return {
    period: "1d", label: "日线", as_of: "2026-10-16", bar_count: 10, last_close: 54,
    candles: Array.from({ length: 10 }, (_, index) => ({
      date: `2026-10-${String(index + 6).padStart(2, "0")}`,
      open: 50 + index, high: 56 + index, low: 48 + index, close: 52 + index,
    })),
    strokes: [
      { direction: "up", low: 45, high: 60, confirmed: true, start: { date: "2026-10-01", kind: "bottom", price: 45 }, end: { date: "2026-10-10", kind: "top", price: 60 } },
      { direction: "down", low: 48, high: 60, confirmed: true, start: { date: "2026-10-10", kind: "top", price: 60 }, end: { date: "2026-10-15", kind: "bottom", price: 48 } },
    ],
    centers: [{ lower: 50, upper: 58, start: "2026-10-02", end: "2026-10-14" }],
    recursive_centers: [], recursive_levels: [], signals: [], pen_signals: [],
    position: { code: "inside", center: { lower: 50, upper: 58 } },
  };
}
const scenarioChart = window.OptionScopeChan.view({
  symbol: "TEST", spot: 54, daily: scenarioFrame(), weekly: scenarioFrame(), summary: {},
}, true, true, 30).daily.chart;
assert.ok(scenarioChart.projections.length > 0, "scenario frame must produce a projection path");
assert.ok(scenarioChart.historyRight < scenarioChart.plotRight, "mobile scenarios must keep an independent right lane");
assert.equal(scenarioChart.scenarioLaneLeft, scenarioChart.historyRight, "the scenario path must start at the independent lane");
assert.ok(scenarioChart.plotRight - scenarioChart.historyRight >= 40, "the mobile scenario lane must leave room for the projection");
assert.ok(scenarioChart.historyRight - scenarioChart.plotLeft >= 280, "the mobile candle area must remain wide");

const degraded = window.OptionScopeChan.withSpot({
  symbol: "TEST",
  spot: 55,
  price_coordinate: { basis: "split_dividend_adjusted", status: "degraded", adjustment_factor: null },
  daily: frame(),
  weekly: frame(),
  summary: {},
}, 110);
assert.equal(degraded.spot, null, "degraded coordinates must clear a stale adjusted spot");
assert.equal(degraded.daily.position.code, "inside", "degraded coordinates must not compare raw live spot to adjusted structures");
const degradedView = window.OptionScopeChan.view(degraded);
assert.equal(degradedView.daily.chart.spot, null, "degraded coordinates must suppress the raw live-price chart line");

console.log("Chan price coordinate checks passed");
