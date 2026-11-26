const assert = require("node:assert/strict");
const fs = require("node:fs");
const vm = require("node:vm");
const { test } = require("node:test");

function setup() {
  const timers = new Map(), storage = new Map();
  let timer = 0, now = 0;
  const window = {
    document: { hidden: false },
    localStorage: { getItem: (key) => storage.get(key), setItem: (key, value) => storage.set(key, value) },
    setTimeout: (callback, delay) => (timers.set(++timer, { callback, delay }), timer),
    clearTimeout: (id) => timers.delete(id),
  };
  vm.runInNewContext(fs.readFileSync("web/static/common/js/chan-relative-strength.js", "utf8"),
    { window, URL, Date: { now: () => now } });
  return { ...window.OptionScopeRelativeStrength, timers, advance: (elapsed) => { now += elapsed; } };
}

const bar = (date, close, is_forming = false, source_date) => ({ date, close, is_forming, source_date });

test("选择标的基准，盘中统一用原价，并支持显式同标的比较", () => {
  const { benchmarkRequest } = setup();
  assert.equal(benchmarkRequest(false, "ORCL", "daily", "raw", "auto"), "");
  assert.equal(benchmarkRequest(true, "ORCL", "daily", "split_dividend_adjusted", "auto"),
    "/api/chan-benchmark/SPY?period=1d&basis=split_dividend_adjusted");
  assert.equal(benchmarkRequest(true, "NQ=F", "1h", "split_dividend_adjusted", "auto"),
    "/api/chan-benchmark/QQQ?period=1h&basis=raw");
  assert.match(benchmarkRequest(true, "QQQ", "weekly", "raw", "auto"), /SPY\?period=1wk/);
  assert.match(benchmarkRequest(true, "SPY", "weekly", "raw", "auto"), /QQQ\?period=1wk/);
  assert.match(benchmarkRequest(true, "QQQ", "daily", "raw", "QQQ"), /&self=true$/);
});

test("只使用同时点行情，视口首个重合点为 100，盘中观察不确认跑赢跑输切换", () => {
  const { calculate } = setup();
  for (const dates of [
    ["2026-10-23", "2026-10-26", "2026-10-27", "2026-10-28", "2026-10-29", "2026-10-30"],
    ["2026-10-23 09:30", "2026-10-23 09:45", "2026-10-23 10:00", "2026-10-23 10:15", "2026-10-23 10:30", "2026-10-23 10:45"],
  ]) {
    const sourceDate = "2026-10-30 10:45";
    const bars = [100, 100, 120, 120, 80, 130].map((close, index) => bar(dates[index], close, index === 5, sourceDate));
    const benchmark = { history: { bars: [1, 2, 4, 5].map((index) => bar(dates[index], 200)),
      live_bar: bar(dates[5], 200, true, sourceDate) } };
    const points = calculate(bars, benchmark);
    assert.deepEqual(Array.from(points, (point) => point?.value ?? null), [null, 100, 120, null, 80, 130]);
    assert.equal(points[4].changed, true);
    assert.equal(points[5].changed, false);
    assert.equal(points[5].live, true);
    assert.equal(calculate(bars.slice(2), benchmark)[0].value, 100);
    assert.equal(calculate(bars, { history: { bars: benchmark.history.bars } })[5], null);
    assert.equal(calculate(bars, { history: { ...benchmark.history,
      live_bar: bar(dates[5], 200, true, "2026-10-30 10:30") } })[5], null);
    assert.equal(calculate(bars, { history: { ...benchmark.history,
      live_bar: bar(dates[5], 200, true) } })[5], null);
    assert.equal(calculate(bars, { history: { live_bar: benchmark.history.live_bar } })[4], null);
    assert.deepEqual(Array.from(calculate(bars, { self: true }), (point) => point.value), [100, 100, 100, 100, 100, 100]);
  }
});

test("复用进行中的基准请求，切换及销毁后忽略过期响应，刷新失败保留历史", async () => {
  const { benchmarkMixin, advance, timers } = setup();
  const requests = [];
  const mixin = benchmarkMixin((path) => {
    const pending = new Promise((resolve, reject) => { requests.push({ path, resolve, reject }); });
    // 期货页请求适配器返回只有 then 的 jQuery Promise。
    return { then: pending.then.bind(pending) };
  });
  const state = { ...mixin.data(), ...mixin.methods };
  const load = (path) => {
    state.rsRequest = path;
    return state.refreshRsHistory();
  };
  const qqq = "/api/chan-benchmark/QQQ?period=1d&basis=raw";
  const spy = "/api/chan-benchmark/SPY?period=1d&basis=raw";
  assert.equal(state.rsPreference, "auto");
  mixin.watch.rsPreference("QQQ");
  assert.equal(mixin.data().rsPreference, "QQQ");
  const first = load(qqq), switched = load(spy), back = load(qqq);
  assert.deepEqual(requests.map((item) => item.path), [qqq, spy]);
  requests[0].resolve({ bars: [{ close: 10 }], refreshing: true });
  await Promise.all([first, back]);
  assert.equal(state.rsHistory.bars[0].close, 10);
  assert.equal(timers.size, 1);
  assert.equal(timers.get(state.rsTimer).delay, 3000);
  requests[1].resolve({ bars: [{ close: 20 }] });
  await switched;
  assert.equal(state.rsHistory.path, qqq);
  advance(3000);
  const failed = load(qqq);
  requests[2].reject(new Error("服务暂不可用"));
  await failed;
  assert.equal(state.rsHistory.bars[0].close, 10);
  advance(15000);
  const last = load(qqq);
  mixin.beforeDestroy.call(state);
  requests[3].resolve({ bars: [{ close: 99 }] });
  await last;
  assert.equal(state.rsHistory.bars[0].close, 10);
  assert.equal(timers.size, 0);
});
