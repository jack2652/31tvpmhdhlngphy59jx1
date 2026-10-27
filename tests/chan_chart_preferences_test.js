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
assert.deepEqual(plain(defaultPreferences.subcharts), { volume: false, macd: false, dmi: false }, "new preferences default all subcharts to hidden");
assert.equal(defaultPreferences.layers.afStochZ, false, "new preferences default AF StochZ layer to hidden");
assert.deepEqual(defaults.readStored().subcharts, { volume: false, macd: false, dmi: false }, "default subchart choices are persisted");

const previouslyCustomizedLayers = {
  pens: false,
  penCenters: false,
  legacySignals: false,
  chanCandles: true,
  segments: true,
  signals: true,
  segmentLevels: {},
  recursiveCenters: {},
};
const versionFour = preferenceContext({
  version: 4,
  period: "weekly",
  layers: previouslyCustomizedLayers,
  subcharts: { volume: true, macd: false, dmi: true },
});
const upgradedVersionFour = versionFour.api.readChanChartPreferences();
assert.deepEqual(plain(upgradedVersionFour.layers), previouslyCustomizedLayers, "version 4 custom layers that resemble older defaults remain unchanged");
assert.deepEqual(plain(upgradedVersionFour.subcharts), { volume: true, macd: false, dmi: true }, "version 4 subchart choices survive preference migration");
assert.equal(versionFour.readStored().version, 6, "version 4 preferences are upgraded to the current format");

const versionThree = preferenceContext({
  version: 3,
  period: "daily",
  layers: previouslyCustomizedLayers,
});
const upgradedVersionThree = versionThree.api.readChanChartPreferences();
assert.equal(upgradedVersionThree.layers.pens, true, "the original pre-version-4 default migration remains active");
assert.equal(upgradedVersionThree.layers.segments, false, "the original pre-version-4 default migration keeps its current default");
assert.deepEqual(plain(upgradedVersionThree.subcharts), { volume: false, macd: false, dmi: false }, "older preferences receive hidden subchart defaults");

versionFour.api.rememberChanChartPreferences("weekly", upgradedVersionFour.layers, { volume: false, macd: true, dmi: false });
const reloaded = preferenceContext(versionFour.readStored()).api.readChanChartPreferences();
assert.deepEqual(plain(reloaded.subcharts), { volume: false, macd: true, dmi: false }, "enabled subcharts persist through a page reload");
assert.equal(reloaded.layers.afStochZ, false, "AF StochZ keeps its hidden default through migration");
assert.equal(reloaded.period, "weekly", "chart preference persistence keeps the selected period");

console.log("Chan chart preference migration and persistence checks passed");
