const assert = require("node:assert/strict");
const fs = require("node:fs");
const vm = require("node:vm");

const window = {};
vm.runInNewContext(fs.readFileSync("web/static/common/js/chan-turtle.js", "utf8"), { window });
const calculate = window.OptionScopeTurtle.calculate;
const baseBars = Array.from({ length: 60 }, (_, index) => ({
  date: `2026-01-${String(index + 1).padStart(2, "0")}`,
  open: 100,
  high: 110,
  low: 100,
  close: 105,
}));

const sequence = [
  ...baseBars,
  { date: "2026-01-21", open: 115, high: 121, low: 105, close: 120, is_forming: true },
  { date: "2026-01-22", open: 122, high: 130, low: 101, close: 125 },
  { date: "2026-01-23", open: 126, high: 132, low: 122, close: 130 },
  { date: "2026-01-24", open: 99, high: 110, low: 99, close: 101 },
  { date: "2026-01-25", open: 100, high: 110, low: 100, close: 100 },
  { date: "2026-01-26", open: 100, high: 110, low: 100, close: 100 },
  { date: "2026-01-27", open: 100, high: 110, low: 100, close: 100 },
  { date: "2026-01-28", open: 100, high: 110, low: 100, close: 100 },
  { date: "2026-01-29", open: 98, high: 110, low: 98, close: 98 },
  { date: "2026-01-30", open: 97, high: 108, low: 97, close: 97 },
  { date: "2026-01-31", open: 133, high: 133, low: 96, close: 130 },
];
const strongDmi = (bars) => bars.map((_, index) => ({
  adx: 30 + index,
  pdi: index < 68 ? 35 : 15,
  mdi: index < 68 ? 15 : 35,
}));
const result = calculate(sequence, strongDmi(sequence));
assert.equal(result.channels.length, sequence.length, "通道结果与 K 线索引对齐");
assert.equal(result.channels[19], null, "不足 20 根前序 K 线时不生成通道");
assert.deepEqual(JSON.parse(JSON.stringify(result.channels[20])), {
  entryUpper: 110,
  entryLower: 100,
  exitUpper: 110,
  exitLower: 100,
}, "通道只读取当前 K 线之前的窗口");
assert.deepEqual(JSON.parse(JSON.stringify(result.events.map(({ index, kind }) => [index, kind]))), [
  [62, "long-entry"],
  [63, "long-exit"],
  [69, "short-entry"],
  [70, "short-exit"],
], "入场经连续收盘确认和 EMA 趋势过滤，持仓按反向通道触及退出");
assert.equal(result.events.some(({ index }) => index === 60 || index === 61), false, "形成中的 K 线和首根突破候选不产生入场");
assert.equal(result.events[0].price, 130, "第二根收盘守住突破位后确认入场");
assert.equal(result.events[0].forming, false, "入场信号来自已收盘 K 线");
assert.equal(result.events[1].price, 99, "低开越过退出通道时使用开盘价");

const rangingBreakout = [
  ...baseBars,
  { date: "2026-03-01", open: 105, high: 112, low: 104, close: 111 },
];
const weakAdx = rangingBreakout.map(() => ({ adx: 20 }));
assert.equal(calculate(rangingBreakout, weakAdx).events.length, 0, "ADX 低于 25 时过滤通道突破入场");

const invalidSequence = baseBars.map((bar) => ({ ...bar }));
invalidSequence[10].close = Number.NaN;
invalidSequence.push(...baseBars.slice(0, 12).map((bar, index) => ({ ...bar, date: `2026-02-${String(index + 1).padStart(2, "0")}` })));
const invalidResult = calculate(invalidSequence);
assert.equal(invalidResult.channels[20], null, "历史窗口含无效 OHLC 时不传播伪通道");
assert.ok(invalidResult.channels[31], "无效 K 线移出窗口后恢复通道计算");
assert.ok(invalidResult.channels.every((channel) => !channel || Object.values(channel).every(Number.isFinite)), "输出通道不含 NaN");
const impossibleRange = calculate([
  ...baseBars,
  { date: "2026-01-21", open: 112, high: 111, low: 99, close: 105 },
]);
assert.equal(impossibleRange.channels[60], null, "开盘或收盘超出最高/最低价时跳过该 K 线");

const template = fs.readFileSync("web/static/index.html", "utf8");
const app = fs.readFileSync("web/static/common/js/app.js", "utf8");
const client = fs.readFileSync("web/static/common/js/chan-client.js", "utf8");
const charts = fs.readFileSync("web/static/common/js/charts.js", "utf8");
assert.match(app, /turtle: false/, "海龟指标默认关闭");
assert.match(client, /layers\?\.turtle === true \? window\.OptionScopeTurtle\?\.calculate\(candlesForDisplay, dmiValues\)/, "海龟入场复用当前周期 DMI/ADX 数据");
assert.match(client, /visibleTurtleChannels\.forEach/, "海龟通道纳入当前视口价格范围");
assert.match(charts, /model\.layers\?\.turtle === true && model\.turtleChannels\?\.length/, "关闭指标时不绘制海龟图层");
assert.match(template, /<button class="detail-seg" type="button" :aria-pressed="chanChartLayers\.turtle \? 'true' : 'false'"[\s\S]*?toggleChanChartLayer\('turtle'\)">海龟交易法<\/button>/, "海龟名称按钮直接控制指标图层");
assert.match(template, /id="chan-turtle-help" class="chan-indicator-description-body"[^>]*v-show="chanChartLayers\.turtle"/, "海龟图层开启时同步显示下方规则说明");
assert.doesNotMatch(template, /aria-label="显示海龟交易法指标"/, "海龟指标不再使用独立显示复选框");

console.log("Turtle channel windows, position events, invalid bars, and UI wiring checks passed");
