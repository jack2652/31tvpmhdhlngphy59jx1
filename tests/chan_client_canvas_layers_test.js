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
// 日期：2026-11-23；执行者：Codex。总开关必须实际恢复每一级及图表，不能只修改无效的布尔值。
assert.equal(window.OptionScopeChan.allSegmentLevelsVisible(layerState, [0, 1]), true, "点击显示全部级别后实际每一级都应开启");
assert.ok([0, 1].every((level) => window.OptionScopeChan.isSegmentLevelVisible(layerState, level)));
const masterShown = window.OptionScopeChan.view(payload, false, false, 30, null, layerState).daily.chart;
assert.equal(masterShown.segments.length, 2, "全部级别显示必须实际生成两级线段");
assert.equal(masterShown.layers.segments, true, "Canvas绘制门应随实际恢复的线段开启");
const masterHidden = window.OptionScopeChan.toggleAllSegmentLevels(layerState, [0, 1]);
assert.equal(window.OptionScopeChan.view(payload, false, false, 30, null, masterHidden).daily.chart.segments.length, 0);
assert.equal(window.OptionScopeChan.allSegmentLevelsVisible(masterHidden, [0, 1]), false);
const oneHidden = window.OptionScopeChan.toggleSegmentLevel(layerState, 0);
assert.equal(window.OptionScopeChan.isSegmentLevelVisible(oneHidden, 0), false);
assert.equal(window.OptionScopeChan.isSegmentLevelVisible(oneHidden, 1), true, "单独隐藏一级应保留总开关提供的其它级别");
const shownAgain = window.OptionScopeChan.toggleAllSegmentLevels(oneHidden, [0, 1]);
assert.equal(window.OptionScopeChan.view(payload, false, false, 30, null, shownAgain).daily.chart.segments.length, 2);
assert.equal(window.OptionScopeChan.isSegmentLevelVisible({}, 0), false, "初始默认仍隐藏线段");
assert.equal(window.OptionScopeChan.allSegmentLevelsVisible(window.OptionScopeChan.toggleAllSegmentLevels({ segments: false }, []), []), true, "无已加载级别时总开关仍可打开以应用于之后的行情");
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
    status: "candidate", date: "2026-10-14", price: 12 }));
const crowdedView = window.OptionScopeChan.view({ ...payload, daily: crowdedFrame }, true, false, 30, null, {
  signals: true, legacySignals: false,
});
const crowdedSignals = crowdedView.daily.chart.signals;
assert.equal(crowdedSignals.length, 6, "拥挤的活动信号必须全部保留且标签仍避让");
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
  [true, false, true, true], "历史失效标签恢复，同日同名同状态的重复文字仅显示一次");
assert.equal(duplicateSignalView.daily.chart.signals.length, 4, "deduplicating labels must keep every signal marker");
assert.equal(duplicateSignalView.daily.recursiveSignals.length, 4, "deduplication should not remove distinct API signal details");

// 日期：2026-11-23；执行者：Codex。同一低点只标一买，独立底背驰及原始详情仍保留。
const overlapBuy = { ...frame().signals_v4[0], source_kind: "pen", level: 0, status: "invalidated", phase: "invalidated" };
const overlapBottom = { ...overlapBuy, signal_id: "bottom-warning", kind: "center_divergence_buy" };
const overlapFrame = { ...frame(), signals_v4: [overlapBuy], structure_warnings_v4: [overlapBottom] };
const overlapPayload = { ...payload, daily: overlapFrame };
const overlapInput = JSON.stringify(overlapPayload);
const overlapView = window.OptionScopeChan.view(overlapPayload, false, false, 30, null, { signals: true }).daily;
assert.deepEqual(Array.from(overlapView.chart.signals.map((s) => s.type)), ["一买"], "同一低点一买优先，不重复画底背驰标记");
assert.deepEqual(Array.from(overlapView.chart.structures.filter((s) => s.signalTarget).map((s) => s.type)), ["一买"], "重复预警不保留悬停目标或占位");
assert.equal(overlapView.chart.signals[0].labelVisible, true);
assert.equal(overlapView.chart.signals[0].invalidated, true, "一买的原状态保持不变");
assert.equal(overlapView.recursiveSignals.length, 2, "完整详情保留两类信号");
assert.equal(JSON.stringify(overlapPayload), overlapInput, "显示去重不修改输入");
const withoutBottom = window.OptionScopeChan.view({ ...overlapPayload, daily: { ...overlapFrame, structure_warnings_v4: [] } }).daily;
assert.equal(overlapView.recursive.activeSignalCount, withoutBottom.recursive.activeSignalCount);
assert.equal(overlapView.recursive.invalidatedSignalCount, withoutBottom.recursive.invalidatedSignalCount);
assert.deepEqual(JSON.parse(JSON.stringify(overlapView.plan)), JSON.parse(JSON.stringify(withoutBottom.plan)));
const bottomOnly = window.OptionScopeChan.view({ ...overlapPayload, daily: { ...overlapFrame, signals_v4: [] } }, false, false, 30, null, { signals: true }).daily;
assert.deepEqual(Array.from(bottomOnly.chart.signals.map((s) => s.type)), ["底背驰预警"], "没有一买时底背驰仍显示");
assert.equal(bottomOnly.chart.signals[0].labelVisible, true);
assert.equal(bottomOnly.chart.totalPlottableSignalCount, 1);
for (const change of [{ kind: "class2_buy" }, { source_kind: "recursive" }, { level: 1 }, { date: "2026-10-14" }, { price: 8.01 }, { price: undefined }, { price: 0 }]) {
  const separate = window.OptionScopeChan.view({ ...overlapPayload, daily: { ...overlapFrame, signals_v4: [{ ...overlapBuy, ...change }] } }, false, false, 30, null, { signals: true }).daily;
  assert.ok(separate.chart.signals.some((s) => s.type === "底背驰预警"), "二买、不同结构或不可绘制一买不得遮掉独立底背驰");
}
for (const status of ["observing", "divergence_confirmed", "confirmed", "invalidated"]) {
  const phaseView = window.OptionScopeChan.view({ ...overlapPayload, daily: { ...overlapFrame, signals_v4: [{ ...overlapBuy, status }] } }, false, false, 30, null, { signals: true }).daily;
  assert.deepEqual(Array.from(phaseView.chart.signals.map((s) => s.type)), ["一买"]);
  assert.equal(phaseView.chart.signals[0].status, status);
}
assert.equal(bottomOnly.chart.totalPlottableSignalCount, overlapView.chart.totalPlottableSignalCount, "一买出现后替换重复预警，图表可绘制数保持一个");
assert.notEqual(bottomOnly.chart, overlapView.chart, "复用同一行情数组增删一买时，缓存须刷新");

// 结构确认时间已到达的一类点须显示确认；观察和失效状态不能被升级。
const classOnePhases = frame();
classOnePhases.signals_v4 = [
  { signal_id: "sell-observing", kind: "class1_sell", side: "sell", status: "observing", phase: "observation", date: "2026-10-13", price: 14, observed_at: "2026-10-14" },
  { signal_id: "sell-confirmed", kind: "class1_sell", side: "sell", status: "divergence_confirmed", phase: "divergence_confirmed", date: "2026-10-13", price: 14, observed_at: "2026-10-14", confirmed_at: "2026-10-15" },
  { signal_id: "sell-invalidated", kind: "class1_sell", side: "sell", status: "invalidated", phase: "invalidated", date: "2026-10-13", price: 14, observed_at: "2026-10-14", confirmed_at: "2026-10-15" },
];
const classOnePhaseView = window.OptionScopeChan.view({ ...payload, daily: classOnePhases }, true, false, 30, null, { signals: true });
const classOneTargets = classOnePhaseView.daily.chart.structures.filter((item) => item.signalTarget);
assert.deepEqual(Array.from(classOneTargets.map((item) => item.confirmed)), [false, true, false]);
assert.equal(classOneTargets[1].signalStatus, "背驰已确认");
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

// 2026-11-23，Codex：独立预警共用图表，但不能进入严格点计数或交易计划。
const warningFrame = frame();
warningFrame.structure_warnings_v4 = [
  { signal_id: "w1", source_kind: "pen", level: 0, kind: "center_divergence_sell", side: "sell", status: "warning_confirmed", phase: "warning_confirmed", date: "2026-10-13", observed_at: "2026-10-14", confirmed_at: "2026-10-15", price: 14, invalidation_price: 14, lower_level_status: "not_required", timeframe_confirmation: { status: "not_required" }, phase_history: [{ phase: "observation", at: "2026-10-14" }, { phase: "warning_confirmed", at: "2026-10-15" }] },
  { signal_id: "w2", source_kind: "pen", level: 0, kind: "rebound_failure_sell", side: "sell", status: "observing", phase: "observation", date: "2026-10-14", observed_at: "2026-10-15", price: 15, invalidation_price: 15, lower_level_status: "not_required", phase_history: [{ phase: "observation", at: "2026-10-15" }] },
  { signal_id: "w3", source_kind: "pen", level: 0, kind: "pullback_hold_buy", side: "buy", status: "invalidated", phase: "invalidated", phase_at: "2026-10-16", date: "2026-10-15", observed_at: "2026-10-15", price: 8, invalidation_price: 8 },
];
const warningPayload = { ...payload, symbol: "ORCL", daily: warningFrame, price_coordinate: { basis: "raw", status: "available" } };
const warningView = window.OptionScopeChan.view(warningPayload, false, false, 30, null, { signals: true });
assert.equal(warningView.daily.chart.signals.length, chart.signals.length + 3);
assert.equal(warningView.daily.chart.signals.find((s) => s.type === "顶背驰预警").confirmed, true);
assert.equal(warningView.daily.chart.signals.find((s) => s.type === "反弹承压").confirmed, false);
assert.equal(warningView.daily.chart.signals.find((s) => s.type === "回踩支撑").invalidated, true);
assert.equal(warningView.daily.chart.structures.find((s) => s.type === "回踩支撑").confirmedAt, "", "失效阶段日期不能作为确认日期");
assert.equal(warningView.daily.recursive.activeSignalCount, allLayers.daily.recursive.activeSignalCount);
assert.equal(warningView.daily.recursive.invalidatedSignalCount, allLayers.daily.recursive.invalidatedSignalCount);
assert.match(warningView.daily.recursive.readiness, /3 条独立结构预警/);
assert.match(warningView.daily.recursiveSignals.find((s) => s.signal_id === "w1").marketAndLevelLabel, /本级独立预警/);
assert.doesNotMatch(warningView.daily.recursiveSignals.find((s) => s.signal_id === "w1").marketAndLevelLabel, /not_required/);
assert.deepEqual(JSON.parse(JSON.stringify(warningView.daily.plan)), JSON.parse(JSON.stringify(window.OptionScopeChan.view({ ...warningPayload, daily: { ...warningFrame, structure_warnings_v4: [] } }).daily.plan)));
const warningOnly = window.OptionScopeChan.view({ ...warningPayload, daily: { ...warningFrame, signals_v4: [] } }, false, false, 30, null, { signals: true });
assert.equal(warningOnly.daily.recursive.emptySignalNotice, "");
assert.equal(warningOnly.daily.recursive.activeSignalCount, 0);
const intradayWarnings = window.OptionScopeChan.intradayView({ ...warningPayload, frame: { ...warningFrame, period: "1h", timeframe: "1h" } }, false, null, false, 30, null, { signals: true });
assert.ok(intradayWarnings.chart.signals.some((s) => s.type === "顶背驰预警"));

// 即使旧位置中枢已不可用，触及预警端点仍须失效；以后确认不得复活。
const noCenterPayload = { ...warningPayload, daily: { ...warningFrame, position: { center: null } } };
const liveBroken = window.OptionScopeChan.withSpot(noCenterPayload, 15, null, { symbol: "ORCL", fetched_at: "2026-10-16T15:00:00Z" });
assert.equal(liveBroken.daily.structure_warnings_v4[1].status, "invalidated");
assert.equal(liveBroken.daily.structure_warnings_v4[1].phase_at, "2026-10-16");
assert.equal(liveBroken.daily.structure_warnings_v4[0].confirmed_at, "2026-10-15");
const laterFixedFrame = { ...warningFrame, structure_warnings_v4: warningFrame.structure_warnings_v4.map((s) => s.signal_id === "w2" ? { ...s, status: "warning_confirmed", confirmed_at: "2026-10-20" } : s) };
const recovered = window.OptionScopeChan.withSpot({ ...warningPayload, daily: laterFixedFrame }, 10, liveBroken);
assert.equal(recovered.daily.structure_warnings_v4[1].status, "invalidated");
assert.equal(recovered.daily.structure_warnings_v4[1].confirmed_at, "");
assert.equal(recovered.daily.structure_warnings_v4[1].phase_at, "2026-10-16");
assert.equal(recovered.daily.structure_warnings_v4[1].phase_history.at(-1).phase, "invalidated");
const newSourceFrame = { ...laterFixedFrame, structure_warnings_v4: laterFixedFrame.structure_warnings_v4.map((s) => ({ ...s, source_kind: "recursive" })) };
assert.equal(window.OptionScopeChan.withSpot({ ...warningPayload, daily: newSourceFrame }, 10, liveBroken).daily.structure_warnings_v4[1].status, "warning_confirmed");
const adjustedWarningPayload = { ...noCenterPayload, price_coordinate: { basis: "split_dividend_adjusted", status: "available", adjustment_factor: .5, factor_date: "2026-10-15" } };
assert.equal(window.OptionScopeChan.withSpot(adjustedWarningPayload, 29).daily.structure_warnings_v4[1].status, "observing");
assert.equal(window.OptionScopeChan.withSpot(adjustedWarningPayload, 30).daily.structure_warnings_v4[1].status, "invalidated");
const buyWarningFrame = { ...warningFrame, structure_warnings_v4: [{ ...warningFrame.structure_warnings_v4[1], kind: "center_divergence_buy", side: "buy", price: 8, invalidation_price: 8 }] };
assert.equal(window.OptionScopeChan.withSpot({ ...warningPayload, daily: buyWarningFrame }, 8).daily.structure_warnings_v4[0].status, "invalidated");
assert.equal(window.OptionScopeChan.withSpot({ ...warningPayload, price_coordinate: { basis: "split_dividend_adjusted", status: "degraded" } }, 30).daily.structure_warnings_v4[1].status, "observing");
// 2026-11-23，Codex：早预警保留真实观察日与持仓保护价，主图恢复为结构信号。
const earlyFrame = { ...frame(), as_of: "2026-10-14", last_close: 12, candles: frame().candles.slice(0, 3), signals: [], signals_v4: [], structure_warnings_v4: [],
  early_risk_warnings_v4: [{ symbol: "QQQ", signal_id: "early1", source_kind: "pen", level: 0, kind: "early_top_sell", side: "sell", status: "observing", phase: "observation", date: "2026-10-14", observed_at: "2026-10-14", pivot_date: "2026-10-13", price: 15, invalidation_price: 15, protection_price: 11, confirmed_at: "", phase_history: [{ phase: "observation", at: "2026-10-14" }] }],
  strokes: [{ chain: 0, confirmed: true, direction: "up", start: { date: "2026-10-12", kind: "bottom", price: 9 }, end: { date: "2026-10-13", kind: "top", price: 15, confirmed_at: "2026-10-14" }, confirmed_at: "2026-10-14" }] };
const earlyPayload = { ...payload, symbol: "QQQ", spot: 13, daily: earlyFrame, weekly: null, price_coordinate: { basis: "raw", status: "unadjusted" } };
const earlySafe = window.OptionScopeChan.withSpot(earlyPayload, 13);
const earlyView = window.OptionScopeChan.view(earlySafe, false, false, 30, null, { signals: true }).daily;
assert.match(earlyView.plan.cards[1].action, /早预警/);
assert.equal(earlyView.chart.signals.length, 0, "主图不叠加早预警");
assert.ok(!earlyView.chart.structures.some((structure) => structure.type === "顶部早预警"));
assert.equal(earlyView.recursiveSignals.length, 1, "早预警仍保留在完整详情列表");
assert.equal(earlySafe.daily.early_risk_warnings_v4[0].observed_at, "2026-10-14");
assert.equal(earlySafe.daily.early_risk_warnings_v4[0].pivot_date, "2026-10-13");
assert.equal(earlySafe.daily.early_risk_warnings_v4[0].protection_price, 11);
assert.equal(earlyView.recursive.activeSignalCount, 0);
// 日期：2026-11-23；执行者：Codex。结构预警完整显示，早预警仅保留详情和持仓计划。
const structureFrame = { ...earlyFrame, structure_warnings_v4: [{ ...earlyFrame.early_risk_warnings_v4[0],
  signal_id: "later-warning", kind: "center_divergence_sell", date: "2026-10-13", pivot_date: undefined,
  observed_at: "2026-10-15", confirmed_at: "2026-10-16", status: "warning_confirmed", phase: "warning_confirmed", protection_price: undefined }] };
const structurePayload = { ...earlyPayload, daily: structureFrame };
const structureInput = JSON.stringify(structurePayload);
const structureView = window.OptionScopeChan.view(structurePayload, false, false, 30, null, { signals: true }).daily;
assert.equal(structureView.chart.signals.length, 1);
const primaryWarning = structureView.chart.signals[0];
assert.equal(primaryWarning.type, "顶背驰预警");
assert.equal(primaryWarning.labelVisible, true);
assert.equal(primaryWarning.start, "2026-10-13");
assert.equal(primaryWarning.formedAt, "2026-10-15");
assert.equal(primaryWarning.confirmedAt, "2026-10-16");
assert.equal(primaryWarning.confirmed, true);
assert.ok(!structureView.chart.structures.some((s) => /早预警/.test(s.type)));
assert.equal(structureView.recursiveSignals.length, 2, "完整详情列表保留结构预警和早预警");
assert.deepEqual(JSON.parse(JSON.stringify(structureView.plan)), JSON.parse(JSON.stringify(earlyView.plan)));
assert.equal(JSON.stringify(structurePayload), structureInput, "显示恢复不得修改原始信号");
const changedStructureFrame = { ...structureFrame, structure_warnings_v4: [{ ...structureFrame.structure_warnings_v4[0], status: "invalidated", phase: "invalidated" }] };
const invalidStructureView = window.OptionScopeChan.view({ ...structurePayload, daily: changedStructureFrame }, false, false, 30, null, { signals: true }).daily;
assert.equal(changedStructureFrame.candles, structureFrame.candles, "复用同一行情数组验证状态更新缓存");
assert.equal(invalidStructureView.chart.signals[0].invalidated, true);
assert.equal(invalidStructureView.chart.signals[0].labelVisible, true, "历史失效结构预警须保留完整标签");
assert.equal(invalidStructureView.chart.signals[0].confirmed, false);
const strictTogether = window.OptionScopeChan.view({ ...structurePayload, daily: { ...structureFrame,
  signals_v4: [{ ...structureFrame.structure_warnings_v4[0], kind: "class1_sell", signal_id: "strict-one" }] } }, false, false, 30, null, { signals: true }).daily;
assert.deepEqual(Array.from(strictTogether.chart.signals.map((s) => s.type)), ["一卖", "顶背驰预警"], "严格信号和结构预警分别显示完整标签");
assert.equal(strictTogether.recursive.activeSignalCount, 1);
assert.deepEqual(JSON.parse(JSON.stringify(earlyView.plan.cards[0])), JSON.parse(JSON.stringify(window.OptionScopeChan.view({ ...earlyPayload, daily: { ...earlyFrame, early_risk_warnings_v4: [] } }).daily.plan.cards[0])), "提前风险不能升级或改变新仓严格入场规则");
const earlyTouch = window.OptionScopeChan.withSpot(earlyPayload, 10, earlySafe, { symbol: "QQQ", fetched_at: "2026-10-15T15:00:00Z" });
assert.equal(earlyTouch.daily.early_risk.touched, true);
assert.equal(earlyTouch.daily.early_risk.touched_at, "2026-10-15");
assert.equal(earlyTouch.daily.early_risk_warnings_v4[0].status, "observing");
assert.match(window.OptionScopeChan.view(earlyTouch).daily.plan.cards[1].action, /保护价失守/);
const earlyRecovery = window.OptionScopeChan.withSpot(earlyPayload, 13, earlyTouch);
assert.equal(earlyRecovery.daily.early_risk.touched, true);
assert.match(window.OptionScopeChan.view(earlyRecovery).daily.plan.cards[1].action, /曾失守/);
const earlyInvalid = window.OptionScopeChan.withSpot(earlyPayload, 15, earlyTouch, { symbol: "QQQ", fetched_at: "2026-10-16T15:00:00Z" });
assert.equal(earlyInvalid.daily.early_risk_warnings_v4[0].status, "invalidated");
assert.equal(earlyInvalid.daily.early_risk, null);
assert.equal(window.OptionScopeChan.withSpot(earlyPayload, 13, earlyInvalid).daily.early_risk_warnings_v4[0].status, "invalidated");
const newEarly = { ...earlyPayload, daily: { ...earlyFrame, early_risk_warnings_v4: earlyFrame.early_risk_warnings_v4.map((s) => ({ ...s, signal_id: "early2" })) } };
assert.equal(window.OptionScopeChan.withSpot(newEarly, 13, earlyTouch).daily.early_risk.touched, false, "新的风险事件不继承旧触及状态");
const earlyIntradayPayload = { ...earlyPayload, daily: undefined, period: "1h", frame: { ...earlyFrame, period: "1h", timeframe: "1h" } };
const earlyIntradayTouch = window.OptionScopeChan.withSpot(earlyIntradayPayload, 10, null, { symbol: "QQQ", fetched_at: "2026-10-15T15:00:00Z" });
assert.equal(earlyIntradayTouch.frame.early_risk.touched, true);
assert.equal(window.OptionScopeChan.withSpot(earlyIntradayPayload, 13, earlyIntradayTouch).frame.early_risk.touched, true);
assert.match(window.OptionScopeChan.intradayView(earlyIntradayTouch).plan.cards[1].action, /保护价失守/);
const triggeredPayload = { ...earlyPayload, daily: { ...earlyFrame, early_risk_warnings_v4: earlyFrame.early_risk_warnings_v4.map((s) => ({ ...s, status: "risk_triggered", phase: "risk_triggered", phase_at: "2026-10-15" })) } };
const triggeredView = window.OptionScopeChan.view(triggeredPayload, false, false, 30, null, { signals: true }).daily;
assert.ok(!triggeredView.chart.structures.some((s) => s.type === "顶部早预警"));
assert.equal(triggeredView.recursive.activeSignalCount, 0, "早风险触发不能确认严格信号");
console.log("Chan canvas layer model checks passed");
