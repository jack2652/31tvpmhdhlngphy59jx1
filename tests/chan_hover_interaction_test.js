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
const horizontal = svgElement("chart-crosshair-h chart-crosshair-price");
const horizontalVolume = svgElement("chart-crosshair-h chart-crosshair-volume");
const horizontalMacd = svgElement("chart-crosshair-h chart-crosshair-macd");
const tagX = svgElement("chart-tag-x", [{ tagName: "rect", setAttribute(name, value) { this[name] = value; } }, { tagName: "text", setAttribute(name, value) { this[name] = value; } }]);
const tagY = svgElement("chart-tag-y", [{ tagName: "rect", setAttribute(name, value) { this[name] = value; } }, { tagName: "text", setAttribute(name, value) { this[name] = value; } }]);
const tagVolume = svgElement("chart-tag-volume", [{ tagName: "rect", setAttribute(name, value) { this[name] = value; } }, { tagName: "text", setAttribute(name, value) { this[name] = value; } }]);
const tagMacd = svgElement("chart-tag-macd", [{ tagName: "rect", setAttribute(name, value) { this[name] = value; } }, { tagName: "text", setAttribute(name, value) { this[name] = value; } }]);
const layer = svgElement("chan-crosshair-layer", [vertical, horizontal, horizontalVolume, horizontalMacd, tagX, tagY, tagVolume, tagMacd]);
layer.setAttribute("visibility", "hidden");
layer.querySelector = (selector) => layer.children.find((child) => child.className.split(/\s+/).includes(selector.slice(1))) || null;

const svgListeners = new Map();
const svg = {
  addEventListener(type, listener) { svgListeners.set(type, listener); },
  removeEventListener(type) { svgListeners.delete(type); },
  getScreenCTM: () => ({ inverse: () => ({}) }),
  createSVGPoint: () => ({ x: 0, y: 0, matrixTransform() { return { x: this.x, y: this.y }; } }),
};
const targetListeners = new Map();
const tooltip = new Element("chan-chart-tooltip");
const headerTexts = [];
const headerCanvas = {
  width: 500, height: 280,
  getContext: () => ({ setTransform() {}, clearRect() {}, fillText(text) { headerTexts.push(text); } }),
};
const target = {
  offsetWidth: 500,
  offsetHeight: 240,
  addEventListener(type, listener) { targetListeners.set(type, listener); },
  removeEventListener(type) { targetListeners.delete(type); },
  querySelector(selector) {
    if (selector === "svg") return svg;
    if (selector === ".chan-crosshair-layer") return layer;
    if (selector === ".chan-chart-tooltip") return tooltip;
    if (selector === "canvas.chan-chart-canvas") return headerCanvas;
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
    volume: 1234500, macd: { dif: 0.2, dea: 0.1, hist: 0.2 },
  }],
  periodLabel: "日线", plotLeft: 50, plotRight: 450, historyRight: 450,
  plotTop: 20, plotBottom: 200, crosshairBottom: 260, width: 500, height: 280,
  volumeTop: 210, volumeBottom: 230, volumeMax: 2000000,
  macdTop: 240, macdBottom: 260, macdMin: -2, macdMax: 2,
  minimum: 90, maximum: 110, structures: [],
};
const directive = window.OptionScopeCharts.chanChartHover;
directive.inserted(target, { value: model });
targetListeners.get("pointermove")({ clientX: 150, clientY: 110, buttons: 0, target: { closest: () => null } });

assert.equal(layer.attributes.visibility, "visible", "pointer movement must show both crosshair guides");
assert.equal(vertical.attributes.x1, "150");
assert.equal(vertical.attributes.y2, "260", "the vertical crosshair must span the price, volume and MACD panes");
assert.equal(horizontal.attributes.y1, "110");
assert.equal(tooltip.hidden, false, "pointer movement must show the candle tooltip");
const volumeRow = tooltip.children.find((child) => child.className.includes("chan-volume-row"));
assert.equal(volumeRow.children[1].textContent, "123.45万", "the tooltip must include candle volume");
assert.ok(!tooltip.children.some((child) => child.className.includes("chan-indicator-row")), "the tooltip must not repeat the MACD pane values");
assert.ok(headerTexts.includes("成交量  123.45万"), "the volume header must follow the hovered candle");
assert.ok(headerTexts.includes("MACD  DIF +0.20 · DEA +0.10 · 柱 +0.20"), "the MACD header must follow the hovered candle");
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

// Indicator panes keep the vertical crosshair and tooltip, but each pane gets
// its own horizontal dashed line and left-axis value tag.
targetListeners.get("pointermove")({ clientX: 150, clientY: 240, buttons: 0, pointerType: "mouse", target: { closest: () => null } });
assert.equal(horizontal.attributes.visibility, "hidden", "the price horizontal crosshair must hide outside the price pane");
assert.equal(tagY.attributes.visibility, "hidden", "the price-axis tag must hide outside the price pane");
assert.ok(!horizontalMacd.attributes.visibility, "the MACD pane must show its horizontal crosshair");
assert.equal(horizontalVolume.attributes.visibility, "hidden", "the volume crosshair must hide while hovering MACD");
assert.equal(tagMacd.querySelector("text").textContent, "+2.00", "the MACD tag must show the value at the cursor");
assert.equal(tagVolume.attributes.visibility, "hidden", "the volume tag must hide while hovering MACD");
assert.equal(tooltip.hidden, false, "indicator panes must keep the OHLC/volume tooltip");

targetListeners.get("pointermove")({ clientX: 150, clientY: 220, buttons: 0, pointerType: "mouse", target: { closest: () => null } });
assert.ok(!horizontalVolume.attributes.visibility, "the volume pane must show its horizontal crosshair");
assert.equal(horizontalMacd.attributes.visibility, "hidden", "the MACD crosshair must hide while hovering volume");
assert.equal(tagVolume.querySelector("text").textContent, "100.00万", "the volume tag must show the value at the cursor");
assert.equal(tagMacd.attributes.visibility, "hidden", "the MACD tag must hide while hovering volume");

targetListeners.get("pointerleave")();
assert.equal(layer.attributes.visibility, "hidden", "leaving the chart must hide crosshair guides");
assert.equal(tooltip.hidden, true, "leaving the chart must hide the tooltip");
directive.unbind(target);
assert.equal(targetListeners.size, 0, "hover handlers must be removed on unbind");
assert.equal(svgListeners.size, 0, "keyboard and pointer handlers must be removed on unbind");

console.log("Chan crosshair and OHLC tooltip interaction checks passed");
