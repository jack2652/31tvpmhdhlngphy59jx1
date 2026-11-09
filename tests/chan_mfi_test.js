const assert = require("node:assert/strict");
const fs = require("node:fs");
const vm = require("node:vm");

const window = {};
vm.runInNewContext(fs.readFileSync("web/static/common/js/chan-mfi.js", "utf8"), { window });

function bars(values, volume = 100) {
  return values.map((close, index) => ({
    date: `2026-01-${String(index + 1).padStart(2, "0")}`,
    open: close - 0.2, high: close + 0.5, low: close - 0.5, close, volume,
  }));
}

const rising = window.OptionScopeMfi.calculate(bars(Array.from({ length: 30 }, (_, index) => 10 + index)));
assert.ok(rising.mfi.slice(13).every((value) => Number.isFinite(value) && value >= 0 && value <= 100), "MFI must stay within 0-100");
assert.equal(rising.mfi.at(-1), 100, "持续流入且无流出时 MFI 为 100");
assert.ok(Number.isFinite(rising.average.at(-1)), "MFI 变色均线在预热后应有值");
assert.equal(rising.trend.at(-1), "flat", "恒定 MFI 均线应标记为横向");

const noVolume = window.OptionScopeMfi.calculate(bars(Array.from({ length: 20 }, (_, index) => 10 + (index % 2)), 0));
assert.ok(noVolume.mfi.slice(13).every((value) => value === 50), "流入和流出都为零时 MFI 为 50");

const formingBars = bars(Array.from({ length: 30 }, (_, index) => 10 + index));
formingBars[29].is_forming = true;
const forming = window.OptionScopeMfi.calculateWithLiveBar(formingBars.slice(0, 29), formingBars, [], 29);
assert.equal(forming.events.some((event) => event.index === 29), false, "形成中的 K 线不能产生确认事件");
assert.ok(Number.isFinite(forming.liveValues.mfi), "形成中的 K 线仍应更新实时 MFI 数值");

const benchmark = bars(Array.from({ length: 30 }, (_, index) => 20 + index));
const compared = window.OptionScopeMfi.calculate(bars(Array.from({ length: 30 }, (_, index) => 10 + index)), benchmark);
assert.equal(compared.benchmarkMfi.length, 30, "基准 MFI 应按个股日期对齐");
assert.ok(compared.benchmarkMfi.at(-1) >= 0 && compared.benchmarkMfi.at(-1) <= 100, "基准 MFI 应在有效范围内");

const cachedSource = bars(Array.from({ length: 30 }, (_, index) => 10 + index));
const emptyBenchmark = bars(Array.from({ length: 30 }, (_, index) => 20 + index), 0);
const cachedWithoutBenchmark = window.OptionScopeMfi.calculateWithLiveBar(cachedSource, cachedSource, emptyBenchmark, -1);
assert.ok(cachedWithoutBenchmark.benchmarkMfi.every((value) => value === null), "首次没有基准成交量时不应绘制标普 MFI");
const cachedWithBenchmark = window.OptionScopeMfi.calculateWithLiveBar(cachedSource, cachedSource, benchmark, -1);
assert.ok(cachedWithBenchmark.benchmarkMfi.some((value) => Number.isFinite(value)), "基准数据补齐后应重新计算标普 MFI");

const unavailableBenchmark = window.OptionScopeMfi.calculate(
  bars(Array.from({ length: 30 }, (_, index) => 10 + index)),
  bars(Array.from({ length: 30 }, (_, index) => 20 + index), 0),
);
assert.ok(unavailableBenchmark.benchmarkMfi.every((value) => value === null), "无成交量的标普序列应视为数据暂缺");

const fallingBenchmark = window.OptionScopeMfi.calculate(
  bars(Array.from({ length: 30 }, (_, index) => 10 + index)),
  bars(Array.from({ length: 30 }, (_, index) => 50 - index)),
);
assert.equal(fallingBenchmark.benchmarkMfi.at(-1), 0, "有效的纯流出序列允许 MFI 为 0");

const eventBars = bars(Array.from({ length: 40 }, (_, index) => index < 22 ? 30 - index : 8 + index));
const eventResult = window.OptionScopeMfi.calculate(eventBars);
for (const event of eventResult.events) {
  assert.ok(Number.isFinite(event.average), "MFI 交叉事件应携带对应均线值");
}

console.log("MFI calculation checks passed");
