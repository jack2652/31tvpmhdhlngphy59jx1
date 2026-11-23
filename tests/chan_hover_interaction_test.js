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
assert.deepEqual(Array.from(tooltip.children.filter((child) => child.className.includes("chan-ohlc-row") && !child.className.includes("change")).map((row) => row.children[1].textContent)), ["100.00", "105.00", "107.00", "99.00"]);

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
assert.equal(layer.attributes.visibility, "hidden", "scenario lane must not show the candle crosshair");
assert.equal(tooltip.hidden, true, "scenario lane must not show a pending-candle tooltip");
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

// CSS 像素标签反算后的高度和位置必须用于命中，避让后仍可查看结构详情。
const support = { x1: 80, x2: 450, lineEndX: 360, y: 120, lineHitPadding: 5,
  labelX: 450, labelY: 150, labelWidth: 82, labelHeight: 28,
  status: "active", statusLabel: "结构支撑", zoneLabel: "100.00", sources: "确认笔",
  date: "2026-10-01", confirmedAt: "2026-10-03", lastCloseDate: "2026-10-15" };
directive.componentUpdated(target, { value: { ...model, structureSupports: [support], layers: { supportResistance: true } } });
targetListeners.get("pointermove")({ clientX: 420, clientY: 161, buttons: 0, pointerType: "mouse", target: { closest: () => null } });
let supportDetail = tooltip.children.find((child) => child.className === "chan-structure-tooltip");
assert.equal(supportDetail.hidden, false, "the full rendered label height must remain hoverable after collision avoidance");
assert.equal(supportDetail.children[0].textContent, "结构支撑 · 100.00");
targetListeners.get("pointermove")({ clientX: 420, clientY: 120, buttons: 0, pointerType: "mouse", target: { closest: () => null } });
assert.equal(supportDetail.hidden, true, "the empty line tail removed before a moved label must not be hoverable");
directive.componentUpdated(target, { value: { ...model, structureSupports: [support], layers: { supportResistance: false } } });
targetListeners.get("pointermove")({ clientX: 420, clientY: 150, buttons: 0, pointerType: "mouse", target: { closest: () => null } });
supportDetail = tooltip.children.find((child) => child.className === "chan-structure-tooltip");
assert.equal(supportDetail.hidden, true, "the shared off state must hide support details");

const reboundSupport = { ...support, statusLabel: "回踩确认支撑", testCount: 2, lastTestDate: "2026-10-12", reactionAtr: 1.8, reestablished: true };
directive.componentUpdated(target, { value: { ...model, structureSupports: [reboundSupport], layers: { supportResistance: true } } });
targetListeners.get("pointermove")({ clientX: 420, clientY: 150, buttons: 0, pointerType: "mouse", target: { closest: () => null } });
supportDetail = tooltip.children.find((child) => child.className === "chan-structure-tooltip");
assert.equal(supportDetail.children[0].textContent, "回踩确认支撑 · 100.00");
assert.match(supportDetail.children[1].textContent, /有效反弹 2 次.*最近测试 2026-10-12.*平均反弹 1.80 ATR.*失守后重新确认/);

// 日期：2026-11-23；执行者：Codex。待确认悬浮显示完成收盘证据，不能再宣称未跌破。
const pendingSupport = { ...reboundSupport, status: "pending-break", statusLabel: "跌破待确认",
  pendingBreakAt: "2026-10-13", lastCloseLabel: "96.00", breakThresholdLabel: "99.40" };
directive.componentUpdated(target, { value: { ...model, structureSupports: [pendingSupport], layers: { supportResistance: true } } });
targetListeners.get("pointermove")({ clientX: 420, clientY: 150, buttons: 0, pointerType: "mouse", target: { closest: () => null } });
supportDetail = tooltip.children.find((child) => child.className === "chan-structure-tooltip");
assert.equal(supportDetail.children[0].textContent, "跌破待确认 · 100.00");
assert.match(supportDetail.children[1].textContent, /首次有效收盘跌破 2026-10-13.*收盘 96.00.*有效跌破阈值 99.40.*尚需下一根完成收盘确认/);
assert.doesNotMatch(supportDetail.children[1].textContent, /未被有效收盘跌破/);

const historicalSupport = { ...support, statusLabel: "历史结构支撑", kind: "historical", hasHistoricalStructure: true,
  historicalDate: "2025-07-02", testCount: 0, reactionAtr: null };
directive.componentUpdated(target, { value: { ...model, structureSupports: [historicalSupport], layers: { supportResistance: true } } });
targetListeners.get("pointermove")({ clientX: 420, clientY: 150, buttons: 0, pointerType: "mouse", target: { closest: () => null } });
supportDetail = tooltip.children.find((child) => child.className === "chan-structure-tooltip");
assert.equal(supportDetail.children[0].textContent, "历史结构支撑 · 100.00");
assert.match(supportDetail.children[1].textContent, /历史结构确认 2025-07-02.*长期参考/);
assert.doesNotMatch(supportDetail.children[1].textContent, /有效反弹|平均反弹/);

const styledSignal = { type: "三买", label: "三买", side: "buy", status: "confirmed", signalStatus: "已确认",
  x: 120, y: 90, markerY: 95, labelX: 120, labelY: 65, start: "2026-10-12", endPrice: 100,
  renderedMarker: { x: 120, y: 95, hitX: 9, hitY: 9 }, renderedLabel: { x: 420, y: 150, width: 70, height: 20 } };
directive.componentUpdated(target, { value: { ...model, signals: [styledSignal], layers: { signals: true } } });
targetListeners.get("pointermove")({ clientX: 440, clientY: 154, buttons: 0, pointerType: "mouse", target: { closest: () => null } });
supportDetail = tooltip.children.find((child) => child.className === "chan-structure-tooltip");
assert.equal(supportDetail.hidden, false, "CSS-pixel signal badges must be hit by their rendered geometry");
assert.equal(supportDetail.children[0].textContent, "三买 · 已确认");
assert.equal(styledSignal.labelX, 120); assert.equal(styledSignal.labelY, 65);
// 日期：2026-11-23；执行者：Codex。失效圆斜杠与完整文字框均可查看原信号详情。
const historicalSignal = { ...styledSignal, type: "一买", status: "invalidated", signalStatus: "已失效", invalidated: true };
directive.componentUpdated(target, { value: { ...model, signals: [historicalSignal], layers: { signals: true } } });
targetListeners.get("pointermove")({ clientX: 120, clientY: 95, buttons: 0, pointerType: "mouse", target: { closest: () => null } });
assert.equal(supportDetail.hidden, false, "历史失效圆斜杠仍能查看详情");
assert.match(supportDetail.children[0].textContent, /一买 · 已失效/);
targetListeners.get("pointermove")({ clientX: 420, clientY: 150, buttons: 0, pointerType: "mouse", target: { closest: () => null } });
assert.equal(supportDetail.hidden, false, "历史失效完整文字框仍能查看详情");
const structureWarning = { ...styledSignal, type: "顶背驰预警", start: "2026-10-14", formedAt: "2026-10-15", confirmedAt: "2026-10-16" };
directive.componentUpdated(target, { value: { ...model, signals: [structureWarning], layers: { signals: true } } });
targetListeners.get("pointermove")({ clientX: 120, clientY: 95, buttons: 0, pointerType: "touch", target: { closest: () => null } });
assert.match(supportDetail.children[0].textContent, /顶背驰预警/);
assert.match(supportDetail.children[1].textContent, /可观察 2026-10-15.*确认 2026-10-16/);

const pressure = { ...support, tone: "resistance", label: "反复受阻压力区", priceLabel: "100.00", zoneLabel: "99.90–100.30", testCount: 3,
  lastTestDate: "2026-10-12", reestablished: true, sources: "已收盘反弹高点" };
directive.componentUpdated(target, { value: { ...model, keyLevels: [pressure], layers: { supportResistance: true } } });
targetListeners.get("pointermove")({ clientX: 420, clientY: 150, buttons: 0, pointerType: "mouse", target: { closest: () => null } });
supportDetail = tooltip.children.find((child) => child.className === "chan-structure-tooltip");
assert.equal(supportDetail.hidden, false, "a pressure label exposes the existing structure tooltip");
assert.equal(supportDetail.children[0].textContent, "反复受阻压力 · 99.90–100.30");
assert.match(supportDetail.children[1].textContent, /独立受阻 3 次.*突破后重新确认/);
const historicalPressure = { ...pressure, kind: "historical-retest", testCount: 1, historicalTestCount: 2,
  historicalBrokenAt: "2026-04-17", sources: "历史多次受阻与突破后新回测" };
directive.componentUpdated(target, { value: { ...model, keyLevels: [historicalPressure], layers: { supportResistance: true } } });
targetListeners.get("pointermove")({ clientX: 420, clientY: 150, buttons: 0, pointerType: "mouse", target: { closest: () => null } });
assert.equal(supportDetail.children[0].textContent, "历史压力回测 · 99.90–100.30");
assert.match(supportDetail.children[1].textContent, /历史受阻 2 次.*旧压力突破 2026-04-17.*本轮回测 1 次/);
assert.doesNotMatch(supportDetail.children[1].textContent, /独立受阻 3 次/, "历史与本轮回测不得混算");
directive.componentUpdated(target, { value: { ...model, keyLevels: [pressure], layers: { supportResistance: false } } });
targetListeners.get("pointermove")({ clientX: 420, clientY: 150, buttons: 0, pointerType: "mouse", target: { closest: () => null } });
assert.equal(tooltip.children.find((child) => child.className === "chan-structure-tooltip").hidden, true, "pressure tooltip obeys the shared switch");

// The empty future area must expose a pending daily candle, including its date
// card, horizontal price guide, and left axis readout.
directive.componentUpdated(target, { value: {
  chart: {
    ...model,
    historyRight: 450,
    plotRight: 450,
    visibleBars: 2,
    endOffset: -1,
    pendingOpenDates: ["2026-11-11"],
    pendingSlotCount: 1,
  },
} });
targetListeners.get("pointermove")({ clientX: 300, clientY: 110, buttons: 0, pointerType: "mouse", target: { closest: () => null } });
assert.equal(layer.attributes.visibility, "visible", "future area must show crosshair guides");
assert.equal(tooltip.hidden, false, "future area must show a pending candle tooltip");
assert.equal(tooltip.children[0].textContent, "2026-11-11", "future tooltip must use the next trading date");
assert.equal(tooltip.children[1].textContent, "日线 · 待开盘 K 线", "future tooltip must identify the pending daily candle");
assert.deepEqual(Array.from(tooltip.children.filter((child) => child.className.includes("chan-ohlc-row")).map((row) => row.children[1].textContent)), ["-", "-", "-", "-"], "pending OHLC values must use dashes");
assert.ok(!horizontal.attributes.visibility, "future price pane must show its horizontal guide");
assert.ok(!tagY.attributes.visibility, "future price pane must show the left price tag");

// 盘中周期也必须使用相同的待开盘槽位交互。
directive.componentUpdated(target, { value: {
  chart: {
    ...model,
    period: "1h",
    periodLabel: "1小时",
    candles: [{
      date: "2026-11-10 15:00", displayDate: "2026-11-10 15:00", x: 150,
      prices: { open: 100, high: 107, low: 99, close: 105 }, closeChangePercent: 5, volume: 1234500,
    }],
    historyRight: 450,
    plotRight: 450,
    visibleBars: 2,
    endOffset: -1,
    pendingOpenDates: ["2026-11-11 09:30"],
    pendingSlotCount: 1,
  },
} });
targetListeners.get("pointermove")({ clientX: 300, clientY: 110, buttons: 0, pointerType: "mouse", target: { closest: () => null } });
assert.equal(tooltip.children[0].textContent, "2026-11-11 09:30", "intraday future tooltip must show its pending timestamp");
assert.equal(tooltip.children[1].textContent, "1小时 · 待开盘 K 线", "intraday future tooltip must identify its period");
assert.ok(!horizontal.attributes.visibility, "intraday future price pane must show its horizontal guide");

targetListeners.get("pointerleave")();
assert.equal(layer.attributes.visibility, "hidden", "leaving the chart must hide crosshair guides");
assert.equal(tooltip.hidden, true, "leaving the chart must hide the tooltip");
directive.unbind(target);
assert.equal(targetListeners.size, 0, "hover handlers must be removed on unbind");
assert.equal(svgListeners.size, 0, "keyboard and pointer handlers must be removed on unbind");

console.log("Chan crosshair and OHLC tooltip interaction checks passed");
