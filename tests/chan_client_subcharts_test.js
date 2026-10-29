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

window.OptionScopeGranville = {
  calculate(candles) {
    return {
      sma: new Array(candles.length).fill(null),
      events: [{ index: 30, date: candles[30].date, rule: 8, side: "sell", price: candles[30].close }],
    };
  },
};
const crowdedFrame = frame();
crowdedFrame.candles[30] = { ...crowdedFrame.candles[30], open: 14, high: 15, low: 13, close: 14.5 };
crowdedFrame.candles[31] = { ...crowdedFrame.candles[31], open: 14.5, high: 15.2, low: 14, close: 14.2 };
crowdedFrame.candles[29] = { ...crowdedFrame.candles[29], high: 17, low: 13.5 };
crowdedFrame.merged_candles = [{ raw_start: 29, raw_end: 30, high: 17, low: 13, chain_id: 1 }];
const crowdedPayload = { daily: crowdedFrame, weekly: frame("1w"), spot: 14, summary: {} };
for (const mobileChart of [false, true]) {
  const chart = window.OptionScopeChan.view(
    crowdedPayload,
    true,
    mobileChart,
    32,
    { daily: { visibleBars: 32 } },
    { granville: true },
  ).daily.chart;
  assert.equal(chart.granvilleEvents.length, 1, "标签避让不删除对应的葛兰碧信号三角");
  const event = chart.granvilleEvents[0];
  assert.ok(Number.isFinite(event.y) && Number.isFinite(event.markerRadius), "即使隐藏拥挤文字仍保留信号三角坐标");
  assert.equal(event.labelVisible, true, "桌面与手机视口能为测试信号找到无重叠位置");
  const mergedCandle = chart.chanCandles.find((candle) => candle.candleIndex === chart.candles.find((item) => item.date === event.date)?.index);
  const signalCandle = chart.candles.find((candle) => candle.date === event.date);
  assert.ok(mergedCandle && signalCandle && mergedCandle.high < signalCandle.high, "测试 K 线的合并影线扩展到原始影线之外");
  const labelBox = {
    left: event.x - Array.from(event.label).length * event.fontSize / 2 - 3,
    right: event.x + Array.from(event.label).length * event.fontSize / 2 + 3,
    top: event.labelY - event.fontSize - 2,
    bottom: event.labelY + 2,
  };
  const slot = chart.candles[1].x - chart.candles[0].x;
  const horizontalGap = Math.max(4, Math.min(6, slot * 0.2));
  const verticalGap = mobileChart ? 6 : 5;
  for (const candle of chart.candles) {
    const chanCandle = chart.chanCandles.find((item) => item.candleIndex === candle.index);
    const candleTop = Math.min(candle.high, candle.low, candle.body, candle.body + candle.bodyHeight, chanCandle?.high ?? Infinity, chanCandle?.low ?? Infinity);
    const candleBottom = Math.max(candle.high, candle.low, candle.body, candle.body + candle.bodyHeight, chanCandle?.high ?? -Infinity, chanCandle?.low ?? -Infinity);
    const halfWidth = candle.bodyWidth / 2 + 1 + horizontalGap;
    const overlaps = labelBox.left < candle.x + halfWidth + 2
      && labelBox.right + 2 > candle.x - halfWidth
      && labelBox.top < candleBottom + verticalGap + 2
      && labelBox.bottom + 2 > candleTop - verticalGap;
    assert.equal(overlaps, false, `${mobileChart ? "手机" : "桌面"}标签不得覆盖原始或合并 K 线影线`);
  }
}

const template = fs.readFileSync("web/static/index.html", "utf8");
const app = fs.readFileSync("web/static/common/js/app.js", "utf8");
const client = fs.readFileSync("web/static/common/js/chan-client.js", "utf8");
const charts = fs.readFileSync("web/static/common/js/charts.js", "utf8");
const styles = fs.readFileSync("web/static/common/css/styles.css", "utf8");
assert.match(template, /<details class="chan-subchart-menu"[^>]*@keydown\.esc\.stop\.prevent="closeChanLayerMenu"/, "副图菜单支持 Escape 关闭");
assert.match(template, /<div class="chan-layer-menu-panel chan-subchart-menu-panel">/, "副图面板复用图层面板容器");
assert.match(template, /<details class="chan-period-menu"[^>]*@keydown\.esc\.stop\.prevent="closeChanLayerMenu"/, "周期使用与副图一致的下拉按钮");
assert.match(template, /<summary aria-label="周期设置">周期<\/summary>/, "周期菜单显示统一的按钮标题");
assert.match(template, /<summary>递归买卖点/, "递归买卖点使用明确折叠标题");
assert.match(template, /<summary>笔级转折预警/, "笔级转折预警使用明确折叠标题");
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
assert.match(template, /<details class="chan-subchart-menu"[\s\S]*?<\/details>[\s\S]*?<details class="chan-indicator-menu"/, "指标入口位于副图入口右侧");
assert.match(template, /<summary aria-label="技术指标设置">指标<\/summary>/, "指标菜单有明确的可访问名称");
assert.doesNotMatch(template, /<details class="chan-indicator-description"/, "指标名称不使用下拉式 disclosure");
assert.doesNotMatch(app, /chanIndicatorHelp|toggleChanIndicatorHelp/, "不维护与图层状态重复的说明状态");
assert.match(template, /<div class="chan-indicator-options">[\s\S]*?aria-controls="chan-af-stochz-help" @click="toggleChanChartLayer\('afStochZ'\)">AF StochZ<\/button>[\s\S]*?aria-controls="chan-turtle-help" @click="toggleChanChartLayer\('turtle'\)">海龟交易法<\/button>[\s\S]*?aria-controls="chan-granville-help" @click="toggleChanChartLayer\('granville'\)">葛兰碧法则<\/button>/, "AF StochZ、海龟和葛兰碧按顺序位于指标菜单并直接切换图层");
const signalMarkerSection = template.match(/<section class="chan-layer-menu-signals">[\s\S]*?<\/section>/)?.[0] || "";
assert.doesNotMatch(signalMarkerSection, />AF StochZ<\/button>/, "AF StochZ 不再归入缠论信号标记");
assert.match(template, /id="chan-turtle-help" class="chan-indicator-description-body"[^>]*v-show="chanChartLayers\.turtle"[\s\S]*?class="chan-turtle-legend"/, "海龟图层开启时在按钮下方显示说明和图例");
assert.match(template, /id="chan-granville-help" class="chan-indicator-description-body"[^>]*v-show="chanChartLayers\.granville"[\s\S]*?class="chan-granville-legend"/, "葛兰碧图层开启时在按钮下方显示说明和图例");
assert.match(template, /id="chan-af-stochz-help" class="chan-indicator-description-body"[^>]*v-show="chanChartLayers\.afStochZ"/, "AF StochZ 图层开启时显示对应说明");
assert.match(template, /结合指标强弱和价格转折提示买卖；两者走势不一致时标记背离（指标买卖与背离）。/, "AF StochZ 说明显示用户指定的指标名称");
assert.doesNotMatch(template, /aria-label="显示海龟交易法指标"|aria-label="显示葛兰碧法则指标"/, "不再显示独立指标复选框");
assert.match(template, /20 根前序 K 线突破入场，10 根反向通道退出/, "海龟菜单保留入场与退出说明");
assert.ok(template.indexOf("/static/common/js/chan-turtle.js") < template.indexOf("/static/common/js/chan-client.js"), "海龟计算脚本先于图表客户端加载");
assert.match(styles, /\.chan-layer-menu>summary,\.chan-subchart-menu>summary,\.chan-period-menu>summary,\.chan-indicator-menu>summary\{min-height:44px\}/, "移动端菜单按钮统一为 44px 触控高度");
assert.match(styles, /\.chan-period-menu-panel\{grid-template-areas:"display";grid-template-columns:minmax\(0,1fr\);width:min\(330px,calc\(100vw - 40px\)\)/, "周期面板按四个选项收窄");
assert.match(styles, /\.chan-indicator-options\{display:flex;flex-wrap:wrap;align-items:flex-start;gap:8px\}/, "指标按钮紧凑排列且窄屏可换行");
assert.match(styles, /\.chan-indicator-description-body\{flex:0 0 100%;/, "指标说明在按钮下方占满面板宽度");
assert.match(styles, /\.chan-indicator-menu-panel\{grid-template-areas:"display";grid-template-columns:minmax\(0,1fr\)/, "窄屏指标面板与说明内容保持单列");
assert.match(styles, /\.chan-indicator-menu-panel>\.chan-layer-menu-close\{grid-area:display;grid-column:1\/-1;justify-self:end/, "窄屏指标面板的关闭按钮与标题并排并保持可见");
assert.match(styles, /@media\(max-width:760px\)\{[\s\S]*?\.chan-indicator-menu-panel\{position:absolute;top:calc\(100% \+ 8px\);right:auto;left:0;bottom:auto;transform:none;[\s\S]*?width:min\(360px,100%\)/, "窄屏指标面板靠左展开且不超出控制区宽度");
assert.match(app, /closest\("\.chan-layer-menu, \.chan-subchart-menu, \.chan-period-menu, \.chan-indicator-menu"\)/, "指标面板支持统一关闭处理");
assert.match(styles, /\.chan-control-options\{width:100%;align-items:center;flex-direction:row;flex-wrap:wrap;gap:8px\}/, "手机端菜单按钮横向排列并自动换行");
assert.match(styles, /@media\(max-width:380px\)\{\s*\.chan-control-options>details>summary\{gap:5px;padding-inline:10px\}\s*\}/, "超窄屏收紧菜单间距以容纳四个入口");
assert.match(styles, /\.chan-layer-menu-signals \.chan-layer-empty\{display:block;width:100%;min-width:0;margin-top:8px;padding:0;white-space:normal;overflow-wrap:anywhere;line-height:1\.5\}/, "图层状态提示独占一行并允许换行");
assert.match(styles, /@media\(max-width:760px\)\{[\s\S]*?\.chan-controls\{grid-template-columns:minmax\(0,1fr\);grid-template-rows:auto auto auto;grid-template-areas:"controls" "status" "levels";gap:10px\}[\s\S]*?\.chan-control-options\{position:relative;width:100%;align-items:center;flex-direction:row;flex-wrap:wrap;gap:8px\}[\s\S]*?\.chan-control-options>details\{position:static\}[\s\S]*?\.chan-layer-menu-panel\{position:absolute;top:calc\(100% \+ 8px\)/, "窄屏图表控制器保持单列，菜单跟随按钮组展开");

console.log("Chan subchart layout and UI checks passed");
