const assert = require("node:assert/strict");
const fs = require("node:fs");
const vm = require("node:vm");

const window = { devicePixelRatio: 1, addEventListener() {}, removeEventListener() {} };
const context = {
  dash: [], strokes: [], texts: [],
  setTransform() {}, clearRect() {}, beginPath() {}, moveTo(x, y) { this.start = [x, y]; },
  lineTo(x, y) { this.end = [x, y]; }, stroke() { this.strokes.push({ start: this.start, end: this.end, dash: [...this.dash], width: this.lineWidth, color: this.strokeStyle }); },
  setLineDash(value) { this.dash = value; }, fillRect() {}, strokeRect() {}, fillText(text, x, y) { this.texts.push({ text, x, y, font: this.font }); }, arc() {}, fill() {}, strokeText() {},
  save() {}, restore() {}, translate() {}, rotate() {}, closePath() {},
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

console.log("Chan canvas segment and scenario rendering checks passed");
