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
