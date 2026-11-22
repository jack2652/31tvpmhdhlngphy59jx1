const assert = require("node:assert/strict");
const fs = require("node:fs");
const vm = require("node:vm");
const window = {};
vm.runInNewContext(fs.readFileSync("web/static/common/js/chan-wyckoff.js", "utf8"), { window });
const { calculate, calculateWithLiveBar } = window.OptionScopeWyckoff;
const bar = (close, volume = 1000, width = 2) => ({ open: close - 0.2, high: close + width / 2, low: close - width / 2, close, volume });
const prep = Array.from({ length: 40 }, (_, index) => bar(120 - index * 0.5));
const tail = [
  { open: 100, high: 102, low: 96, close: 100, volume: 2000 },
  bar(100.5),
  { open: 100.5, high: 106, low: 100, close: 105, volume: 1300 },
  bar(102),
  { open: 96.8, high: 97.6, low: 96.2, close: 97.4, volume: 700 },
  bar(99),
  { open: 96, high: 97.5, low: 95, close: 97, volume: 800 },
  bar(98),
  { open: 96.5, high: 97.8, low: 96.4, close: 97.6, volume: 500 },
  { open: 104.5, high: 108, low: 104.5, close: 107.5, volume: 1800 },
  bar(108),
  { open: 106.8, high: 107.1, low: 106.1, close: 107, volume: 500 },
  bar(111), bar(112), bar(113),
  bar(103), bar(102), bar(92), bar(91),
];
const dated = (items) => items.map((item, index) => ({ ...item, date: new Date(Date.UTC(2025, 0, index + 1)).toISOString().slice(0, 10) }));
const input = dated([...prep, ...tail]);
const result = calculate(input);
for (let end = 1; end <= input.length; end += 1) {
  const prefix = calculate(input.slice(0, end));
  assert.deepEqual(JSON.parse(JSON.stringify(prefix)), JSON.parse(JSON.stringify({ snapshots: result.snapshots.slice(0, end), events: result.events.filter((event) => event.index < end) })), "阶段与事件不能因未来行情回写历史");
}
assert.deepEqual(Array.from(result.events, (event) => [event.index, event.kind]), [
  [40, "SC"], [42, "AR"], [44, "ST"], [46, "Spring"], [48, "Test"], [49, "SOS"], [51, "LPS"], [54, "E"],
], "有效量价路径能依次确认A–E事件");
assert.equal(result.snapshots[56].phase, "B", "趋势延伸回到区间两根后撤销突破并回到区间建设");
assert.equal(result.snapshots[58], null, "反向有效破坏后不延续旧吸筹区间");
assert.equal(new Set(result.events.map((event) => event.index)).size, result.events.length, "同根不得假设先反弹再测试等多步价格路径");
const mirror = input.map((item) => ({ ...item, open: 240 - item.open, high: 240 - item.low, low: 240 - item.high, close: 240 - item.close }));
assert.deepEqual(Array.from(calculate(mirror).events, (event) => event.kind), ["BC", "AR", "ST", "UTAD", "Test", "SOW", "LPSY", "E"], "派发为量价吸筹的镜像，路径对称");

const changed = (index, fields) => input.map((item, cursor) => cursor === index ? { ...item, ...fields } : item);
for (const volume of [undefined, null, NaN, Infinity, 0, -1]) {
  assert.equal(calculate(changed(44, { volume })).events.some((event) => event.index === 44), false, "缺量不得冒充缩量二次测试");
  assert.equal(calculate(changed(48, { volume })).events.some((event) => event.index === 48), false, "缺量不得冒充成功回测");
  assert.equal(calculate(changed(49, { volume })).events.some((event) => event.index === 49), false, "缺量不得确认量价突破");
}
assert.equal(calculate(changed(48, { volume: 900 })).events.some((event) => event.kind === "Test"), false, "回测量大于探底量不得叫成功回测");
assert.equal(calculate(changed(48, { low: 94.9 })).events.some((event) => event.index === 48 && event.kind === "Test"), false, "更低低点不得叫成功回测");
assert.equal(calculate(changed(44, { volume: 2100 })).events.some((event) => event.index === 44 && event.kind === "ST"), false, "高潮等量重探不得叫缩量二测，之后有效测试仍可恢复");
assert.equal(calculate(changed(51, { low: 100 })).events.some((event) => event.kind === "LPS"), false, "深跌回区间即使收回也不标最后支撑");
const lowVolumeBreakout = input.slice(0, 49).concat([
  { ...input[49], volume: 300 }, { ...input[50], open: 107, high: 112, low: 107, close: 111, volume: 300 },
]);
assert.equal(calculate(lowVolumeBreakout).events.some((event) => event.kind === "SOS"), false, "两根宽幅低量突破不能代替量价证据");
const noSpring = input.slice(0, 46).concat(input.slice(49, 55));
assert.ok(calculate(noSpring).events.some((event) => event.kind === "SOS"), "不存在Spring的有效突破路径仍能推进D");
const sameBarReaction = input.slice(0, 42).concat([{ ...input[42], low: 96, volume: 700 }]);
assert.deepEqual(Array.from(calculate(sameBarReaction).events, (event) => event.kind), ["SC", "AR"], "一根宽幅bar碰两端只能确认AR，不能同根ST");
const noReaction = input.slice(0, 41).concat(Array.from({ length: 46 }, () => bar(97.2, 900, 0.6)));
assert.equal(calculate(noReaction).snapshots.at(-1), null, "45根未有自动反应的A阶段过期");
const brokenD = input.slice(0, 50).concat([bar(102), bar(101)]);
assert.equal(calculate(brokenD).snapshots.at(-1).phase, "B", "D阶段假突破两根回区间后回退");
const failedProbe = input.slice(0, 47).concat([{ open: 94.85, high: 95.05, low: 94.75, close: 94.9, volume: 900 }]);
assert.equal(calculate(failedProbe).snapshots.at(-1).phase, "B", "C探底失守原试探价后回到B");

for (const separator of [{ ...bar(100), close: NaN }, { ...bar(100), is_forming: true }, { ...bar(100), chain_id: 2 }]) {
  const after = [...input.slice(0, 50), separator, ...prep.map((item) => ({ ...item, chain_id: separator.chain_id }))];
  const fresh = calculate(after);
  assert.equal(fresh.snapshots[51], null, "断链/坏价/中间形成bar不沿用旧阶段");
  assert.equal(fresh.events.some((event) => event.index >= 51), false, "重新预热期间不得编造阶段事件");
}
assert.equal(calculate(dated([...prep.slice(0, 20), tail[0]])).events.length, 0, "没有完整30根先前行情不提前识别高潮");
const source = input.slice(0, 50), display = source.map((item, index) => index === 49 ? { ...item, is_forming: true } : item);
const live = calculateWithLiveBar(source, display, 49);
assert.equal(live.events.some((event) => event.kind === "SOS"), false, "替换bar时剔除其已完成突破，实时不提前进入D");
assert.equal(live.snapshots[49].phase, "C", "实时阶段沿用之前已确认的C");
assert.equal(calculateWithLiveBar(source, display.map((item, index) => index === 49 ? { ...item, chain_id: 2 } : item), 49).snapshots[49], null, "形成bar换链后不得显示旧阶段");
assert.equal(calculateWithLiveBar(source, source, -1).snapshots[49].phase, "D", "普通完成路径与live截止缓存分开");
console.log("Wyckoff causal phase paths, volume evidence, lifecycle, and live-bar checks passed");
