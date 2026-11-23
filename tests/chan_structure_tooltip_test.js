const assert = require("node:assert/strict");
const fs = require("node:fs");
const vm = require("node:vm");

const window = { devicePixelRatio: 1, addEventListener() {}, removeEventListener() {} };
const sandbox = {
  window, document: {}, formatMoney: (value) => Number(value).toFixed(2),
  getComputedStyle: () => ({ getPropertyValue: () => "" }),
  ResizeObserver: undefined, MutationObserver: undefined,
};
vm.runInNewContext(fs.readFileSync("web/static/common/js/charts.js", "utf8"), sandbox);

const details = window.OptionScopeCharts.chanStructureTooltipDetails({
  type: "线段", level: 2, confirmed: true, start: "2026-10-01", end: "2026-10-12",
  startPrice: 100, endPrice: 120, formedAt: "2026-10-12", confirmedAt: "2026-10-14",
  composition: "输入笔 3–7；输入 bar 20–80", ruleVersion: "chan-recursive-v4-preview.4",
});
assert.equal(details.title, "第3层线段 · 已确认");
assert.match(details.body, /2026-10-01 100.00 → 2026-10-12 120.00/);
assert.match(details.body, /形成 2026-10-12 · 确认 2026-10-14/);
assert.match(details.body, /输入笔 3–7；输入 bar 20–80/);
assert.match(details.body, /chan-recursive-v4-preview.4/);

const forming = window.OptionScopeCharts.chanStructureTooltipDetails({
  type: "笔", level: 0, confirmed: false, startPrice: 8, endPrice: 10,
  composition: "输入 bar 1–未知", ruleVersion: "strict-old-bi-v3",
}, "recursive-fallback");
assert.equal(forming.title, "笔 · 形成中");
assert.match(forming.body, /形成时间未知 · 尚未确认/);
assert.match(forming.body, /规则 strict-old-bi-v3/);
assert.equal(window.OptionScopeCharts.chanStructureTooltipDetails(null), null);

// 2026-11-23，Codex：预警详情须显示真实状态和观察/确认时间。
const warning = window.OptionScopeCharts.chanStructureTooltipDetails({
  type: "反弹承压", signalTarget: true, signalStatus: "已失效", signalPhase: "已失效", confirmed: false,
  start: "2026-10-06", endPrice: 146.4, invalidationPrice: 146.4, formedAt: "2026-10-08", confirmedAt: "2026-10-22",
});
assert.equal(warning.title, "反弹承压 · 已失效");
assert.match(warning.body, /可观察 2026-10-08 · 确认 2026-10-22/);
const unconfirmedWarning = window.OptionScopeCharts.chanStructureTooltipDetails({
  type: "底背驰预警", signalStatus: "已失效", formedAt: "2026-10-26", confirmedAt: "",
});
assert.match(unconfirmedWarning.body, /确认 未确认/);
assert.doesNotMatch(unconfirmedWarning.title, /形成中/);
// 日期：2026-11-23；执行者：Codex。独立早风险详情保留实际观察日和固定保护价。
const earlyWarning = window.OptionScopeCharts.chanStructureTooltipDetails({
  key: "early", type: "顶部早预警", signalStatus: "收盘失守预警保护价", start: "2026-10-07",
  formedAt: "2026-10-07", confirmedAt: "", pivotDate: "2026-10-06", endPrice: 762.86,
  protectionPrice: 751.75, invalidationPrice: 762.86,
});
assert.match(earlyWarning.title, /顶部早预警.*收盘失守/);
assert.match(earlyWarning.body, /可观察 2026-10-07 · 确认 未确认/);
assert.match(earlyWarning.body, /预警保护价 751.75 · 参考顶部日期 2026-10-06/);

console.log("Chan structure tooltip detail checks passed");
