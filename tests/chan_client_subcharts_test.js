const assert = require("node:assert/strict");
const fs = require("node:fs");
const vm = require("node:vm");

const window = {};
vm.runInNewContext(fs.readFileSync("web/static/common/js/chan-plan.js", "utf8"), { window });
vm.runInNewContext(fs.readFileSync("web/static/common/js/chan-client.js", "utf8"), { window });

function frame(period = "1d") {
  const candles = Array.from({ length: 40 }, (_, index) => {
    const close = 10 + index * 0.1 + Math.sin(index / 2);
    return {
      date: `2026-10-${String(index + 1).padStart(2, "0")}`,
      open: close - 0.2,
      high: close + 0.8,
      low: close - 0.9,
      close,
      volume: 1000 + index * 10,
    };
  });
  return {
    period,
    label: period === "15m" ? "15分钟" : period === "4h" ? "4小时" : period === "1w" ? "周线" : "日线",
    as_of: candles[candles.length - 1].date,
    bar_count: candles.length,
    last_close: candles[candles.length - 1].close,
    candles,
    strokes: [],
    centers: [],
    segments: [],
    recursive_levels: [],
    recursive_centers: [],
    signals: [],
    signals_v4: [],
    position: { label: "观察", detail: "测试", center: null },
    quality: { price_basis: "raw", status: "ok", accepted_bars: candles.length, input_bars: candles.length, gaps: [] },
  };
}

const payload = { daily: frame(), weekly: frame("1w"), spot: 13, summary: {} };
const hidden = window.OptionScopeChan.view(payload).daily.chart;
assert.ok([hidden.volumeTop, hidden.macdTop, hidden.dmiTop].every(Number.isNaN), "副图默认全部隐藏");
assert.equal(hidden.height, hidden.priceBottom + 38, "隐藏所有副图时画布不保留空白区域");

for (const name of ["volume", "macd", "dmi"]) {
  const chart = window.OptionScopeChan.view(payload, true, false, 30, null, null, { [name]: true }).daily.chart;
  assert.equal([chart.volumeTop, chart.macdTop, chart.dmiTop].filter(Number.isFinite).length, 1, `${name} 单独开启时只显示自己的副图`);
  assert.ok(Number.isFinite(chart[`${name}Top`]), `${name} 单独开启时应获得绘图区`);
  assert.equal(chart.crosshairBottom, chart[`${name}Bottom`], `${name} 单独开启时十字线延伸到副图底部`);
  assert.equal(chart.height, chart[`${name}Bottom`] + 38, `${name} 单独开启时画布高度包含副图`);
}

const volumeAndDmi = window.OptionScopeChan.view(payload, true, false, 30, null, null, { volume: true, dmi: true }).daily.chart;
assert.ok(Number.isFinite(volumeAndDmi.volumeTop) && Number.isFinite(volumeAndDmi.dmiTop), "可同时开启不相邻的副图");
assert.ok(Number.isNaN(volumeAndDmi.macdTop), "未开启的 MACD 不占用绘图区");
assert.equal(volumeAndDmi.indicatorDividers.length, 1, "只在已开启副图之间绘制分隔线");
assert.equal(volumeAndDmi.crosshairBottom, volumeAndDmi.dmiBottom, "组合副图时十字线延伸到最下方副图");

const allSubcharts = window.OptionScopeChan.view(payload, true, false, 30, null, null, { volume: true, macd: true, dmi: true });
assert.ok([allSubcharts.daily.chart.volumeTop, allSubcharts.daily.chart.macdTop, allSubcharts.daily.chart.dmiTop].every(Number.isFinite), "三个副图可同时开启");
assert.equal(allSubcharts.daily.chart.indicatorDividers.length, 2, "三个副图之间绘制两条分隔线");
assert.match(allSubcharts.daily.chart.description, /价格图下方显示成交量、MACD（12、26、9）、DMI（14、6）/, "图表说明列出当前已启用副图");
assert.ok(Number.isFinite(allSubcharts.weekly.chart.volumeTop), "日线与周线共用副图选择");

const intraday = window.OptionScopeChan.intradayView({ period: "15m", frame: frame("15m"), spot: 13 }, true, null, false, 30, null, null, { macd: true }).chart;
assert.ok(Number.isFinite(intraday.macdTop), "盘中图表使用相同的副图偏好");
assert.ok(Number.isNaN(intraday.volumeTop) && Number.isNaN(intraday.dmiTop), "盘中图表隐藏未开启的副图");

const template = fs.readFileSync("web/static/index.html", "utf8");
const app = fs.readFileSync("web/static/common/js/app.js", "utf8");
const client = fs.readFileSync("web/static/common/js/chan-client.js", "utf8");
const charts = fs.readFileSync("web/static/common/js/charts.js", "utf8");
const styles = fs.readFileSync("web/static/common/css/styles.css", "utf8");
assert.match(template, /<details class="chan-subchart-menu"[^>]*@keydown\.esc\.stop\.prevent="closeChanLayerMenu"/, "副图菜单支持 Escape 关闭");
assert.match(template, /<div class="chan-layer-menu-panel chan-subchart-menu-panel">/, "副图面板复用图层面板容器");
assert.match(template, /<details class="chan-period-menu"[^>]*@keydown\.esc\.stop\.prevent="closeChanLayerMenu"/, "周期使用与副图一致的下拉按钮");
assert.match(template, /<summary aria-label="周期设置">周期<\/summary>/, "周期菜单显示统一的按钮标题");
assert.match(template, />递归买卖点<\/button>/, "递归买卖点使用明确图层名称");
assert.match(template, />笔级转折预警<\/button>/, "笔级转折预警使用明确图层名称");
assert.match(template, /<div class="chan-layer-menu-panel chan-period-menu-panel">[\s\S]*aria-label="K 线周期"/, "点击周期按钮后显示四个周期选项");
assert.doesNotMatch(template, /chan-period-control-group/, "周期不再使用常驻分段控件");
for (const [name, label] of [["volume", "成交量"], ["macd", "MACD"], ["dmi", "DMI"]]) {
  assert.ok(template.includes(`:aria-pressed="chanSubcharts.${name} ? 'true' : 'false'"`), `${label} 按钮提供 pressed 状态`);
  assert.ok(template.includes(`@click="toggleChanSubchart('${name}')">${label}</button>`), `${label} 按钮独立切换`);
}
assert.match(app, /const DEFAULT_CHAN_SUBCHARTS = \{ volume: false, macd: false, dmi: false \}/, "副图默认全部隐藏");
assert.match(app, /toggleChanSubchart\(name\) \{[\s\S]*?rememberChanChartPreferences\(this\.chanPeriod, this\.chanChartLayers, this\.chanSubcharts\)/, "切换副图后保存到当前偏好");
assert.match(client, /const subchartCacheKey = \["volume", "macd", "dmi"\][\s\S]*?\$\{subchartCacheKey\}/, "图表布局缓存区分副图组合");
assert.match(charts, /Array\.isArray\(model\.indicatorDividers\)[\s\S]*?for \(const dividerY of dividerPositions\)/, "画布只绘制已启用副图之间的分隔线");
assert.match(styles, /\.chan-subchart-menu-panel\{grid-template-areas:"display";grid-template-columns:minmax\(0,1fr\);width:min\(260px,calc\(100vw - 40px\)\)/, "副图面板按内容收窄并沿用图层网格结构");
assert.match(styles, /\.chan-layer-menu>summary,\.chan-subchart-menu>summary,\.chan-period-menu>summary\{min-height:44px\}/, "移动端三个菜单按钮统一为 44px 触控高度");
assert.match(styles, /\.chan-period-menu-panel\{grid-template-areas:"display";grid-template-columns:minmax\(0,1fr\);width:min\(330px,calc\(100vw - 40px\)\)/, "周期面板按四个选项收窄");
assert.match(app, /closest\("\.chan-layer-menu, \.chan-subchart-menu, \.chan-period-menu"\)/, "周期面板支持统一关闭处理");
assert.match(styles, /\.chan-control-options\{width:100%;align-items:center;flex-direction:row;flex-wrap:wrap;gap:8px\}/, "手机端菜单按钮横向排列并自动换行");
assert.match(styles, /\.chan-layer-menu-display \.chan-layer-empty\{display:block;flex:0 0 100%;width:100%;min-width:0;white-space:normal;overflow-wrap:anywhere;line-height:1\.5\}/, "图层状态提示独占一行并允许换行");
assert.match(styles, /@media\(max-width:760px\)\{[\s\S]*?\.chan-controls\{grid-template-columns:minmax\(0,1fr\);grid-template-rows:auto auto auto;grid-template-areas:"controls" "status" "levels";gap:10px\}[\s\S]*?\.chan-control-options\{position:relative;width:100%;align-items:center;flex-direction:row;flex-wrap:wrap;gap:8px\}[\s\S]*?\.chan-control-options>details\{position:static\}[\s\S]*?\.chan-layer-menu-panel\{position:absolute;top:calc\(100% \+ 8px\)/, "窄屏图表控制器保持单列，菜单跟随按钮组展开");

console.log("Chan subchart layout and UI checks passed");
