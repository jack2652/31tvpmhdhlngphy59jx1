const assert = require("node:assert/strict");
const fs = require("node:fs");
const vm = require("node:vm");

const window = { devicePixelRatio: 1, addEventListener() {}, removeEventListener() {} };
const context = {
  dash: [], strokes: [], texts: [], rects: [], fills: 0,
  setTransform() {}, clearRect() {}, beginPath() {}, moveTo(x, y) { this.start = [x, y]; },
  lineTo(x, y) { this.end = [x, y]; }, stroke() { this.strokes.push({ start: this.start, end: this.end, dash: [...this.dash], width: this.lineWidth, color: this.strokeStyle }); },
  setLineDash(value) { this.dash = value; }, fillRect(x, y, width, height) { this.rects.push({ x, y, width, height, color: this.fillStyle }); }, strokeRect() {}, fillText(text, x, y) { this.texts.push({ text, x, y, font: this.font }); }, arc() {}, fill() { this.fills += 1; }, strokeText() {},
  save() {}, restore() {}, translate() {}, rotate() {}, closePath() {}, rect() {}, clip() {},
};
const canvas = { width: 0, height: 0, style: {}, getContext: () => context };
const svg = { getBoundingClientRect: () => ({ width: 960, height: 340 }) };
const target = { querySelector: (selector) => selector.startsWith("canvas") ? canvas : svg };
const sandbox = { window, document: {}, getComputedStyle: () => ({ getPropertyValue: () => "" }), ResizeObserver: undefined, MutationObserver: undefined };
vm.runInNewContext(fs.readFileSync("web/static/common/js/charts.js", "utf8"), sandbox);

function render(segments, enabled) {
  context.strokes = [];
  window.OptionScopeCharts.renderChanCanvas(target, {
    chart: { width: 960, height: 340, plotLeft: 50, plotRight: 940, plotTop: 10, plotBottom: 300, historyRight: 940, grid: [], candles: [], strokes: [], points: [], centers: [], recursiveCenters: [], keyLevels: [], signals: [], dates: [], segments },
    layers: { pens: false, penCenters: false, segments: enabled, recursiveCenters: {}, signals: false },
  });
  return context.strokes.filter((stroke) => stroke.start[0] === 10 && stroke.start[1] === 20 && stroke.end[0] === 30 && stroke.end[1] === 40);
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
  return context.strokes.filter((stroke) => stroke.start[0] === 708 && stroke.start[1] === 120 && stroke.end[0] === 932 && stroke.end[1] === 40);
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

assert.equal(renderScenarios(false).length, 0, "hidden scenarios must not paint projection routes");
const projections = renderScenarios(true);
assert.equal(projections.length, 1, "visible scenarios must paint the route on the canvas");
assert.deepEqual(projections[0].dash, [6, 5], "scenario routes keep their dashed style");
assert.equal(projections[0].width, 1.8);
const toggleGlyph = context.texts.find((text) => Math.abs(text.x - 689) < 0.01 && Math.abs(text.y - 289.5) < 0.01);
assert.ok(toggleGlyph, "the scenario toggle glyph must be painted on the canvas");
assert.match(toggleGlyph.text, /^[?\ue7b3]$/, "toggle falls back to ? until the icon font is ready");

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
const paneDivider = indicator.strokes.find((stroke) => Math.abs(stroke.start[0] - 50) < 0.01
  && Math.abs(stroke.end[0] - 940) < 0.01
  && Math.abs(stroke.start[1] - dividerY) < 0.01
  && Math.abs(stroke.end[1] - dividerY) < 0.01);
assert.ok(paneDivider, "volume and MACD panes must be separated by a divider line");

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
assert.ok(context.strokes.some((stroke) => stroke.start[0] === 120 && stroke.start[1] === 95 && stroke.end[0] === 120 && stroke.end[1] === 82), "葛兰碧引线从三角尖端连到标签框边缘");

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
