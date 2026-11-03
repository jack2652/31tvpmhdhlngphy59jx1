const assert = require("node:assert/strict");
const fs = require("node:fs");
const vm = require("node:vm");

const template = fs.readFileSync("web/static/index.html", "utf8");
const app = fs.readFileSync("web/static/common/js/app.js", "utf8");

assert.match(app, /const CHAIN_VIEW_STORAGE_KEY = "option-scope-chain-view"/, "the selected option-chain view must use its own preference key");
assert.match(app, /function readChainViewPreference\(\)[\s\S]*?localStorage\.getItem\(CHAIN_VIEW_STORAGE_KEY\)[\s\S]*?stored === "table" \|\| stored === "heatmap" \? stored : "heatmap"[\s\S]*?catch \(error\) \{ return "heatmap"; \}/, "only supported view values may be restored, with heatmap as the fallback");
assert.match(app, /function rememberChainViewPreference\(view\)[\s\S]*?localStorage\.setItem\(CHAIN_VIEW_STORAGE_KEY, view\)[\s\S]*?catch \(error\)/, "the view preference write must tolerate unavailable storage");
assert.match(app, /chainView: readChainViewPreference\(\)/, "the Vue state must restore the saved view or use the default");

const storageStart = app.indexOf("const CHAIN_VIEW_STORAGE_KEY");
const storageEnd = app.indexOf("function copyDefaultChanChartLayers()", storageStart);
const preferenceSource = `${app.slice(storageStart, storageEnd)}\nthis.readChainViewPreference = readChainViewPreference;\nthis.rememberChainViewPreference = rememberChainViewPreference;`;
const storedValues = new Map();
const preferenceContext = { localStorage: {
  getItem(key) { return storedValues.get(key) ?? null; },
  setItem(key, value) { storedValues.set(key, value); },
} };
vm.runInNewContext(preferenceSource, preferenceContext);
assert.equal(preferenceContext.readChainViewPreference(), "heatmap", "a first visit must default to the heatmap");
preferenceContext.rememberChainViewPreference("table");
assert.equal(preferenceContext.readChainViewPreference(), "table", "a saved table choice must be restored after reload");
preferenceContext.rememberChainViewPreference("heatmap");
assert.equal(preferenceContext.readChainViewPreference(), "heatmap", "a saved heatmap choice must be restored after reload");
storedValues.set("option-scope-chain-view", "invalid");
assert.equal(preferenceContext.readChainViewPreference(), "heatmap", "an unsupported saved value must fall back to the heatmap");

const unavailableContext = { localStorage: {
  getItem() { throw new Error("storage unavailable"); },
  setItem() { throw new Error("storage unavailable"); },
} };
vm.runInNewContext(preferenceSource, unavailableContext);
assert.equal(unavailableContext.readChainViewPreference(), "heatmap", "unavailable storage must keep the default view");
assert.doesNotThrow(() => unavailableContext.rememberChainViewPreference("table"), "unavailable storage must not break view switching");
assert.match(app, /chainTitle: "GEX 热力图 · 选择到期日查看"/, "the initial section title must identify the heatmap");
assert.match(template, /id="chain-title"[^>]*>GEX 热力图 · 选择到期日查看/, "the initial template title must identify the heatmap");
assert.match(template, /aria-label="GEX 热力图明细"/, "the collapsible section must have an accessible heatmap name");
assert.match(template, /<span class="eyebrow">GEX HEATMAP<\/span>/, "the section eyebrow must identify the heatmap");
assert.match(template, /class="table-wrap" v-show="chainView === 'table'"/, "the option-chain table must remain available as the alternate view");
assert.match(template, /class="chain-heatmap-panel" v-show="chainView === 'heatmap'"/, "the heatmap must be visible in the default view");
const heatmapHeaders = template.match(/<th v-for="column in view\.chainHeatmap\.columns"[\s\S]*?<\/th>/)?.[0] || "";
assert.match(heatmapHeaders, /<button class="chain-heatmap-expiration-button" type="button"/, "each heatmap expiry header must use a native button");
assert.match(heatmapHeaders, /:aria-label="'选择到期日 ' \+ column\.expiration"/, "each expiry button must expose its full date as its accessible name");
assert.match(heatmapHeaders, /:aria-current="column\.current \? 'date' : null"/, "the selected expiry must be announced as current");
assert.match(heatmapHeaders, /:disabled="loading"/, "expiry buttons must be disabled during a chain load");
assert.match(heatmapHeaders, /@click="selectHeatmapExpiration\(column\.expiration\)"/, "clicking a heatmap expiry must select that date");

const toggle = template.match(/<button class="chain-view-toggle"[\s\S]*?<\/button>/)?.[0] || "";
assert.match(toggle, /chainView === 'heatmap' \? '期权链' : 'GEX 热力图'/, "the view button must name the other available view");
assert.match(toggle, /切换到期权链表格[\s\S]*切换到 GEX 热力图/, "the view button title must describe the destination view");
assert.match(toggle, /el-icon-s-grid[\s\S]*el-icon-s-data/, "the view button must use the matching table or heatmap icon");
assert.match(toggle, /aria-hidden="true"/, "the decorative view icon must be hidden from assistive technology");
assert.doesNotMatch(toggle, /aria-pressed/, "the destination-action button must not report the opposite view as pressed");

assert.match(app, /function chainViewTitle\(symbol, expiration, view = state\.chainView\)/, "the panel title must be built from the current view");
assert.match(app, /const label = view === "heatmap" \? "GEX 热力图" : "期权链"/, "the title must track heatmap and table modes");
assert.match(app, /this\.view\.chainTitle = chainViewTitle\(state\.symbol, state\.expiration, this\.chainView\)/, "switching views must update the section title");
const toggleView = app.slice(app.indexOf("toggleChainView() {"), app.indexOf("toggleTheme() {"));
assert.match(toggleView, /this\.chainView = this\.chainView === "heatmap" \? "table" : "heatmap";\s*rememberChainViewPreference\(this\.chainView\)/, "each view switch must persist the selected mode");

const loadSymbol = app.slice(app.indexOf("async function loadSymbol()"), app.indexOf("let expirationSwitchToken"));
const loadExpirations = app.slice(app.indexOf("async function loadExpirations("), app.indexOf("function displayedSnapshotMatches()"));
assert.match(loadExpirations, /if \(state\.chainView === "heatmap"\) void loadChainHeatmapBackground\(payload\);\s*await loadChain\(\{ loadId \}\)/, "other expiry caches must load in the background once the current expiry is known");
assert.match(app, /async function loadChainHeatmapBackground\(catalogPayload = null\)[\s\S]*?const catalog = catalogPayload \|\| await request/, "the existing expiry catalog must be reused instead of fetching it again on initial load");
assert.match(app, /state\.view\.chainTitle = chainViewTitle\(state\.symbol, state\.expiration\);\s*return true;/, "selecting the current expiration must update the title before loading its chain");
assert.match(app, /state\.view\.chainTitle = chainViewTitle\(state\.symbol, expiration\)/, "changing expiry must immediately update the section title");
const selectHeatmapExpiration = app.slice(app.indexOf("selectHeatmapExpiration(expiration) {"), app.indexOf("filterChanged() {", app.indexOf("selectHeatmapExpiration(expiration) {")));
assert.match(selectHeatmapExpiration, /this\.loading \|\| !this\.expirationOptions\.includes\(expiration\) \|\| expiration === this\.expiration/, "expiry selection must ignore loading, unavailable, and already selected dates");
assert.match(selectHeatmapExpiration, /this\.expiration = expiration;\s*return switchExpiration\(expiration\)/, "the selector and dashboard must switch through the existing expiry loader");
const switchCalls = [];
const selectMethodContext = { switchExpiration(expiration) { switchCalls.push(expiration); return expiration; } };
vm.runInNewContext(`this.selectHeatmapExpiration = ({${selectHeatmapExpiration.trim()}}).selectHeatmapExpiration`, selectMethodContext);
const selection = {
  loading: false,
  expiration: "2026-11-06",
  expirationOptions: ["2026-11-06", "2026-11-13"],
};
assert.equal(selectMethodContext.selectHeatmapExpiration.call(selection, "2026-11-13"), "2026-11-13", "selecting another listed expiry must start the existing switch flow");
assert.equal(selection.expiration, "2026-11-13", "selecting a heatmap header must update the top selector state");
assert.deepEqual(switchCalls, ["2026-11-13"], "a valid heatmap expiry must invoke the existing loader once");
assert.equal(selectMethodContext.selectHeatmapExpiration.call(selection, "2026-11-13"), undefined, "selecting the current expiry must be a no-op");
assert.equal(selectMethodContext.selectHeatmapExpiration.call(selection, "2026-11-20"), undefined, "an unavailable expiry must be a no-op");
assert.equal(selectMethodContext.selectHeatmapExpiration.call({ ...selection, loading: true }, "2026-11-06"), undefined, "a pending chain load must block another expiry switch");
assert.deepEqual(switchCalls, ["2026-11-13"], "ignored expiry selections must not start extra loader calls");

console.log("Option-chain view UI contract checks passed");
