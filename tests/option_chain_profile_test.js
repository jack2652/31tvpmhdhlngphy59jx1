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
const aggregatePoints = aggregationContext.aggregateByStrike([
  { contract_type: "call", strike: 100, volume: 20, open_interest: 100, expiration: "2030-01-18" },
  { contract_type: "put", strike: 100, volume: 10, open_interest: 80, expiration: "2030-01-18" },
], 100);

assert.equal(aggregatePoints.length, 1, "contracts at the same strike must share one profile row");
assert.equal(aggregatePoints[0].callGex, 0.5, "call open-interest GEX must be positive");
assert.equal(aggregatePoints[0].putGex, -0.4, "put open-interest GEX must be negative");
assert.equal(aggregatePoints[0].callVolumeGex, 0.1, "call-volume GEX must use volume as its weight");
assert.equal(aggregatePoints[0].putVolumeGex, -0.05, "put-volume GEX must retain the negative sign");
assert.equal(aggregatePoints[0].contracts, 2, "profile aggregation must retain the contract count");
assert.equal(gammaCalls[0].spot, 100, "Gamma must use the selected underlying price");
assert.doesNotMatch(app, /loadChainProfileHistory|chainProfileHistorySamples|chainProfileMaxChange|chainProfileRequestToken|\/api\/chain\/[`$\{][^\s]+\/history/, "the profile must not fetch or estimate historical samples");

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
  scrollKey: "SPY|2030-01-18|all",
});

assert.match(chartTarget.innerHTML, /<svg[^>]+role="img"[^>]+aria-label="按行权价显示的 GEX 分布图"/, "the profile must render an accessible SVG chart");
assert.match(chartTarget.innerHTML, /class="chain-profile-axis-header"/, "the title and legend must remain sticky while rows scroll");
assert.equal((chartTarget.innerHTML.match(/class="chain-profile-legend-group"/g) || []).length, 2, "the legend must group key levels and current GEX without historical samples");
assert.doesNotMatch(chartTarget.innerHTML, /chain-profile-history|<circle/, "the profile must not render historical sample markers");
assert.match(chartTarget.innerHTML, /伽马敞口（十亿美元）/, "the horizontal axis must display Chinese units in billions");
assert.match(chartTarget.innerHTML, /class="chain-profile-zero" x1="196" x2="196"/, "all four current measures must share the centered zero axis within the GEX plot");
assert.match(chartTarget.innerHTML, /class="chain-profile-label-divider" x1="56" x2="56"/, "strike labels must stay in a separate gutter outside the GEX plot");
assert.match(chartTarget.innerHTML, /class="chain-profile-strike" x="48" y="\d+" text-anchor="end">100\.0<\/text>/, "strike labels must use the reserved left gutter outside the GEX plot");
assert.match(chartTarget.innerHTML, /class="chain-profile-axis-baseline" x1="56" x2="336"/, "the horizontal axis must align with the GEX plot bounds");
assert.equal(chartTitle.textContent, "SPY 按行权价伽马敞口", "the chart title must use the symbol and Chinese terminology");
assert.match(styles, /\.chain-profile-panel\{--chain-profile-bg:var\(--panel\)/, "the profile canvas must follow the active application theme");
assert.match(styles, /\.chain-profile-svg \.chain-profile-oi-positive,\.chain-profile-legend-item \.chain-profile-oi-positive\{fill:color-mix\(in srgb,var\(--up\) 52%,#000\);background:color-mix\(in srgb,var\(--up\) 52%,#000\)\}/, "call open-interest bars and legend must use a stronger shade of the existing theme color");
assert.match(styles, /\.chain-profile-svg \.chain-profile-oi-negative,\.chain-profile-legend-item \.chain-profile-oi-negative\{fill:color-mix\(in srgb,var\(--down\) 52%,#000\);background:color-mix\(in srgb,var\(--down\) 52%,#000\)\}/, "put open-interest bars and legend must use a stronger shade of the existing theme color");
assert.match(styles, /\.chain-profile-legend\{display:grid;grid-template-columns:minmax\(0,\.9fr\) minmax\(0,1\.25fr\)/, "the profile legend must give the current GEX group enough room for its four series");
assert.match(styles, /\.chain-profile-axis-title\{fill:var\(--chain-profile-muted\)/, "the axis title must use the chart canvas text color");
assert.match(styles, /\.chain-profile-levels dt\{color:var\(--chain-profile-muted\)/, "profile metric labels must remain readable on the dark canvas");
assert.match(styles, /\.chain-profile-svg \.chain-profile-strike\{fill:var\(--chain-profile-text\)/, "strike labels must use the chart canvas text color");
assert.doesNotMatch(styles, /chain-profile-history|chain-profile-change-row|chain-profile-legend-dot/, "profile styles must not retain historical marker or summary rules");
assert.match(styles, /\.chain-profile-chart\{max-height:min\(620px,75vh\);overflow:auto;margin:0;padding-bottom:14px\}/, "the mobile chart must leave enough room for its axis and align the last strike row");
assert.match(chartTarget.innerHTML, /class="chain-profile-level-spot"/, "the profile must mark the current spot");
assert.match(chartTarget.innerHTML, /class="chain-profile-level-flip"/, "the profile must mark zero Gamma");
assert.match(chartTarget.innerHTML, /class="chain-profile-level-positive"/, "the profile must mark the positive Gamma strike");
assert.match(chartTarget.innerHTML, /class="chain-profile-level-negative"/, "the profile must mark the negative Gamma strike");
for (const series of ["oi-positive", "oi-negative", "volume-positive", "volume-negative"]) {
  assert.match(chartTarget.innerHTML, new RegExp(`class="chain-profile-${series}"`), `the profile must draw ${series} from the shared zero axis`);
}
const profileRects = (series) => [...chartTarget.innerHTML.matchAll(new RegExp(`<rect class="chain-profile-${series}"([^>]+)>`, "g"))].map((match) => ({
  position: match.index,
  ...Object.fromEntries([...match[1].matchAll(/\b(x|y|width|height)="([^"]+)"/g)].map((attribute) => [attribute[1], Number(attribute[2])])),
}));
for (const [direction, ratio] of [["positive", 4], ["negative", 5]]) {
  const holding = profileRects(`oi-${direction}`)[1];
  const volume = profileRects(`volume-${direction}`)[1];
  assert.equal(volume.height, 6, "the bright volume bar must use a consistent six-pixel thickness");
  assert.equal(holding.height, 6, "the dark holding bar must use the same six-pixel thickness");
  assert.equal(holding.y, volume.y, "the same-side holding and volume bars must fully overlap vertically");
  assert.ok(volume.position < holding.position, "the dark holding bar must be painted above the bright volume background");
  assert.ok(Math.abs(holding.width / volume.width - ratio) < 0.000001, "overlapping bars must preserve the original independent GEX magnitudes");
  assert.ok(Math.abs((direction === "negative" ? holding.x + holding.width : holding.x) - 196) < 0.000001, "overlapping bars must retain the shared zero axis on each side");
}
assert.equal(profileRects("oi-positive")[1].y - profileRects("oi-positive")[0].y, 26, "strike rows must use the compact 26-pixel pitch");
assert.ok(chartTarget.innerHTML.indexOf(">100.0</text>") < chartTarget.innerHTML.indexOf(">95.0</text>"), "higher strikes must appear above lower strikes");
assert.equal(chartTarget.dataset.chartWidth, "360", "the renderer must retain its measured width for resize checks");

chartContext.renderStrikeGexProfile("profile", [{
  strike: 100, callGex: 0.5, putGex: -0.3, callVolumeGex: 2, putVolumeGex: -0.3,
}], { spot: 100 });
assert.ok(profileRects("volume-positive")[0].width > profileRects("oi-positive")[0].width, "volume exceeding open interest must retain its longer independent bar");
assert.equal(profileRects("volume-negative")[0].width, profileRects("oi-negative")[0].width, "equal negative measures must share their full length");
assert.equal(profileRects("oi-negative")[0].y, profileRects("volume-negative")[0].y, "equal-length holding and volume bars must share one vertical footprint");

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
assert.equal(chartTarget.scrollTop % 26, 0, "the initial chart scroll must align compact strike rows to avoid clipped labels");
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
