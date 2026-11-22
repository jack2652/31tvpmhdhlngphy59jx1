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

const bar = (close, width = 2) => ({ open: close - 0.2, high: close + width / 2, low: close - width / 2, close, volume: 1000 });
const trend = Array.from({ length: 250 }, (_, index) => bar(80 + index * 0.12));
const average = trend.slice(-200).reduce((sum, item) => sum + item.close, 0) / 200;
const recoveryTail = [
  { open: average + 0.5, high: average + 0.7, low: average - 0.4, close: average - 0.2 },
  { open: average, high: average + 0.9, low: average - 0.1, close: average + 0.7 },
];
const touchTail = [
  { open: average + 1, high: average + 1.2, low: average + 0.2, close: average + 0.4 },
  { open: average + 0.3, high: average + 1, low: average + 0.15, close: average + 0.9 },
];
const mirror = (items) => items.map((item) => ({ ...item, open: 220 - item.open, high: 220 - item.low, low: 220 - item.high, close: 220 - item.close }));
for (const [input, expected] of [
  [[...trend, ...recoveryTail], 2], [[...trend, ...touchTail], 3],
  [mirror([...trend, ...recoveryTail]), 6], [mirror([...trend, ...touchTail]), 7],
]) {
  assert.deepEqual(Array.from(calculate(input).events, (event) => event.rule), [expected], "趋势内短穿越与未穿越回踩分类正确，多空对称");
}
const deepTouch = [...trend, touchTail[0], { ...touchTail[1], low: average - 5 }];
assert.equal(calculate(deepTouch).events.some((event) => event.rule === 3), false, "深刺穿均线不得叫未破回踩买3");
assert.equal(calculate(mirror(deepTouch)).events.some((event) => event.rule === 7), false, "深穿均线不得叫未破反抽卖7");
const deepRecovery = [...trend, { ...recoveryTail[0], low: average - 5 }, recoveryTail[1]];
assert.equal(calculate(deepRecovery).events.some((event) => event.rule === 2), false, "深度越界后恢复不冒充短暂跌破");
const slowRecovery = [...trend, recoveryTail[0], ...Array.from({ length: 11 }, () => bar(average - 0.3, 0.4)), recoveryTail[1]];
assert.equal(calculate(slowRecovery).events.some((event) => event.rule === 2), false, "越界超过10根后不沿用旧回撤候选");
const repeatedTouch = [...trend, ...touchTail];
for (let index = 0; index < 6; index += 1) {
  const avg = repeatedTouch.slice(-199).reduce((sum, item) => sum + item.close, 0) / 199;
  const close = avg + (index % 2 ? 0.9 : 0.6);
  repeatedTouch.push({ open: close - 0.1, high: close + 0.1, low: avg + 0.25, close });
}
assert.equal(calculate(repeatedTouch).events.filter((event) => event.rule === 3).length, 1, "同一轮近均线反复回踩只提示一次");
const flat = Array.from({ length: 230 }, () => bar(100));
assert.ok(calculate([...flat, bar(99), bar(102)]).events.some((event) => event.rule === 1), "均线转升后穿越识别买1");
assert.ok(calculate(mirror([...flat, bar(99), bar(102)])).events.some((event) => event.rule === 5), "均线转降后穿越识别卖5");
assert.ok(calculate([...flat, bar(80), bar(84)]).events.some((event) => event.rule === 4), "下跌乖离先建立，再确认向上反应买4");
assert.ok(calculate(mirror([...flat, bar(80), bar(84)])).events.some((event) => event.rule === 8), "上涨乖离反应卖8镜像");
for (const separator of [{ ...bar(100), close: NaN }, { ...bar(100), is_forming: true }]) {
  const input = [...trend, separator, ...trend];
  assert.equal(calculate(input).sma[251 + 198], null, "无效或中间形成bar后均线重新预热");
  assert.ok(Number.isFinite(calculate(input).sma[251 + 199]), "完整200根后恢复均线");
}
const chained = [...trend, ...trend.map((item) => ({ ...item, chain_id: 2 }))];
assert.equal(calculate(chained).sma[250 + 198], null, "链切换后不能沿用旧SMA");
const replacementSource = [...trend, ...recoveryTail];
const complete = calculate(replacementSource);
for (let end = 200; end <= replacementSource.length; end += 1) {
  const prefix = calculate(replacementSource.slice(0, end));
  assert.deepEqual(JSON.parse(JSON.stringify(prefix)), JSON.parse(JSON.stringify({ sma: complete.sma.slice(0, end), events: complete.events.filter((event) => event.index < end) })), "葛兰碧新增未来行情不修改已确认历史");
}
const liveDisplay = replacementSource.map((item, index) => index === 251 ? { ...item, is_forming: true } : item);
const live = window.OptionScopeGranville.calculateWithLiveBar(replacementSource, liveDisplay, 251);
assert.equal(live.events.length, 0, "实时替换已收盘信号不能提前确认");
assert.ok(Number.isFinite(live.liveSma), "同链实时均线仍能显示");
assert.equal(window.OptionScopeGranville.calculateWithLiveBar(replacementSource, [...liveDisplay.slice(0, 251), { ...liveDisplay[251], chain_id: 2 }], 251).liveSma, null, "新链形成bar不混合旧均线");

console.log("Granville sell-eight excursion lock checks passed");
