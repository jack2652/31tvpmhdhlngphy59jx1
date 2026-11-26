const assert = require("node:assert/strict");
const fs = require("node:fs");
const vm = require("node:vm");

const window = {};
vm.runInNewContext(fs.readFileSync("web/static/common/js/chan-turtle.js", "utf8"), { window });
const calculate = window.OptionScopeTurtle.calculate;
assert.deepEqual(Array.from(window.OptionScopeTurtle.calculateWithLiveBar([], [], undefined, -1).events), [], "缺失DMI或空行情仍返回空结果");
const baseBars = Array.from({ length: 60 }, (_, index) => ({
  date: `2026-01-${String(index + 1).padStart(2, "0")}`,
  open: 100,
  high: 110,
  low: 100,
  close: 105,
}));

const sequence = [
  ...baseBars,
  { date: "2026-01-21", open: 105, high: 110, low: 100, close: 108 },
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
assert.equal(result.events.some(({ index }) => index === 60 || index === 61), false, "未突破和首根突破候选不产生入场");
assert.equal(result.events[0].price, 130, "第二根收盘守住突破位后确认入场");
assert.equal(result.events[0].forming, false, "入场信号来自已收盘 K 线");
assert.equal(result.events[1].price, 99, "低开越过退出通道时使用开盘价");
assert.ok(result.events[0].riskN > 0, "入场记录确认前Wilder N");
assert.ok(Math.abs(result.events[0].riskN - 10.95) < 1e-10, "前序TR从10跳到29后，N按1/20递推为10.95，不用常规EMA或本根波幅");
assert.equal(result.events[0].stopPrice, result.events[0].price - 2 * result.events[0].riskN, "2N止损固定在确认参考价下方");

// 止损后原趋势未变，新突破仍需两根完成收盘，允许强ADX保持而非持续加速。
const retryBars = [
  ...sequence.slice(0, 63),
  { date: "2026-04-01", open: 130, high: 131, low: result.events[0].stopPrice, close: 128 },
  { date: "2026-04-02", open: 132, high: 136, low: 131, close: 134 },
  { date: "2026-04-03", open: 134, high: 137, low: 132, close: 135 },
];
const retryDmi = strongDmi(retryBars).map((value, index) => index >= 63 ? { ...value, adx: 90 - (index - 63) } : value);
const retryResult = calculate(retryBars, retryDmi);
assert.deepEqual(Array.from(retryResult.events, event => event.index), [62, 63, 65], "2N退出后强但不加速的同向趋势允许新突破再次确认");
assert.equal(retryResult.events[2].kind, "long-entry");
assert.equal(retryResult.events[2].reentry, true, "再次入场明确记录，不冒充初次入场");
assert.equal(retryResult.events[2].stopPrice, retryResult.events[2].price - 2 * retryResult.events[2].riskN, "再次入场按新的前序N重新设定2N，而非复用旧止损");
assert.equal(calculate(retryBars.slice(0, 65), retryDmi).events.length, 2, "再次突破的首根收盘不能直接入场");
assert.equal(calculate(sequence.slice(0, 63), strongDmi(sequence).map(value => ({ ...value, adx: 35 }))).events.length, 0, "初次入场仍要求ADX高于3根前，不能全局放宽");
for (const changed of [
  retryDmi.map((value, index) => index === 63 ? { ...value, adx: 20 } : value),
  retryDmi.map((value, index) => index === 64 ? { ...value, adx: 20 } : value),
  retryDmi.map((value, index) => index === 64 ? { ...value, pdi: 15, mdi: 35 } : value),
]) {
  assert.equal(calculate(retryBars, changed).events.length, 2, "趋势强度变弱或DI反向会清除再次入场许可");
}
const retryBelowEma = retryBars.map((bar, index) => index === 64 ? { ...bar, open: 100, high: 136, low: 99, close: 100 } : bar);
assert.equal(calculate(retryBelowEma, retryDmi).events.length, 2, "价格回到均线反侧后不能沿用旧趋势重入场许可");
const retryWithoutBreak = retryBars.map((bar, index) => index >= 64 ? { ...bar, high: 131, open: 130, low: 126, close: 130 } : bar);
assert.equal(calculate(retryWithoutBreak, retryDmi).events.length, 2, "趋势未变仍需重新突破20根通道，不能止损后直接买回");
const retryForming = retryBars.map((bar, index) => index === 65 ? { ...bar, is_forming: true } : bar);
assert.equal(calculate(retryForming, retryDmi).events.length, 2, "形成中的确认K线不能产生再次入场");
assert.equal(window.OptionScopeTurtle.calculateWithLiveBar(retryBars, retryForming, retryDmi, 65).events.length, 2, "live替换确认bar不污染完成历史");
const retryNewChain = retryBars.map((bar, index) => ({ ...bar, chain_id: index >= 64 ? 1 : 0 }));
assert.equal(calculate(retryNewChain, retryDmi).events.length, 2, "新行情链清除旧趋势再次入场许可");
const mirroredRetry = retryBars.map(bar => ({ ...bar, open: 300 - bar.open, high: 300 - bar.low, low: 300 - bar.high, close: 300 - bar.close }));
const mirroredDmi = retryDmi.map(value => ({ ...value, pdi: value.mdi, mdi: value.pdi }));
const shortRetry = calculate(mirroredRetry, mirroredDmi);
assert.deepEqual(Array.from(shortRetry.events, event => event.kind), ["short-entry", "short-exit", "short-entry"], "空头同向再次突破保持镜像规则");
assert.equal(shortRetry.events[2].reentry, true);
assert.equal(shortRetry.events[2].stopPrice, shortRetry.events[2].price + 2 * shortRetry.events[2].riskN);
const channelExitBars = [
  ...sequence.slice(0, 63),
  ...Array.from({ length: 12 }, (_, index) => ({ date: `2026-05-${String(index + 1).padStart(2, "0")}`, open: 130 + index, high: 134 + index, low: 123 + index, close: 132 + index })),
  { date: "2026-05-13", open: 143, high: 145, low: 124, close: 142 },
  { date: "2026-05-14", open: 146, high: 150, low: 145, close: 149 },
  { date: "2026-05-15", open: 149, high: 151, low: 146, close: 150 },
];
const channelExitDmi = channelExitBars.map((bar, index) => ({ adx: index >= 75 ? 100 - (index - 75) : 30 + index, pdi: 35, mdi: 15 }));
const channelExitResult = calculate(channelExitBars, channelExitDmi);
assert.equal(channelExitResult.events[1].reason, "channel-exit", "夹具通过更高的10根通道退出，不触及固定止损");
assert.equal(channelExitResult.events.length, 2, "正常通道退出不放宽新入场的ADX上升要求");
for (let end = 1; end <= retryBars.length; end += 1) {
  const prefix = calculate(retryBars.slice(0, end), retryDmi);
  assert.deepEqual(JSON.parse(JSON.stringify(prefix.events)), JSON.parse(JSON.stringify(retryResult.events.filter(event => event.index < end))), "再次入场状态不使用未来确认");
}

const entryBars = sequence.slice(0, 63);
const entry = calculate(entryBars, strongDmi(entryBars)).events[0];
const stopBar = { date: "2026-03-01", open: 130, high: 131, low: entry.stopPrice, close: 128 };
const stopped = calculate([...entryBars, stopBar], strongDmi([...entryBars, stopBar]));
assert.equal(stopped.events.at(-1).reason, "risk-stop", "触及固定2N止损而没有触及通道时退出");
assert.equal(stopped.events.at(-1).price, entry.stopPrice, "等于止损线也触发");
const liveStop = { ...stopBar, is_forming: true };
const sourceWithCompletedTail = [...entryBars, stopBar];
const liveResult = window.OptionScopeTurtle.calculateWithLiveBar(sourceWithCompletedTail, [...entryBars, liveStop], strongDmi(sourceWithCompletedTail), 63);
assert.equal(liveResult.events.length, 1, "替换bar的已完成退出不进入缓存历史");
assert.equal(liveResult.liveEvent.reason, "risk-stop", "盘中能显示临时止损退出");
assert.equal(liveResult.liveEvent.forming, true, "盘中退出标记为未收盘");
const recoveredLive = window.OptionScopeTurtle.calculateWithLiveBar(entryBars, [...entryBars, { ...stopBar, low: 125, is_forming: true }], strongDmi(entryBars), 63);
assert.equal(recoveredLive.liveEvent, null, "新的临时行情不保留先前临时退出状态");
const strongCacheDmi = strongDmi(entryBars), weakCacheDmi = entryBars.map(() => ({ adx: 10, pdi: 0, mdi: 0 }));
assert.equal(window.OptionScopeTurtle.calculateWithLiveBar(entryBars, entryBars, strongCacheDmi, -1).events.length, 1);
assert.equal(window.OptionScopeTurtle.calculateWithLiveBar(entryBars, entryBars, weakCacheDmi, -1).events.length, 0, "同源数组更换DMI不得复用旧信号");
for (const separator of [{ ...stopBar, close: NaN }, { ...stopBar, chain_id: 2 }, { ...stopBar, is_forming: true }]) {
  const broken = [...entryBars, separator, { ...stopBar, open: 99, high: 101, low: 98, close: 100, chain_id: separator.chain_id }];
  const after = calculate(broken, strongDmi(broken));
  assert.equal(after.events.some((event) => event.index === 64), false, "断链/无效/中间形成bar后不延续旧仓位");
  assert.equal(after.channels[64], null, "断开的通道重新预热");
}

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
for (let end = 1; end <= sequence.length; end += 1) {
  const prefix = calculate(sequence.slice(0, end), strongDmi(sequence));
  assert.deepEqual(JSON.parse(JSON.stringify(prefix.channels)), JSON.parse(JSON.stringify(result.channels.slice(0, end))), "新增未来bar不改历史海龟通道");
  assert.deepEqual(JSON.parse(JSON.stringify(prefix.events)), JSON.parse(JSON.stringify(result.events.filter((event) => event.index < end))), "入场/退出仅在当时确认，不回写历史");
}
const app = fs.readFileSync("web/static/common/js/app.js", "utf8");
const client = fs.readFileSync("web/static/common/js/chan-client.js", "utf8");
const charts = fs.readFileSync("web/static/common/js/charts.js", "utf8");
assert.match(app, /turtle: false/, "海龟指标默认关闭");
assert.match(client, /layers\?\.turtle === true[\s\S]*?window\.OptionScopeTurtle\?\.calculateWithLiveBar\(sourceCandles, candlesForDisplay, dmiValues/, "海龟入场复用当前周期 DMI/ADX 数据并支持形成中 K 线");
assert.doesNotMatch(client, /visibleTurtleChannels\.forEach/, "海龟通道不扩大主图价格范围");
assert.match(charts, /model\.layers\?\.turtle === true && model\.turtleChannels\?\.length/, "关闭指标时不绘制海龟图层");
assert.match(template, /<button class="detail-seg" type="button" :aria-pressed="chanChartLayers\.turtle \? 'true' : 'false'"[\s\S]*?toggleChanChartLayer\('turtle'\)">海龟交易法<\/button>/, "海龟名称按钮直接控制指标图层");
assert.match(template, /id="chan-turtle-help" class="chan-indicator-description-body"[^>]*v-show="chanChartLayers\.turtle"/, "海龟图层开启时同步显示下方规则说明");
assert.match(template, /多再入／空再入[\s\S]*?趋势变弱或反向即清除再次入场许可/, "帮助说明准确区分再次入场与初次入场");
assert.match(client, /reentry: event\.reentry === true/, "再次入场身份传入当前周期图表模型");
assert.doesNotMatch(template, /aria-label="显示海龟交易法指标"/, "海龟指标不再使用独立显示复选框");

console.log("Turtle channel windows, position events, invalid bars, and UI wiring checks passed");
