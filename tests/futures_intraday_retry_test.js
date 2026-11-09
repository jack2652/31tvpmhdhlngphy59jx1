const assert = require("assert");
const fs = require("fs");

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
