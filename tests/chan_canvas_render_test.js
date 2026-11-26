const assert = require("node:assert/strict");
const fs = require("node:fs");
const vm = require("node:vm");

const window = { devicePixelRatio: 1, addEventListener() {}, removeEventListener() {} };
const context = {
  dash: [], strokes: [], texts: [], strokeTexts: [], rects: [], arcs: [], fills: 0, transform: [1, 0, 0, 1, 0, 0], stack: [], globalAlpha: 1,
  setTransform(...value) { this.transform = value; }, clearRect() {}, beginPath() { this.start = null; this.end = null; }, moveTo(x, y) { this.start = [x, y]; },
  lineTo(x, y) { this.end = [x, y]; }, stroke() { this.strokes.push({ start: this.start, end: this.end, dash: [...this.dash], width: this.lineWidth, color: this.strokeStyle, alpha: this.globalAlpha, transform: [...this.transform] }); },
  setLineDash(value) { this.dash = value; }, fillRect(x, y, width, height) { this.rects.push({ x, y, width, height, color: this.fillStyle }); }, strokeRect() {}, fillText(text, x, y) { this.texts.push({ text, x, y, font: this.font, baseline: this.textBaseline, color: this.fillStyle, transform: [...this.transform] }); }, measureText(text) { return { width: Array.from(String(text)).length * 8 }; }, arc(x, y, radius) { this.arcs.push({ x, y, radius }); }, fill() { this.fills += 1; }, strokeText(text, x, y) { this.strokeTexts.push({ text, x, y, color: this.strokeStyle, width: this.lineWidth }); },
  save() { this.stack.push({ transform: [...this.transform], globalAlpha: this.globalAlpha, font: this.font, fillStyle: this.fillStyle, strokeStyle: this.strokeStyle, lineWidth: this.lineWidth, textAlign: this.textAlign, textBaseline: this.textBaseline }); },
  restore() { Object.assign(this, this.stack.pop()); }, translate() {}, rotate() {}, closePath() {}, rect() {}, clip() {}, arcTo() {},
};
const canvas = { width: 0, height: 0, style: {}, getContext: () => context };
let displayBounds = { width: 960, height: 340 };
const svg = { getBoundingClientRect: () => displayBounds };
const target = { querySelector: (selector) => selector.startsWith("canvas") ? canvas : selector.startsWith("svg") ? svg : null };
const sandbox = { window, document: {}, getComputedStyle: () => ({ getPropertyValue: () => "" }), ResizeObserver: undefined, MutationObserver: undefined };
vm.runInNewContext(fs.readFileSync("web/static/common/js/charts.js", "utf8"), sandbox);

// 日期：2026-11-23；执行者：Codex。异步绘制可能遇到尚未生成或已移除的容器，直接跳过该目标。
const initialWindow = {};
vm.runInNewContext(fs.readFileSync("web/static/common/js/charts.js", "utf8"), {
  window: initialWindow, document: { getElementById: () => null },
  getComputedStyle() { throw new Error("缺失容器不应进入尺寸计算"); },
});
for (const points of [[], [{ strike: 100, callGex: 1, putGex: -1 }]]) {
  assert.doesNotThrow(() => initialWindow.OptionScopeCharts.renderSignedChart("gex-chart", points, "callGex", "putGex", "M", "等待行情"), "缺失容器不得读取getBoundingClientRect或innerHTML");
}

function render(segments, enabled) {
  context.strokes = [];
  window.OptionScopeCharts.renderChanCanvas(target, {
    chart: { width: 960, height: 340, plotLeft: 50, plotRight: 940, plotTop: 10, plotBottom: 300, historyRight: 940, grid: [], candles: [], strokes: [], points: [], centers: [], recursiveCenters: [], keyLevels: [], signals: [], dates: [], segments },
    layers: { pens: false, penCenters: false, segments: enabled, recursiveCenters: {}, signals: false },
  });
  return context.strokes.filter((stroke) => stroke.start && stroke.end && stroke.start[0] === 10 && stroke.start[1] === 20 && stroke.end[0] === 30 && stroke.end[1] === 40);
}

function renderScenarios(visible) {
  context.strokes = [];
  context.texts = [];
  window.OptionScopeCharts.renderChanCanvas(target, {
    chart: { width: 960, height: 340, plotLeft: 50, plotRight: 940, plotTop: 10, plotBottom: 300, historyRight: 700,
      grid: [], candles: [], strokes: [], points: [], centers: [], recursiveCenters: [], keyLevels: [], signals: [], dates: [], segments: [],
      projections: [{ key: "up", tone: "up", points: [{ x: 708, y: 120 }, { x: 932, y: 40 }] }] },
    layers: { pens: false, penCenters: false, segments: false, recursiveCenters: {}, signals: false },
    scenariosVisible: visible,
  });
  return context.strokes.filter((stroke) => stroke.start && stroke.end && stroke.start[0] === 708 && stroke.start[1] === 120 && stroke.end[0] === 932 && stroke.end[1] === 40);
}

function renderStructureSupports(enabled, positions = [120, 170], keyLevels = [], includeSupports = true, supportStates = ["active", "broken"]) {
  context.strokes = [];
  context.texts = [];
  window.OptionScopeCharts.renderChanCanvas(target, {
    chart: { width: 960, height: 340, plotLeft: 50, plotRight: 940, plotTop: 10, plotBottom: 300, priceBottom: 300, historyRight: 940, grid: [], candles: [], strokes: [], points: [], centers: [], recursiveCenters: [], keyLevels, signals: [], dates: [], segments: [],
      structureSupports: includeSupports ? [
        { key: "active", x1: 80, x2: 930, y: positions[0], pointX: 100, pointY: positions[0], labelX: 930, labelY: positions[0], label: "支撑 100.00", priceLabel: "100.00", labelWidth: 80, status: supportStates[0] },
        { key: "broken", x1: 80, x2: 930, y: positions[1], pointX: 110, pointY: positions[1], labelX: 930, labelY: positions[1], label: "失守 90.00", priceLabel: "90.00", labelWidth: 80, status: supportStates[1] },
      ] : [] },
    layers: { pens: false, penCenters: false, segments: false, recursiveCenters: {}, signals: false, supportResistance: enabled },
  });
  return context;
}

function renderChanSignals(signals) {
  context.strokes = [];
  context.texts = [];
  context.fills = 0;
  window.OptionScopeCharts.renderChanCanvas(target, {
    chart: { width: 960, height: 340, plotLeft: 50, plotRight: 940, plotTop: 10, plotBottom: 300, historyRight: 940,
      grid: [], candles: [], strokes: [], points: [], centers: [], recursiveCenters: [], keyLevels: [], signals, dates: [], segments: [] },
    layers: { pens: false, penCenters: false, segments: false, recursiveCenters: {}, signals: true },
  });
  return context;
}

function renderTurningWarnings() {
  context.strokes = [];
  context.texts = [];
  window.OptionScopeCharts.renderChanCanvas(target, {
    chart: { width: 960, height: 340, plotLeft: 50, plotRight: 940, plotTop: 10, plotBottom: 300, priceBottom: 300, historyRight: 940,
      grid: [], candles: [], strokes: [], points: [{ x: 180, y: 180, labelX: 180, labelY: 202, label: "109.21" }], centers: [], recursiveCenters: [], keyLevels: [], signals: [], dates: [], segments: [],
      markers: [{ x: 180, y: 180, labelX: 180, labelY: 230, labelHeight: 15, label: "底观察", side: "buy", labelConnector: true }] },
    layers: { pens: true, legacySignals: true, segments: false, recursiveCenters: {}, signals: false },
  });
  return context;
}

const indicatorChart = {
  width: 960, height: 548, plotLeft: 50, plotRight: 940, plotTop: 10, plotBottom: 300, priceBottom: 300,
  volumeTop: 320, volumeBottom: 392, volumeMax: 1000,
  macdTop: 412, macdBottom: 508, macdMin: -2, macdMax: 2, macdZeroY: 460,
  historyRight: 940, grid: [], candles: [
    { x: 100, bodyWidth: 6, direction: "up", volume: 500, macd: { dif: 1, dea: 0.5, hist: 1 } },
    { x: 120, bodyWidth: 6, direction: "down", volume: 1000, macd: { dif: -1, dea: -0.5, hist: -1 } },
  ], strokes: [], points: [], centers: [], recursiveCenters: [], keyLevels: [], signals: [], dates: [], segments: [],
};

function renderIndicators() {
  context.strokes = [];
  context.rects = [];
  window.OptionScopeCharts.renderChanCanvas(target, {
    chart: indicatorChart,
    layers: { pens: false, penCenters: false, segments: false, recursiveCenters: {}, signals: false },
  });
  return context;
}

function renderSweepFvg() {
  context.strokes = [];
  context.texts = [];
  context.rects = [];
  context.arcs = [];
  window.OptionScopeCharts.renderChanCanvas(target, {
    chart: { width: 960, height: 340, plotLeft: 50, plotRight: 940, plotTop: 10, plotBottom: 300, historyRight: 940,
      grid: [], candles: [], strokes: [], points: [], centers: [], recursiveCenters: [], keyLevels: [], signals: [], dates: [], segments: [],
      sweepFvg: {
        sweeps: [
          { x: 220, y: 180, x1: 140, y1: 160, x2: 220, y2: 160, direction: "bullish", label: "扫低" },
          { x: 240, y: 183, x1: 170, y1: 163, x2: 240, y2: 163, direction: "bullish", label: "扫低" },
          { x: 258, y: 178, x1: 190, y1: 158, x2: 258, y2: 158, direction: "bullish", label: "扫低" },
        ],
        breaks: [],
        events: [{ x: 420, y: 210, side: "buy", label: "反转买" }],
        windows: [{ direction: "bullish", active: true, x: 400, y: 190, width: 100, height: 32, fvgX: 400, fvgY: 170, fvgWidth: 100, fvgHeight: 14 }],
      } },
    layers: { pens: false, penCenters: false, segments: false, recursiveCenters: {}, signals: false, sweepFvg: true },
  });
  return context;
}

const segments = [
  { level: 0, x1: 10, y1: 20, x2: 30, y2: 40, confirmed: true },
  { level: 0, x1: 10, y1: 20, x2: 30, y2: 40, confirmed: false, forming: true },
];
assert.equal(render(segments, false).length, 0, "hidden segment layer must not paint segments");
const painted = render(segments, true);
assert.equal(painted.length, 2, "enabled segment layer paints both confirmed and forming segments");
assert.deepEqual(painted[0].dash, [], "confirmed segment is a solid line");
assert.deepEqual(painted[1].dash, [6, 4], "forming segment is a dashed line");
assert.equal(painted[0].width, 2.6);

const sweepRender = renderSweepFvg();
const reversalLabel = sweepRender.texts.find((item) => item.text === "反转买");
assert.ok(reversalLabel, "confirmed reversal labels remain painted");
assert.match(reversalLabel.font, /10px/, "desktop reversal labels use compact readable text");
assert.equal(sweepRender.texts.filter((item) => item.text === "扫低" || item.text === "扫高").length, 0, "sweep markers do not add repeated text over candles");
assert.equal(sweepRender.arcs.filter((arc) => [220, 240, 258].includes(arc.x)).length, 3, "clustered sweep markers remain individually visible");
assert.ok(sweepRender.arcs.some((arc) => arc.x === 220 && arc.radius === 4.5), "sweep marker remains visible without a text label");
assert.ok(sweepRender.fills >= 2, "reversal marker is visibly filled");
assert.ok(!sweepRender.strokeTexts.some((item) => item.text === "扫低" || item.text === "扫高"), "sweep markers do not draw text outlines");
assert.ok(sweepRender.strokes.some((stroke) => stroke.dash[0] === 3 && stroke.alpha === 0.52), "sweep level lines remain subtle");

const supportRender = renderStructureSupports(true);
const activeSupportLine = supportRender.strokes.find((stroke) => stroke.start?.[0] === 80 && stroke.start?.[1] === 120 && stroke.end?.[1] === 120);
const brokenSupportLine = supportRender.strokes.find((stroke) => stroke.start?.[0] === 80 && stroke.start?.[1] === 170 && stroke.end?.[1] === 170);
assert.deepEqual(activeSupportLine.dash, [], "active structural supports use a solid horizontal line");
assert.deepEqual(brokenSupportLine.dash, [4, 4], "closed breaks use an even dashed horizontal line");
assert.equal(activeSupportLine.width, 1.25);
assert.equal(brokenSupportLine.width, 1);
assert.ok(supportRender.texts.some((item) => item.text === "支撑") && supportRender.texts.some((item) => item.text === "100.00"), "active support exposes its status and complete price");
assert.ok(supportRender.texts.some((item) => item.text === "失守") && supportRender.texts.some((item) => item.text === "90.00"), "broken support exposes a textual status and complete price");
const activeStatus = supportRender.texts.find((item) => item.text === "支撑");
const activePrice = supportRender.texts.find((item) => item.text === "100.00");
assert.ok(activeSupportLine.end[0] <= activeStatus.x - 8, "the support line ends before its label without a trailing fragment");
assert.notEqual(activeStatus.color, activePrice.color, "status and price have distinct visual hierarchy");
const hiddenSupportRender = renderStructureSupports(false);
assert.ok(!hiddenSupportRender.strokes.some((stroke) => stroke.start?.[0] === 80 && [120, 170].includes(stroke.start?.[1])), "hidden support layer paints no active or broken support line");
assert.ok(!hiddenSupportRender.texts.some((item) => item.text === "支撑" || item.text === "失守"), "hidden support layer paints no support labels");

// 宽屏、窄屏与高像素密度保持相同的视觉线宽、字号和虚线间隔。
for (const [width, height, ratio] of [[1440, 510, 1], [390, 340, 1], [1440, 510, 2]]) {
  displayBounds = { width, height };
  window.devicePixelRatio = ratio;
  const scaled = renderStructureSupports(true);
  const solid = scaled.strokes.find((stroke) => stroke.start?.[0] === 80 * width / 960 && stroke.start?.[1] === 120 * height / 340 && stroke.end?.[1] === stroke.start[1]);
  const dashed = scaled.strokes.find((stroke) => stroke.start?.[0] === 80 * width / 960 && stroke.dash.length === 2 && stroke.dash[0] === 4);
  assert.equal(solid.width * solid.transform[3] / ratio, 1.25, "rendered horizontal line width stays 1.25 CSS pixels");
  assert.equal(dashed.dash[0] * dashed.transform[0] / ratio, 4, "dash length stays 4 CSS pixels");
  const price = scaled.texts.find((item) => item.text === "100.00");
  assert.equal(Number(price.font.match(/(\d+)px/)[1]) * price.transform[3] / ratio, 12, "rendered label font stays 12 CSS pixels");
  const crowded = renderStructureSupports(true, [280, 282]);
  const labels = crowded.texts.filter((item) => item.text === "支撑" || item.text === "失守");
  assert.ok(labels[1].y - labels[0].y >= 20, "nearby label prices retain at least 20 CSS pixels of separation");
  assert.ok(labels.every((item) => item.y <= 300 * height / 340 - 38), "labels leave room for the bottom-right scenario toggle");
}
displayBounds = { width: 960, height: 340 };
window.devicePixelRatio = 1;

// 日期：2026-11-23；执行者：Codex。首次跌破显示金色待确认并保留价位及标签避让。
for (const [width, height, ratio] of [[1440, 510, 1], [390, 340, 2]]) {
  displayBounds = { width, height }; window.devicePixelRatio = ratio;
  const pendingRender = renderStructureSupports(true, [120, 122], [], true, ["pending-break", "broken"]);
  const pendingText = pendingRender.texts.find((item) => item.text === "跌破待确认");
  const brokenText = pendingRender.texts.find((item) => item.text === "失守");
  assert.ok(pendingText && pendingRender.texts.some((item) => item.text === "100.00"));
  assert.equal(pendingText.color, "#e7b45c", "待确认复用主题金色，文本明确状态");
  assert.ok(brokenText.y - pendingText.y >= 20, "较长待确认标签与相邻失守标签仍错开");
  const pendingLine = pendingRender.strokes.filter((stroke) => stroke.start?.[0] === 80 * width / 960
    && stroke.start?.[1] === 120 * height / 340 && stroke.end?.[1] === stroke.start[1]);
  assert.equal(pendingLine.length, 1); assert.deepEqual(pendingLine[0].dash, [6, 3]);
  assert.ok(pendingLine[0].end[0] <= pendingText.x - 8);
  const off = renderStructureSupports(false, [120, 122], [], true, ["pending-break", "broken"]);
  assert.ok(!off.texts.some((item) => item.text === "跌破待确认"));
}
displayBounds = { width: 960, height: 340 }; window.devicePixelRatio = 1;

// 压力参考复用结构标签，只有压力线时也显示完整价格，不重复绘制红线。
const pressureLevel = () => ({ tone: "resistance", x1: 400, x2: 930, y: 40, label: "形成中高点", price: 762.86, priceLabel: "762.86" });
for (const [width, height, ratio] of [[960, 340, 1], [1440, 510, 2], [390, 340, 1]]) {
  displayBounds = { width, height }; window.devicePixelRatio = ratio;
  const pressureOnly = renderStructureSupports(true, undefined, [pressureLevel()], false);
  const pressure = pressureOnly.texts.find((item) => item.text === "压力");
  const price = pressureOnly.texts.find((item) => item.text === "762.86");
  assert.ok(pressure && price, "pressure and its formatted price paint even without structural supports");
  assert.equal(pressure.color, "#f04d68", "pressure status uses the existing red theme color");
  assert.notEqual(price.color, pressure.color, "pressure price uses the same neutral hierarchy as support labels");
  assert.equal(Number(price.font.match(/(\d+)px/)[1]) * price.transform[3] / ratio, 12);
  const pressureLines = pressureOnly.strokes.filter((stroke) => stroke.start?.[0] === 400 * width / 960 && stroke.start?.[1] === 40 * height / 340 && stroke.end?.[1] === stroke.start[1]);
  assert.equal(pressureLines.length, 1, "a pressure reference must paint only once");
  assert.deepEqual(pressureLines[0].dash, [3, 4], "the pressure reference remains dashed");
  assert.ok(pressureLines[0].end[0] <= pressure.x - 8, "pressure line stops before the price label");
  const crowded = renderStructureSupports(true, [120, 170], [{ ...pressureLevel(), y: 118 }]);
  const labels = crowded.texts.filter((item) => ["压力", "支撑", "失守"].includes(item.text));
  assert.equal(labels.length, 3);
  assert.ok(labels.slice(1).every((item, index) => item.y - labels[index].y >= 20), "pressure and support labels share collision avoidance");
  const off = renderStructureSupports(false, undefined, [pressureLevel()]);
  assert.ok(!off.texts.some((item) => item.text === "压力" || item.text === "762.86"), "the shared switch hides pressure labels");
  assert.ok(!off.strokes.some((stroke) => stroke.color === "#f04d68"), "the shared switch hides pressure lines");
}
displayBounds = { width: 960, height: 340 }; window.devicePixelRatio = 1;

// 使用真实反弹和结构证据进入Canvas，确认同区支撑实际只绘制一次。
vm.runInNewContext(fs.readFileSync("web/static/common/js/chan-plan.js", "utf8"), sandbox);
vm.runInNewContext(fs.readFileSync("web/static/common/js/chan-client.js", "utf8"), sandbox);
const supportFrame = {
  period: "1d", label: "日线", as_of: "2026-10-30", last_close: 110,
  candles: Array.from({ length: 30 }, (_, index) => ({ date: `2026-10-${String(index + 1).padStart(2, "0")}`, open: 110, high: 111, low: 109, close: 110 })),
  strokes: [{ confirmed: true, confirmed_at: "2026-10-22", start: { kind: "bottom", date: "2026-10-18", price: 100 }, end: { kind: "top", date: "2026-10-21", price: 120 }, low: 100, high: 120 }],
  centers: [], segments: [], recursive_centers: [], recursive_levels: [], signals: [], position: {},
};
supportFrame.candles[17] = { ...supportFrame.candles[17], open: 103, close: 102, high: 104, low: 100 };
supportFrame.candles[18] = { ...supportFrame.candles[18], open: 104, close: 105, high: 106, low: 103 };
supportFrame.candles[20].high = 120;
const supportPayload = { symbol: "TEST", spot: 110, daily: supportFrame, weekly: supportFrame, summary: {} };
const supportLayers = { pens: false, penCenters: false, chanCandles: false, signals: false, legacySignals: false, supportResistance: true };
const supportModel = window.OptionScopeChan.view(supportPayload, false, false, 30, null, supportLayers).daily.chart;
context.strokes = [];
context.texts = [];
window.OptionScopeCharts.renderChanCanvas(target, { chart: supportModel, layers: supportLayers });
assert.ok(context.texts.some((item) => item.text === "压力") && context.texts.some((item) => item.text === "120.00"), "real key-level model exposes its formatted resistance price");
const supportY = supportModel.structureSupports[0].y;
const mainLinesAtSupport = () => context.strokes.filter((stroke) => stroke.start?.[1] === supportY && stroke.end?.[1] === supportY
  && stroke.start[0] === supportModel.structureSupports[0].x1 && stroke.end[0] === supportModel.structureSupports[0].lineEndX && stroke.dash.length === 0);
assert.equal(mainLinesAtSupport().length, 1, "the same support price must paint exactly one horizontal reference line");
const disabledSupportLayers = { ...supportLayers, supportResistance: false };
const disabledSupportModel = window.OptionScopeChan.view(supportPayload, false, false, 30, null, disabledSupportLayers).daily.chart;
context.strokes = [];
context.texts = [];
window.OptionScopeCharts.renderChanCanvas(target, { chart: disabledSupportModel, layers: disabledSupportLayers });
assert.equal(mainLinesAtSupport().length, 0, "the shared switch must remove both duplicate sources from the canvas");
assert.ok(!context.texts.some((item) => item.text === "压力"), "the shared off-state model removes resistance labels");

// 长期结构和近期区域共用标签布局，线条强度有区分，合并来源仍只绘制一条。
const historicalFrame = { ...supportFrame, candles: Array.from({ length: 300 }, (_, index) => ({
  date: new Date(Date.UTC(2026, 9, index + 1)).toISOString().slice(0, 10), open: 110, high: 111, low: 109, close: 110
})) };
historicalFrame.as_of = historicalFrame.candles.at(-1).date;
const historicalPayload = { ...supportPayload, daily: historicalFrame, weekly: historicalFrame };
const historicalModel = window.OptionScopeChan.view(historicalPayload, false, false, 30, null, supportLayers).daily.chart;
assert.equal(historicalModel.structureSupportLevels[0].kind, "historical");
// 扩大价格范围只用于渲染夹具，使完整历史的100元参考在实际Canvas中可见。
const visibleHistoricalModel = { ...historicalModel, structureSupports: [{ ...historicalModel.structureSupportLevels[0],
  x1: 50, x2: 920, y: 160, pointY: 160, labelX: 910, labelY: 160, label: "支撑 100.00" }] };
context.strokes = []; context.texts = [];
window.OptionScopeCharts.renderChanCanvas(target, { chart: visibleHistoricalModel, layers: supportLayers });
const historicalLine = context.strokes.filter((stroke) => stroke.start?.[0] === 50 && stroke.start?.[1] === 160 && stroke.end?.[1] === 160 && stroke.dash.length === 0);
assert.equal(historicalLine.length, 1);
assert.equal(historicalLine[0].alpha, historicalModel.structureSupportLevels[0].lineAlpha);
assert.ok(historicalLine[0].alpha >= 0.48 && historicalLine[0].alpha < 0.68);
assert.ok(context.texts.some((item) => item.text === "支撑") && context.texts.some((item) => item.text === "100.00"));

const duplicateSignalRender = renderChanSignals([
  { side: "buy", status: "candidate", x: 180, y: 180, markerY: 180, labelX: 180, labelY: 202, label: "一买", labelVisible: true },
  { side: "buy", status: "candidate", x: 180, y: 190, markerY: 190, labelX: 180, labelY: 222, label: "一买", labelVisible: false },
]);
assert.equal(duplicateSignalRender.texts.filter((item) => item.text === "一买").length, 1, "duplicate signal labels paint only once");
assert.equal(duplicateSignalRender.texts.filter((item) => item.text === "待确认").length, 1);
assert.ok(duplicateSignalRender.strokes.some((stroke) => stroke.start?.[0] === 180 && stroke.start?.[1] === 194.5), "hiding a duplicate label keeps its signal marker visible");
// 2026-11-23，Codex：保护价已失守须直接显示风险，不能继续画成等待确认。
const triggeredRender = renderChanSignals([{ side: "sell", status: "risk_triggered", confirmed: false, x: 180, y: 100, labelX: 180, labelY: 70, label: "顶部早预警", type: "顶部早预警" }]);
assert.ok(triggeredRender.texts.some((text) => text.text === "失守"));
assert.ok(!triggeredRender.texts.some((text) => text.text === "确认" || text.text === "待确认"));
// 2026-11-23，Codex：Canvas 必须消费模型确认标记，背驰和预警使用各自状态名。
for (const [status, title] of [["divergence_confirmed", "一卖"], ["warning_confirmed", "顶背驰预警"]]) {
  const confirmedRender = renderChanSignals([{ side: "sell", status, confirmed: true, x: 180, y: 100, labelX: 180, labelY: 70, label: title, type: title }]);
  assert.ok(confirmedRender.texts.some((text) => text.text === "确认"), `${title}确认不能误画为待确认`);
  assert.ok(!confirmedRender.texts.some((text) => text.text === "待确认"));
}
for (const [width, height, ratio] of [[1326, 470, 1], [328, 406, 2]]) {
  displayBounds = { width, height }; window.devicePixelRatio = ratio;
  const signals = [
    { type: "三买", label: "三买", side: "buy", status: "confirmed", x: 200, y: 200, markerY: 210, labelX: 200, labelY: 235 },
    { type: "三卖", label: "三卖", side: "sell", status: "candidate", x: 500, y: 100, markerY: 90, labelX: 500, labelY: 65 },
    { type: "三买", label: "三买 · 已失效", side: "buy", status: "invalidated", invalidated: true, x: 700, y: 200, markerY: 210, labelX: 700, labelY: 235 },
  ];
  const original = JSON.parse(JSON.stringify(signals));
  const visual = renderChanSignals(signals);
  assert.ok(["确认", "待确认", "失效"].every((status) => visual.texts.some((text) => text.text === status)), "确认、待确认和历史失效状态均须显示");
  const labels = visual.texts.filter((text) => ["三买", "三卖"].includes(text.text));
  assert.equal(labels.length, 3);
  assert.ok(labels.every((text) => Math.abs(Number(text.font.match(/(\d+)px/)[1]) * text.transform[3] / ratio - 12) < 0.1), "signal title font stays at 12 CSS pixels across chart widths and DPR");
  assert.equal(labels[0].color, "#19bd83"); assert.equal(labels[1].color, "#f04d68");
  assert.equal(labels[2].color, "#81909c");
  assert.ok(signals[2].renderedLabel, "失效标签恢复完整文字框");
  assert.ok(signals[2].renderedMarker, "失效圆斜杠仍可查看详情");
  signals.forEach((signal, index) => {
    if (signal.renderedLabel) assert.ok(Math.abs(signal.renderedLabel.height * height / 340 - 20) < 0.1);
    const { renderedLabel, renderedMarker, ...unchanged } = signal;
    assert.deepEqual(unchanged, original[index], "styling cannot change signal status, text or model coordinates");
  });
}
displayBounds = { width: 960, height: 340 }; window.devicePixelRatio = 1;
// 日期：2026-11-23；执行者：Codex。完整结构预警和失效连接线恢复为参考截图样式。
const historicalWarning = { type: "反弹承压", label: "反弹承压 · 已失效", side: "sell", invalidated: true,
  status: "invalidated", x: 180, y: 100, labelX: 180, labelY: 250 };
const historicalRender = renderChanSignals([historicalWarning]);
assert.ok(["反弹承压", "失效"].every((title) => historicalRender.texts.some((text) => text.text === title)));
assert.ok(historicalWarning.renderedLabel && historicalWarning.renderedMarker);
assert.ok(historicalRender.strokes.some((stroke) => stroke.start?.[0] === 180 && stroke.end?.[0] === 180
  && Math.abs(stroke.end[1] - stroke.start[1]) > 50), "历史失效文字框仍连接原信号位置");
const turningWarningRender = renderTurningWarnings();
assert.ok(turningWarningRender.texts.some((item) => item.text === "109.21"), "pivot price labels remain painted next to turning warnings");
assert.ok(turningWarningRender.texts.some((item) => item.text === "底观察" && item.x === 180 && item.y === 230 && item.baseline === "middle"), "turning warnings use their collision-resolved center position");
assert.ok(turningWarningRender.strokes.some((stroke) => stroke.start?.[0] === 180 && stroke.start?.[1] === 186
  && stroke.end?.[0] === 180 && stroke.end?.[1] === 222.5), "relocated turning warnings draw a connector from the pivot");

assert.equal(renderScenarios(false).length, 0, "hidden scenarios must not paint projection routes");
const projections = renderScenarios(true);
assert.equal(projections.length, 1, "visible scenarios must paint the route on the canvas");
assert.deepEqual(projections[0].dash, [6, 5], "scenario routes keep their dashed style");
assert.equal(projections[0].width, 1.8);
const toggleGlyph = context.texts.find((text) => Math.abs(text.x - 689) < 0.01 && Math.abs(text.y - 289.5) < 0.01);
assert.ok(toggleGlyph, "the scenario toggle glyph must be painted on the canvas");
assert.match(toggleGlyph.text, /^[?\ue73d]$/, "toggle falls back to ? until the icon font is ready");

const indicator = renderIndicators();
assert.ok(indicator.rects.some((rect) => Math.abs(rect.x - 97) < 0.01 && rect.width === 6), "volume and MACD bars must follow candle slots");
assert.ok(indicator.strokes.some((stroke) => stroke.color === "#e7b45c"), "MACD DIF line must be painted");
assert.ok(indicator.strokes.some((stroke) => stroke.color === "#69b7ff"), "MACD DEA line must be painted");
const volumeHeader = indicator.texts.find((text) => text.text.startsWith("成交量"));
assert.ok(volumeHeader, "volume pane must be labelled");
assert.equal(volumeHeader.x, 50, "volume title and latest volume must sit on the left header");
assert.match(volumeHeader.text, /成交量\s+1000$/, "the volume header must show the latest candle volume");
assert.ok(!indicator.texts.some((text) => text.text === "1000" && text.x < 50), "the old max-volume gutter label must be removed");
const macdHeader = indicator.texts.find((text) => text.text.startsWith("MACD"));
assert.ok(macdHeader, "MACD pane must be labelled");
assert.equal(macdHeader.x, 50, "MACD title and latest values must sit on the left");
assert.match(macdHeader.text, /DIF -1\.00 · DEA -0\.50 · 柱 -1\.00/, "latest DIF/DEA/histogram values must follow the MACD title");
const dividerY = 392 + (412 - 392) / 3;
const paneDivider = indicator.strokes.find((stroke) => stroke.start && stroke.end && Math.abs(stroke.start[0] - 50) < 0.01
  && Math.abs(stroke.end[0] - 940) < 0.01
  && Math.abs(stroke.start[1] - dividerY) < 0.01
  && Math.abs(stroke.end[1] - dividerY) < 0.01);
assert.ok(paneDivider, "volume and MACD panes must be separated by a divider line");

function renderMfiBenchmark(benchmarkValues) {
  context.strokes = [];
  context.texts = [];
  window.OptionScopeCharts.renderChanCanvas(target, {
    chart: {
      width: 960, height: 578, plotLeft: 50, plotRight: 940, plotTop: 10, plotBottom: 300, priceBottom: 300,
      mfiTop: 520, mfiBottom: 560, historyRight: 940, grid: [], strokes: [], points: [], centers: [],
      recursiveCenters: [], keyLevels: [], signals: [], dates: [], segments: [],
      candles: benchmarkValues.map((benchmark, index) => ({
        x: 100 + index * 20, bodyWidth: 6, direction: "up", volume: 500,
        mfi: { value: 30, average: 28, benchmark },
      })),
    },
    layers: { pens: false, penCenters: false, segments: false, recursiveCenters: {}, signals: false },
  });
  return context;
}

const missingBenchmark = renderMfiBenchmark([null, null]);
const missingMfiHeader = missingBenchmark.texts.find((text) => text.text.startsWith("MFI "));
assert.match(missingMfiHeader.text, /标普 --/, "missing benchmark MFI must not be formatted as zero");
assert.equal(missingBenchmark.strokes.some((stroke) => stroke.color === "#d58aa6"), false, "missing benchmark values must not draw a false zero line");
const zeroBenchmark = renderMfiBenchmark([0, 0]);
const zeroMfiHeader = zeroBenchmark.texts.find((text) => text.text.startsWith("MFI "));
assert.match(zeroMfiHeader.text, /标普 \+?0\.00/, "a real benchmark MFI of zero must remain visible");
assert.ok(zeroBenchmark.strokes.some((stroke) => stroke.color === "#d58aa6"), "a real zero benchmark MFI must still draw");

function renderAFStochZ(enabled) {
  context.strokes = [];
  context.texts = [];
  window.OptionScopeCharts.renderChanCanvas(target, {
    chart: {
      width: 960, height: 340, plotLeft: 50, plotRight: 940, plotTop: 10, plotBottom: 300, historyRight: 940,
      grid: [], candles: [], strokes: [], points: [], centers: [], recursiveCenters: [], keyLevels: [], signals: [], segments: [],
      afSignals: [{ side: "buy", label: "BUY", divergence: null, x: 180, y: 220, markerY: 220, labelX: 180, labelY: 242, hitLabelWidth: 34 },
        { side: "sell", label: "H-Bear", divergence: "hidden", x: 360, y: 100, markerY: 100, labelX: 360, labelY: 78, hitLabelWidth: 48 }],
    },
    layers: { pens: false, penCenters: false, segments: false, recursiveCenters: {}, signals: false, afStochZ: enabled },
  });
  return context.texts;
}

assert.ok(!renderAFStochZ(false).some((text) => text.text === "BUY" || text.text === "H-Bear"), "hidden AF StochZ layer must not paint labels");
const afLabels = renderAFStochZ(true);
assert.ok(afLabels.some((text) => text.text === "BUY"), "enabled AF StochZ layer paints leading BUY labels");
assert.ok(afLabels.some((text) => text.text === "H-Bear"), "enabled AF StochZ layer paints hidden divergence labels");

context.texts = [];
context.fills = 0;
window.OptionScopeCharts.renderChanCanvas(target, {
  chart: {
    width: 960, height: 340, plotLeft: 50, plotRight: 940, plotTop: 10, plotBottom: 300,
    historyRight: 940, grid: [], candles: [], strokes: [], points: [], centers: [], recursiveCenters: [],
    keyLevels: [], signals: [], dates: [], segments: [], granvilleAverage: [],
    granvilleEvents: [{ side: "sell", x: 120, y: 100, markerRadius: 4, upMarker: false, label: "卖8", labelY: 80, fontSize: 8, labelVisible: false }],
  },
  layers: { pens: false, penCenters: false, segments: false, recursiveCenters: {}, signals: false, granville: true },
});
assert.ok(!context.texts.some((text) => text.text === "卖8"), "没有无重叠位置时隐藏葛兰碧文字");
assert.equal(context.fills, 1, "隐藏葛兰碧文字时仍绘制对应三角标记");

context.strokes = [];
context.texts = [];
context.rects = [];
window.OptionScopeCharts.renderChanCanvas(target, {
  chart: {
    width: 960, height: 340, plotLeft: 50, plotRight: 940, plotTop: 10, plotBottom: 300,
    historyRight: 940, grid: [], candles: [], strokes: [], points: [], centers: [], recursiveCenters: [],
    keyLevels: [], signals: [], dates: [], segments: [], granvilleAverage: [],
    granvilleEvents: [{ side: "sell", x: 120, y: 100, markerRadius: 4, upMarker: false, label: "卖8", labelY: 80, fontSize: 8, labelVisible: true, labelConnector: true }],
  },
  layers: { pens: false, penCenters: false, segments: false, recursiveCenters: {}, signals: false, granville: true },
});
assert.ok(context.rects.some((rect) => rect.x === 109 && rect.y === 70 && rect.width === 22 && rect.height === 12), "葛兰碧标签使用信号色底框");
assert.ok(context.texts.some((text) => text.text === "卖8"), "可见的葛兰碧标签绘制文字");
assert.ok(context.strokes.some((stroke) => stroke.start && stroke.end && stroke.start[0] === 120 && stroke.start[1] === 95 && stroke.end[0] === 120 && stroke.end[1] === 82), "葛兰碧引线从三角尖端连到标签框边缘");

context.strokes = [];
context.texts = [];
context.rects = [];
context.fills = 0;
window.OptionScopeCharts.renderChanCanvas(target, {
  chart: {
    width: 960, height: 340, plotLeft: 50, plotRight: 940, plotTop: 10, plotBottom: 300,
    historyRight: 940, grid: [], candles: [], strokes: [], points: [], centers: [], recursiveCenters: [],
    keyLevels: [], signals: [], dates: [], segments: [],
    macdSma200Averages: {
      fast: [{ x: 100, y: 180, trend: "up" }, { x: 120, y: 170, trend: "up" }],
      slow: [{ x: 100, y: 190, trend: "neutral" }, { x: 120, y: 185, trend: "neutral" }],
      trend: [{ x: 100, y: 210, rising: true }, { x: 120, y: 205, rising: true }],
    },
    macdSma200Events: [{ side: "buy", x: 180, y: 220, markerRadius: 4, upMarker: true, label: "多头条件", labelY: 244, fontSize: 8, labelVisible: true }],
  },
  layers: { pens: false, penCenters: false, segments: false, recursiveCenters: {}, signals: false, macdSma200: true },
});
assert.ok(context.strokes.some((stroke) => stroke.color === "#19bd83"), "MACD + SMA200 paints the trend-colored 12-period average");
assert.ok(context.strokes.some((stroke) => stroke.color === "#69b7ff"), "MACD + SMA200 paints the neutral-colored 26-period average");
assert.equal(context.strokes.some((stroke) => stroke.color === "#e7b45c"), false, "MACD + SMA200 does not use a fixed amber color for its 26-period average");
assert.ok(context.strokes.some((stroke) => stroke.color === "#19bd83"), "MACD + SMA200 colors a rising 200-period average with the up color");
assert.ok(context.fills >= 2, "MACD + SMA200 fills the area between its 12- and 200-period averages");
assert.ok(context.texts.some((text) => text.text === "多头条件"), "MACD + SMA200 paints condition labels when enabled");
context.strokes = [];
context.texts = [];
window.OptionScopeCharts.renderChanCanvas(target, {
  chart: {
    width: 960, height: 340, plotLeft: 50, plotRight: 940, plotTop: 10, plotBottom: 300,
    historyRight: 940, grid: [], candles: [], strokes: [], points: [], centers: [], recursiveCenters: [],
    keyLevels: [], signals: [], dates: [], segments: [],
    macdSma200Averages: { fast: [{ x: 100, y: 180 }, { x: 120, y: 170 }], slow: [], trend: [] },
    macdSma200Events: [{ side: "buy", x: 180, y: 220, markerRadius: 4, upMarker: true, label: "多头条件", labelY: 244, fontSize: 8, labelVisible: true }],
  },
  layers: { pens: false, penCenters: false, segments: false, recursiveCenters: {}, signals: false, macdSma200: false },
});
assert.equal(context.texts.some((text) => text.text === "多头条件"), false, "hidden MACD + SMA200 layer does not paint its conditions");

context.strokes = [];
context.texts = [];
window.OptionScopeCharts.renderChanCanvas(target, {
  chart: {
    ...indicatorChart, mobileChart: true,
  },
  layers: { pens: false, penCenters: false, segments: false, recursiveCenters: {}, signals: false },
});
const mobileVolumeHeader = context.texts.find((text) => text.text.startsWith("成交量"));
const mobileMacdHeader = context.texts.find((text) => text.text.startsWith("MACD"));
assert.match(mobileVolumeHeader.font, /^14px/, "mobile volume header must use the mobile label scale");
assert.match(mobileMacdHeader.font, /^14px/, "mobile MACD header must use the mobile label scale");

console.log("Chan canvas segment and scenario rendering checks passed");
