const assert = require("node:assert/strict");
const fs = require("node:fs");
const vm = require("node:vm");

const template = fs.readFileSync("web/static/index.html", "utf8");
const window = {};
vm.runInNewContext(fs.readFileSync("web/static/common/js/chan-plan.js", "utf8"), { window });
vm.runInNewContext(fs.readFileSync("web/static/common/js/chan-client.js", "utf8"), { window });

function frame() {
  const candles = [
    { date: "2026-10-12", open: 10, high: 12, low: 9, close: 11 },
    { date: "2026-10-13", open: 11, high: 14, low: 10, close: 13 },
    { date: "2026-10-14", open: 13, high: 15, low: 11, close: 12 },
    { date: "2026-10-15", open: 12, high: 13, low: 8, close: 9 },
  ];
  return {
    period: "1d", label: "日线", as_of: "2026-10-15", bar_count: candles.length, last_close: 9, quality: { price_basis: "split_dividend_adjusted", status: "partial", accepted_bars: 4, input_bars: 5, gaps: [{ expected_date: "2026-10-14", next_date: "2026-10-15" }] },
    market_environment: { benchmark: "^GSPC", status: "classified", regime: "bull", date: "2026-10-15", reason: "SMA50 位于 SMA200 上方", rule_version: "sp500-sma50-sma200-v1" },
    candles,
    strokes: [{ direction: "up", low: 8, high: 13, confirmed: true, start: { date: "2026-10-14", kind: "bottom", price: 11 }, end: { date: "2026-10-15", kind: "top", price: 13 } }],
    centers: [], signals: [{ kind: "bottom_reversal_candidate", side: "buy", status: "candidate", origin_date: "2026-10-14", date: "2026-10-15", price: 11, label: "底部反转候选", observed_at: "2026-10-15", confirmed_at: "2026-10-15" }],
    pen_signals: [{ kind: "third_buy_candidate", side: "buy", status: "candidate", date: "2026-10-14", price: 11, label: "旧笔级三买" }],
    position: { label: "观察", detail: "测试", center: null },
    segments: [{ level: 0, chain_id: 0, direction: "up", start: "2026-10-12", end: "2026-10-14", start_price: 9, end_price: 15, low: 9, high: 15, confirmed: true, forming: false, confirmed_at: "2026-10-15" }],
    recursive_levels: [
      { level: 0, segments: [{ level: 0, chain_id: 0, direction: "up", start: "2026-10-12", end: "2026-10-14", start_price: 9, end_price: 15, low: 9, high: 15, confirmed: true, forming: false }] },
      { level: 1, segments: [{ level: 1, chain_id: 0, direction: "down", start: "2026-10-13", end: "2026-10-15", start_price: 14, end_price: 8, low: 8, high: 14, confirmed: false, forming: true }] },
    ],
    recursive_centers: [{ level: 1, kind: "initial", center_id: "center-l1", chain_id: 0, start: "2026-10-12", end: "2026-10-14", zd: 10, zg: 12, forming: true, extension_count: 2 }],
    signals_v4: [
      { symbol: "QQQ", signal_id: "s1", level: 1, kind: "class1_buy", side: "buy", status: "awaiting_reversal", phase: "observation", rule_version: "chan-recursive-v4-preview.4", date: "2026-10-15", observed_at: "2026-10-16", price: 8, conditions: ["同级确认走势段"], missing_conditions: ["后续反向线段确认走势端点"], invalidation_price: 8, market_filter: "classified", market_environment: { benchmark: "^GSPC", status: "classified", regime: "bull", date: "2026-10-15", rule_version: "sp500-sma50-sma200-v1" }, lower_level_status: "awaiting_lower_level", structure_ids: ["seg-1"] },
      { symbol: "QQQ", signal_id: "s2", level: 1, kind: "class2_buy", side: "buy", status: "observing", phase: "first_retest", rule_version: "chan-recursive-v4-preview.4", date: "2026-10-14", observed_at: "2026-10-15", price: 9, conditions: ["首次回试低点严格高于一买低点"], missing_conditions: ["等待本级恢复方向的已确认线段"], invalidation_price: 8, market_filter: "unfiltered", lower_level_status: "ready", related_signal_id: "s1", structure_ids: ["seg-2"] },
      { symbol: "QQQ", signal_id: "s3", level: 1, kind: "class3_sell", side: "sell", status: "confirmed", phase: "class3_confirmed", rule_version: "chan-recursive-v4-preview.4", date: "2026-10-13", observed_at: "2026-10-14", confirmed_at: "2026-10-15", price: 12, conditions: ["回试高点严格低于 ZD"], missing_conditions: [], invalidation_price: 13, market_filter: "unfiltered", lower_level_status: "unconfirmed", related_center_id: "center-1", structure_ids: ["center-1", "seg-3"] },
      { symbol: "QQQ", signal_id: "s4", level: 1, kind: "class2_sell", side: "sell", status: "awaiting_class1_confirmation", phase: "awaiting_class1_confirmation", rule_version: "chan-recursive-v4-preview.12", date: "2026-10-15", observed_at: "2026-10-16", conditions: ["一类点背驰候选已出现；一类点确认后才检查次级别回试"], missing_conditions: ["等待一卖反向结构确认"], invalidation_price: 14, market_filter: "unfiltered", lower_level_status: "awaiting_class1_confirmation", related_signal_id: "s5", structure_ids: ["s5"] },
      { symbol: "QQQ", signal_id: "s6", level: 0, source_kind: "pen", kind: "class1_sell", side: "sell", status: "invalidated", phase: "invalidated", rule_version: "chan-recursive-v4-preview.18", date: "2026-10-13", observed_at: "2026-10-14", phase_at: "2026-10-15", price: 14, conditions: ["历史一类卖点"], missing_conditions: [], invalidation_price: 14, market_filter: "unfiltered", lower_level_status: "unconfirmed", structure_ids: ["pen-segment-1"] },
    ],
  };
}

const payload = { daily: frame(), weekly: frame(), spot: 9, summary: {}, price_coordinate: { factor_date: "2026-10-15", basis: "split_dividend_adjusted" } };
const allLayers = window.OptionScopeChan.view(payload, true, false, 30, null, {
  pens: true, penCenters: true, legacySignals: true, segments: true, segmentLevels: { 0: true, 1: true }, recursiveCenters: { 1: true }, signals: true,
});
let layerState = { segments: true, segmentLevels: { 0: true, 1: true } };
assert.equal(window.OptionScopeChan.allSegmentLevelsVisible(layerState, [0, 1]), true);
layerState = window.OptionScopeChan.toggleAllSegmentLevels(layerState, [0, 1]);
assert.equal(layerState.segments, false, "master switch should hide all levels");
layerState = window.OptionScopeChan.toggleSegmentLevel(layerState, 0);
assert.equal(window.OptionScopeChan.isSegmentLevelVisible(layerState, 0), true, "L0 should be independently enabled while master default stays off");
assert.equal(window.OptionScopeChan.isSegmentLevelVisible(layerState, 1), false, "enabling L0 must leave other levels hidden");
assert.equal(window.OptionScopeChan.allSegmentLevelsVisible(layerState, [0, 1]), false, "master state must report mixed visibility as not all visible");
layerState = window.OptionScopeChan.toggleAllSegmentLevels(layerState, [0, 1]);
assert.equal(layerState.segments, true, "master switch should restore every level from mixed state");
assert.deepEqual(Object.keys(layerState.segmentLevels), [], "master switch should clear stale per-level overrides");
const chart = allLayers.daily.chart;
assert.equal(allLayers.daily.recursive.emptySignalNotice, "", "a plottable recursive signal should suppress the empty-signal notice");
assert.equal(chart.segments.length, 2, "chart should include base and recursive-level segments");
assert.deepEqual(Array.from(chart.layerLevels), [1], "center toggle levels should derive from available centers");
assert.equal(chart.recursiveCenters.length, 1, "selected recursive center should be mapped");
assert.match(chart.recursiveCenters[0].title, /第1级中枢 10.00–12.00/);
assert.equal(chart.signals.length, 4, "enabled recursive and pen-level signals should be mapped");
assert.equal(chart.markers.length, 1, "enabled legacy signal markers should be mapped");
const turningPoint = chart.points.find((point) => point.date === "2026-10-14" && point.price === 11);
const turningWarning = chart.markers[0];
const turningPointHalfWidth = (Array.from(turningPoint.label).length * 9 * 0.55 + 8) / 2;
const turningWarningHalfWidth = (turningWarning.hitLabelWidth || 48) / 2;
const turningWarningHalfHeight = (turningWarning.labelHeight || 18) / 2;
const turningPointBox = {
  left: turningPoint.labelX - turningPointHalfWidth, right: turningPoint.labelX + turningPointHalfWidth,
  top: turningPoint.labelY - 11, bottom: turningPoint.labelY + 4,
};
const turningWarningBox = {
  left: turningWarning.labelX - turningWarningHalfWidth, right: turningWarning.labelX + turningWarningHalfWidth,
  top: turningWarning.labelY - turningWarningHalfHeight, bottom: turningWarning.labelY + turningWarningHalfHeight,
};
const turningLabelsSeparated = turningWarningBox.right + 4 <= turningPointBox.left
  || turningWarningBox.left - 4 >= turningPointBox.right
  || turningWarningBox.bottom + 4 <= turningPointBox.top
  || turningWarningBox.top - 4 >= turningPointBox.bottom;
assert.equal(turningLabelsSeparated, true, `turning warning labels must leave space around pivot prices: ${JSON.stringify({ turningPointBox, turningWarningBox, marker: turningWarning })}`);
assert.equal(turningWarning.labelConnector, true, "relocated turning warning labels must remain connected to their pivot");
assert.equal(allLayers.daily.legacySignals.length, 1, "legacy panel should contain only the separate turning warning");
assert.ok(!allLayers.daily.signals.some((signal) => signal.kind === "third_buy_candidate"), "legacy pen observations must not re-enter the canonical structural signal list");
assert.deepEqual(Array.from(chart.signals.map((signal) => signal.label)), ["一买", "二买", "三卖", "一卖 · 已失效"]);
assert.equal(chart.signals.find((signal) => signal.type === "一卖").invalidated, true, "historical invalidated class points should remain visible on the chart");
assert.equal(allLayers.daily.recursiveSignals.length, 5, "signal details should retain every level and waiting candidates without chart prices");
assert.equal(allLayers.daily.recursiveSignals.find((signal) => signal.signal_id === "s6").source_kind, "pen", "signal details should preserve the source of pen-level points");
assert.equal(allLayers.daily.recursiveSignals.find((signal) => signal.kind === "class2_sell").statusLabel, "等待一类点确认");
assert.equal(allLayers.daily.recursiveSignals.find((signal) => signal.kind === "class2_sell").price, undefined);
assert.match(allLayers.daily.recursiveSignals.find((signal) => signal.kind === "class2_sell").evidenceLabel, /等待一卖反向结构确认/);
assert.match(allLayers.daily.recursiveSignals.find((signal) => signal.kind === "class2_sell").marketAndLevelLabel, /下级别确认：等待一类点确认/);
assert.ok(allLayers.daily.dataSummary.includes("拆股/分红复权"));
assert.match(allLayers.daily.dataSummary, /基准日 2026-10-15/);
assert.ok(allLayers.daily.dataSummary.includes("输入质量 partial（4/5 根）"));
assert.match(allLayers.daily.dataSummary, /缺口 1 处/);
assert.match(allLayers.daily.marketSummary, /S&P 500 牛市环境 · 2026-10-15/);
assert.match(allLayers.daily.marketSummary, /SMA50 位于 SMA200 上方/);
assert.equal(allLayers.daily.signals.filter((signal) => signal.kind === "class2_buy")[0].label, "二买");
assert.match(allLayers.daily.signals.find((signal) => signal.kind === "class2_buy").evidenceLabel, /等待本级恢复方向的已确认线段/);
const crowdedFrame = frame();
crowdedFrame.signals_v4 = ["class1_buy", "class2_buy", "class3_buy", "class1_sell", "class2_sell", "class3_sell"]
  .map((kind, index) => ({ signal_id: `crowded-${index}`, kind, side: kind.endsWith("buy") ? "buy" : "sell",
    status: "invalidated", date: "2026-10-14", price: 12 }));
const crowdedView = window.OptionScopeChan.view({ ...payload, daily: crowdedFrame }, true, false, 30, null, {
  signals: true, legacySignals: false,
});
const crowdedSignals = crowdedView.daily.chart.signals;
assert.equal(crowdedSignals.length, 6, "crowded historical signals must all remain in the canvas model");
for (let leftIndex = 0; leftIndex < crowdedSignals.length; leftIndex += 1) {
  for (let rightIndex = leftIndex + 1; rightIndex < crowdedSignals.length; rightIndex += 1) {
    const left = crowdedSignals[leftIndex], right = crowdedSignals[rightIndex];
    const overlaps = Math.abs(left.labelX - right.labelX) < (left.hitLabelWidth + right.hitLabelWidth) / 2
      && Math.abs(left.labelY - right.labelY) < 16;
    assert.equal(overlaps, false, `Chan signal labels overlap: ${left.type} and ${right.type}`);
  }
}
const duplicateSignalFrame = frame();
duplicateSignalFrame.signals_v4 = [
  { signal_id: "duplicate-pen", level: 0, source_kind: "pen", kind: "class1_sell", side: "sell", status: "invalidated", date: "2026-10-13", price: 14 },
  { signal_id: "duplicate-recursive", level: 1, source_kind: "recursive", kind: "class1_sell", side: "sell", status: "invalidated", date: "2026-10-13", price: 14 },
  { signal_id: "different-date", level: 1, source_kind: "recursive", kind: "class1_sell", side: "sell", status: "invalidated", date: "2026-10-14", price: 13 },
  { signal_id: "different-status", level: 1, source_kind: "recursive", kind: "class1_sell", side: "sell", status: "confirmed", date: "2026-10-13", price: 14 },
];
const duplicateSignalView = window.OptionScopeChan.view({ ...payload, daily: duplicateSignalFrame }, true, false, 30, null, { signals: true });
assert.deepEqual(Array.from(duplicateSignalView.daily.chart.signals.map((signal) => signal.labelVisible)),
  [true, false, true, true], "only repeated same-date rendered labels should be hidden");
assert.equal(duplicateSignalView.daily.chart.signals.length, 4, "deduplicating labels must keep every signal marker");
assert.equal(duplicateSignalView.daily.recursiveSignals.length, 4, "deduplication should not remove distinct API signal details");
assert.match(allLayers.daily.signals.find((signal) => signal.kind === "class1_buy").marketAndLevelLabel, /S&P 500 牛市环境 · 2026-10-15 · 环境已分类 · 下级别确认：等待下级别确认 · 跨周期确认：未提供/);
assert.match(allLayers.daily.signals.find((signal) => signal.kind === "class1_buy").evidenceLabel, /规则证据：同级确认走势段/);
assert.match(allLayers.daily.signals.find((signal) => signal.kind === "class1_buy").evidenceLabel, /失效边界 8.00/);
assert.match(allLayers.daily.signals.find((signal) => signal.kind === "class1_buy").marketAndLevelLabel, /S&P 500 牛市环境 · 2026-10-15 · 环境已分类 · 下级别确认：等待下级别确认 · 跨周期确认：未提供/);
assert.match(allLayers.daily.signals.find((signal) => signal.kind === "class3_sell").marketAndLevelLabel, /下级别确认：尚未确认 · 跨周期确认：未提供/);
const penDetail = chart.structures.find((item) => item.type === "笔");
assert.ok(penDetail, "visible pen should expose a hover detail target");
assert.equal(penDetail.ruleVersion, "strict-old-bi-v3");
assert.equal(penDetail.start, "2026-10-14");
const segmentDetail = chart.structures.find((item) => item.type === "线段");
assert.ok(segmentDetail, "visible confirmed and forming segments should expose hover details");
assert.equal(segmentDetail.level, 0);
assert.match(segmentDetail.composition, /多笔走势/);
const centerDetail = chart.structures.find((item) => item.type === "第1级中枢");
assert.ok(centerDetail?.area, "selected recursive centers should expose area hover details");
assert.equal(centerDetail.ruleVersion, "chan-recursive-v4-preview.4");
const signalDetail = chart.structures.find((item) => item.type === "一买");
assert.ok(signalDetail, "recursive signals should expose rule and time details");
assert.match(signalDetail.composition, /一买 · L1/);
assert.match(signalDetail.composition, /可观察 2026-10-16/);
assert.match(signalDetail.composition, /待满足：后续反向线段确认走势端点/);
assert.match(signalDetail.composition, /失效边界 8.00/);
assert.match(signalDetail.composition, /关联结构/);
assert.equal(signalDetail.ruleVersion, "chan-recursive-v4-preview.4");

const levelZeroOnly = window.OptionScopeChan.view(payload, true, false, 30, null, {
  pens: false, penCenters: false, segments: false, segmentLevels: { 0: true }, recursiveCenters: {}, signals: false,
});
assert.equal(levelZeroOnly.daily.chart.segments.length, 1, "L0 override should show its segments while the all-segments default is off");
assert.equal(levelZeroOnly.daily.chart.segments[0].level, 0);

const higherLevelsOnly = window.OptionScopeChan.view(payload, true, false, 30, null, {
  pens: false, penCenters: false, segments: true, segmentLevels: { 0: false, 1: true }, recursiveCenters: {}, signals: false,
});
assert.equal(higherLevelsOnly.daily.chart.segments.length, 1, "an L0 override should hide only L0 while other segment levels remain enabled");
assert.equal(higherLevelsOnly.daily.chart.segments[0].level, 1);

const allOffThenL0On = window.OptionScopeChan.view(payload, true, false, 30, null, {
  pens: false, penCenters: false, segments: false, segmentLevels: { 0: true }, recursiveCenters: {}, signals: false,
});
assert.deepEqual(Array.from(allOffThenL0On.daily.chart.availableSegmentLevels), [0, 1], "segment controls should represent all available recursive levels, independent of visibility");
assert.equal(allOffThenL0On.daily.chart.segments.length, 1, "turning L0 back on after the master default is off should restore only L0");
assert.equal(allOffThenL0On.daily.chart.segments[0].level, 0);

const restoreAllAfterL0Filter = window.OptionScopeChan.view(payload, true, false, 30, null, {
  pens: false, penCenters: false, segments: true, segmentLevels: { 0: true, 1: true }, recursiveCenters: {}, signals: false,
});
assert.equal(restoreAllAfterL0Filter.daily.chart.segments.length, 2, "explicitly enabling both levels should render every recursive segment");

const hiddenLayers = window.OptionScopeChan.view(payload, true, false, 30, null, {
  pens: false, penCenters: false, segments: false, recursiveCenters: {}, signals: false,
});
assert.equal(hiddenLayers.daily.chart.segments.length, 0, "hidden segments should not be included in the canvas model");
assert.equal(hiddenLayers.daily.chart.recursiveCenters.length, 0, "hidden recursive centers should not be included in the canvas model");
assert.equal(hiddenLayers.daily.chart.signals.length, 0, "hidden recursive signals should not be included in the canvas model");
assert.equal(hiddenLayers.daily.chart.markers.length, 0, "hidden legacy signal markers should not be included in the canvas model");
assert.equal(hiddenLayers.daily.chart.structures.length, 0, "hidden structure layers should not leave invisible hover targets");
assert.deepEqual(Array.from(hiddenLayers.daily.chart.layers.recursiveCenters), [], "disabled center layers should remain off");

const waitingPayload = { ...payload, daily: { ...frame(), centers: [], signals_v4: [], recursive_centers: [], recursive_levels: [
  { level: 0, status: "forming", waiting_for: "等待特征序列破坏和后续原方向笔确认", segments: [
    { chain_id: -1, direction: "down", start: "2026-10-12", end: "2026-10-14", confirmed: true },
    { chain_id: -2, direction: "up", start: "2026-10-14", end: "2026-10-15", confirmed: false },
  ] },
  { level: 1, status: "awaiting_lower_level", waiting_for: "等待三个连续且方向交替的下级已确认线段", segments: [] },
], pen_signals: [{ kind: "third_buy_candidate", side: "buy" }] } };
const waitingView = window.OptionScopeChan.view(waitingPayload, true);
assert.match(waitingView.daily.recursive.readiness, /最长连续已确认线段为 1 条/);
assert.match(waitingView.daily.recursive.readiness, /等待特征序列破坏和后续原方向笔确认/);
assert.match(waitingView.daily.recursive.emptySignalNotice, /最长连续已确认线段为 1 条/);
assert.doesNotMatch(waitingView.daily.recursive.emptySignalNotice, /旧字段|笔级候选/);
assert.ok(!waitingView.daily.signals.some((signal) => signal.kind === "third_buy_candidate"), "waiting view must ignore deprecated pen signals");
assert.ok(template.includes('class="chan-summary-readiness" role="status">{{ chanFrame.recursive.emptySignalNotice }}</p>'), "the structure summary must render recursive signal blockers visibly");

console.log("Chan canvas layer model checks passed");
