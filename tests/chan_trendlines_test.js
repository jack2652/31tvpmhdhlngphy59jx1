const assert = require("node:assert/strict");
const fs = require("node:fs");
const vm = require("node:vm");

const window = {};
for (const script of ["chan-plan", "chan-trendlines", "chan-client"]) {
  vm.runInNewContext(fs.readFileSync(`web/static/common/js/${script}.js`, "utf8"), { window });
}

const date = (index) => new Date(Date.UTC(2026, 7, index + 1)).toISOString().slice(0, 10);
const highAt = (index) => 125 - (index - 8) / 3;
const candleAt = (index) => {
  const close = highAt(index) - 3;
  return { date: date(index), open: close - 0.25, high: highAt(index) - 2, low: close - 1, close };
};
const candles = Array.from({ length: 52 }, (_, index) => candleAt(index));
const tops = [8, 20, 35];
for (const index of tops) candles[index].high = highAt(index);
const strokes = tops.map((index) => ({
  start: { date: date(index - 4), kind: "bottom", price: candles[index - 4].low },
  end: { date: date(index), kind: "top", price: highAt(index) },
  confirmed: true, confirmed_at: date(index + 2),
}));
const frame = { period: "1d", as_of: date(51), bar_count: candles.length, candles, strokes };
const layers = { trendlines: true, supportResistance: false };

function chart(visibleBars, endOffset, daily = frame, liveBar = null) {
  return window.OptionScopeChan.view({ daily, spot: 110, live_daily_bar: liveBar }, false,
    false, 30, { daily: { visibleBars, endOffset } }, layers).daily.chart;
}

const pressure = (model) => model.trendlines?.find((line) => line.side === "resistance");
const slope = ({ x1, y1, x2, y2 }) => (y2 - y1) / (x2 - x1);
function assertContinuous(line) {
  assert.ok(line?.solid && line.extension, "预期同时看到实线和虚线");
  assert.equal(line.solid.x2, line.extension.x1);
  assert.equal(line.solid.y2, line.extension.y1);
  assert.ok(Math.abs(slope(line.solid) - slope(line.extension)) < 1e-10, "实线和虚线必须共用同一个斜率");
}

const full = chart(52, 0);
const original = pressure(full);
assertContinuous(original);
assert.equal(original.key, `resistance:${date(8)}:${date(35)}`);
assert.equal(original.anchors.length, 2);
original.anchors.forEach((anchor, index) => {
  const candle = full.candles[index === 0 ? 8 : 35];
  assert.equal(anchor.x, candle.x);
  assert.ok(Math.abs(anchor.y - candle.high) < 1e-10, "锚定实际影线高点");
});
const future = chart(36, -9);
assertContinuous(pressure(future));
assert.equal(pressure(future).key, original.key, "平移到未来槽位仍使用同一对锚点");
assert.equal(pressure(future).solid.x1, future.plotLeft);
assert.equal(pressure(future).anchors.length, 1, "裁剪边缘不生成假锚点");
assert.equal(pressure(chart(24, 3)).key, original.key, "缩放并查看较早窗口仍使用同一对锚点");
assert.equal(pressure(chart(23, -17)).solid, null, "锚点已离开视口时只绘制虚线");
assert.equal(pressure(chart(23, -17)).anchors.length, 0);
assert.equal(pressure(chart(35, 15)), undefined, "确认 K 线尚未进入视口时不提前显示");

const calculate = window.OptionScopeTrendlines.calculate;
const selection = calculate(frame);
assert.strictEqual(calculate({ ...frame }), selection, "同一份行情复用选线结果");
assert.strictEqual(calculate({ ...frame, last_close: 500 }), selection, "现价变化不参与选线");
const line = selection.find((item) => item.side === "resistance");
assert.equal(line.slope, (highAt(35) - highAt(8)) / 27);
assert.equal(line.endIndex, 62, "延长最多一个锚点跨度");
const extraFuture = chart(52, -30);
assert.ok(pressure(extraFuture).extension.x2 < extraFuture.historyRight, "延长上限不随未来空白增大");

const noStrokes = { ...frame, strokes: [] };
assert.equal(pressure(chart(52, 0, noStrokes)).key, original.key, "无缠论端点也能识别有效的反抽高点");
const compact = { ...noStrokes, recursive_levels: [{ level: 0, strokes }] };
assert.deepEqual(calculate(compact), selection, "紧凑响应可使用基础级别确认笔");
assert.equal(pressure(chart(52, 0, { ...frame, as_of: date(33) })), undefined, "切换截止日须更新布局缓存");

const makeBreach = (index, clearance, wick = clearance) => ({
  date: date(index), open: highAt(index) - 3.25, low: highAt(index) - 4,
  close: highAt(index) + clearance, high: highAt(index) + wick,
});
const live = { ...makeBreach(52, 12), is_forming: true };
assert.equal(pressure(chart(40, 0, frame, live)).key, original.key, "实时价格穿线不修改锚点");
const withTail = (extra) => ({ ...frame, candles: [...candles, ...extra], as_of: extra.at(-1).date });
assert.equal(pressure(chart(40, 0, withTail([live]))).key, original.key, "行情中的形成中 K 线也不参与破位");
assert.equal(calculate(withTail([{ ...live, is_forming: false }])).some((item) => item.key === line.key), false, "明显收盘穿越使相应趋势线失效");

const smallBreach = makeBreach(52, 0.8);
assert.equal(calculate(withTail([smallBreach])).some((item) => item.key === line.key), true, "一次轻微扫过可保留");
assert.equal(calculate(withTail([smallBreach, makeBreach(53, 0.8)])).some((item) => item.key === line.key), false, "连续两次有效收盘穿越使该线失效");
assert.equal(calculate(withTail([makeBreach(52, -3, 8)])).some((item) => item.key === line.key), false, "明显穿过影线的线不绘制");

const newChain = candles.map((bar, index) => index < 29 ? bar : { ...bar, chain_id: 1 });
assert.equal(calculate({ ...frame, candles: newChain }).length, 0, "趋势线不跨数据断链");
const badHistory = [...candles];
badHistory[27] = { ...badHistory[27], low: -1 };
assert.equal(calculate({ ...frame, candles: badHistory }).length, 0, "无效价格也切断候选区间");
const reflected = candles.map((bar) => ({ ...bar, open: 250 - bar.open, high: 250 - bar.low,
  low: 250 - bar.high, close: 250 - bar.close }));
const support = calculate({ ...noStrokes, candles: reflected }).find((item) => item.side === "support");
assert.equal(support.key, `support:${date(8)}:${date(35)}`);
assert.equal(support.endPrice, reflected[35].low);

const untested = Array.from({ length: 80 }, (_, index) => candleAt(index));
for (const index of [8, 66]) untested[index].high = highAt(index);
assert.equal(calculate({ ...noStrokes, as_of: date(79), candles: untested }).length, 0, "长期无回测的锚点不能连成主线");
