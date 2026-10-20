const assert = require("node:assert/strict");
const fs = require("node:fs");
const vm = require("node:vm");

class Element {
  constructor(className = "") {
    this.className = className;
    this.children = [];
    this.attributes = {};
    this.style = {};
    this.hidden = false;
    this.textContent = "";
    this.offsetWidth = 180;
  }
  append(...children) { this.children.push(...children); }
  replaceChildren(...children) { this.children = [...children]; }
  setAttribute(name, value) { this.attributes[name] = String(value); }
  removeAttribute(name) { delete this.attributes[name]; }
  querySelector(selector) {
    if (selector === "rect") return this.children.find((child) => child.tagName === "rect") || null;
    if (selector === "text") return this.children.find((child) => child.tagName === "text") || null;
    return null;
  }
}

function svgElement(className, children = []) {
  const element = new Element(className);
  element.children = children;
  return element;
}

const vertical = svgElement("chart-crosshair");
const horizontal = svgElement("chart-crosshair-h");
const tagX = svgElement("chart-tag-x", [{ tagName: "rect", setAttribute(name, value) { this[name] = value; } }, { tagName: "text", setAttribute(name, value) { this[name] = value; } }]);
const tagY = svgElement("chart-tag-y", [{ tagName: "rect", setAttribute(name, value) { this[name] = value; } }, { tagName: "text", setAttribute(name, value) { this[name] = value; } }]);
const layer = svgElement("chan-crosshair-layer", [vertical, horizontal, tagX, tagY]);
layer.querySelector = (selector) => layer.children.find((child) => child.className === selector.slice(1)) || null;

const svgListeners = new Map();
const svg = {
  addEventListener(type, listener) { svgListeners.set(type, listener); },
  removeEventListener(type) { svgListeners.delete(type); },
  getScreenCTM: () => ({ inverse: () => ({}) }),
  createSVGPoint: () => ({ x: 0, y: 0, matrixTransform() { return { x: this.x, y: this.y }; } }),
};
const targetListeners = new Map();
const tooltip = new Element("chan-chart-tooltip");
const target = {
  offsetWidth: 500,
  offsetHeight: 240,
  addEventListener(type, listener) { targetListeners.set(type, listener); },
  removeEventListener(type) { targetListeners.delete(type); },
  querySelector(selector) {
    if (selector === "svg") return svg;
    if (selector === ".chan-crosshair-layer") return layer;
    if (selector === ".chan-chart-tooltip") return tooltip;
    return null;
  },
  getBoundingClientRect: () => ({ left: 0, top: 0, width: 500, height: 240 }),
};
const window = {
  devicePixelRatio: 1,
  requestAnimationFrame(callback) { callback(); return 1; },
  cancelAnimationFrame() {},
};
const sandbox = {
  window,
  document: { createElement: () => new Element() },
  getComputedStyle: () => ({ getPropertyValue: () => "" }),
  formatMoney: (value) => "$" + Number(value).toFixed(2),
};
vm.runInNewContext(fs.readFileSync("web/static/common/js/charts.js", "utf8"), sandbox);

const model = {
  candles: [{
    date: "2026-10-15", displayDate: "2026-10-15", x: 150,
    prices: { open: 100, high: 107, low: 99, close: 105 }, closeChangePercent: 5,
  }],
  periodLabel: "日线", plotLeft: 50, plotRight: 450, historyRight: 450,
  plotTop: 20, plotBottom: 200, width: 500, height: 240,
  minimum: 90, maximum: 110, structures: [],
};
const directive = window.OptionScopeCharts.chanChartHover;
directive.inserted(target, { value: model });
targetListeners.get("pointermove")({ clientX: 150, clientY: 110, buttons: 0, target: { closest: () => null } });

assert.equal(layer.attributes.visibility, "visible", "pointer movement must show both crosshair guides");
assert.equal(vertical.attributes.x1, "150");
assert.equal(horizontal.attributes.y1, "110");
assert.equal(tooltip.hidden, false, "pointer movement must show the candle tooltip");
assert.equal(tooltip.children[0].textContent, "2026-10-15");
const changeRow = tooltip.children.find((child) => child.className.includes("chan-ohlc-change-row"));
assert.ok(changeRow, "tooltip must include the close-change row");
assert.equal(changeRow.children[1].textContent, "+5.00%");
assert.deepEqual(Array.from(tooltip.children.filter((child) => child.className.includes("chan-ohlc-row") && !child.className.includes("change")).map((row) => row.children[1].textContent)), ["$100.00", "$107.00", "$99.00", "$105.00"]);

// Projection routes live on the canvas; hover hit testing must still open and
// close the scenario card without any per-route SVG nodes.
const scenarioEvents = [];
directive.componentUpdated(target, { value: {
  chart: { ...model, historyRight: 360, plotRight: 450, projections: [{ key: "up", tone: "up", points: [{ x: 368, y: 100 }, { x: 440, y: 50 }] }] },
  scenariosVisible: true,
  onScenarioHover: (key) => scenarioEvents.push(["show", key]),
  onScenarioLeave: () => scenarioEvents.push(["hide"]),
} });
targetListeners.get("pointermove")({ clientX: 400, clientY: 78, buttons: 0, pointerType: "mouse", target: { closest: () => null } });
assert.deepEqual(scenarioEvents, [["show", "up"]], "moving onto a canvas projection route must open its scenario card");
targetListeners.get("pointermove")({ clientX: 150, clientY: 110, buttons: 0, pointerType: "mouse", target: { closest: () => null } });
assert.deepEqual(scenarioEvents, [["show", "up"], ["hide"]], "leaving the projection route must close the scenario card");

targetListeners.get("pointerleave")();
assert.equal(layer.attributes.visibility, "hidden", "leaving the chart must hide crosshair guides");
assert.equal(tooltip.hidden, true, "leaving the chart must hide the tooltip");
directive.unbind(target);
assert.equal(targetListeners.size, 0, "hover handlers must be removed on unbind");
assert.equal(svgListeners.size, 0, "keyboard and pointer handlers must be removed on unbind");

console.log("Chan crosshair and OHLC tooltip interaction checks passed");
