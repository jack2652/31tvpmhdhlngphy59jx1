const assert = require("node:assert/strict");
const fs = require("node:fs");
const vm = require("node:vm");

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
    centers: [], signals: [{ kind: "bottom_reversal_candidate", side: "buy", status: "candidate", origin_date: "2026-10-14", date: "2026-10-15", price: 11, label: "底部反转候选", observed_at: "2026-10-15", confirmed_at: "2026-10-15" }], position: { label: "观察", detail: "测试", center: null },
    segments: [{ level: 0, chain_id: 0, direction: "up", start: "2026-10-12", end: "2026-10-14", start_price: 9, end_price: 15, low: 9, high: 15, confirmed: true, forming: false, confirmed_at: "2026-10-15" }],
    recursive_levels: [
      { level: 0, segments: [{ level: 0, chain_id: 0, direction: "up", start: "2026-10-12", end: "2026-10-14", start_price: 9, end_price: 15, low: 9, high: 15, confirmed: true, forming: false }] },
      { level: 1, segments: [{ level: 1, chain_id: 0, direction: "down", start: "2026-10-13", end: "2026-10-15", start_price: 14, end_price: 8, low: 8, high: 14, confirmed: false, forming: true }] },
    ],
    recursive_centers: [{ level: 1, kind: "initial", center_id: "center-l1", chain_id: 0, start: "2026-10-12", end: "2026-10-14", zd: 10, zg: 12, forming: true, extension_count: 2 }],
    signals_v4: [
      { symbol: "QQQ", signal_id: "s1", level: 1, kind: "class1_buy", side: "buy", status: "awaiting_reversal", phase: "observation", rule_version: "chan-recursive-v4-preview.2", date: "2026-10-15", observed_at: "2026-10-16", price: 8, conditions: ["同级确认走势段"], missing_conditions: ["后续反向线段确认走势端点"], invalidation_price: 8, market_filter: "classified", market_environment: { benchmark: "^GSPC", status: "classified", regime: "bull", date: "2026-10-15", rule_version: "sp500-sma50-sma200-v1" }, lower_level_status: "awaiting_lower_level", structure_ids: ["seg-1"] },
      { symbol: "QQQ", signal_id: "s2", level: 1, kind: "class2_buy", side: "buy", status: "observing", phase: "first_retest", rule_version: "chan-recursive-v4-preview.2", date: "2026-10-14", observed_at: "2026-10-15", price: 9, conditions: ["首次回试低点严格高于一买低点"], missing_conditions: ["等待本级恢复方向的已确认线段"], invalidation_price: 8, market_filter: "unfiltered", lower_level_status: "ready", related_signal_id: "s1", structure_ids: ["seg-2"] },
      { symbol: "QQQ", signal_id: "s3", level: 1, kind: "class3_sell", side: "sell", status: "confirmed", phase: "class3_confirmed", rule_version: "chan-recursive-v4-preview.2", date: "2026-10-13", observed_at: "2026-10-14", confirmed_at: "2026-10-15", price: 12, conditions: ["回试高点严格低于 ZD"], missing_conditions: [], invalidation_price: 13, market_filter: "unfiltered", lower_level_status: "unconfirmed", related_center_id: "center-1", structure_ids: ["center-1", "seg-3"] },
    ],
  };
}

const payload = { daily: frame(), weekly: frame(), spot: 9, summary: {}, price_coordinate: { factor_date: "2026-10-15", basis: "split_dividend_adjusted" } };
const allLayers = window.OptionScopeChan.view(payload, true, false, 30, null, {
  pens: true, penCenters: true, segments: true, recursiveCenters: { 1: true }, signals: true,
});
let layerState = { segments: true, segmentLevels: {} };
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
assert.equal(chart.segments.length, 2, "chart should include base and recursive-level segments");
assert.deepEqual(Array.from(chart.layerLevels), [1], "center toggle levels should derive from available centers");
assert.equal(chart.recursiveCenters.length, 1, "selected recursive center should be mapped");
assert.match(chart.recursiveCenters[0].title, /递归 L1 中枢 10.00–12.00/);
assert.equal(chart.signals.length, 3, "enabled recursive signals should be mapped");
assert.equal(chart.markers.length, 1, "enabled legacy signal markers should be mapped");
assert.deepEqual(Array.from(chart.signals.map((signal) => signal.label)), ["一买", "二买", "三卖"]);
assert.match(chart.signals[0].title, /chan-recursive-v4-preview.2/);
assert.match(chart.signals[0].title, /等待下级别确认/);
assert.ok(allLayers.daily.dataSummary.includes("拆股/分红复权"));
assert.match(allLayers.daily.dataSummary, /基准日 2026-10-15/);
assert.ok(allLayers.daily.dataSummary.includes("输入质量 partial（4/5 根）"));
assert.match(allLayers.daily.dataSummary, /缺口 1 处/);
assert.match(allLayers.daily.marketSummary, /S&P 500 牛市环境 · 2026-10-15/);
assert.match(allLayers.daily.marketSummary, /SMA50 位于 SMA200 上方/);
assert.equal(allLayers.daily.signals.filter((signal) => signal.kind === "class2_buy")[0].label, "二买");
assert.match(allLayers.daily.signals.find((signal) => signal.kind === "class2_buy").evidenceLabel, /等待本级恢复方向的已确认线段/);
assert.match(allLayers.daily.signals.find((signal) => signal.kind === "class1_buy").marketAndLevelLabel, /S&P 500 牛市环境 · 2026-10-15 · 环境已分类 · 递归级别确认：等待下级别确认 · 跨周期确认：未提供/);
assert.match(allLayers.daily.signals.find((signal) => signal.kind === "class1_buy").evidenceLabel, /规则证据：同级确认走势段/);
assert.match(allLayers.daily.signals.find((signal) => signal.kind === "class1_buy").evidenceLabel, /失效边界 8.00/);
assert.match(allLayers.daily.signals.find((signal) => signal.kind === "class1_buy").marketAndLevelLabel, /S&P 500 牛市环境 · 2026-10-15 · 环境已分类 · 递归级别确认：等待下级别确认 · 跨周期确认：未提供/);
assert.match(allLayers.daily.signals.find((signal) => signal.kind === "class3_sell").marketAndLevelLabel, /递归级别确认：尚未确认 · 跨周期确认：未提供/);
const penDetail = chart.structures.find((item) => item.type === "笔");
assert.ok(penDetail, "visible pen should expose a hover detail target");
assert.equal(penDetail.ruleVersion, "strict-old-bi-v3");
assert.equal(penDetail.start, "2026-10-14");
const segmentDetail = chart.structures.find((item) => item.type === "线段");
assert.ok(segmentDetail, "visible confirmed and forming segments should expose hover details");
assert.equal(segmentDetail.level, 0);
assert.match(segmentDetail.composition, /输入笔/);
const centerDetail = chart.structures.find((item) => item.type === "递归中枢");
assert.ok(centerDetail?.area, "selected recursive centers should expose area hover details");
assert.equal(centerDetail.ruleVersion, "chan-recursive-v4-preview.2");
const signalDetail = chart.structures.find((item) => item.type === "一买");
assert.ok(signalDetail, "recursive signals should expose rule and time details");
assert.match(signalDetail.composition, /一买 · L1/);
assert.match(signalDetail.composition, /可观察 2026-10-16/);
assert.match(signalDetail.composition, /待满足：后续反向线段确认走势端点/);
assert.match(signalDetail.composition, /失效边界 8.00/);
assert.match(signalDetail.composition, /关联结构 seg-1/);
assert.equal(signalDetail.ruleVersion, "chan-recursive-v4-preview.2");

const levelZeroOnly = window.OptionScopeChan.view(payload, true, false, 30, null, {
  pens: false, penCenters: false, segments: false, segmentLevels: { 0: true }, recursiveCenters: {}, signals: false,
});
assert.equal(levelZeroOnly.daily.chart.segments.length, 1, "L0 override should show its segments while the all-segments default is off");
assert.equal(levelZeroOnly.daily.chart.segments[0].level, 0);

const higherLevelsOnly = window.OptionScopeChan.view(payload, true, false, 30, null, {
  pens: false, penCenters: false, segments: true, segmentLevels: { 0: false }, recursiveCenters: {}, signals: false,
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
  pens: false, penCenters: false, segments: true, segmentLevels: {}, recursiveCenters: {}, signals: false,
});
assert.equal(restoreAllAfterL0Filter.daily.chart.segments.length, 2, "restoring the master switch should clear stale per-level overrides and show every level");

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
] } };
const waitingView = window.OptionScopeChan.view(waitingPayload, true);
assert.match(waitingView.daily.guidance.signalReason, /最长连续已确认线段为 1 条/);
assert.match(waitingView.daily.guidance.signalReason, /等待特征序列破坏和后续原方向笔确认/);

console.log("Chan canvas layer model checks passed");
