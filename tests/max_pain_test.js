const assert = require("node:assert/strict");
const fs = require("node:fs");
const vm = require("node:vm");

const app = fs.readFileSync("web/static/common/js/app.js", "utf8");
const charts = fs.readFileSync("web/static/common/js/charts.js", "utf8");
const template = fs.readFileSync("web/static/index.html", "utf8");
const styles = fs.readFileSync("web/static/common/css/styles.css", "utf8");

const calculatorStart = app.indexOf("function calculateMaxPainByStrike(");
const calculatorEnd = app.indexOf("// 到期日时间戳", calculatorStart);
assert.ok(calculatorStart >= 0 && calculatorEnd > calculatorStart, "maximum-pain calculation must remain an isolated function");
const calculatorContext = {};
vm.runInNewContext(`${app.slice(calculatorStart, calculatorEnd)}\nthis.calculateMaxPainByStrike = calculateMaxPainByStrike;`, calculatorContext);

const result = calculatorContext.calculateMaxPainByStrike([
  { contract_type: "call", strike: 90, open_interest: 100 },
  { contract_type: "call", strike: 100, open_interest: 1 },
  { contract_type: "call", strike: 110, open_interest: 0 },
  { contract_type: "put", strike: 90, open_interest: 0 },
  { contract_type: "put", strike: 100, open_interest: 1 },
  { contract_type: "put", strike: 110, open_interest: 100 },
]);
assert.equal(result.available, true, "valid open interest must produce maximum-pain data");
assert.equal(result.maxPain.strike, 100, "the minimum aggregate payout must determine maximum pain");
assert.equal(result.maxPain.callPayoff, 0.1, "call payout must include the standard 100-share multiplier");
assert.equal(result.maxPain.putPayoff, 0.1, "put payout must include the standard 100-share multiplier");
assert.equal(result.maxPain.totalPayoff, 0.2, "the total payout must sum call and put payout");
assert.equal(result.contracts, 4, "only contracts with positive open interest must count as valid");

const tied = calculatorContext.calculateMaxPainByStrike([
  { contract_type: "call", strike: 90, open_interest: 10 },
  { contract_type: "call", strike: 100, open_interest: 1 },
  { contract_type: "put", strike: 90, open_interest: 1 },
  { contract_type: "put", strike: 100, open_interest: 10 },
]);
assert.equal(tied.maxPain.strike, 90, "equal minimum payouts must select the lower strike");

const zeroOiCandidate = calculatorContext.calculateMaxPainByStrike([
  { contract_type: "call", strike: 90, open_interest: 10 },
  { contract_type: "call", strike: 100, open_interest: 0 },
  { contract_type: "put", strike: 110, open_interest: 10 },
]);
assert.deepEqual(Array.from(zeroOiCandidate.points, (point) => point.strike), [90, 100, 110], "valid zero-OI strikes must remain settlement candidates");
assert.equal(zeroOiCandidate.contracts, 2, "zero-OI candidates must not count as valid open-interest contracts");

const filtered = calculatorContext.calculateMaxPainByStrike([
  null,
  { contract_type: "call", strike: 80, open_interest: 0 },
  { contract_type: "call", strike: 90, open_interest: -1 },
  { contract_type: "put", strike: 100, open_interest: "not-a-number" },
  { contract_type: "call", strike: 110, open_interest: null },
  { contract_type: "put", strike: "invalid", open_interest: 20 },
]);
assert.equal(filtered.available, false, "invalid or missing open interest must not produce a result");
assert.deepEqual(Array.from(filtered.points), [], "invalid contracts must not contribute payout candidates");
assert.equal(calculatorContext.calculateMaxPainByStrike([
  { contract_type: "call", strike: 90, open_interest: 0 },
  { contract_type: "put", strike: 100, open_interest: 0 },
]).available, false, "zero open interest must not create a misleading zero-payout result");
assert.equal(calculatorContext.calculateMaxPainByStrike([]).available, false, "empty option rows must be unavailable");

const chartTarget = {
  dataset: {},
  innerHTML: "",
  ariaLabel: "Gamma 敞口图",
  getAttribute(name) { return name === "aria-label" ? this.ariaLabel : null; },
  getBoundingClientRect() { return { left: 0, top: 0, width: 640, height: 380 }; },
  querySelector(selector) {
    if (selector === "svg") return chartSvg;
    if (selector === ".chart-crosshair") return crosshair;
    if (selector === ".chart-tooltip") return tooltip;
    return null;
  },
};
const crosshair = { setAttribute() {} };
const tooltip = { hidden: true, innerHTML: "", style: {}, offsetWidth: 180 };
const chartListeners = new Map();
let chartWidth = 640;
const chartSvg = {
  getScreenCTM() { return null; },
  getBoundingClientRect() { return { left: 0, top: 0, width: 640, height: 380 }; },
  addEventListener(name, callback) { chartListeners.set(name, callback); },
};
const rendererStart = charts.indexOf("function renderSignedChart(");
const rendererEnd = charts.indexOf("function renderStrikeGexProfile(", rendererStart);
const chartContext = {
  document: { getElementById() { return chartTarget; } },
  getComputedStyle() { return { paddingLeft: "0px", paddingRight: "0px", paddingTop: "0px", paddingBottom: "0px" }; },
  chartContentBox() { return { width: chartWidth, height: 380 }; },
  formatChartValue(value) { return `${Number(value).toFixed(2)}M`; },
  formatMoney(value) { return Number(value).toFixed(2); },
  markerLabelWidth(value) { return String(value).length * 6; },
  strikePosition(points, strike) {
    if (!points.length || strike < points[0].strike || strike > points[points.length - 1].strike) return null;
    for (let index = 1; index < points.length; index += 1) {
      if (strike <= points[index].strike) {
        const distance = points[index].strike - points[index - 1].strike;
        return index - 1 + (distance ? (strike - points[index - 1].strike) / distance : 0);
      }
    }
    return points.length - 1;
  },
  scheduleChartPointerMove() { return () => {}; },
};
vm.runInNewContext(`${charts.slice(rendererStart, rendererEnd)}\nthis.renderSignedChart = renderSignedChart;`, chartContext);
chartContext.renderSignedChart("gex-chart", [
  { strike: 90, callPayoff: 0, putPayoff: 0.02, totalPayoff: 0.02 },
  { strike: 100, callPayoff: 0.01, putPayoff: 0.01, totalPayoff: 0.02 },
  { strike: 110, callPayoff: 0.02, putPayoff: 0, totalPayoff: 0.02 },
], "callPayoff", "putPayoff", "M", "无有效数据", {
  positiveBoth: true,
  valueLabel: "赔付",
  tooltipTitle: "结算价",
  ariaLabel: "最大痛点赔付图",
  spot: 99,
  priceMarkers: [
    { value: 99, className: "chart-spot-price", label: "现价" },
    { value: 100, className: "chart-max-pain", label: "最大痛点" },
  ],
});
assert.match(chartTarget.innerHTML, /class="chart-max-pain-call"/, "call payout must draw upward in the maximum-pain chart");
assert.match(chartTarget.innerHTML, /class="chart-max-pain-put"/, "put payout must draw upward in the maximum-pain chart");
assert.doesNotMatch(chartTarget.innerHTML, /chart-total-line|chart-max-pain-point/, "stacked payout bars must not be covered by a total-payout curve");
assert.match(chartTarget.innerHTML, /class="chart-spot-price"/, "the chart must include the current-price marker");
assert.match(chartTarget.innerHTML, /class="chart-max-pain"/, "the chart must include the maximum-pain marker");
assert.match(chartTarget.innerHTML, /aria-label="最大痛点赔付图"/, "the maximum-pain SVG must have a Chinese accessible name");
assert.match(chartTarget.innerHTML, /0\.02M/, "the maximum-pain vertical axis must scale to the payout range");
assert.match(chartTarget.innerHTML, /class="chart-max-pain-call"[^>]+height="(?:[1-9][0-9]+(?:\.[0-9]+)?|[1-9])"/, "small payouts must use the full chart height instead of the signed GEX minimum scale");
// 几何校验覆盖叠柱总高和密集数据间距，避免只检查类名而漏掉实际绘制回退。
function chartRects(className) {
  return [...chartTarget.innerHTML.matchAll(new RegExp(`<rect class="${className}"([^>]+)>`, "g"))].map((match) =>
    Object.fromEntries([...match[1].matchAll(/(x|y|width|height)="([^"]+)"/g)].map((attribute) => [attribute[1], Number(attribute[2])])));
}
const callRects = chartRects("chart-max-pain-call");
const putRects = chartRects("chart-max-pain-put");
for (let index = 0; index < callRects.length; index += 1) {
  assert.equal(callRects[index].x, putRects[index].x, "call and put payouts must share a single strike column");
  assert.equal(callRects[index].width, putRects[index].width, "both parts of a stack must have the same width");
  assert.ok(Math.abs(callRects[index].y + callRects[index].height - putRects[index].y) < 1e-8, "stack parts must adjoin without overlap or gaps");
  assert.ok(Math.abs(callRects[index].height + putRects[index].height - putRects[0].height) < 1e-8, "equal total payouts must produce equal column heights");
}
assert.match(chartTarget.innerHTML, /chart-spot-price-label[^>]+>99<title>现价 99/, "price markers must show numeric prices above the plot");
assert.match(chartTarget.innerHTML, /chart-max-pain-label[^>]+>100<title>最大痛点 100/, "the pain marker must display its actual strike");
const markerRows = [...chartTarget.innerHTML.matchAll(/class="chart-(?:spot-price|max-pain)-label chart-price-marker-label"[^>]+y="([^"]+)"/g)].map((match) => Number(match[1]));
assert.equal(markerRows.length, 2);
assert.ok(Math.abs(markerRows[0] - markerRows[1]) >= 22, "nearby price labels must occupy separate readable rows");
assert.equal((chartTarget.innerHTML.match(/text-anchor="end">(?:0|[0-9.]+M)<\/text>/g) || []).length, 5, "payout charts must show five vertical-axis ticks");
assert.match(chartTarget.innerHTML, /chart-axis-title[^>]+>（行权价）/, "the horizontal-axis unit must be explicit");
chartListeners.get("focus")?.();
assert.match(tooltip.innerHTML, /看涨赔付/, "the tooltip must name call payout in Chinese");
assert.match(tooltip.innerHTML, /看跌赔付/, "the tooltip must name put payout in Chinese");
assert.match(tooltip.innerHTML, /总赔付/, "the tooltip must show aggregate payout");
assert.match(tooltip.innerHTML, /当前价格 99\.00/, "the tooltip must retain the spot-price context without a total line");

chartWidth = 320;
const densePoints = Array.from({ length: 193 }, (_, index) => ({ strike: 500 + index, callPayoff: index, putPayoff: 193 - index }));
chartContext.renderSignedChart("gex-chart", densePoints, "callPayoff", "putPayoff", "M", "无有效数据", {
  positiveBoth: true,
  priceMarkers: [
    { value: null, className: "chart-max-pain", label: "最大痛点" },
    { value: 499, className: "chart-spot-price", label: "现价" },
    { value: NaN, className: "chart-max-pain", label: "最大痛点" },
  ],
});
const denseRects = chartRects("chart-max-pain-call");
assert.equal(denseRects.length, densePoints.length, "narrow screens must retain every strike");
assert.ok(denseRects[0].width < denseRects[1].x - denseRects[0].x, "dense strikes must leave visible gaps between columns");
assert.doesNotMatch(chartTarget.innerHTML, /class="chart-(?:spot-price|max-pain)"/, "missing or out-of-range marker values must not draw misleading price lines");
assert.ok((chartTarget.innerHTML.match(/text-anchor="(?:start|middle|end)">[0-9]+<\/text>/g) || []).length <= 4, "narrow screens must thin strike labels instead of thinning data");
chartWidth = 640;

chartContext.renderSignedChart("gex-chart", [{ strike: 100, callGex: 2, putGex: -1 }], "callGex", "putGex", "M", "无有效数据");
assert.match(chartTarget.innerHTML, /class="chart-call"/, "switching back to Gamma must preserve signed call bars");
assert.match(chartTarget.innerHTML, /class="chart-put"/, "switching back to Gamma must preserve signed put bars");
assert.doesNotMatch(chartTarget.innerHTML, /chart-max-pain-call|chart-axis-title/, "payout-specific rendering must not leak into Gamma");

const skeletonStart = charts.indexOf("function renderChartSkeleton(");
const skeletonEnd = charts.indexOf("\nfunction showEmpty(", skeletonStart);
assert.ok(skeletonStart >= 0 && skeletonEnd > skeletonStart, "chart skeleton renderer must remain independently testable");
vm.runInNewContext(`${charts.slice(skeletonStart, skeletonEnd)}\nthis.renderChartSkeleton = renderChartSkeleton;`, chartContext);
chartTarget.ariaLabel = "最大痛点赔付图";
chartContext.renderChartSkeleton("gex-chart", { ariaLabel: "Gamma 敞口图" });
assert.match(chartTarget.innerHTML, /aria-label="Gamma 敞口图"/, "switching to Gamma with no data must replace the previous view's chart label");

const gammaPanelStart = template.indexOf('<article class="analysis-panel panel"', template.indexOf('id="chart-group"'));
const gammaPanelEnd = template.indexOf("</article>", gammaPanelStart);
const gammaPanel = template.slice(gammaPanelStart, gammaPanelEnd);
assert.match(gammaPanel, /class="gamma-view-switch detail-segmented"[\s\S]*?Gamma 敞口[\s\S]*?最大痛点/, "both Chinese view choices must be inside the Gamma chart card");
assert.match(gammaPanel, /:aria-pressed="gammaViewMode === 'gamma'/, "the selected Gamma view must be exposed to assistive technology");
assert.match(gammaPanel, /:aria-pressed="gammaViewMode === 'maxPain'/, "the selected maximum-pain view must be exposed to assistive technology");
assert.match(gammaPanel, /v-if="gammaViewMode === 'maxPain'" class="chart-legend max-pain-legend"/, "the maximum-pain view must explain its chart colors and markers");
assert.doesNotMatch(gammaPanel, /chainView/, "the Gamma view switch must remain independent of the GEX heatmap selector");
assert.match(app, /gammaViewMode: "gamma"/, "Gamma exposure must remain the default chart view");
assert.match(app, /function calculateMaxPainByStrike\([\s\S]*?contractMultiplier = 100/, "the calculation must use the standard 100-share option multiplier");
assert.match(app, /if \(!state\.lastAnalysis\)[\s\S]*?暂无法计算最大痛点/, "switching before data loads must show the maximum-pain empty state instead of a stale Gamma skeleton");
assert.match(app, /renderChartSkeleton\("gex-chart", \{ ariaLabel: "Gamma 敞口图" \}\)/, "the empty Gamma view must pass its own accessible chart name to the skeleton renderer");
assert.match(styles, /\.gamma-view-switch \.detail-seg\[aria-pressed="true"\]/, "the selected chart view must have a visible theme state");
assert.match(gammaPanel, /最大痛点价：<strong/, "the main heading must emphasize the maximum-pain price");
assert.match(gammaPanel, /id="gex-chart" key="gamma-shared-chart"/, "Vue must keep the imperative SVG container stable when metadata appears during view switching");
assert.doesNotMatch(gammaPanel, /max-pain-legend-line total/, "the legend must match the reference's four entries");
assert.match(styles, /\.chart-max-pain\{stroke:var\(--gex\)/, "maximum pain must use the orange marker color");
assert.match(styles, /\.chart-max-pain-call\{fill:var\(--down\)/, "call payout must match the reference's red bars");
assert.match(styles, /\.chart-max-pain-put\{fill:var\(--up\)/, "put payout must match the reference's green bars");

console.log("Maximum pain calculation and UI checks passed");
