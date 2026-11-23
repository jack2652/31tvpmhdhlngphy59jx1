const assert = require("assert");
const fs = require("fs");
const vm = require("node:vm");

const source = fs.readFileSync("web/static/common/js/futures.js", "utf8");

assert.match(source, /const intradayPeriods = \["15m", "1h", "2h", "4h"\]/,
  "期货页盘中周期必须包含1小时和2小时");
assert.match(source, /function intradayFrameReady\(payload\)/,
  "期货页必须区分有 K 线的响应和后台刷新空帧");
assert.match(source, /scheduleIntradayRetry\(value\)/,
  "期货页必须为盘中空帧安排重试");
assert.match(source, /if \(intradayFrameReady\(payload\) \|\| !intradayFrameReady\(existing\)\)/,
  "刷新中的空响应不能覆盖已经可显示的指标数据");
assert.match(source, /if \(intradayPeriods\.includes\(value\)\) this\.loadIntraday\(value\);/,
  "切换任意盘中周期都必须触发数据加载或重试");

console.log("Futures intraday empty-frame retry checks passed");

// 2026-11-23，Codex：执行真实边界刷新函数，验证各周期仅取已闭合区间及35秒等待。
const appSource = fs.readFileSync("web/static/common/js/app.js", "utf8");
let fakeNow = Date.parse("2026-10-23T14:30:34Z");
class ReplayDate extends Date { constructor(...args) { super(...(args.length ? args : [fakeNow])); } static now() { return fakeNow; } }
const calls = [], scheduled = [];
const timingState = { symbol: "QQQ", chanPeriod: "15m", chanIntradayLoading: {}, chanIntradayPayload: {}, lastQuote: { market_state: "REGULAR" } };
const timing = { Date: ReplayDate, Intl, state: timingState, quoteIsFresh: () => true,
  setTimeout: (fn, delay) => { scheduled.push({ fn, delay }); return scheduled.length; }, clearTimeout: () => {},
  window: { optionScopeApp: { ensureChanIntraday: (...args) => calls.push(args) } } };
vm.createContext(timing);
vm.runInContext(appSource.slice(appSource.indexOf("const CHAN_INTRADAY_PERIODS"), appSource.indexOf("const CHAN_CHART_PREFERENCES_VERSION"))
  + appSource.slice(appSource.indexOf("function newYorkIntradayParts"), appSource.indexOf('document.addEventListener("visibilitychange"', appSource.indexOf("function newYorkIntradayParts"))), timing);
const periods = { "15m": "10:15", "1h": "09:30", "2h": "09:30" };
for (const [period, expected] of Object.entries(periods)) {
  timingState.chanPeriod = period;
  timingState.chanIntradayPayload[period] = { frame: { candles: [{ date: "2026-10-23 09:30" }] } };
  fakeNow = Date.parse("2026-10-23T15:30:34Z"); // 美东11:30，2小时也已闭合。
  timingState.chanIntradayPayload[period].frame.candles[0].date = "2026-10-22 09:30";
  const priorCount = calls.length;
  vm.runInContext('refreshChanIntradayAtBoundary("QQQ")', timing);
  assert.equal(calls.length, priorCount, "收盘34秒时还不请求");
  fakeNow += 1000;
  vm.runInContext('refreshChanIntradayAtBoundary("QQQ")', timing);
  assert.equal(calls.length, priorCount + 1, `${period} 收盘35秒必须请求`);
  assert.equal(calls.at(-1)[0], period);
  vm.runInContext('refreshChanIntradayAtBoundary("QQQ")', timing);
  assert.equal(calls.length, priorCount + 1, "同一闭合区间不能重复抓取");
  const latest = vm.runInContext(`previousIntradayCandleDate(newYorkIntradayParts(), ${JSON.stringify(period)})`, timing);
  assert.equal(latest, `2026-10-23 ${period === "15m" ? "11:15" : period === "1h" ? "10:30" : expected}`);
}
timingState.chanPeriod = "4h";
timingState.chanIntradayPayload["4h"] = { frame: { candles: [{ date: "2026-10-23 09:30" }] } };
fakeNow = Date.parse("2026-10-23T17:15:35Z");
const before4h = calls.length;
vm.runInContext('refreshChanIntradayAtBoundary("QQQ")', timing);
assert.equal(calls.length, before4h, "4小时未闭合时不请求新结构");
timingState.chanIntradayPayload["4h"].frame.candles[0].date = "2026-10-22 09:30";
fakeNow = Date.parse("2026-10-23T17:30:35Z");
vm.runInContext('refreshChanIntradayAtBoundary("QQQ")', timing);
assert.equal(calls.length, before4h + 1);
assert.equal(calls.at(-1)[0], "4h");
vm.runInContext('scheduleChanIntradayBoundaryRefresh("4h", "QQQ")', timing);
assert.equal(scheduled.at(-1).delay, 15 * 60 * 1000, "下一15分钟边界调度仍保持35秒偏移");
vm.runInContext('scheduleChanIntradayCacheExpiryRetry("QQQ", "2026-10-23 09:30", "2026-10-23T17:30:30Z", false, "4h")', timing);
assert.equal(scheduled.at(-1).delay, 30000, "重试等待对应30秒TTL而非旧三分钟");
console.log("Chan intraday boundary timing checks passed");
