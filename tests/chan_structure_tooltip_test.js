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
  composition: "输入笔 3–7；输入 bar 20–80", ruleVersion: "chan-recursive-v4-preview.2",
});
assert.equal(details.title, "线段 · L2 · 已确认");
assert.match(details.body, /2026-10-01 100.00 → 2026-10-12 120.00/);
assert.match(details.body, /形成 2026-10-12 · 确认 2026-10-14/);
assert.match(details.body, /输入笔 3–7；输入 bar 20–80/);
assert.match(details.body, /chan-recursive-v4-preview.2/);

const forming = window.OptionScopeCharts.chanStructureTooltipDetails({
  type: "笔", level: 0, confirmed: false, startPrice: 8, endPrice: 10,
  composition: "输入 bar 1–未知", ruleVersion: "strict-old-bi-v3",
}, "recursive-fallback");
assert.equal(forming.title, "笔 · L0 · 形成中");
assert.match(forming.body, /形成时间未知 · 尚未确认/);
assert.match(forming.body, /规则 strict-old-bi-v3/);
assert.equal(window.OptionScopeCharts.chanStructureTooltipDetails(null), null);

console.log("Chan structure tooltip detail checks passed");
