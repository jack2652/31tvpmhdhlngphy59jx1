const assert = require("node:assert/strict");
const fs = require("node:fs");
const vm = require("node:vm");

const window = {};
vm.runInNewContext(fs.readFileSync("web/static/common/js/chan-macd-sma.js", "utf8"), { window });
const { calculate, calculateWithLiveBar } = window.OptionScopeMacdSma200;

function barsFromCloses(closes) {
  return closes.map((close, index) => ({
    date: `2026-01-${String(index + 1).padStart(3, "0")}`,
    open: close,
    high: close + 1,
    low: close - 1,
    close,
  }));
}

const flat = calculate(barsFromCloses(Array(205).fill(100)));
assert.equal(flat.smaFast[11], 100, "12-period SMA warms up on the twelfth valid candle");
assert.equal(flat.smaSlow[25], 100, "26-period SMA warms up on the twenty-sixth valid candle");
assert.equal(flat.smaTrend[198], null, "200-period SMA remains unavailable before 200 valid candles");
assert.equal(flat.smaTrend[199], 100, "200-period SMA warms up on the 200th valid candle");
assert.equal(flat.macd[199], 0, "Pine MACD uses the difference between simple averages");
assert.equal(flat.signal[199], 0, "signal line is the 9-period SMA of MACD");
assert.equal(flat.histogram[199], 0, "histogram is MACD minus its signal SMA");
assert.equal(flat.events.length, 0, "a flat series does not create crossover signals");

const longCloses = [...Array(220).fill(100), 55, 100, ...Array(30).fill(100)];
const longResult = calculate(barsFromCloses(longCloses));
const longSignal = longResult.events.find((event) => event.side === "buy");
assert.ok(longSignal, "a bullish histogram crossover with the Pine filters creates a long condition");
assert.equal(longSignal.index, 232, "long conditions use the Pine histogram crossover and 26-bar trend comparison");

const shortCloses = [...Array(220).fill(100), 110, 100, ...Array(30).fill(100)];
const shortResult = calculate(barsFromCloses(shortCloses));
assert.equal(shortResult.events.find((event) => event.side === "sell")?.index, 232, "short conditions apply the inverse Pine filters");

const liveHistory = barsFromCloses(Array(199).fill(100));
const liveBar = { date: "2026-08-01", open: 100, high: 102, low: 99, close: 101, is_forming: true };
const liveResult = calculateWithLiveBar(liveHistory, [...liveHistory, liveBar], liveHistory.length);
assert.equal(liveResult.events.length, 0, "a forming candle does not publish a strategy condition");
assert.equal(liveResult.liveValues.smaTrend, 100.005, "live SMA200 includes the latest forming close");
assert.equal(liveResult.liveIndex, liveHistory.length, "live values retain their chart index");

const interrupted = barsFromCloses(Array(230).fill(100));
interrupted[202].high = 99;
const reset = calculate(interrupted);
assert.equal(reset.smaFast[202], null, "invalid OHLC resets the rolling averages");
assert.equal(reset.smaFast[213], null, "fast average warms up again after invalid OHLC");
assert.equal(reset.smaFast[214], 100, "fast average recovers after twelve consecutive valid candles");
assert.equal(reset.smaTrend[interrupted.length - 1], null, "slow trend stays unavailable until a new 200-bar window forms");

const unfinished = barsFromCloses(longCloses);
unfinished[unfinished.length - 1].is_forming = true;
const unfinishedResult = calculate(unfinished);
assert.equal(unfinishedResult.events.some((event) => event.index === unfinished.length - 1), false, "forming bars do not create crossover events");

console.log("MACD + SMA200 calculations, strategy filters, invalid bars, and live values passed");
