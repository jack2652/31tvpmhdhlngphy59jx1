const assert = require("node:assert/strict");
const fs = require("node:fs");
const vm = require("node:vm");

const app = fs.readFileSync("web/static/common/js/app.js", "utf8");
const charts = fs.readFileSync("web/static/common/js/charts.js", "utf8");
const styles = fs.readFileSync("web/static/common/css/styles.css", "utf8");

const aggregateStart = app.indexOf("function aggregateByStrike(");
const aggregateEnd = app.indexOf("// 到期日时间戳", aggregateStart);
const aggregateSource = app.slice(aggregateStart, aggregateEnd);
const gammaCalls = [];
const aggregationContext = {
  state: { expiration: "2030-01-18" },
  gammaSpotBucket() { return "spot-bucket"; },
  gammaAtSpot(spot, row, expiration, spotKey, timeBucket, valuationTime) {
    gammaCalls.push({ spot, expiration, spotKey, timeBucket, valuationTime });
    return 0.5;
  },
};
vm.runInNewContext(`${aggregateSource}\nthis.aggregateByStrike = aggregateByStrike;`, aggregationContext);
const valuationTime = Date.UTC(2026, 10, 20, 15, 10);
const aggregatePoints = aggregationContext.aggregateByStrike([
  { contract_type: "call", strike: 100, volume: 20, open_interest: 100, expiration: "2030-01-18" },
  { contract_type: "put", strike: 100, volume: 10, open_interest: 80, expiration: "2030-01-18" },
], 100, valuationTime);

assert.equal(aggregatePoints.length, 1, "contracts at the same strike must share one profile row");
assert.equal(aggregatePoints[0].callGex, 0.5, "call open-interest GEX must be positive");
assert.equal(aggregatePoints[0].putGex, -0.4, "put open-interest GEX must be negative");
assert.equal(aggregatePoints[0].callVolumeGex, 0.1, "call-volume GEX must use volume as its weight");
assert.equal(aggregatePoints[0].putVolumeGex, -0.05, "put-volume GEX must retain the negative sign");
assert.equal(aggregatePoints[0].contracts, 2, "profile aggregation must retain the contract count");
assert.equal(gammaCalls[0].valuationTime, valuationTime, "historical gamma must use the actual sample time");
assert.equal(gammaCalls[0].timeBucket, Math.floor(valuationTime / 30000), "historical gamma caching must use the matching time bucket");

const historyStart = app.indexOf("function chainProfileRows(");
const historyEnd = app.indexOf("function renderChainProfileView(", historyStart);
const historySource = app.slice(historyStart, historyEnd);
const sampleCalls = [];
const historyContext = {
  state: { chainFilter: "all" },
  chainProfileHistory: {
    key: "QQQ|2030-01-18",
    loadedAt: 0,
    points: [
      { minutes_ago: 1, fetched_at: "2026-11-20T15:10:00Z", spot: 101, data: [{ contract_type: "call", strike: 100 }] },
      { minutes_ago: 5, unavailable_reason: "目标时间附近没有可用的期权链快照" },
    ],
  },
  aggregateByStrike(rows, spot, at) {
    sampleCalls.push({ rows, spot, at });
    return [{ strike: 100, callGex: spot / 100, putGex: 0, callVolumeGex: 0, putVolumeGex: 0, contracts: 1, missingGamma: 0 }];
  },
  formatEasternTime(value) { return `美东 ${value}`; },
  formatMoney(value) { return String(value); },
  formatGex(value) { return value.toFixed(2); },
};
vm.runInNewContext(`${historySource}\nthis.chainProfileHistorySamples = chainProfileHistorySamples;\nthis.chainProfileMaxChange = chainProfileMaxChange;`, historyContext);
const samples = historyContext.chainProfileHistorySamples("QQQ|2030-01-18");

assert.equal(samples.length, 5, "the profile must always reserve five historical sample slots");
assert.equal(samples[0].time, "美东 2026-11-20T15:10:00Z", "the sample must display the actual fetched time");
assert.equal(samples[0].spot, "标的 101", "the sample must display its own underlying price");
assert.equal(samples[0].netValue, 1.01, "the sample total must be calculated from the historical spot");
assert.equal(sampleCalls[0].at, Date.parse("2026-11-20T15:10:00Z"), "the sample aggregation must receive the actual snapshot time");
assert.equal(samples[0].profile[0].strike, 100, "the historical profile must retain its strike for chart markers");
assert.equal(samples[0].markerClass, "chain-profile-history-0", "each history interval must have a stable chart color");
assert.equal(samples[1].status, "目标时间附近没有可用的期权链快照", "a missing sample must keep the server reason visible");
assert.equal(samples[1].netValue, null, "a missing sample must not be represented as zero GEX");
const maximumChange = historyContext.chainProfileMaxChange(
  [{ strike: 100, callGex: 2, putGex: -0.5 }],
  [{ strike: 100, value: 0.25 }, { strike: 105, value: -0.5 }],
);
assert.equal(maximumChange.strike, 100, "maximum change must compare net GEX independently at each strike");
assert.equal(maximumChange.change, 1.25, "maximum change must use the current and historical net values");

const rendererStart = charts.indexOf("function renderStrikeGexProfile(");
const rendererEnd = charts.indexOf("function renderDistributionSummary(", rendererStart);
const rendererSource = charts.slice(rendererStart, rendererEnd);
const chartTitle = { textContent: "" };
const chartHeader = { offsetHeight: 45 };
const chartTarget = {
  dataset: {},
  innerHTML: "",
  textContent: "",
  clientHeight: 100,
  scrollTop: 0,
  querySelector(selector) {
    if (selector === ".chain-profile-chart-title") return chartTitle;
    if (selector === ".chain-profile-axis-header") return chartHeader;
    return null;
  },
};
const chartContext = {
  document: { getElementById() { return chartTarget; } },
  chartContentBox() { return { width: 360, height: 300 }; },
  formatMoney(value) { return Number(value).toFixed(1); },
  formatChartValue(value) { return Number(value).toFixed(1); },
  formatNumber(value, digits) { return Number(value).toFixed(digits); },
};
vm.runInNewContext(`${rendererSource}\nthis.renderStrikeGexProfile = renderStrikeGexProfile;`, chartContext);
chartContext.renderStrikeGexProfile("profile", [
  { strike: 105, callGex: 0, putGex: 0, callVolumeGex: 0, putVolumeGex: 0 },
  { strike: 95, callGex: 0, putGex: 0, callVolumeGex: 0, putVolumeGex: 0 },
  { strike: 100, callGex: 2, putGex: -1, callVolumeGex: 0.5, putVolumeGex: -0.2 },
], {
  title: "SPY 按行权价伽马敞口",
  spot: 100,
  zeroGamma: 105,
  positiveStrike: 100,
  negativeStrike: 95,
  history: [1, 5, 10, 15, 30].map((minutesAgo, index) => ({
    label: `${minutesAgo} 分钟前`,
    points: [{ strike: 100, value: 0.2 - index * 0.1 }],
  })),
  scrollKey: "SPY|2030-01-18|all",
});

assert.match(chartTarget.innerHTML, /<svg[^>]+role="img"[^>]+aria-label="按行权价显示的 GEX 分布图"/, "the profile must render an accessible SVG chart");
assert.match(chartTarget.innerHTML, /class="chain-profile-axis-header"/, "the title and legend must remain sticky while rows scroll");
assert.equal((chartTarget.innerHTML.match(/class="chain-profile-legend-group"/g) || []).length, 3, "the legend must keep history, key levels and current GEX in separate groups");
assert.match(chartTarget.innerHTML, /伽马敞口（十亿美元）/, "the horizontal axis must display Chinese units in billions");
assert.match(chartTarget.innerHTML, /class="chain-profile-zero" x1="196" x2="196"/, "all four current measures must share the centered zero axis within the GEX plot");
assert.match(chartTarget.innerHTML, /class="chain-profile-label-divider" x1="56" x2="56"/, "strike labels must stay in a separate gutter outside the GEX plot");
assert.match(chartTarget.innerHTML, /class="chain-profile-strike" x="48" y="\d+" text-anchor="end">100\.0<\/text>/, "strike labels must use the reserved left gutter instead of covering history markers");
assert.match(chartTarget.innerHTML, /class="chain-profile-axis-baseline" x1="56" x2="336"/, "the horizontal axis must align with the GEX plot bounds");
assert.equal(chartTitle.textContent, "SPY 按行权价伽马敞口", "the chart title must use the symbol and Chinese terminology");
assert.match(styles, /\.chain-profile-panel\{--chain-profile-bg:var\(--panel\)/, "the profile canvas must follow the active application theme");
assert.match(styles, /\.chain-profile-legend\{display:grid;grid-template-columns:minmax\(0,\.9fr\) minmax\(0,\.9fr\) minmax\(0,1\.25fr\)/, "the profile legend must give the current GEX group enough room for its four series");
assert.match(styles, /\.chain-profile-axis-title\{fill:var\(--chain-profile-muted\)/, "the axis title must use the chart canvas text color");
assert.match(styles, /\.chain-profile-levels dt\{color:var\(--chain-profile-muted\)/, "profile metric labels must remain readable on the dark canvas");
assert.match(styles, /\.chain-profile-change-row small\{grid-column:3;color:var\(--chain-profile-muted\)/, "unavailable history reasons must remain readable on the dark canvas");
assert.match(styles, /\.chain-profile-svg \.chain-profile-strike\{fill:var\(--chain-profile-text\)/, "strike labels must use the chart canvas text color");
assert.match(styles, /\.chain-profile-legend-item \.chain-profile-legend-dot\{width:7px;height:7px;border-radius:50%/, "history legend markers must use one compact circular shape");
assert.match(styles, /\.chain-profile-chart\{max-height:min\(620px,75vh\);overflow:auto;margin:0;padding-bottom:14px\}/, "the mobile chart must leave enough room for its axis and align the last strike row");
assert.match(styles, /\.chain-profile-history-0\{fill:#ff4fc3;background:#ff4fc3\}/, "the most recent sample must use a high-contrast color");
assert.match(styles, /\.chain-profile-history-1\{fill:#ffc857;background:#ffc857\}/, "the five-minute sample must use a distinct color");
assert.match(styles, /\.chain-profile-history-2\{fill:#42d9ff;background:#42d9ff\}/, "the ten-minute sample must use a distinct color");
assert.match(styles, /\.chain-profile-history-3\{fill:#b895ff;background:#b895ff\}/, "the fifteen-minute sample must use a distinct color");
assert.match(styles, /\.chain-profile-history-4\{fill:#f7f7f7;background:#f7f7f7\}/, "the thirty-minute sample must use a distinct color");
assert.match(chartTarget.innerHTML, /class="chain-profile-level-spot"/, "the profile must mark the current spot");
assert.match(chartTarget.innerHTML, /class="chain-profile-level-flip"/, "the profile must mark zero Gamma");
assert.match(chartTarget.innerHTML, /class="chain-profile-level-positive"/, "the profile must mark the positive Gamma strike");
assert.match(chartTarget.innerHTML, /class="chain-profile-level-negative"/, "the profile must mark the negative Gamma strike");
for (const series of ["oi-positive", "oi-negative", "volume-positive", "volume-negative"]) {
  assert.match(chartTarget.innerHTML, new RegExp(`class="chain-profile-${series}"`), `the profile must draw ${series} from the shared zero axis`);
}
for (let index = 0; index < 5; index += 1) {
  assert.match(chartTarget.innerHTML, new RegExp(`class="chain-profile-history-${index}"`), `the profile must render history markers for interval ${index + 1}`);
}
assert.match(chartTarget.innerHTML, /<circle class="chain-profile-history-0" cx="[^"]+" cy="32" r="2\.8"/, "the one-minute history marker must use the top lane in its strike row");
assert.match(chartTarget.innerHTML, /<circle class="chain-profile-history-1" cx="[^"]+" cy="37" r="2\.8"/, "the five-minute history marker must use a separate circular lane");
assert.match(chartTarget.innerHTML, /<circle class="chain-profile-history-2" cx="[^"]+" cy="42" r="2\.8"/, "the ten-minute history marker must use the center circular lane");
assert.match(chartTarget.innerHTML, /<circle class="chain-profile-history-3" cx="[^"]+" cy="47" r="2\.8"/, "the fifteen-minute history marker must use a separate circular lane");
assert.match(chartTarget.innerHTML, /<circle class="chain-profile-history-4" cx="[^"]+" cy="52" r="2\.8"/, "the thirty-minute history marker must use the bottom circular lane");
assert.ok(chartTarget.innerHTML.indexOf(">100.0</text>") < chartTarget.innerHTML.indexOf(">95.0</text>"), "higher strikes must appear above lower strikes");
assert.equal(chartTarget.dataset.chartWidth, "360", "the renderer must retain its measured width for resize checks");

chartContext.renderStrikeGexProfile("profile", Array.from({ length: 200 }, (_, index) => ({
  strike: 100 + index,
  callGex: index === 50 ? 2 : 0,
  putGex: index === 50 ? -1 : 0,
  callVolumeGex: index === 50 ? 0.5 : 0,
  putVolumeGex: index === 50 ? -0.2 : 0,
})), { spot: 150, zeroGamma: 150, positiveStrike: 160, negativeStrike: 140 });
assert.equal((chartTarget.innerHTML.match(/class="chain-profile-strike"/g) || []).length, 41, "the chart must show ten strikes around each key level");
assert.match(chartTarget.innerHTML, />130\.0<\/text>/, "the visible window must include its lower padding strike");
assert.match(chartTarget.innerHTML, />170\.0<\/text>/, "the visible window must include its upper padding strike");
assert.doesNotMatch(chartTarget.innerHTML, />129\.0<\/text>|>171\.0<\/text>/, "strikes outside the key-level window must not crowd the chart");

chartContext.renderStrikeGexProfile("profile", Array.from({ length: 20 }, (_, index) => ({
  strike: 90 + index,
  callGex: index === 10 ? 2 : 0,
  putGex: index === 10 ? -1 : 0,
  callVolumeGex: index === 10 ? 0.5 : 0,
  putVolumeGex: index === 10 ? -0.2 : 0,
})), { spot: 100, scrollKey: "QQQ|2030-01-18|all" });
assert.ok(chartTarget.scrollTop > 0, "the profile must initially center the chart on the current spot");
assert.equal(chartTarget.scrollTop % 28, 0, "the initial chart scroll must align strike rows to avoid clipped labels");
chartTarget.scrollTop = 80;
chartContext.renderStrikeGexProfile("profile", Array.from({ length: 20 }, (_, index) => ({
  strike: 90 + index,
  callGex: index === 10 ? 2 : 0,
  putGex: index === 10 ? -1 : 0,
  callVolumeGex: index === 10 ? 0.5 : 0,
  putVolumeGex: index === 10 ? -0.2 : 0,
})), { spot: 100, scrollKey: "QQQ|2030-01-18|all" });
assert.equal(chartTarget.scrollTop, 80, "quote refreshes must preserve a user's chart scroll position");

chartContext.renderStrikeGexProfile("profile", [], { emptyMessage: "没有可绘制数据" });
assert.equal(chartTarget.textContent, "没有可绘制数据", "the profile must preserve an explicit empty state");
assert.equal(chartTarget.scrollTop, 0, "an empty profile must reset the scroll position");

console.log("Strike-based GEX profile checks passed");
