const assert = require("node:assert/strict");
const fs = require("node:fs");
const vm = require("node:vm");

const template = fs.readFileSync("web/static/index.html", "utf8");
assert.match(template, /卖8同一轮高乖离只提示一次，回到均线 1 ATR 内后重置/, "指标说明反映卖8重复抑制与重置条件");

const window = {};
vm.runInNewContext(fs.readFileSync("web/static/common/js/chan-granville.js", "utf8"), { window });
const calculate = window.OptionScopeGranville.calculate;
const bars = Array.from({ length: 230 }, (_, index) => ({
  date: `2026-01-${String(index + 1).padStart(3, "0")}`,
  open: 100,
  high: 101,
  low: 99,
  close: 100,
}));

bars.push(
  { date: "2026-08-09", open: 110, high: 111, low: 109, close: 110 },
  { date: "2026-08-10", open: 110, high: 110.5, low: 108, close: 108.5 },
  { date: "2026-08-11", open: 111, high: 113, low: 109.5, close: 112 },
  { date: "2026-08-12", open: 111, high: 112, low: 108, close: 108.5 },
  { date: "2026-08-13", open: 101, high: 102, low: 100, close: 100.5 },
);

for (let index = 0; index < 30; index += 1) {
  bars.push({
    date: `2026-09-${String(index + 1).padStart(2, "0")}`,
    open: 100.5,
    high: 101.5,
    low: 99.5,
    close: 100.5,
  });
}

bars.push(
  { date: "2026-10-01", open: 110, high: 111, low: 109, close: 110 },
  { date: "2026-10-02", open: 110, high: 110.5, low: 108, close: 108.5 },
);

const sellEightEvents = calculate(bars).events.filter((event) => event.rule === 8);
assert.equal(sellEightEvents.length, 2, "同一轮远离均线期间只产生一个卖8，回归均线范围后下一轮可重新触发");
assert.equal(sellEightEvents[0].index, 231, "第一轮乖离在首次确认回落时触发");
assert.equal(sellEightEvents[1].index, 266, "重新武装后第二轮乖离可触发");

console.log("Granville sell-eight excursion lock checks passed");
