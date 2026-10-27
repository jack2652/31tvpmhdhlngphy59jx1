const assert = require("node:assert/strict");
const fs = require("node:fs");
const vm = require("node:vm");

const context = {};
context.window = context;
vm.runInNewContext(fs.readFileSync("web/static/common/js/chan-stochz.js", "utf8"), context);
const { calculate, normaliseOptions } = context.OptionScopeStochZ;

const bars = [];
for (let index = 0; index < 180; index += 1) {
  const close = 100 + Math.sin(index / 5) * 8 + index * 0.03;
  bars.push({
    date: `2026-${String(Math.floor(index / 30) + 1).padStart(2, "0")}-${String((index % 30) + 1).padStart(2, "0")}`,
    open: close, high: close + 1, low: close - 1, close, volume: 1000,
  });
}

const options = normaliseOptions({ minLen: 20, maxLen: 8, divLookbackMin: 12, divLookbackMax: 5, maxPriceLag: 99 });
assert.equal(options.minLen, 20, "minimum adaptive length keeps its configured lower bound");
assert.equal(options.maxLen, 20, "maximum adaptive length clamps up to minimum length");
assert.equal(options.divLookbackMin, 12, "divergence minimum lookback is preserved");
assert.equal(options.divLookbackMax, 12, "divergence maximum lookback clamps up to minimum lookback");
assert.equal(options.maxPriceLag, 10, "price lag follows Pine input maxval");

const result = calculate(bars);
assert.equal(result.options.minLen, 8);
assert.equal(result.options.maxLen, 34);
assert.ok(result.series.z.findIndex((value) => value != null) >= 30, "z-score waits for stochastic and rolling warmup");
assert.ok(result.series.adaptiveLengths.every((value) => value >= 8 && value <= 34), "adaptive length stays inside configured bounds");
assert.ok(result.events.length >= 3, "fixture produces confirmed AF StochZ events");
assert.ok(result.events.every((event) => event.confirmedIndex - event.index === 2), "price pivots appear only after the configured right-bar confirmation");
assert.ok(result.events.some((event) => event.label === "BUY"), "fixture produces a leading BUY");
assert.ok(result.events.some((event) => event.label === "R-Bear"), "fixture produces regular bearish divergence");
assert.ok(result.events.some((event) => event.label === "H-Bull"), "fixture produces hidden bullish divergence");
assert.ok(result.events.every((event) => Number.isFinite(event.markerPrice) && event.date < event.confirmedAt), "events carry a price marker and a later confirmation date");

const noRegular = calculate(bars, { showRegularDiv: false });
assert.ok(!noRegular.events.some((event) => event.divergence === "regular"), "regular divergence can be disabled independently");

console.log("AF StochZ calculation and confirmation checks passed");
