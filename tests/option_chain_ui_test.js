const assert = require("node:assert/strict");
const fs = require("node:fs");
const vm = require("node:vm");

const template = fs.readFileSync("web/static/index.html", "utf8");
const app = fs.readFileSync("web/static/common/js/app.js", "utf8");
const styles = fs.readFileSync("web/static/common/css/styles.css", "utf8");

assert.match(template, /<article v-if="!indexSymbol \|\| indexOptionsAvailable" class="metric panel">[\s\S]*?id="total-count"/, "supported indices must show the active expiry contract count");
assert.match(template, /<article v-if="!indexSymbol \|\| indexOptionsAvailable" class="metric call-metric panel">[\s\S]*?id="call-volume"/, "supported indices must show call volume");
assert.match(template, /<article v-if="!indexSymbol \|\| indexOptionsAvailable" class="metric put-metric panel">[\s\S]*?id="put-volume"/, "supported indices must show put volume");
assert.match(template, /<section v-show="!indexSymbol \|\| indexOptionsAvailable" class="detail-group panel" id="chart-group"/, "supported indices must show the GEX and distribution charts");
assert.match(template, /<section v-show="!indexSymbol \|\| indexOptionsAvailable" class="detail-group panel option-analysis-group" id="option-analysis-group"/, "supported indices must show the option analysis panel");
assert.match(template, /id="chain-title"[^>]*>GEX 热力图/, "supported indices must show the option-chain heatmap panel");
assert.ok(styles.includes(".headline-grid{grid-template-columns:minmax(240px,1.8fr) repeat(3,minmax(120px,.85fr)) repeat(2,minmax(220px,1.25fr));align-items:stretch}"), "desktop headline cards must share one six-column row");
assert.ok(styles.includes("@media(max-width:900px){.headline-grid{grid-template-columns:repeat(2,minmax(0,1fr))}.quote{grid-column:1/-1}}"), "headline cards must retain a responsive two-column layout on smaller screens");
assert.match(template, /<span class="eyebrow">OPTION ANALYSIS<\/span><span class="detail-title">期权买方结构分析<\/span>/, "the option-analysis panel must use the English eyebrow and Chinese buyer-structure title");
assert.match(template, /class="buyer-structure-fold" id="buyer-structure-fold"[\s\S]*?view\.buyer\.available/, "buyer structure must remain directly visible in the analysis panel");
assert.doesNotMatch(template, /class="buyer-structure-fold"[^>]*hidden/, "buyer structure must not remain hidden behind the removed view toggle");
assert.doesNotMatch(template, /期权流向|成交流向|data-option-view|option-flow|view\.flow/, "the option-flow view and its toggle must be removed from the template");
assert.doesNotMatch(app, /\/api\/flow\/|renderOptionFlow|resetOptionFlow|formatFlow(?:Interval|Strike|Volume)|view\.flow/, "the browser must not retain option-flow requests or state handlers");
assert.doesNotMatch(styles, /\.option-flow(?:-|\b)|\.flow-(?:overview|signal|window|kpi|directions|direction|strike|method|warning)/, "the stylesheet must not retain option-flow presentation rules");
const symbolLoad = app.slice(app.indexOf("async function loadSymbol()"), app.indexOf("let expirationSwitchToken"));
assert.match(symbolLoad, /await loadExpirations\(loadId, previousSnapshot\)/, "index expiry discovery must first reuse its cached catalog");
assert.doesNotMatch(symbolLoad, /await loadExpirations\(loadId, previousSnapshot, isIndexMarketSymbol\(symbol\)\)/, "loading an index must not force a slow upstream catalog refresh");

assert.match(app, /const CHAIN_VIEW_STORAGE_KEY = "option-scope-chain-view"/, "the selected option-chain view must use its own preference key");
assert.match(app, /function readChainViewPreference\(\)[\s\S]*?localStorage\.getItem\(CHAIN_VIEW_STORAGE_KEY\)[\s\S]*?stored === "table" \|\| stored === "profile" \|\| stored === "heatmap" \? stored : "heatmap"[\s\S]*?catch \(error\) \{ return "heatmap"; \}/, "only supported view values may be restored, with heatmap as the fallback");
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
preferenceContext.rememberChainViewPreference("profile");
assert.equal(preferenceContext.readChainViewPreference(), "profile", "a saved strike-profile choice must be restored after reload");
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
assert.match(template, /<span class="eyebrow">GEX 热力图<\/span>/, "the section eyebrow must use Chinese text");
assert.match(template, /class="table-wrap" v-show="chainView === 'table'"/, "the option-chain table must remain available as the alternate view");
assert.match(template, /class="chain-heatmap-panel" v-show="chainView === 'heatmap'"/, "the heatmap must be visible in the default view");
assert.match(template, /id="chain-view-select" v-model="chainView"[\s\S]*@change="changeChainView"/, "the view button must be replaced by a bound view selector");
assert.match(template, /<label class="chain-filter-label" for="chain-type-filter">类型<\/label>/, "the chain type filter must have an associated visible label");
assert.match(template, /<label class="chain-filter-label" for="chain-view-select">视图<\/label>/, "the view selector must have an associated visible label");
assert.doesNotMatch(template, /chain-expiration-select|chain-expiration-filter/, "the extra GEX expiry dropdown must be removed");
const profileExpirationToolbar = template.slice(template.indexOf('<div class="chain-profile-expiration-toolbar"'), template.indexOf('<div class="chain-profile-meta"'));
assert.match(profileExpirationToolbar, /v-for="column in view\.chainHeatmap\.columns"/, "profile expiry buttons must use the heatmap cache columns");
assert.match(profileExpirationToolbar, /:aria-current="column\.current \? 'date' : null"/, "the profile expiry strip must mark the active date");
assert.match(profileExpirationToolbar, /{{ column\.source }} {{ column\.updatedAt }}/, "the profile expiry strip must show cache source and update time");
assert.match(profileExpirationToolbar, /@click="selectHeatmapExpiration\(column\.expiration\)"/, "profile expiry buttons must reuse the heatmap date switch behavior");
const heatmapBatchRefreshButton = template.match(/<button v-show="chainView === 'heatmap'" class="chain-view-toggle chain-batch-refresh"[\s\S]*?<\/button>/)?.[0] || "";
const profileBatchRefreshButton = profileExpirationToolbar.match(/<button class="chain-view-toggle chain-batch-refresh chain-profile-batch-refresh"[\s\S]*?<\/button>/)?.[0] || "";
assert.match(heatmapBatchRefreshButton, /@click="refreshChainHeatmapExpirations"/, "heatmap batch refresh must remain available");
assert.match(profileBatchRefreshButton, /@click="refreshChainHeatmapExpirations"/, "profile batch refresh must sit beside the expiry strip");
assert.match(template, /class="chain-batch-refresh-status" role="status" aria-live="polite" v-show="chainView === 'profile' && !chainHeatmapRefreshRunning && chainBatchRefreshStatus"/, "profile batch refresh results must have a visible live status");
assert.match(app, /chainBatchRefreshStatus: ""/, "batch refresh result must have its own state");
const batchRefreshMethod = app.slice(app.indexOf("async refreshChainHeatmapExpirations() {"), app.indexOf("filterChanged() {", app.indexOf("async refreshChainHeatmapExpirations() {")));
assert.match(batchRefreshMethod, /this\.chainBatchRefreshStatus = status/, "batch refresh completion must populate the profile status");
assert.match(styles, /\.chain-view-select\{[^}]*color:var\(--muted\)[^}]*font-size:12px/, "the view label must match the type filter label color and size");
assert.match(styles, /\.chain-filter \.el-select \.el-input__inner,\.chain-view-select \.el-select \.el-input__inner\{[^}]*background:var\(--field\);color:var\(--text\);font:inherit\}/, "the view selector must share the type filter theme styles");
assert.match(styles, /\.chain-profile-expiration-strip\{[^}]*overflow-x:auto/, "profile expiry dates must scroll horizontally when they exceed the available width");
assert.match(styles, /\.chain-profile-expiration-button\{width:92px;height:auto;min-height:42px/, "profile expiry buttons must keep stable heatmap-sized tiles");
assert.match(template, /<el-option value="heatmap" label="GEX 热力图"><\/el-option><el-option value="profile" label="按行权价"><\/el-option><el-option value="table" label="期权链"><\/el-option>/, "the selector must offer all three Chinese view choices");
assert.match(template, /class="chain-profile-panel" v-show="chainView === 'profile'"/, "the strike-profile view must occupy the existing option-chain section");
assert.doesNotMatch(template, /view\.chainProfile\.history|chain-profile-history|行权价 GEX 最大变化/, "the strike-profile view must not expose browser-estimated historical samples");
assert.match(template, /成交量加权估算不代表真实方向性持仓/, "volume-weighted GEX must be identified as an estimate");
assert.match(template, /class="chain-profile-summary"[\s\S]*?netVolumeGex/, "profile metrics must remain available below the chart");
const heatmapHeaders = template.match(/<th v-for="column in view\.chainHeatmap\.columns"[\s\S]*?<\/th>/)?.[0] || "";
assert.match(heatmapHeaders, /<button class="chain-heatmap-expiration-button" type="button"/, "each heatmap expiry header must use a native button");
assert.match(heatmapHeaders, /:aria-label="'选择到期日 ' \+ column\.expiration"/, "each expiry button must expose its full date as its accessible name");
assert.match(heatmapHeaders, /:aria-current="column\.current \? 'date' : null"/, "the selected expiry must be announced as current");
assert.match(heatmapHeaders, /:disabled="loading"/, "expiry buttons must be disabled during a chain load");
assert.match(heatmapHeaders, /@click="selectHeatmapExpiration\(column\.expiration\)"/, "clicking a heatmap expiry must select that date");

assert.match(app, /function chainViewTitle\(symbol, expiration, view = state\.chainView\)/, "the panel title must be built from the current view");
assert.match(app, /function isGexChainView\(view = state\.chainView\)\s*\{\s*return view === "heatmap" \|\| view === "profile";/, "heatmap date caches must support both GEX views");
assert.match(app, /const labels = \{ heatmap: "GEX 热力图", profile: "按行权价 GEX", table: "期权链" \}/, "the title must track all three view modes");
const changeView = app.slice(app.indexOf("changeChainView(view) {"), app.indexOf("toggleTheme() {"));
assert.match(changeView, /this\.chainView = view;\s*rememberChainViewPreference\(view\)/, "each selected view must persist the selected mode");
assert.match(changeView, /if \(chainHeatmapLoadingSymbol !== state\.symbol\) void loadChainHeatmapBackground\(\)/, "entering the strike-profile view must load the heatmap expiry cache when idle");
assert.match(app, /aggregateByStrike\(rows, state\.chainSpot\)/, "the strike profile must estimate GEX from the currently displayed chain and spot");
assert.doesNotMatch(app, /loadChainProfileHistory|chainProfileHistorySamples|chainProfileMaxChange|chainProfileRequestToken|\/api\/chain\/[`$\{][^\s]+\/history/, "the option-chain UI must not request historical profile samples");

const loadSymbol = app.slice(app.indexOf("async function loadSymbol()"), app.indexOf("let expirationSwitchToken"));
const loadExpirations = app.slice(app.indexOf("async function loadExpirations("), app.indexOf("function displayedSnapshotMatches()"));
assert.match(loadExpirations, /if \(isGexChainView\(\)\) void loadChainHeatmapBackground\(payload\);\s*await loadChain\(\{ loadId \}\)/, "other expiry caches must load in the background for either GEX view");
const heatmapBackgroundStart = app.indexOf("async function loadChainHeatmapBackground(");
const heatmapBackgroundEnd = app.indexOf("\nasync function refreshHeatmapExpiration(", heatmapBackgroundStart);
const heatmapBackground = app.slice(heatmapBackgroundStart, heatmapBackgroundEnd);
assert.match(heatmapBackground, /let catalog = catalogPayload;[\s\S]*catalog = catalog \|\| await request/, "the initial expiry catalog must be reused when provided");
assert.match(heatmapBackground, /if \(chainHeatmapLoadingSymbol === symbol\) \{\s*chainHeatmapReloadRequested = true;\s*return;/, "directory refresh triggers during an in-flight scan must be coalesced");
assert.match(heatmapBackground, /const reloadRequested = chainHeatmapReloadRequested;\s*chainHeatmapReloadRequested = false;[\s\S]*if \(reloadRequested && symbol === state\.symbol && isGexChainView\(\)\) \{\s*void loadChainHeatmapBackground\(\);/, "a coalesced refresh must reread the latest directory in either GEX view");
assert.match(app, /const batchResult = status\?\.refresh\?\.result \|\| \{\};\s*\/\/ Gamma 每批可能把新的到期日写入 SQLite，完成后立即补读热力图目录。\s*if \(status\?\.refresh && isGexChainView\(\)\) \{\s*void loadChainHeatmapBackground\(\);/, "each completed Gamma batch must refresh the heatmap cache directory in either GEX view");
assert.match(app, /state\.view\.chainTitle = chainViewTitle\(state\.symbol, state\.expiration\);\s*return true;/, "selecting the current expiration must update the title before loading its chain");
assert.match(app, /state\.view\.chainTitle = chainViewTitle\(state\.symbol, expiration\)/, "changing expiry must immediately update the section title");
const selectHeatmapExpiration = app.slice(app.indexOf("selectHeatmapExpiration(expiration) {"), app.indexOf("filterChanged() {", app.indexOf("selectHeatmapExpiration(expiration) {")));
assert.match(selectHeatmapExpiration, /this\.loading \|\| \(!this\.expirationOptions\.includes\(expiration\) && !availableFromCache\) \|\| expiration === this\.expiration/, "expiry selection must allow cached dates and ignore loading, unavailable, and already selected dates");
assert.match(selectHeatmapExpiration, /this\.expiration = expiration;\s*return switchExpiration\(expiration\)/, "the selector and dashboard must switch through the existing expiry loader");
const switchCalls = [];
const selectMethodContext = {
  state: { symbol: "TEST" },
  isIndexMarketSymbol() { return false; },
  supportsIndexOptions() { return false; },
  chainHeatmapCache: { symbol: "TEST", availableExpirations: new Set(["2026-11-20"]) },
  isActiveExpiration() { return true; },
  switchExpiration(expiration) { switchCalls.push(expiration); return expiration; },
};
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
assert.equal(selectMethodContext.selectHeatmapExpiration.call(selection, "2026-11-20"), "2026-11-20", "a cached expiry missing from the selector must still be switchable");
assert.deepEqual(Array.from(selection.expirationOptions), ["2026-11-06", "2026-11-13", "2026-11-20"], "a selected cached expiry must be added to the selector");
assert.equal(selectMethodContext.selectHeatmapExpiration.call(selection, "2026-11-27"), undefined, "an unavailable expiry must be a no-op");
assert.equal(selectMethodContext.selectHeatmapExpiration.call({ ...selection, loading: true }, "2026-11-06"), undefined, "a pending chain load must block another expiry switch");
assert.deepEqual(switchCalls, ["2026-11-13", "2026-11-20"], "ignored expiry selections must not start extra loader calls");

async function verifyHeatmapDirectoryRefreshCoalescing(chainView) {
  const currentExpiration = "2026-11-06";
  const discoveredExpiration = "2026-11-13";
  const testState = {
    symbol: "TEST",
    loadId: 1,
    chainView,
    expiration: currentExpiration,
    expirationOptions: [currentExpiration],
    chainRows: [],
    chainFetchedSymbol: "",
    chainFetchedExpiration: "",
    chainFetchedAt: null,
    view: { chainHeatmapMeta: "", chainHeatmap: { columns: [], rows: [] } },
  };
  let resolveFirstCatalog;
  let catalogRequests = 0;
  const context = {
    state: testState,
    isIndexMarketSymbol() { return false; },
    supportsIndexOptions() { return false; },
    isGexChainView(view = testState.chainView) { return view === "heatmap" || view === "profile"; },
    CHAIN_HEATMAP_DISCOVERY_BASE_DELAY_MS: 4000,
    chainHeatmapLoadToken: 0,
    chainHeatmapLoadingSymbol: "",
    chainHeatmapReloadRequested: false,
    chainHeatmapDiscoveryDelayMs: 4000,
    chainHeatmapCache: { symbol: "", expirations: new Map(), availableExpirations: new Set() },
    cancelChainHeatmapDiscovery() {},
    renderChainHeatmap() {},
    activeExpirationDates(dates) { return [...new Set(dates || [])].sort(); },
    heatmapExpiryDates(dates) { return [...new Set(dates || [])].sort(); },
    applyExpirationCutoffs() {},
    applyExpirations(dates) { testState.expirationOptions = dates; },
    isActiveExpiration() { return true; },
    updateChainHeatmapMeta() {},
    scheduleChainHeatmapDiscovery() {},
    request(path) {
      if (path.startsWith("/api/expirations/")) {
        catalogRequests += 1;
        if (catalogRequests === 1) {
          return new Promise((resolve) => { resolveFirstCatalog = resolve; });
        }
        return Promise.resolve({
          expirations: [currentExpiration, discoveredExpiration],
          cached_expirations: [currentExpiration, discoveredExpiration],
          source: "sqlite",
        });
      }
      return Promise.resolve({
        data: [{ strike: 100 }],
        fetched_at: "2026-11-05T15:00:00Z",
        source: "sqlite",
      });
    },
  };
  vm.runInNewContext(`${heatmapBackground}\nthis.loadChainHeatmapBackground = loadChainHeatmapBackground;`, context);

  const initialLoad = context.loadChainHeatmapBackground();
  const coalescedLoad = context.loadChainHeatmapBackground();
  await coalescedLoad;
  assert.equal(context.chainHeatmapReloadRequested, true, "an in-flight directory read must remember a later refresh request");
  resolveFirstCatalog({
    expirations: [currentExpiration],
    cached_expirations: [currentExpiration],
    source: "sqlite",
  });
  await initialLoad;
  await new Promise((resolve) => setImmediate(resolve));

  assert.equal(catalogRequests, 2, "a coalesced refresh must reread the directory once after the original scan");
  assert.equal(context.chainHeatmapCache.expirations.has(discoveredExpiration), true, "the newly cached expiry must be added without reloading the page");
}

async function verifyBatchRefreshStatus() {
  const refreshMethodStart = app.indexOf("async refreshChainHeatmapExpirations() {");
  const refreshMethodEnd = app.indexOf("filterChanged() {", refreshMethodStart);
  const refreshMethod = app.slice(refreshMethodStart, refreshMethodEnd);
  const refreshedExpirations = [];
  const state = {
    symbol: "TEST",
    chainView: "profile",
    expiration: "2026-11-06",
  };
  const context = {
    state,
    isIndexMarketSymbol() { return false; },
    supportsIndexOptions() { return true; },
    heatmapExpiryDates(dates) { return dates; },
    chainHeatmapCache: { symbol: "TEST", expirations: new Map(), availableExpirations: new Set() },
    async refreshHeatmapExpiration(symbol, expiration) {
      refreshedExpirations.push(expiration);
      return { data: [], fetched_at: "2026-11-06T15:00:00Z" };
    },
    renderChainHeatmap() {},
  };
  vm.runInNewContext(`this.refreshChainHeatmapExpirations = ({${refreshMethod.trim()}}).refreshChainHeatmapExpirations`, context);
  const component = {
    loading: false,
    expirationOptions: ["2026-11-06", "2026-11-13"],
    chainHeatmapRefreshRunning: false,
    chainHeatmapRefreshProgress: "",
    chainHeatmapRefreshTarget: "",
    chainBatchRefreshStatus: "旧状态",
    view: { chainHeatmap: { columns: [] }, chainHeatmapMeta: "" },
  };

  await context.refreshChainHeatmapExpirations.call(component);

  assert.deepEqual(refreshedExpirations, ["2026-11-13"], "batch refresh must continue to refresh other expiries when invoked from the profile view");
  assert.match(component.chainBatchRefreshStatus, /批量刷新完成：成功 1\/1/, "profile must receive a completion status after batch refresh");
  assert.equal(component.chainHeatmapRefreshRunning, false, "batch refresh must release its running state after completion");
}

Promise.all([
  verifyHeatmapDirectoryRefreshCoalescing("heatmap"),
  verifyHeatmapDirectoryRefreshCoalescing("profile"),
  verifyBatchRefreshStatus(),
]).then(() => {
  console.log("Option-chain view UI contract checks passed");
}).catch((error) => {
  console.error(error);
  process.exitCode = 1;
});
