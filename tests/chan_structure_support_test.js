const assert = require("node:assert/strict");
const fs = require("node:fs");
const vm = require("node:vm");
const window = {};
for (const file of ["chan-plan.js", "chan-client.js"]) vm.runInNewContext(fs.readFileSync("web/static/common/js/" + file, "utf8"), { window });
const plain = (value) => JSON.parse(JSON.stringify(value));
const date = (index) => new Date(Date.UTC(2026, 0, index + 1)).toISOString().slice(0, 10);
function candles(count = 70, troughs = [[20, 100], [35, 100.3], [50, 99.9]]) {
  const bars = Array.from({ length: count }, (_, index) => ({ date: date(index), open: 106, close: 106, high: 107, low: 105, chain_id: 0 }));
  for (const [index, price] of troughs) {
    if (index >= bars.length) continue;
    bars[index] = { ...bars[index], open: price + 3, close: price + 2, high: price + 4, low: price };
    bars[index + 1] = { ...bars[index + 1], open: price + 4, close: price + 5, high: price + 6, low: price + 3 };
  }
  return bars;
}
function stroke(price = 100, start = 20, confirmed = 23, options = {}) {
  return { confirmed: true, confirmed_at: date(confirmed), chain: 0,
    start: { kind: "bottom", date: date(start), price, chain_id: 0 },
    end: { kind: "top", date: date(confirmed - 1), price: 110, chain_id: 0 }, ...options };
}
function frame(bars = candles(), options = {}) {
  return { period: "1d", label: "日线", as_of: bars[bars.length - 1]?.date, candles: bars, strokes: [], segments: [], centers: [], recursive_centers: [], recursive_levels: [], signals: [], position: {}, ...options };
}
function chart(source, layers = {}, viewport = null, spot = 106, mobile = false) {
  return window.OptionScopeChan.view({ symbol: "TEST", spot, daily: source, weekly: source, summary: {} }, false, mobile, 30,
    viewport ? { daily: viewport } : null, { pens: false, penCenters: false, signals: false, supportResistance: true, ...layers }).daily.chart;
}
const source = frame(), baseline = chart(source), zone = baseline.structureSupportLevels[0];
assert.equal(zone.price, 100, "independent rebound lows produce a median support reference without requiring Chan endpoints");
assert.equal(zone.zoneLow, 99.9); assert.equal(zone.zoneHigh, 100.3);
assert.equal(zone.testCount, 3); assert.equal(zone.lastTestDate, date(50));
assert.equal(zone.confirmedAt, date(52)); assert.ok(zone.reactionAtr > 0.75);
assert.equal(zone.statusLabel, "回踩确认支撑");
assert.deepEqual(plain(source), plain(frame()), "support calculation never mutates upstream OHLC");
assert.deepEqual(plain(chart(source, {}, { visibleBars: 12, endOffset: 0 }, 200).structureSupportLevels), plain(baseline.structureSupportLevels), "live spot and zoom do not change closed-candle evidence");

const singleBars = candles(70, [[20, 100]]);
assert.equal(chart(frame(singleBars)).structureSupportLevels.length, 0, "one unsupported rebound is insufficient");
const structured = frame(singleBars, { strokes: [stroke()] });
const confirmed = chart(structured).structureSupportLevels[0];
assert.equal(confirmed.testCount, 1); assert.equal(confirmed.confirmedAt, date(23));
assert.equal(confirmed.statusLabel, "结构反弹确认");
assert.match(confirmed.sources, /已确认笔低点/);
for (const invalidStroke of [stroke(100, 20, 90), stroke(100, 20, 23, { confirmed: false }), stroke(100, 20, 23, { confirmed_at: "" }),
  stroke(100, 20, 23, { end: { kind: "top", date: date(90), price: 110 } }), stroke(100, 20, 23, { first_observed_at: date(90) })]) {
  assert.equal(chart(frame(singleBars, { strokes: [invalidStroke] })).structureSupportLevels.length, 0, "future, forming or undated structures cannot supply evidence");
}
const reference = chart(frame(candles(70, []), { strokes: [stroke()] })).structureSupportLevels[0];
assert.equal(reference.kind, "historical", "a confirmed structural low is a long-term reference, independently of recent rebounds");
assert.equal(reference.statusLabel, "历史结构支撑"); assert.equal(reference.testCount, 0); assert.equal(reference.reactionAtr, null);

const weak = candles(70, [[20, 100]]);
for (let index = 21; index < weak.length; index += 1) weak[index] = { ...weak[index], open: 101, close: 101, high: 102, low: 100.8 };
assert.equal(chart(frame(weak, { strokes: [stroke()] })).structureSupportLevels[0].kind, "historical", "a weak closing bounce is not relabeled as rebound-confirmed support");
assert.equal(chart(frame(candles(70, [[20, 100], [24, 100.2]]))).structureSupportLevels.length, 0, "one rebound episode cannot count as two independent tests");
assert.equal(chart(frame(candles(70, []))).structureSupportLevels.length, 0, "flat candles cannot create support");

const early = frame(candles().slice(0, 36));
assert.equal(chart(early).structureSupportLevels.length, 0, "the second low needs both right-hand bars before confirmation");
assert.deepEqual(plain(chart(source, {}, { visibleBars: 36, endOffset: 34 }).structureSupportLevels), plain(chart(early).structureSupportLevels), "historical panning equals genuinely truncated data");
const forming = frame(candles().slice(0, 38)); forming.candles[37].is_forming = true;
assert.equal(chart(forming).structureSupportLevels.length, 0);
const beforeStructure = frame(singleBars.slice(0, 23), { strokes: [stroke()] });
assert.equal(chart(beforeStructure).structureSupportLevels.length, 0, "actual rebound does not anticipate a later structural confirmation");

const center = { lower: 90, upper: 100, start: date(15), end: date(18), confirmed_at: date(18), chain: 0 };
const noRetest = chart(frame(candles(70, []), { centers: [center] })).structureSupportLevels;
assert.ok(noRetest.some((level) => level.price === 90 && level.kind === "historical"));
assert.ok(!noRetest.some((level) => level.sources.includes("上沿回踩")), "standing above a center is not a successful upper-boundary retest");
const retest = chart(frame(singleBars, { centers: [center] })).structureSupportLevels[0];
assert.equal(retest.price, 100); assert.equal(retest.pointDate, date(20));
assert.match(retest.sources, /上沿回踩/);
const beforeBreakout = singleBars.map((bar, index) => index < 20 ? { ...bar, open: 94, close: 94, high: 95, low: 93 } : bar);
assert.ok(!chart(frame(beforeBreakout, { centers: [center] })).structureSupportLevels.some((level) => level.sources.includes("上沿回踩")), "the breakout low itself is not a later upper-boundary retest");
assert.equal(chart(frame(singleBars, { centers: [{ ...center, forming: true }] })).structureSupportLevels.length, 0);
assert.equal(chart(frame(singleBars, { centers: [{ ...center, end: date(90) }] })).structureSupportLevels.length, 0, "a currently extended center cannot supply future boundaries to a historical cutoff");

function breakBars(bars, indices = [55, 56], close = 96) {
  for (const index of indices) bars[index] = { ...bars[index], open: close + 1, close, high: close + 2, low: close - 1 };
  return bars;
}
const brokenBars = breakBars(candles(70, [[20, 100], [35, 100.3]]));
for (let index = 57; index < brokenBars.length; index += 1) brokenBars[index] = { ...brokenBars[index], open: 97, close: 97, high: 98, low: 96 };
const broken = chart(frame(brokenBars)).structureSupportLevels.find((level) => level.price === 100.15);
assert.equal(broken.status, "broken"); assert.equal(broken.brokenAt, date(56)); assert.equal(broken.testCount, 2);
assert.equal(chart(frame(breakBars(candles(70, [[20, 100], [35, 100.3]]), [55]))).structureSupportLevels[0].status, "active", "one closing breach does not invalidate support");
const wick = candles(); wick[55].low = 80;
assert.equal(chart(frame(wick)).structureSupportLevels.find((level) => level.price === 100).status, "active", "a wick alone does not invalidate support");
const renewedBars = breakBars(candles(110, [[20, 100], [35, 100.3], [75, 100.1], [95, 100.2]]));
const renewed = chart(frame(renewedBars, { strokes: [stroke()] })).structureSupportLevels.find((level) => level.price > 99);
assert.equal(renewed.testCount, 2); assert.equal(renewed.reestablished, true);
assert.ok(renewed.tests.every((test) => test.date > date(56)), "old tests cannot revive after a break");
const oneRenewed = breakBars(candles(100, [[20, 100], [35, 100.3], [75, 100.1]]));
assert.ok(!chart(frame(oneRenewed, { strokes: [stroke()] })).structureSupportLevels.some((level) => level.price > 99 && level.status === "active"), "even structural evidence requires two fresh rebounds after invalidation");

const highVolatility = candles(80, [[20, 100], [35, 100.3]]);
for (let index = 45; index < 60; index += 1) highVolatility[index].high = 125;
breakBars(highVolatility, [55, 56], 99.5);
assert.ok(chart(frame(highVolatility)).structureSupportLevels.some((level) => level.price > 99 && level.status === "active"), "small breaches inside high-volatility ATR tolerance do not become fixed-percent false breaks");
const aged = chart(frame(candles(130, [[20, 100], [35, 100.3]]))).structureSupportLevels[0];
const fresh = chart(frame(candles(130, [[100, 100], [115, 100.3]]))).structureSupportLevels[0];
assert.ok(fresh.score > aged.score, "recent evidence ranks above otherwise identical stale evidence");
assert.equal(chart(frame(candles(230, [[20, 100], [35, 100.3]]))).structureSupportLevels.length, 0, "untested old supports expire");
const lateStructure = chart(frame(candles(230, [[20, 100]]), { strokes: [stroke(100, 20, 220)] })).structureSupportLevels[0];
assert.equal(lateStructure.kind, "historical", "late structural confirmation does not refresh or fabricate recent rebound evidence");
assert.equal(lateStructure.testCount, 0);

const disconnected = candles(); for (let index = 42; index < disconnected.length; index += 1) disconnected[index].chain_id = 1;
assert.equal(chart(frame(disconnected)).structureSupportLevels.length, 0);
const invalid = candles(); invalid[42].high = NaN;
assert.equal(chart(frame(invalid)).structureSupportLevels.length, 0);
const interruptedEvidence = candles(80, [[20, 100], [50, 100]]); interruptedEvidence[30].high = NaN;
assert.equal(chart(frame(interruptedEvidence, { strokes: [stroke()] })).structureSupportLevels.length, 0, "old structural evidence cannot leak across an invalid OHLC gap even when the data chain id is reused");
const recursive = frame(singleBars, { strokes_v4: [stroke(), stroke(100, 20, 23, { chain: 4, start: { kind: "bottom", date: date(20), price: 100, chain_id: 4 } })],
  structure_chains_v4: [{ chain_id: 0, data_chain_id: 0, start: date(0) }, { chain_id: 4, data_chain_id: 0, start: date(15) }] });
assert.equal(chart(recursive).structureSupportLevels[0].testCount, 1);
assert.equal(chart(frame(singleBars, { strokes_v4: [] })).structureSupportLevels.length, 0, "an empty recursive stream cannot borrow legacy structures");
assert.equal(chart(frame(singleBars, { strokes_v4: [], segments: [{ level: 0, confirmed: true, forming: false, confirmed_at: date(23), chain_id: 0, start: date(20), start_kind: "bottom", start_price: 100 }] })).structureSupportLevels[0].sources, "已确认线段低点");

const separate = chart(frame(candles(100, [[20, 90], [35, 90.3], [60, 100], [75, 100.3]])));
assert.equal(separate.structureSupportLevels.length, 2, "distant prices do not chain into one support band");
const crowded = chart(frame(candles(160, [[20, 70], [35, 70.3], [55, 80], [70, 80.3], [90, 90], [105, 90.3], [125, 100], [140, 100.3]])));
assert.equal(crowded.structureSupportLevels.filter((level) => level.status === "active").length, 3, "only the strongest three active zones are displayed");
const mergedEvidence = chart(frame(singleBars, { strokes: [stroke()], segments: [{ level: 0, confirmed: true, confirmed_at: date(25), chain_id: 0, start: date(20), start_kind: "bottom", start_price: 100 }] }), {}, { visibleBars: 70, endOffset: 0 });
assert.equal(mergedEvidence.structureSupports.length, 1); assert.match(mergedEvidence.structureSupports[0].sources, /笔低点.*线段/);
assert.ok(!mergedEvidence.keyLevels.some((level) => level.tone === "support"), "ordinary lows cannot bypass support verification or duplicate an existing zone");
assert.equal(chart(source, { supportResistance: false }).structureSupportLevels.length, 0);
assert.equal(chart(source).structureSupportLevels.length, 1, "toggle restoration returns verified zones");
assert.equal(chart(frame([])), null);

const adjusted = candles().map((bar) => ({ ...bar, open: bar.open / 2, close: bar.close / 2, high: bar.high / 2, low: bar.low / 2 }));
const adjustedZone = chart(frame(adjusted), {}, null, 53).structureSupportLevels[0];
assert.equal(adjustedZone.price, zone.price / 2); assert.equal(adjustedZone.testCount, zone.testCount);
const intraday = candles().map((bar, index) => ({ ...bar, date: new Date(Date.UTC(2026, 0, 1, 9, 30 + index * 15)).toISOString().slice(0, 16).replace("T", " ") }));
const intradayView = window.OptionScopeChan.intradayView({ period: "15m", symbol: "TEST", spot: 106, frame: frame(intraday, { period: "15m" }) }, false);
assert.equal(intradayView.chart.structureSupportLevels[0].testCount, 3);
const mobileChart = chart(source, {}, null, 106, true);
assert.ok(mobileChart.structureSupports.every((level) => level.labelY >= mobileChart.plotTop && level.labelY <= mobileChart.priceBottom));

// 历史结构保留与近期反弹使用不同契约，完整历史失守不能被窗口裁剪遗忘。
const longSource = frame(candles(500, [[20, 100]]), { strokes: [stroke()] });
const longReference = chart(longSource).structureSupportLevels[0];
assert.equal(longReference.price, 100); assert.equal(longReference.status, "active");
assert.equal(longReference.kind, "historical"); assert.equal(longReference.testCount, 0);
assert.ok(longReference.ageBars > 252, "historical structure survives beyond the recent reaction window");
assert.ok(longReference.lineAlpha >= 0.48 && longReference.lineAlpha < zone.lineAlpha, "long-term references remain readable with lower line emphasis");
assert.deepEqual(plain(chart(longSource, {}, { visibleBars: 20, endOffset: 0 }, 250).structureSupportLevels), plain(chart(longSource).structureSupportLevels));
const cutoffLong = frame(longSource.candles.slice(0, 200), { strokes: [stroke()] });
assert.deepEqual(plain(chart(longSource, {}, { visibleBars: 80, endOffset: 300 }).structureSupportLevels), plain(chart(cutoffLong).structureSupportLevels), "historical structure and ATR only use the visible cutoff");

const earlyBreak = frame(breakBars(candles(500, [[20, 100]]), [80, 81]), { strokes: [stroke()] });
assert.ok(!chart(earlyBreak).structureSupportLevels.some((level) => level.status === "active"), "a breach before the latest 266 bars cannot resurrect an old low");
const lateAfterBreak = frame(earlyBreak.candles, { strokes: [stroke(100, 20, 480)] });
assert.ok(!chart(lateAfterBreak).structureSupportLevels.some((level) => level.status === "active"), "a delayed confirmation of the same old source cannot undo an earlier breach");
const newLowAfterBreak = frame(earlyBreak.candles, { strokes: [stroke(), stroke(100, 450, 453)] });
assert.ok(chart(newLowAfterBreak).structureSupportLevels.some((level) => level.price === 100 && level.status === "active"), "a new confirmed structure source after the breach can establish a new reference");
const recentlyLost = frame(breakBars(candles(500, [[20, 100]]), [470, 471]), { strokes: [stroke()] });
const lostReference = chart(recentlyLost).structureSupportLevels[0];
assert.equal(lostReference.status, "broken"); assert.equal(lostReference.brokenAt, date(471));
assert.ok(recentlyLost.candles[499].close > lostReference.price, "later closes back above the level do not automatically restore it");
const oneClose = frame(breakBars(candles(500, [[20, 100]]), [499], 96), { strokes: [stroke()] });
assert.equal(chart(oneClose).structureSupportLevels[0].status, "pending-break", "首次完成收盘有效跌破应保留长期参考并提示待确认");
assert.equal(chart(oneClose).structureSupportLevels[0].statusLabel, "跌破待确认");
assert.equal(chart(oneClose).structureSupportLevels[0].pendingBreakAt, date(499));
assert.equal(chart(oneClose).structureSupports[0].status, "pending-break", "布局不能把待确认重置为支撑");
assert.equal(chart(oneClose).structureSupports[0].label, "跌破待确认 100.00");
assert.equal(chart(oneClose).supportStatus, "", "待确认并不表示当前没有未失守参考");

// 日期：2026-11-23；执行者：Codex。待确认使用完成收盘及合并区域下沿，保留两根失守门槛。
const pendingReactionBars = breakBars(candles(70, [[20, 100], [35, 100.3]]), [69]);
const pendingReaction = chart(frame(pendingReactionBars)).structureSupportLevels[0];
assert.ok(pendingReaction, "首次跌破不能因支撑价高于现价被隐藏");
assert.equal(pendingReaction.kind, "reaction"); assert.equal(pendingReaction.status, "pending-break");
assert.equal(pendingReaction.pendingBreakAt, date(69)); assert.equal(pendingReaction.testCount, 2);
assert.equal(pendingReaction.lastCloseLabel, "96.00");
assert.ok(Number(pendingReaction.breakThresholdLabel) < pendingReaction.zoneLow);
const pendingSource = frame(pendingReactionBars);
assert.deepEqual(plain(chart(pendingSource, {}, { visibleBars: 12, endOffset: 0 }, 200).structureSupportLevels),
  plain(chart(pendingSource).structureSupportLevels), "现价和缩放不能改变待确认收盘证据");
assert.deepEqual(plain(chart(pendingSource, {}, { visibleBars: 30, endOffset: 1 }).structureSupportLevels),
  plain(chart(frame(pendingReactionBars.slice(0, -1))).structureSupportLevels), "历史视口不能泄漏下一根跌破");
const pendingForming = frame(pendingReactionBars.map((bar, index) => index === 69 ? { ...bar, is_forming: true } : bar));
assert.equal(chart(pendingForming).structureSupportLevels[0].status, "active", "未完成K线跌破不能触发待确认");
const recoveredBars = [...pendingReactionBars, ...candles(71, []).slice(70)];
const recovered = chart(frame(recoveredBars)).structureSupportLevels[0];
assert.equal(recovered.status, "active"); assert.equal(recovered.statusLabel, "回踩确认支撑");
assert.ok(!recovered.pendingBreakAt, "收回后清除首次跌破提示");
const confirmedBreakBars = breakBars([...pendingReactionBars, ...candles(71, []).slice(70)], [70]);
assert.equal(chart(frame(confirmedBreakBars)).structureSupportLevels[0].status, "broken");
assert.equal(chart(frame(confirmedBreakBars)).structureSupportLevels[0].brokenAt, date(70));
const mergedPendingBars = breakBars(candles(70, []), [69], 99.8);
const protectedLower = chart(frame(mergedPendingBars, { strokes: [stroke(99.6), stroke(100.3)] })).structureSupportLevels;
assert.equal(protectedLower.length, 1); assert.equal(protectedLower[0].zoneLow, 99.6);
assert.equal(protectedLower[0].status, "active", "只跌破合并区上方参考不能把仍有效下沿标为待确认");
const historicalRecovery = frame([...oneClose.candles, ...candles(501, []).slice(500)], { strokes: [stroke()] });
assert.equal(chart(historicalRecovery).structureSupportLevels[0].status, "active");
const historicalSecond = frame(breakBars([...oneClose.candles, ...candles(501, []).slice(500)], [500]), { strokes: [stroke()] });
assert.equal(chart(historicalSecond).structureSupportLevels[0].status, "broken");
assert.equal(chart(historicalSecond).structureSupportLevels[0].brokenAt, date(500));
const toleratedFinalBars = candles(70, [[20, 100], [35, 100.3]]);
for (let index = 45; index < toleratedFinalBars.length; index += 1) toleratedFinalBars[index].high = 125;
breakBars(toleratedFinalBars, [69], 99.5);
assert.equal(chart(frame(toleratedFinalBars)).structureSupportLevels[0].status, "active", "完成收盘轻微低于参考但仍在ATR容差内时不能误标待确认");
const earlySingle = frame(breakBars(candles(500, [[20, 100]]), [80]), { strokes: [stroke()] });
assert.equal(chart(earlySingle).structureSupportLevels[0].status, "active");
const noisyLongBars = candles(500, [[20, 100]]);
for (let index = 60; index <= 90; index += 1) noisyLongBars[index].high = 125;
breakBars(noisyLongBars, [80, 81], 99.5);
assert.equal(chart(frame(noisyLongBars, { strokes: [stroke()] })).structureSupportLevels[0].status, "active", "historical lifecycle uses the ATR at the time of the possible breach");

const longCenterSource = frame(candles(500, [[20, 100]]), { centers: [center] });
const upperReference = chart(longCenterSource).structureSupportLevels.find((level) => level.sources.includes("上沿回踩"));
assert.equal(upperReference.kind, "historical"); assert.equal(upperReference.testCount, 1); assert.equal(upperReference.lastTestDate, date(20));
assert.ok(!chart(frame(candles(500, []), { centers: [center] })).structureSupportLevels.some((level) => level.sources.includes("上沿回踩")), "a long-ago breakout still needs an actual later retest");
const earlyLostUpper = breakBars(candles(500, [[20, 100]]), [80, 81]);
assert.ok(!chart(frame(earlyLostUpper, { centers: [center] })).structureSupportLevels.some((level) => level.price === 100 && level.status === "active"));

const unified = chart(frame(candles(), { strokes: [stroke(100.4)] }), {}, { visibleBars: 70, endOffset: 0 });
assert.equal(unified.structureSupportLevels.length, 1); assert.equal(unified.structureSupports.length, 1);
assert.equal(unified.structureSupportLevels[0].kind, "reaction"); assert.equal(unified.structureSupportLevels[0].testCount, 3);
assert.equal(unified.structureSupportLevels[0].zoneHigh, 100.4); assert.equal(unified.structureSupportLevels[0].zoneLow, 99.9);
assert.match(unified.structureSupportLevels[0].sources, /已确认笔低点/);
assert.ok(unified.structureSupportLevels[0].hasHistoricalStructure);
const crowdedReferences = chart(frame(candles(500, []), { strokes: [60, 70, 80, 90, 100].map((price) => stroke(price)) })).structureSupportLevels;
assert.deepEqual(plain(crowdedReferences.map((level) => level.price)), [100, 90, 80, 70], "the nearest four historical structures are retained without crowding the chart");
const boundedReferences = chart(frame(candles(500, []), { strokes: [100, 100.7, 101.4, 102.1].map((price) => stroke(price)) })).structureSupportLevels;
assert.equal(boundedReferences.length, 2, "adjacent reference prices cannot chain into an oversized band");
assert.ok(boundedReferences.every((level) => level.zoneHigh - level.zoneLow <= 1), "the two-ATR-tolerance diameter bound holds across each merged band");
const sharedBoundaryBars = candles(70, []);
for (const index of [50, 51]) sharedBoundaryBars[index] = { ...sharedBoundaryBars[index], open: 99.8, close: 99.6, high: 100.1, low: 99.4 };
const sharedBoundary = chart(frame(sharedBoundaryBars, { strokes: [stroke(99.9), stroke(100.3)] })).structureSupportLevels;
assert.ok(sharedBoundary.some((level) => level.zoneLow <= 99.9 && level.zoneHigh >= 99.9 && level.status === "active"), "breaching a nearby upper reference cannot invalidate the unbroken lower boundary of the same zone");

const longGap = candles(500, [[20, 100]]); longGap[80].high = NaN;
const interruptedLong = frame(longGap, { strokes_v4: [stroke()], structure_chains_v4: [{ chain_id: 0, data_chain_id: 0, start: date(0) }] });
assert.equal(chart(interruptedLong).structureSupportLevels.length, 0, "a declared structural chain cannot bypass the newer invalid-OHLC boundary");
const switchedLong = candles(500, [[20, 100]]); switchedLong.slice(80).forEach((bar) => { bar.chain_id = 1; });
assert.equal(chart(frame(switchedLong, { strokes: [stroke()] })).structureSupportLevels.length, 0);
const futurePoint = stroke(); futurePoint.start.first_observed_at = date(600);
assert.equal(chart(frame(candles(500, []), { strokes: [futurePoint] })).structureSupportLevels.length, 0, "future point observations do not leak into historical references");
assert.ok(!chart(frame(candles(500, []), { strokes_v4: [], strokes: [stroke()] })).structureSupportLevels.length);
console.log("Support zones: reactions, retained historical structures, full-history ATR breaks, retests, renewal, aging, cutoffs, deduplication and coordinates passed");
