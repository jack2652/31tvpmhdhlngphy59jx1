const assert = require("node:assert/strict");
const fs = require("node:fs");
const vm = require("node:vm");
const window = {};
for (const file of ["chan-plan.js", "chan-client.js"]) vm.runInNewContext(fs.readFileSync("web/static/common/js/" + file, "utf8"), { window });
const plain = (value) => JSON.parse(JSON.stringify(value));
const date = (index) => new Date(Date.UTC(2026, 0, index + 1)).toISOString().slice(0, 10);
function candles(count = 70, peaks = [[20, 100], [35, 100.3], [50, 99.9]]) {
  const bars = Array.from({ length: count }, (_, index) => ({ date: date(index), open: 94, close: 94, high: 95, low: 93, chain_id: 0 }));
  for (const [index, price] of peaks) {
    if (index >= bars.length) continue;
    bars[index] = { ...bars[index], open: 97, close: 98, high: price, low: 96 };
    bars[index + 1] = { ...bars[index + 1], open: 96, close: 95, high: 97, low: 94 };
  }
  return bars;
}
function frame(bars, options = {}) {
  return { period: "1d", label: "日线", as_of: bars[bars.length - 1]?.date, candles: bars, strokes: [], segments: [], centers: [], recursive_centers: [], recursive_levels: [], signals: [], position: {}, ...options };
}
function chart(source, viewport = null, layers = {}, spot = 94) {
  return window.OptionScopeChan.view({ symbol: "TEST", spot, daily: source, weekly: source, summary: {} }, false, false, 30,
    viewport ? { daily: viewport } : null, { pens: false, penCenters: false, signals: false, supportResistance: true, ...layers }).daily.chart;
}
const source = frame(candles());
const baseline = chart(source);
assert.equal(baseline.resistanceLevels.length, 1, "repeated nearby rejected peaks produce one pressure zone without Chan endpoints");
const zone = baseline.resistanceLevels[0];
assert.equal(zone.testCount, 3);
assert.equal(zone.price, 100, "the displayed reference is the median observed price rather than a hardcoded round number");
assert.equal(zone.zoneLow, 99.9);
assert.equal(zone.zoneHigh, 100.3);
assert.equal(zone.confirmedAt, date(52), "both right-hand bars and the close rejection are available before confirmation");
assert.equal(plain(baseline.keyLevels).filter((level) => level.testCount).length, 1);
assert.deepEqual(plain(source), plain(frame(candles())), "pressure calculation cannot mutate source candles");
assert.deepEqual(plain(chart(source, { visibleBars: 12, endOffset: 0 }, {}, 200).resistanceLevels), plain(baseline.resistanceLevels), "zoom and live spot do not change closed-candle pressure zones");
assert.equal(chart(source, null, { supportResistance: false }).resistanceLevels.length, 0);
assert.ok(!chart(source, null, { supportResistance: false }).keyLevels.some((level) => level.tone === "resistance"));

const early = frame(candles().slice(0, 36));
assert.equal(chart(early).resistanceLevels.length, 0, "an unconfirmed second peak cannot use future bars");
assert.deepEqual(plain(chart(source, { visibleBars: 36, endOffset: 34 }).resistanceLevels), plain(chart(early).resistanceLevels), "panning to an earlier cutoff matches a truly truncated dataset");
const forming = frame(candles().slice(0, 38));
forming.candles[37].is_forming = true;
assert.equal(chart(forming).resistanceLevels.length, 0, "a forming bar cannot confirm the second rejection");

assert.equal(chart(frame(candles(70, [[20, 100]]))).resistanceLevels.length, 0, "one rejection is insufficient");
assert.equal(chart(frame(candles(70, []))).resistanceLevels.length, 0, "flat candles cannot become artificial repeated touches");
const nearby = candles(70, [[20, 100], [24, 100.3]]);
assert.equal(chart(frame(nearby)).resistanceLevels.length, 0, "nearby peaks within one rebound count as one test");
const separated = chart(frame(candles(90, [[20, 100], [35, 100.3], [55, 108], [70, 108.3]])));
assert.equal(separated.resistanceLevels.length, 2, "well-separated levels do not chain into one broad band");

const brokenBars = candles(80, [[20, 100], [35, 100.3]]);
for (const index of [55, 56]) brokenBars[index] = { ...brokenBars[index], open: 101, close: 102, high: 103, low: 101 };
assert.equal(chart(frame(brokenBars)).resistanceLevels.length, 0, "two completed closes above the pressure invalidate old tests even after a later decline");
const wickBars = candles(80, [[20, 100], [35, 100.3]]);
wickBars[55].high = 105;
assert.equal(chart(frame(wickBars)).resistanceLevels[0].testCount, 2, "an intrabar wick alone does not break resistance");
const oneClose = candles(80, [[20, 100], [35, 100.3]]);
oneClose[55] = { ...oneClose[55], open: 101, close: 102, high: 103, low: 101 };
assert.equal(chart(frame(oneClose)).resistanceLevels[0].testCount, 2, "a single close above the tolerance requires a second confirmation");
const rebuilt = candles(110, [[20, 100], [35, 100.3], [75, 100.1], [95, 100.2]]);
for (const index of [55, 56]) rebuilt[index] = { ...rebuilt[index], open: 101, close: 102, high: 103, low: 101 };
const renewed = chart(frame(rebuilt)).resistanceLevels[0];
assert.equal(renewed.testCount, 2, "only fresh independent rejections restore a broken zone");
assert.equal(renewed.reestablished, true);
assert.ok(renewed.tests.every((test) => test.date > date(56)));

// 旧压力两次受阻后被突破，新一轮单次受阻可作为历史回测，不能冒充三次新受阻。
const historicalRetestBars = candles(100, [[20, 100], [35, 100.3], [75, 100.1]]);
for (const index of [55, 56]) historicalRetestBars[index] = { ...historicalRetestBars[index], open: 101, close: 102, high: 103, low: 101 };
const historicalRetest = chart(frame(historicalRetestBars)).resistanceLevels.find((level) => level.kind === "historical-retest");
assert.ok(historicalRetest, "旧确认压力突破后出现一次新独立受阻，应显示历史压力回测参考");
assert.equal(historicalRetest.testCount, 1);
assert.equal(historicalRetest.historicalTestCount, 2);
assert.equal(historicalRetest.price, 100.1);
assert.equal(historicalRetest.confirmedAt, date(77));
assert.equal(historicalRetest.historicalBrokenAt, date(56));
assert.deepEqual(Array.from(historicalRetest.tests, (test) => test.date), [date(75)], "本轮次数与历史次数分开");
assert.ok(historicalRetest.historicalTests.every((test) => test.date < date(56)));
assert.equal(chart(frame(historicalRetestBars.slice(0, 77))).resistanceLevels.length, 0, "新回测右侧未确认前不复活历史压力");
const historicalForming = historicalRetestBars.map((bar, index) => index === 77 ? { ...bar, is_forming: true } : bar).slice(0, 78);
assert.equal(chart(frame(historicalForming)).resistanceLevels.length, 0, "形成中的bar不能确认历史压力回测");
const onlySingleHistory = candles(100, [[35, 100], [75, 100.1]]);
for (const index of [55, 56]) onlySingleHistory[index] = { ...onlySingleHistory[index], open: 101, close: 102, high: 103, low: 101 };
assert.equal(chart(frame(onlySingleHistory)).resistanceLevels.length, 0, "原来只有一次高点不算已确认历史压力");
const rebrokenHistory = historicalRetestBars.map((bar) => ({ ...bar }));
for (const index of [85, 86]) rebrokenHistory[index] = { ...rebrokenHistory[index], open: 102, close: 103, high: 104, low: 101 };
assert.equal(chart(frame(rebrokenHistory)).resistanceLevels.some((level) => level.price < 101), false, "原100附近历史回测被再次突破后清除；更高处新形成的压力仍可成立");
for (const end of [54, 57, 75, 77, 78, 100]) {
  assert.deepEqual(plain(chart(frame(historicalRetestBars), { visibleBars: end, endOffset: historicalRetestBars.length - end }).resistanceLevels),
    plain(chart(frame(historicalRetestBars.slice(0, end))).resistanceLevels), "历史回测在过去截止日与真实截断行情一致，不读取未来新测试");
}
const historyChain = historicalRetestBars.map((bar, index) => ({ ...bar, chain_id: index < 65 ? 0 : 1 }));
assert.equal(chart(frame(historyChain)).resistanceLevels.length, 0, "新数据链不能沿用旧链压力证据来确认单次新回测");
const historyExpired = candles(225, [[20, 100], [35, 100.3], [75, 100.1]]);
for (const index of [55, 56]) historyExpired[index] = { ...historyExpired[index], open: 101, close: 102, high: 103, low: 101 };
assert.equal(chart(frame(historyExpired)).resistanceLevels.length, 0, "历史回测长期无新受阻仍按近期证据过期");

const chains = candles();
for (let index = 42; index < chains.length; index += 1) chains[index].chain_id = 1;
assert.equal(chart(frame(chains)).resistanceLevels.length, 0, "old-chain pressure does not leak through a new data chain");
const invalid = candles();
invalid[42].high = NaN;
assert.equal(chart(frame(invalid)).resistanceLevels.length, 0, "invalid candles reset contiguous pressure history");
assert.equal(chart(frame([])), null, "empty market history keeps the existing empty chart contract");
const stale = candles(230, [[20, 100], [35, 100.3]]);
assert.equal(chart(frame(stale)).resistanceLevels.length, 0, "very old zones expire rather than remaining permanently on the chart");

const endpoint = { confirmed: true, confirmed_at: date(40), start: { kind: "bottom", date: date(15), price: 90 }, end: { kind: "top", date: date(35), price: 100.3 } };
const overlap = chart(frame(candles(), { strokes: [endpoint] }), { visibleBars: 70, endOffset: 0 });
assert.equal(overlap.keyLevels.filter((level) => level.tone === "resistance").length, 1, "a structural high inside a displayed pressure zone does not draw a duplicate line");
const majorBars = candles();
majorBars[35].high = 110;
const withMajor = chart(frame(majorBars, { strokes: [{ ...endpoint, end: { ...endpoint.end, price: 110 } }] }), { visibleBars: 70, endOffset: 0 });
assert.equal(withMajor.keyLevels.filter((level) => level.tone === "resistance").length, 2, "a separate major structural high stays visible beside repeated pressure");

// 近期波动扩大时采用同一聚类尺度，历史低波动不拆散当前同一压力区。
const changedVolatility = candles(70, [[20, 100], [35, 101.2]]);
for (let index = 60; index < changedVolatility.length; index += 1) changedVolatility[index] = { ...changedVolatility[index], high: 99, low: 86 };
assert.equal(chart(frame(changedVolatility)).resistanceLevels[0].testCount, 2);

const crowded = chart(frame(candles(160, [[20, 100], [35, 100.3], [55, 110], [70, 110.3], [90, 120], [105, 120.3], [125, 130], [140, 130.3]])));
assert.equal(crowded.resistanceLevels.length, 3, "the chart caps historical zones instead of drawing every candidate");
const historyCrowdedBars = candles(260, [[20, 100], [35, 100.3], [45, 110], [60, 110.3], [70, 120], [85, 120.3], [95, 130], [110, 130.3],
  [140, 100.1], [155, 110.1], [170, 120.1], [185, 130.1], [195, 140], [205, 140.3], [215, 150], [225, 150.3], [235, 160], [245, 160.3]]);
for (const index of [120, 121]) historyCrowdedBars[index] = { ...historyCrowdedBars[index], open: 170, close: 171, high: 172, low: 169 };
const historyCrowded = chart(frame(historyCrowdedBars)).resistanceLevels;
assert.equal(historyCrowded.filter((level) => level.kind === "reaction").length, 3, "历史参考不挤掉三个近期重复压力");
assert.equal(historyCrowded.filter((level) => level.kind === "historical-retest").length, 2, "历史压力回测最多保留两个");

const overlapBars = candles(85, [[50, 100.3], [65, 99.9]]);
for (let index = 0; index < 40; index += 1) overlapBars[index] = { ...overlapBars[index], open: 106, close: 106, high: 107, low: 105 };
for (const [index, low] of [[20, 100], [35, 100.2]]) {
  overlapBars[index] = { ...overlapBars[index], open: 103, close: 102, high: 104, low };
  overlapBars[index + 1] = { ...overlapBars[index + 1], open: 104, close: 105, high: 106, low: 103 };
}
const supported = chart(frame(overlapBars), { visibleBars: 85, endOffset: 0 });
assert.equal(supported.keyLevels.filter((level) => level.tone === "resistance").length, 0, "a pressure zone covering an existing structural level shares its line");
assert.equal(supported.structureSupports.find((level) => level.status === "broken").resistanceZone.testCount, 2, "the shared line retains pressure evidence for hover");

const adjustedBars = candles().map((bar) => ({ ...bar, open: bar.open / 2, high: bar.high / 2, low: bar.low / 2, close: bar.close / 2 }));
const adjustedZone = chart(frame(adjustedBars), null, {}, 47).resistanceLevels[0];
assert.equal(adjustedZone.price, zone.price / 2, "adjusted OHLC preserves the pressure price coordinate");
assert.equal(adjustedZone.testCount, zone.testCount);

const intradayBars = candles().map((bar, index) => ({ ...bar, date: new Date(Date.UTC(2026, 0, 1, 9, 30 + index * 15)).toISOString().slice(0, 16).replace("T", " ") }));
const intradayZone = chart(frame(intradayBars, { period: "15m" })).resistanceLevels[0];
assert.equal(intradayZone.testCount, zone.testCount, "intraday timestamps preserve independent completed-bar tests");
assert.equal(intradayZone.confirmedAt, intradayBars[52].date);

console.log("Resistance zones: independent tests, rejection, breakout, renewed confirmation, historical cutoffs and chains passed");
