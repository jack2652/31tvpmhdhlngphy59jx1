const assert = require("node:assert/strict");
const fs = require("node:fs");
const vm = require("node:vm");

const window = {
  devicePixelRatio: 1,
  addEventListener() {},
  removeEventListener() {},
  setTimeout(callback) { return setTimeout(callback, 0); },
  clearTimeout(timer) { clearTimeout(timer); },
  requestAnimationFrame(callback) { callback(); return 1; },
  cancelAnimationFrame() {},
};
const sandbox = {
  window, document: {}, getComputedStyle: () => ({ getPropertyValue: () => "" }),
  ResizeObserver: undefined, MutationObserver: undefined,
};
vm.runInNewContext(fs.readFileSync("web/static/common/js/charts.js", "utf8"), sandbox);

function makeChart() {
  const listeners = new Map();
  const svg = {
    getScreenCTM: () => ({ inverse: () => ({}) }),
    createSVGPoint: () => ({
      x: 0, y: 0,
      matrixTransform() { return { x: this.x, y: this.y }; },
    }),
  };
  const element = {
    classList: { add() {}, remove() {} },
    querySelector: (selector) => selector === "svg" ? svg : null,
    addEventListener(type, listener) { listeners.set(type, listener); },
    removeEventListener(type) { listeners.delete(type); },
    setPointerCapture() {}, releasePointerCapture() {},
    isConnected: true,
  };
  const model = {
    candles: Array.from({ length: 40 }, (_, index) => ({ date: String(index) })),
    totalBars: 100, visibleBars: 40, windowStart: 20,
    plotLeft: 50, historyRight: 250, plotRight: 290, plotTop: 20, plotBottom: 180,
  };
  return { element, listeners, model };
}
function pointer(type, id, x, y, extra = {}) {
  return { type, pointerId: id, pointerType: extra.pointerType || "mouse", button: extra.button ?? 0, clientX: x, clientY: y, target: { closest: () => null }, ...extra };
}

const { element, listeners, model } = makeChart();
const pans = [];
const zooms = [];
const directive = window.OptionScopeCharts.chanChartInteraction;
directive.inserted(element, { value: { chart: model, onPan: (bars) => pans.push(bars), onZoom: (...args) => zooms.push(args) } });

// Projection routes are painted on the canvas, so taps are hit-tested by the
// interaction controller instead of relying on per-route SVG click targets.
const scenarioClicks = [];
model.projections = [{ key: "up", tone: "up", points: [{ x: 252, y: 100 }, { x: 288, y: 100 }] }];
directive.componentUpdated(element, { value: {
  chart: model,
  onPan: (bars) => pans.push(bars),
  onZoom: (...args) => zooms.push(args),
  scenariosVisible: true,
  onScenarioClick: (key) => scenarioClicks.push(key),
} });
let scenarioStopped = false;
listeners.get("click")({ clientX: 270, clientY: 100, preventDefault() {}, stopPropagation() { scenarioStopped = true; } });
assert.deepEqual(scenarioClicks, ["up"], "clicking a canvas projection route must forward the scenario key");
assert.equal(scenarioStopped, true, "scenario clicks must not fall through to the chart dismiss handler");

// Dragging right pans toward older bars and produces whole-bar steps.
listeners.get("pointerdown")(pointer("pointerdown", 1, 100, 80));
listeners.get("pointermove")(pointer("pointermove", 1, 130, 80));
assert.deepEqual(pans, [6], "horizontal pointer drag should pan by candle slots");
listeners.get("pointerup")(pointer("pointerup", 1, 130, 80));

// Ctrl-wheel and the macOS Meta-wheel zoom around the pointer; plain wheel is left alone.
let prevented = false;
listeners.get("wheel")({ ctrlKey: true, metaKey: false, deltaY: -1, clientX: 150, preventDefault() { prevented = true; } });
assert.equal(prevented, true, "modified wheel zoom should prevent page scrolling");
assert.equal(zooms.length, 1);
assert.equal(zooms[0][0], 0.82);
assert.equal(zooms[0][2], 0.5, "wheel zoom should anchor at the pointer position");
prevented = false;
listeners.get("wheel")({ ctrlKey: false, metaKey: true, deltaY: 1, clientX: 200, preventDefault() { prevented = true; } });
assert.equal(prevented, true, "Meta-wheel should support macOS zoom");
assert.equal(zooms[1][0], 1.22);
assert.equal(zooms[1][2], 0.75);
prevented = false;
listeners.get("wheel")({ ctrlKey: false, metaKey: false, deltaY: -1, clientX: 150, preventDefault() { prevented = true; } });
assert.equal(prevented, false, "unmodified wheel should keep its normal page behavior");
assert.equal(zooms.length, 2);

// Two touch pointers pinch around their midpoint and keep the chart's anchor.
const beforePinch = zooms.length;
listeners.get("pointerdown")(pointer("pointerdown", 2, 100, 80, { pointerType: "touch" }));
listeners.get("pointerdown")(pointer("pointerdown", 3, 200, 80, { pointerType: "touch" }));
listeners.get("pointermove")(pointer("pointermove", 3, 250, 80, { pointerType: "touch" }));
assert.equal(zooms.length, beforePinch + 1, "two-finger pinch should change visible range");
assert.equal(zooms.at(-1)[0], 2 / 3);
assert.equal(zooms.at(-1)[2], 0.5, "pinch midpoint should remain the zoom anchor");
assert.equal(zooms.at(-1)[3], 40, "pinch should use the starting visible bar count");
listeners.get("pointerup")(pointer("pointerup", 3, 250, 80, { pointerType: "touch" }));
listeners.get("pointerup")(pointer("pointerup", 2, 100, 80, { pointerType: "touch" }));

directive.unbind(element);
assert.equal(listeners.size, 0, "interaction handlers should be removed with the directive");
console.log("Chan chart pan, wheel zoom, pinch zoom, and cleanup checks passed");
