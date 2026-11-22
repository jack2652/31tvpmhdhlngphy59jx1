const assert = require("node:assert/strict");
const fs = require("node:fs");
const vm = require("node:vm");

const app = fs.readFileSync("web/static/common/js/app.js", "utf8");
const start = app.indexOf("const CHAN_SCENARIOS_STORAGE_KEY =");
const end = app.indexOf("const initialChanChartPreferences =", start);
assert.ok(start >= 0 && end > start, "chart preference code should remain a self-contained app section");
const preferenceSource = `${app.slice(start, end)}
globalThis.preferenceApi = { readChanChartPreferences, rememberChanChartPreferences };`;
const storageKey = "option-scope-chan-chart-preferences";

function preferenceContext(initialValue = null) {
  const values = new Map();
  if (initialValue !== null) values.set(storageKey, JSON.stringify(initialValue));
  const localStorage = {
    getItem(key) { return values.get(key) ?? null; },
    setItem(key, value) { values.set(key, String(value)); },
  };
  const context = { localStorage };
  vm.runInNewContext(preferenceSource, context);
  return {
    api: context.preferenceApi,
    readStored() {
      const value = values.get(storageKey);
      return value ? JSON.parse(value) : null;
    },
  };
}

function plain(value) {
  return JSON.parse(JSON.stringify(value));
}

const defaults = preferenceContext();
const defaultPreferences = defaults.api.readChanChartPreferences();
assert.deepEqual(plain(defaultPreferences.subcharts), { volume: false, macd: false, dmi: false, mfi: false }, "new preferences default all subcharts to hidden");
assert.equal(defaultPreferences.layers.afStochZ, false, "new preferences default AF StochZ layer to hidden");
assert.equal(defaultPreferences.layers.turtle, false, "new preferences default Turtle layer to hidden");
assert.equal(defaultPreferences.layers.macdSma200, false, "new preferences default MACD + SMA200 layer to hidden");
assert.equal(defaultPreferences.layers.mfi, false, "new preferences default MFI layer to hidden");
assert.deepEqual(defaults.readStored().subcharts, { volume: false, macd: false, dmi: false, mfi: false }, "default subchart choices are persisted");
assert.doesNotMatch(app, /const EXCLUSIVE_CHAN_INDICATOR_LAYERS = \[.*"mfi"/, "MFI is independent from price-overlay indicator exclusivity");

const toggleStart = app.indexOf("    toggleChanChartLayer(layer) {");
const toggleEnd = app.indexOf("    toggleChanSegmentLevel(level) {", toggleStart);
const toggleSource = app.slice(toggleStart, toggleEnd).trim().replace(/,\s*$/, "");
const toggleContext = { window: {} };
vm.runInNewContext(fs.readFileSync("web/static/common/js/chan-client.js", "utf8"), toggleContext);
vm.runInNewContext(`
  const EXCLUSIVE_CHAN_INDICATOR_LAYERS = ["afStochZ", "turtle", "granville", "macdSma200", "supertrend", "wyckoff"];
  const OptionScopeChan = window.OptionScopeChan;
  const rememberChanChartPreferences = (...values) => { globalThis.savedPreferences = values; };
  globalThis.toggleChanChartLayer = ({ ${toggleSource} }).toggleChanChartLayer;
`, toggleContext);
const layerState = {
  chanChartLayers: { afStochZ: true, turtle: false, granville: false, macdSma200: false, supertrend: false, wyckoff: false, mfi: false },
  chanPeriod: "daily",
  chanSubcharts: {},
  chanPayload: null,
};
toggleContext.toggleChanChartLayer.call(layerState, "macdSma200");
assert.deepEqual(plain(layerState.chanChartLayers), { afStochZ: false, turtle: false, granville: false, macdSma200: true, supertrend: false, wyckoff: false, mfi: false }, "enabling MACD + SMA200 disables the previously selected exclusive indicator");
toggleContext.toggleChanChartLayer.call(layerState, "macdSma200");
assert.deepEqual(plain(layerState.chanChartLayers), { afStochZ: false, turtle: false, granville: false, macdSma200: false, supertrend: false, wyckoff: false, mfi: false }, "the selected MACD + SMA200 indicator can be disabled");
const supportLayerState = { chanChartLayers: { supportResistance: true } };
toggleContext.toggleChanChartLayer.call(supportLayerState, "supportResistance");
assert.equal(supportLayerState.chanChartLayers.supportResistance, false, "one switch disables both kinds of price levels");
assert.ok(!("structureSupports" in supportLayerState.chanChartLayers), "the merged switch must not create a second layer state");

// 日期：2026-11-23；执行者：Codex。使用真实app按钮方法及共享判断验证开启和刷新保留。
const segmentAppState = { chanChartLayers: { ...defaultPreferences.layers, segments: false, segmentLevels: {} },
  chanSegmentLevels: [0, 1], chanPeriod: "daily", chanSubcharts: defaultPreferences.subcharts, chanPayload: null };
const segmentApi = toggleContext.window.OptionScopeChan;
toggleContext.toggleChanChartLayer.call(segmentAppState, "segments");
assert.equal(segmentApi.allSegmentLevelsVisible(segmentAppState.chanChartLayers, [0, 1]), true, "真实全部级别按钮必须开启实际级别");
const segmentPreferences = preferenceContext();
segmentPreferences.api.rememberChanChartPreferences(...toggleContext.savedPreferences);
const reloadedSegments = preferenceContext(segmentPreferences.readStored()).api.readChanChartPreferences();
assert.equal(segmentApi.allSegmentLevelsVisible(reloadedSegments.layers, [0, 1]), true, "刷新重读偏好仍显示全部级别");
toggleContext.toggleChanChartLayer.call(segmentAppState, "segments");
assert.equal(segmentApi.allSegmentLevelsVisible(segmentAppState.chanChartLayers, [0, 1]), false, "再次点击全部级别仍可隐藏");

for (const [supportResistance, structureSupports] of [[false, false], [false, true], [true, false], [true, true]]) {
  const previous = preferenceContext({ version: 14, period: "daily", layers: { supportResistance, structureSupports }, subcharts: {} });
  const updated = previous.api.readChanChartPreferences();
  assert.equal(updated.layers.supportResistance, supportResistance || structureSupports, "migration preserves either enabled price layer");
  assert.ok(!("structureSupports" in updated.layers), "the old independent state must be removed");
  assert.equal(previous.readStored().version, 15);
  assert.ok(!("structureSupports" in previous.readStored().layers), "reloaded preferences persist only the shared switch");
}

const selectedMacdSma200 = { ...defaultPreferences.layers, macdSma200: true };
defaults.api.rememberChanChartPreferences("daily", selectedMacdSma200, defaultPreferences.subcharts);
assert.equal(preferenceContext(defaults.readStored()).api.readChanChartPreferences().layers.macdSma200, true, "enabled MACD + SMA200 selection persists through reload");

const previouslyCustomizedLayers = {
  pens: false,
  penCenters: false,
  legacySignals: false,
  chanCandles: true,
  segments: true,
  signals: true,
  segmentLevels: {},
  recursiveCenters: {},
  supportResistance: true,
  afStochZ: false,
  trendlines: false,
  turtle: false,
  granville: false,
  macdSma200: false,
  supertrend: false,
  wyckoff: false,
  mfi: false,
};
const versionFour = preferenceContext({
  version: 4,
  period: "weekly",
  layers: previouslyCustomizedLayers,
  subcharts: { volume: true, macd: false, dmi: true },
});
const upgradedVersionFour = versionFour.api.readChanChartPreferences();
assert.deepEqual(plain(upgradedVersionFour.layers), previouslyCustomizedLayers, "version 4 custom layers remain unchanged after merging the price controls");
assert.deepEqual(plain(upgradedVersionFour.subcharts), { volume: true, macd: false, dmi: true, mfi: false }, "version 4 subchart choices survive preference migration");
assert.equal(versionFour.readStored().version, 15, "version 4 preferences are upgraded to the current format");

const versionThree = preferenceContext({
  version: 3,
  period: "daily",
  layers: previouslyCustomizedLayers,
});
const upgradedVersionThree = versionThree.api.readChanChartPreferences();
assert.equal(upgradedVersionThree.layers.pens, true, "the original pre-version-4 default migration remains active");
assert.equal(upgradedVersionThree.layers.segments, false, "the original pre-version-4 default migration keeps its current default");
assert.deepEqual(plain(upgradedVersionThree.subcharts), { volume: false, macd: false, dmi: false, mfi: false }, "older preferences receive hidden subchart defaults");

versionFour.api.rememberChanChartPreferences("weekly", upgradedVersionFour.layers, { volume: false, macd: true, dmi: false });
const reloaded = preferenceContext(versionFour.readStored()).api.readChanChartPreferences();
assert.deepEqual(plain(reloaded.subcharts), { volume: false, macd: true, dmi: false, mfi: false }, "enabled subcharts persist through a page reload");
assert.equal(reloaded.layers.afStochZ, false, "AF StochZ keeps its hidden default through migration");
assert.equal(reloaded.layers.turtle, false, "Turtle keeps its hidden default through migration");
assert.equal(reloaded.layers.macdSma200, false, "MACD + SMA200 keeps its hidden default through migration");
assert.equal(reloaded.period, "weekly", "chart preference persistence keeps the selected period");

const legacyMfi = preferenceContext({
  version: 13,
  period: "daily",
  layers: { ...previouslyCustomizedLayers, mfi: true },
  subcharts: { volume: false, macd: false, dmi: false },
});
const migratedMfi = legacyMfi.api.readChanChartPreferences();
assert.equal(migratedMfi.subcharts.mfi, true, "legacy MFI layer migrates into the subchart preference");
assert.equal(migratedMfi.layers.mfi, true, "migrated MFI keeps the renderer flag enabled");

// 期货页沿用同样的合并规则，避免两个入口在升级旧选择时出现差异。
const futures = fs.readFileSync("web/static/common/js/futures.js", "utf8");
const futuresDefinitions = futures.slice(futures.indexOf("  const defaultLayers ="), futures.indexOf("  const scenarioVisibilityKey ="));
const futuresFunctions = futures.slice(futures.indexOf("  function copyLayers("), futures.indexOf("  function marketStateLabel("));
for (const [supportResistance, structureSupports] of [[false, false], [false, true], [true, false], [true, true]]) {
  const values = new Map([["option-scope-futures-chart-preferences", JSON.stringify({ version: 1, period: "daily", layers: { supportResistance, structureSupports }, subcharts: {} })]]);
  const context = { localStorage: { getItem: (key) => values.get(key) ?? null, setItem: (key, value) => values.set(key, String(value)) } };
  vm.runInNewContext(`${futuresDefinitions}\nconst periods = [{value: "daily"}];\n${futuresFunctions}\nthis.readChartPreferences = readChartPreferences;`, context);
  const updated = context.readChartPreferences();
  assert.equal(updated.layers.supportResistance, supportResistance || structureSupports);
  assert.ok(!("structureSupports" in updated.layers));
  const stored = JSON.parse(values.get("option-scope-futures-chart-preferences"));
  assert.equal(stored.version, 2);
  assert.ok(!("structureSupports" in stored.layers));
}

console.log("Chan chart preference migration and persistence checks passed");
