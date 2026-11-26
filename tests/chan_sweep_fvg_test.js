const assert = require("node:assert/strict");
const fs = require("node:fs");
const vm = require("node:vm");

const window = {};
vm.runInNewContext(fs.readFileSync("web/static/common/js/chan-sweep-fvg.js", "utf8"), { window });
const { calculate, calculateWithLiveBar } = window.OptionScopeSweepFvg;
const bar = (index, open, high, low, close, extra = {}) => ({
  date: `2026-01-${String(index + 1).padStart(2, "0")}`, open, high, low, close, ...extra,
});

// 先预热 ATR，再构造“高点抬升失败后的扫高”，最后用带 FVG 的大阴线跌破抬高低点。
const bearishBars = [];
for (let index = 0; index < 14; index += 1) {
  const close = 100 + index * 0.1;
  bearishBars.push(bar(index, close - 0.2, close + 0.8, close - 0.8, close));
}
bearishBars.push(
  bar(14, 96, 97, 94, 95), bar(15, 100, 103, 99, 102), bar(16, 102, 105, 101, 104),
  bar(17, 104, 112, 103, 110), bar(18, 110, 111, 98, 100), bar(19, 100, 108, 99, 105),
  bar(20, 105, 109, 103, 108), bar(21, 108, 116, 107, 115), bar(22, 115, 115, 104, 105),
  bar(23, 105, 106, 95, 96),
);
const bearish = calculate(bearishBars);
assert.deepEqual(Array.from(bearish.sweeps, ({ index, direction }) => [index, direction]), [[21, "bearish"]]);
assert.deepEqual(Array.from(bearish.events, ({ index, side }) => [index, side]), [[23, "sell"]]);
assert.equal(bearish.events[0].fvg.top, 107);
assert.equal(bearish.events[0].fvg.bottom, 106);
assert.equal(bearish.structureBreaks[0].level, 98);
assert.equal(bearish.entryWindows[0].active, true);

// 同一个数组对象被行情刷新原地替换时，完成结果缓存不能继续返回旧信号。
const mutableBars = bearishBars.map((item) => ({ ...item }));
assert.equal(calculateWithLiveBar(mutableBars, mutableBars, -1).events.length, 1);
mutableBars[23] = bar(23, 105, 106, 103, 105);
assert.equal(calculateWithLiveBar(mutableBars, mutableBars, -1).events.length, 0, "原地更新已收盘 K 线后不得复用旧扫流动性信号");

// 镜像行情必须产生看涨扫低和买入事件。
const bullishBars = bearishBars.map((item) => ({
  ...item, open: 220 - item.open, high: 220 - item.low, low: 220 - item.high, close: 220 - item.close,
}));
const bullish = calculate(bullishBars);
assert.equal(bullish.sweeps[0].direction, "bullish");
assert.equal(bullish.events[0].side, "buy");
assert.equal(bullish.structureBreaks[0].direction, "bullish");

// 枢轴必须等待左右确认，未来 K 线不能把信号写回前一根。
for (let end = 1; end <= bearishBars.length; end += 1) {
  const prefix = calculate(bearishBars.slice(0, end));
  assert.deepEqual(JSON.parse(JSON.stringify(prefix.events)), JSON.parse(JSON.stringify(bearish.events.filter((event) => event.index < end))));
  assert.equal(prefix.events.some((event) => event.index < 21), false);
}
const live = calculateWithLiveBar(bearishBars.slice(0, 23), bearishBars.map((item, index) => index === 22 ? { ...item, is_forming: true } : item), 22);
assert.equal(live.events.length, 0, "形成中的扫流动性 K 线不产生已确认事件");
assert.equal(live.live, null, "尚未形成入场窗口时不虚构实时窗口");

// 断链会清除此前结构；窗口按 20 根有效 K 线到期。
const chained = calculate(bearishBars.map((item, index) => index >= 22 ? { ...item, chain_id: 2 } : item));
assert.equal(chained.events.length, 0);
const formingGap = bearishBars.map((item, index) => index === 22 ? { ...item, is_forming: true } : item);
const afterFormingGap = calculate(formingGap.concat(bar(24, 105, 106, 95, 96)));
assert.equal(afterFormingGap.events.length, 0, "中间形成中的 K 线后不得沿用旧枢轴和 ATR 状态");
const extended = calculate(bearishBars.concat(Array.from({ length: 26 }, (_, offset) => bar(24 + offset, 96, 98, 94, 96))));
assert.equal(extended.entryWindows[0].endIndex, 43);
assert.equal(extended.entryWindows[0].active, false);

// 连续反转信号只能保留最后一个有效入场窗口，旧窗口必须转为历史窗口。
function seededBars(seed, count) {
  let state = seed >>> 0;
  let price = 100;
  const result = [];
  const random = () => {
    state = (Math.imul(state, 1664525) + 1013904223) >>> 0;
    return state / 4294967296;
  };
  for (let index = 0; index < count; index += 1) {
    const open = price;
    const close = Math.max(1, open + (random() - 0.5) * 20);
    const high = Math.max(open, close) + random() * 10;
    const low = Math.max(1, Math.min(open, close) - random() * 10);
    result.push(bar(index, open, high, low, close));
    price = close;
  }
  return result;
}
const repeatedSignals = calculate(seededBars(23, 180), {
  pivotLength: 2, atrLength: 5, bodyAtrMultiple: 0.1, minFvgAtrMultiple: 0, maxReversalBars: 10,
});
assert.deepEqual(Array.from(repeatedSignals.events, ({ index, side }) => [index, side]),
  [[80, "buy"], [128, "sell"], [156, "buy"], [172, "sell"]]);
assert.deepEqual(Array.from(repeatedSignals.entryWindows, ({ startIndex, active }) => [startIndex, active]),
  [[80, false], [128, false], [156, false], [172, true]]);

const chainReset = calculate(bearishBars.concat([
  bar(24, 96, 98, 94, 96, { chain_id: 2 }),
]));
assert.equal(chainReset.entryWindows[0].active, false, "断链后旧入场窗口不得继续保持有效");

// 不完整或不可能的 OHLC 不应抛异常，也不能生成结构。
assert.doesNotThrow(() => calculate([{ date: "bad", open: 10, high: 9, low: 11, close: 10 }]));
assert.equal(calculate([{ date: "bad", open: 10, high: 9, low: 11, close: 10 }]).events.length, 0);

console.log("Sweep liquidity reversal, FVG qualification, causal pivots, live bars, and expiry checks passed");
