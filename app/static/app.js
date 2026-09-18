const state = { symbol: "QQQ", expiration: null, timer: null, analysisReady: false, loadId: 0, refreshing: false, refreshInFlight: null, analysisRefreshSymbol: null, chainFetchedAt: null, levelsKey: "", levelsPayload: null, chainFilter: "all", chainRows: [], chainSpot: null, levelBasisMode: "live", lastQuote: null, accessKey: "" };
const GAMMA_MIN_MINUTES = 30;
// 自动刷新间隔（秒）：页面提示文案与定时器共用同一个值。
const AUTO_REFRESH_SECONDS = 60;
// 本地快照新鲜期（秒）：SQLite 里的快照比它更新时直接复用，不再请求上游接口。
const SNAPSHOT_FRESH_SECONDS = 60;
// 压力位/支撑位各展示的条数。
const LEVEL_COUNT = 10;
// 交易计划（买入 / 加仓 / 卖出）各自展示的条数。
const PLAN_COUNT = 5;
// 期权链热力底色：成交量与未平仓各自按本屏最大值归一，得到 0~100 的相对强度；
// 底色深浅（含白天/黑夜各自的透明度区间）交给 styles.css 的 --heat-floor / --heat-gain 换算。
// 达到这个强度的格子算「热点」：底色已经很亮，文字换成深色墨色，避免亮底浅字看不清。
// 阈值按方向分开——绿底比红底亮得多，绿色格子更早需要换深色字（阈值取自两种字色对比度的交叉点）。
const HEAT_HOT_LEVEL = { call: 68, put: 80 };
// 期权链筛选下拉框的取值与中文标签。
const CHAIN_FILTERS = { all: "全部", call: "看涨", put: "看跌" };
// 时段标签：数据源给的是上游的 marketState 口径（PRE/REGULAR/POST/CLOSED），夜盘由本地时钟补充。
const MARKET_STATE_LABELS = { PRE: "盘前", REGULAR: "正常交易", POST: "盘后", OVERNIGHT: "夜盘", CLOSED: "休市" };
const $ = (id) => document.getElementById(id);

// 主题：默认黑夜模式，用户可在右上角切换到白天；偏好写入 localStorage，刷新后保持。
const THEME_KEY = "option-scope-theme";
const ACCESS_KEY_STORAGE = "option-scope-access-key";
// 按钮文案展示当前生效的主题名称。
const THEME_LABELS = { light: "白天", dark: "黑夜" };

// 应用主题：切换根节点 data-theme，并同步按钮文案、提示与无障碍状态。
function applyTheme(theme) {
  const next = theme === "light" ? "light" : "dark";
  document.documentElement.dataset.theme = next;
  const button = $("theme-toggle");
  if (!button) return;
  button.textContent = THEME_LABELS[next];
  button.title = next === "dark" ? "切换到白天模式" : "切换到黑夜模式";
  button.setAttribute("aria-pressed", String(next === "dark"));
}

// 初始化主题：读取本地偏好并绑定切换按钮；localStorage 不可用时静默降级为白天。
function initTheme() {
  let stored = null;
  try { stored = localStorage.getItem(THEME_KEY); } catch (error) { stored = null; }
  applyTheme(stored === "light" ? "light" : "dark");
  const button = $("theme-toggle");
  if (!button) return;
  button.addEventListener("click", () => {
    const next = document.documentElement.dataset.theme === "dark" ? "light" : "dark";
    applyTheme(next);
    try { localStorage.setItem(THEME_KEY, next); } catch (error) { /* 隐私模式等写入失败时忽略 */ }
  });
}

// 折叠组通用逻辑：内容体用 hidden 控制显隐（[hidden] 在 flex/grid 上下文里会被覆盖，样式里补了 [hidden]{display:none}），
// 按钮同步 aria-expanded 与「展开/收起」文案，展开状态记在 sessionStorage——同一标签页里换标的、跳 URL 不必重复展开，
// 关掉标签页就回到各自默认状态（分析详情与期权链默认折叠、图表默认展开）。
function bindFoldGroup({ headerId, toggleId, bodyId, actionId, storageKey, defaultExpanded, onChange, shouldIgnore }) {
  const header = $(headerId);
  const body = $(bodyId);
  const toggle = $(toggleId);
  if (!header || !body || !toggle) return;
  const apply = (expanded) => {
    body.hidden = !expanded;
    toggle.setAttribute("aria-expanded", expanded ? "true" : "false");
    const action = actionId ? $(actionId) : null;
    if (action) action.textContent = expanded ? "收起" : "展开";
    if (onChange) onChange(expanded);
  };
  let stored = null;
  try { stored = sessionStorage.getItem(storageKey); } catch (error) { stored = null; }
  apply(stored === null ? defaultExpanded : stored === "1");
  header.addEventListener("click", (event) => {
    if (shouldIgnore && shouldIgnore(event)) return;
    const expanded = body.hidden;
    apply(expanded);
    try { sessionStorage.setItem(storageKey, expanded ? "1" : "0"); } catch (error) { /* 隐私模式等写入失败时忽略 */ }
  });
}

// 分析详情折叠组：趋势通道、交易计划与压力位/支撑位四张明细表，默认折叠。
const DETAIL_KEY = "option-scope-detail";

function initDetailGroup() {
  bindFoldGroup({
    headerId: "detail-header",
    toggleId: "detail-toggle",
    bodyId: "detail-body",
    actionId: "detail-action",
    storageKey: DETAIL_KEY,
    defaultExpanded: false,
    // 基准价开关只在展开时出现：折叠态下不占位，也避免误点。
    onChange: (expanded) => { const modes = $("detail-modes"); if (modes) modes.hidden = !expanded; },
    // 标题栏里混着「基准价」开关：命中开关就切口径，开关容器里的空白则不改折叠状态，避免贴着按钮点空时把面板收了。
    shouldIgnore: (event) => {
      const basisButton = event.target.closest("[data-basis]");
      if (basisButton) { applyBasisMode(basisButton.dataset.basis); return true; }
      return Boolean(event.target.closest("#detail-modes"));
    },
  });
}

// 期权链折叠组：表格与热力图例可折叠、默认展开；类型筛选放在折叠区里，收起时一并隐藏。
const CHAIN_KEY = "option-scope-chain";

function initChainGroup() {
  bindFoldGroup({
    headerId: "chain-header",
    toggleId: "chain-toggle",
    bodyId: "chain-fold",
    actionId: "chain-action",
    storageKey: CHAIN_KEY,
    defaultExpanded: true,
    // 标题行右侧是数据来源与快照时间，点它不折叠。
    shouldIgnore: (event) => Boolean(event.target.closest(".panel-status")),
  });
}

// 图表折叠组：Gamma 敞口、压力位/支撑位、成交量分布、持仓量分布四张图表，默认展开。
const CHART_KEY = "option-scope-charts";

function initChartGroup() {
  bindFoldGroup({
    headerId: "chart-header",
    toggleId: "chart-toggle",
    bodyId: "chart-fold",
    actionId: "chart-action",
    storageKey: CHART_KEY,
    defaultExpanded: true,
    // 图表按容器实际尺寸绘制：折叠期间若被后台刷新重绘过（隐藏时只有最小尺寸），展开后必须补一次重绘。
    onChange: (expanded) => { if (expanded) requestAnimationFrame(() => redrawChartsIfResized()); },
  });
}

function parsePageQuery(search) {
  const params = new URLSearchParams(search || "");
  const symbol = (params.get("symbol") || "").trim().toUpperCase();
  const expiration = (params.get("expiration") || "").trim();
  const key = (params.get("key") || "").trim();
  return {
    symbol: /^[A-Z0-9][A-Z0-9.-]{0,9}$/.test(symbol) ? symbol : "",
    expiration: /^\d{4}-\d{2}-\d{2}$/.test(expiration) ? expiration : "",
    key,
  };
}

function buildPageQuery(symbol, expiration, pathname) {
  const params = new URLSearchParams();
  if (state.accessKey) params.set("key", state.accessKey);
  if (symbol) params.set("symbol", symbol);
  if (expiration) params.set("expiration", expiration);
  const query = params.toString();
  return query ? `${pathname}?${query}` : pathname;
}

function accessKeyRequired() {
  const meta = document.querySelector('meta[name="option-scope-access-required"]');
  return meta?.content === "true";
}

function readStoredAccessKey() {
  try { return (localStorage.getItem(ACCESS_KEY_STORAGE) || "").trim(); } catch (error) { return ""; }
}

function rememberAccessKey(key) {
  try { localStorage.setItem(ACCESS_KEY_STORAGE, key); return true; } catch (error) { return false; }
}

function initializeAccessKey() {
  const queryKey = parsePageQuery(location.search).key;
  if (queryKey) {
    // 首次带 key 访问时写入 localStorage；写入失败说明浏览器不允许持久化，按密钥丢失处理。
    state.accessKey = rememberAccessKey(queryKey) ? queryKey : "";
    return state.accessKey;
  }
  state.accessKey = readStoredAccessKey();
  return state.accessKey;
}

function showAccessDenied() {
  if (state.timer) clearInterval(state.timer);
  state.timer = null;
  document.body.classList.add("access-denied-page");
  const view = $("access-denied-view");
  if (view) view.hidden = false;
}

function syncPageQuery() {
  const next = buildPageQuery(state.symbol, state.expiration, location.pathname);
  const current = `${location.pathname}${location.search}`;
  if (next === current) return;
  history.replaceState(null, "", next);
}

function setError(message) { $("error-box").textContent = message || ""; $("error-box").hidden = !message; }
// 快照年龄（秒）：时间戳缺失或无法解析返回 null，时钟偏差导致的负值按 0 处理。
function snapshotAgeSeconds(fetchedAt) {
  if (!fetchedAt) return null;
  const time = new Date(fetchedAt).getTime();
  return Number.isNaN(time) ? null : Math.max((Date.now() - time) / 1000, 0);
}
function formatNumber(value, digits = 0) { if (value === null || value === undefined || value === "") return "--"; return Number(value).toLocaleString("en-US", { maximumFractionDigits: digits }); }
function formatMoney(value) { return value == null ? "--" : Number(value).toFixed(2); }
function formatTime(value) { if (!value) return "--"; const date = new Date(value); return Number.isNaN(date.getTime()) ? value : date.toLocaleString("zh-CN", { hour12: false }); }
// 回溯的未平仓量只展示到日期，避免角标里塞入完整时间。
function formatDay(value) { if (!value) return "--"; const date = new Date(value); return Number.isNaN(date.getTime()) ? "--" : date.toLocaleDateString("zh-CN"); }
// GEX 数值内部按「百万美元」存储，展示时换算成中文单位：≥1 亿用「亿」，其余用「万」，不足万元的显示原值。
function formatGex(value, digits = 2) {
  if (value === null || value === undefined || value === "") return "--";
  const millions = Number(value);
  if (!Number.isFinite(millions)) return "--";
  const sign = millions < 0 ? "-" : "";
  const absolute = Math.abs(millions);
  if (absolute >= 100) return `${sign}${formatNumber(absolute / 100, digits)}亿`;
  if (absolute >= 1) return `${sign}${formatNumber(absolute * 100, 0)}万`;
  if (absolute >= 0.01) return `${sign}${formatNumber(absolute * 100, 1)}万`;
  return `${sign}${formatNumber(absolute * 1000000, 0)}`;
}
// 图表数值统一入口：GEX 走中文单位，成交量/持仓量保持千分位原样。
// 成交量/持仓量按「万/亿」显示，不足一万保留千分位原值。
function formatCount(value, digits = 2) {
  if (value === null || value === undefined || value === "") return "--";
  const number = Number(value);
  if (!Number.isFinite(number)) return "--";
  const sign = number < 0 ? "-" : "";
  const absolute = Math.abs(number);
  if (absolute >= 100000000) return `${sign}${formatNumber(absolute / 100000000, digits)}亿`;
  if (absolute >= 10000) return `${sign}${formatNumber(absolute / 10000, digits)}万`;
  return `${sign}${formatNumber(absolute, 0)}`;
}
// 图表数值统一入口：GEX 走中文金额单位，成交量/持仓量走中文计数单位。
function formatChartValue(value, digits, unit) { return unit === "M" ? formatGex(value, digits) : formatCount(value, digits); }
// 刷新按钮只由「是否正在刷新」决定，避免多条并发路径各自改写 disabled 后被误启用；
// 没有到期日（标的没有挂牌期权）时同样允许手动刷新现货快照。
function syncRefreshButton() {
  $("refresh-button").disabled = state.refreshing;
  $("refresh-note").textContent = state.refreshing ? "正在刷新…" : `每 ${AUTO_REFRESH_SECONDS} 秒自动更新`;
}
function setBusy(busy) { $("load-button").disabled = busy; $("expiration-select").disabled = busy || !state.expiration; syncRefreshButton(); }

function aggregateByStrike(rows, spot) {
  const grouped = new Map();
  for (const row of rows) {
    const strike = Number(row.strike);
    if (!Number.isFinite(strike)) continue;
    if (!grouped.has(strike)) grouped.set(strike, { strike, callVolume: 0, putVolume: 0, callOi: 0, putOi: 0, callGex: 0, putGex: 0 });
    const item = grouped.get(strike);
    const volume = Number(row.volume) || 0;
    const oi = Number(row.open_interest) || 0;
    const recalculatedGamma = gammaAtSpot(spot, row);
    const gamma = recalculatedGamma == null ? (Number(row.gamma) || 0) : recalculatedGamma;
    const gex = gamma * oi * 100 * (Number(spot) || 0) ** 2 * 0.01 / 1000000;
    if (row.contract_type === "call") { item.callVolume += volume; item.callOi += oi; item.callGex += gex; }
    else { item.putVolume += volume; item.putOi += oi; item.putGex -= gex; }
  }
  return [...grouped.values()].sort((a, b) => a.strike - b.strike);
}

// 到期日时间戳按到期日字符串缓存：零 Gamma 扫描会对同一批合约求值十几万次，
// 每次重新构造 Date 与 Intl.DateTimeFormat 会把主线程占满数秒，必须复用结果。
const expiryCache = new Map();
function optionExpiry(expiration) {
  if (!expiration) return null;
  if (expiryCache.has(expiration)) return expiryCache.get(expiration);
  // 美股股票期权通常在到期日 16:00 America/New_York 截止；先按夏令时 -04:00，再校正冬令时。
  let expiry = new Date(`${expiration}T16:00:00-04:00`);
  let result = null;
  if (!Number.isNaN(expiry.getTime())) {
    const hour = Number(new Intl.DateTimeFormat("en-US", { timeZone: "America/New_York", hour: "numeric", hourCycle: "h23" }).format(expiry));
    if (hour !== 16) expiry = new Date(`${expiration}T16:00:00-05:00`);
    result = Number.isNaN(expiry.getTime()) ? null : expiry;
  }
  expiryCache.set(expiration, result);
  return result;
}

function gammaAtSpot(spot, row, expiration) {
  const spotValue = Number(spot);
  const strike = Number(row.strike);
  // 模型 IV 由后端按合约价格反解，优先于数据源里常为占位值的 implied_volatility。
  const volatility = Number(row.model_iv ?? row.implied_volatility);
  if (!Number.isFinite(spotValue) || spotValue <= 0 || !Number.isFinite(strike) || strike <= 0) return null;
  if (!Number.isFinite(volatility) || volatility <= 0) return null;
  const expiry = optionExpiry(row.expiration || expiration);
  if (!expiry) return null;
  const timeYears = Math.max((expiry.getTime() - Date.now()) / (365 * 24 * 60 * 60 * 1000), GAMMA_MIN_MINUTES / (365 * 24 * 60));
  const volatilityTime = volatility * Math.sqrt(timeYears);
  const d1 = (Math.log(spotValue / strike) + (0.005 + 0.5 * volatility ** 2) * timeYears) / volatilityTime;
  return Math.exp(-0.5 * d1 ** 2) / (spotValue * volatilityTime * Math.sqrt(2 * Math.PI));
}

// 扫描前的预计算：一次零 Gamma 扫描会在上百个价位上重复求值同一批合约，
// 先把每行的常量（对数行权价、漂移项、预乘权重）算好，热点循环里只做一次指数运算。
function prepareGexRows(rows, expiration) {
  const now = Date.now();
  const prepared = [];
  for (const row of rows) {
    const strike = Number(row.strike);
    // 模型 IV 由后端按合约价格反解，优先于数据源里常为占位值的 implied_volatility。
    const volatility = Number(row.model_iv ?? row.implied_volatility);
    const openInterest = Number(row.open_interest) || 0;
    const expiry = optionExpiry(row.expiration || expiration);
    if (!Number.isFinite(strike) || strike <= 0 || !Number.isFinite(volatility) || volatility <= 0 || openInterest <= 0 || !expiry) continue;
    const timeYears = Math.max((expiry.getTime() - now) / (365 * 24 * 60 * 60 * 1000), GAMMA_MIN_MINUTES / (365 * 24 * 60));
    const volatilityTime = volatility * Math.sqrt(timeYears);
    if (!Number.isFinite(volatilityTime) || volatilityTime <= 0) continue;
    const sign = row.contract_type === "call" ? 1 : -1;
    // 预乘常量：方向 × 持仓量 × 合约乘数 × 1% 价格变动 ÷ 100 万，与页面 GEX 的「百万美元」口径一致。
    prepared.push({
      logStrike: Math.log(strike),
      drift: 0.005 * timeYears + 0.5 * volatilityTime ** 2,
      volatilityTime,
      constant: sign * openInterest * 100 * 0.01 / 1000000 / (volatilityTime * Math.sqrt(2 * Math.PI)),
    });
  }
  return prepared;
}

// 指定价位上的净 GEX：与 gammaAtSpot 同口径，只是把循环里的常量提前算好。
function totalGexAtSpot(prepared, spotValue) {
  const logSpot = Math.log(spotValue);
  let total = 0;
  for (const item of prepared) {
    const d1 = (logSpot - item.logStrike + item.drift) / item.volatilityTime;
    total += item.constant * spotValue * Math.exp(-0.5 * d1 ** 2);
  }
  return total;
}

function findGammaFlip(rows, currentSpot, expiration) {
  const strikes = rows.map((row) => Number(row.strike)).filter((strike) => Number.isFinite(strike) && strike > 0);
  const spot = Number(currentSpot);
  if (!strikes.length || !Number.isFinite(spot) || spot <= 0 || !expiration) return null;
  const prepared = prepareGexRows(rows, expiration);
  if (!prepared.length) return null;
  // 公开 GEX 实现常用现价 ±15% 的扫描带，避免把远端低信号根误当成交易区间的 Flip。
  const lower = Math.max(Math.min(...strikes), spot * 0.85);
  const upper = Math.min(Math.max(...strikes), spot * 1.15);
  if (!(upper > lower)) return null;
  const samples = 121;
  const roots = [];
  let previousSpot = lower;
  let previousValue = totalGexAtSpot(prepared, previousSpot);
  for (let index = 1; index <= samples; index += 1) {
    const nextSpot = lower + (upper - lower) * index / samples;
    const nextValue = totalGexAtSpot(prepared, nextSpot);
    if (previousValue === 0) roots.push(previousSpot);
    if (previousValue * nextValue < 0) {
      let left = previousSpot;
      let right = nextSpot;
      let leftValue = previousValue;
      for (let iteration = 0; iteration < 45; iteration += 1) {
        const middle = (left + right) / 2;
        const middleValue = totalGexAtSpot(prepared, middle);
        if (leftValue * middleValue <= 0) right = middle;
        else { left = middle; leftValue = middleValue; }
      }
      roots.push((left + right) / 2);
    }
    previousSpot = nextSpot;
    previousValue = nextValue;
  }
  if (!roots.length) return null;
  return { strike: roots.reduce((nearest, root) => Math.abs(root - spot) < Math.abs(nearest - spot) ? root : nearest) };
}

function strikePosition(points, strike) {
  if (!points.length || strike < points[0].strike || strike > points[points.length - 1].strike) return null;
  for (let index = 1; index < points.length; index += 1) {
    if (strike <= points[index].strike) {
      const distance = points[index].strike - points[index - 1].strike;
      const ratio = distance === 0 ? 0 : (strike - points[index - 1].strike) / distance;
      return index - 1 + ratio;
    }
  }
  return points.length - 1;
}

// 图表容器的内容尺寸（扣除内边距），用于让 SVG 与容器按 1:1 像素渲染。
function chartContentBox(target) {
  const rect = target.getBoundingClientRect();
  const style = getComputedStyle(target);
  return {
    width: Math.max(240, Math.round(rect.width - parseFloat(style.paddingLeft || 0) - parseFloat(style.paddingRight || 0))),
    height: Math.max(140, Math.round(rect.height - parseFloat(style.paddingTop || 0) - parseFloat(style.paddingBottom || 0))),
  };
}

// 柱端标记的文字宽度测量上下文（与 CSS 中 10px 粗体一致），用于贴边时避免文字被 SVG 视口裁剪。
let markerLabelContext = null;
function markerLabelWidth(text, weight = 700) {
  if (!markerLabelContext) markerLabelContext = document.createElement("canvas").getContext("2d");
  markerLabelContext.font = `${weight} 10px Inter, ui-sans-serif, system-ui, -apple-system, BlinkMacSystemFont, "Segoe UI", sans-serif`;
  return markerLabelContext.measureText(text).width;
}

function renderSignedChart(targetId, points, positiveKey, negativeKey, unit, emptyMessage, options = {}) {
  const target = $(targetId);
  if (!points.length || points.every((point) => !(Math.abs(point[positiveKey]) + Math.abs(point[negativeKey])))) { target.innerHTML = `<div class="chart-empty">${emptyMessage}</div>`; return; }
  // 按容器实际像素绘制：viewBox 与元素 1:1，手机窄屏不会再把柱子和坐标文字整体等比缩小。
  const { width, height } = chartContentBox(target);
  target.dataset.chartWidth = String(width); target.dataset.chartHeight = String(height);
  // 成交量/持仓量图按行情软件样式把数值列放在右侧，Gamma 图保留左侧数值列。
  const axisOnRight = options.axis === "right";
  const pad = axisOnRight ? { left: 16, right: 56, top: 14, bottom: 26 } : { left: 48, right: 12, top: 14, bottom: 26 }; const innerWidth = width - pad.left - pad.right; const innerHeight = height - pad.top - pad.bottom; const baseline = pad.top + innerHeight / 2;
  // 上下各留一条固定高度的「标签通道」：柱子最高只顶到通道内侧的标高线，柱端文字与方向角标住在通道里，不会再压到坐标刻度。
  const gutter = Math.min(44, Math.max(16, innerHeight / 2 - 16)); const scaleTop = pad.top + gutter; const scaleBottom = height - pad.bottom - gutter; const maxBarHeight = innerHeight / 2 - gutter;
  const maxValue = Math.max(1, ...points.map((point) => Math.max(Math.abs(point[positiveKey]), Math.abs(point[negativeKey])))); const slot = innerWidth / points.length; const barWidth = Math.max(3, Math.min(26, slot * 0.68));
  const yLabel = formatChartValue(maxValue, 2, unit);
  // 左侧数值列右边界：最左行权价的柱端文字压到数值时向右避让。
  const axisColumnWidth = 6 + Math.max(markerLabelWidth(yLabel, 400), markerLabelWidth(`-${yLabel}`, 400));
  const labelColumnRight = axisOnRight ? 0 : axisColumnWidth + 4;
  // 刻度文字按可用宽度决定数量：桌面宽屏约每 84px 一个，手机上自动减少避免重叠。
  const labelEvery = Math.max(1, Math.ceil(points.length / Math.max(2, Math.floor(innerWidth / 84))));
  const bars = points.map((point, index) => { const x = pad.left + index * slot + (slot - barWidth) / 2; const positive = Number(point[positiveKey]) || 0; const negative = Number(point[negativeKey]) || 0; const positiveHeight = Math.abs(positive) / maxValue * maxBarHeight; const negativeHeight = Math.abs(negative) / maxValue * maxBarHeight; const label = index % labelEvery === 0 ? `<text class="chart-label" x="${x + barWidth / 2}" y="${height - 8}" text-anchor="middle">${formatMoney(point.strike)}</text>` : ""; return `<rect class="chart-call" x="${x}" y="${baseline - positiveHeight}" width="${barWidth}" height="${positiveHeight}" rx="1"><title>${formatMoney(point.strike)} 看涨 ${formatChartValue(positive, 2, unit)}</title></rect><rect class="chart-put" x="${x}" y="${baseline}" width="${barWidth}" height="${negativeHeight}" rx="1"><title>${formatMoney(point.strike)} 看跌 ${formatChartValue(Math.abs(negative), 2, unit)}</title></rect>${label}`; }).join("");
  // 柱端标记：Gamma 图标注看涨墙/看跌墙，成交量与持仓量图标注最高看涨柱/最高看跌柱。
  const markers = (options.markers || []).filter((marker) => marker.point).map((marker) => {
    const index = points.indexOf(marker.point);
    if (index < 0) return "";
    const x = pad.left + index * slot + slot / 2;
    const positive = Number(marker.point[positiveKey]) || 0;
    const negative = Number(marker.point[negativeKey]) || 0;
    const barHeight = marker.position === "top"
      ? Math.abs(positive) / maxValue * maxBarHeight
      : Math.abs(negative) / maxValue * maxBarHeight;
    const tipY = marker.position === "top" ? baseline - barHeight : baseline + barHeight;
    const labelText = `${marker.label} ${formatMoney(marker.point.strike)}`;
    // 柱端文字始终贴在柱端外侧（看涨在上、看跌在下），并落在标签通道范围内，不会翻到柱身上。
    const labelY = Math.min(Math.max(marker.position === "top" ? tipY - 14 : tipY + 22, pad.top + 27), scaleBottom + 24);
    // 最左行权价的文字若压到左侧数值列则向右避让；最左/最右再按 SVG 视口收边，避免被裁剪。
    const halfLabel = markerLabelWidth(labelText) / 2 + 1;
    let labelX = Math.min(Math.max(x, halfLabel), width - halfLabel);
    const overScaleRow = [scaleTop, scaleBottom].some((line) => labelY - 11 < line + 7 && labelY + 3 > line - 7);
    if (overScaleRow && !axisOnRight && labelX - halfLabel < labelColumnRight) labelX = Math.min(labelColumnRight + halfLabel, width - halfLabel);
    if (overScaleRow && axisOnRight && labelX + halfLabel > width - axisColumnWidth) labelX = Math.max(width - axisColumnWidth - halfLabel, halfLabel);
    return `<text class="${marker.className}-label" x="${labelX}" y="${labelY}" text-anchor="middle">${labelText}</text>`;
  }).join("");
  const gammaFlip = options.gammaFlip;
  const gammaFlipMarker = gammaFlip ? (() => {
    const position = strikePosition(points, gammaFlip.strike);
    if (position == null) return "";
    const x = pad.left + position * slot + slot / 2;
    return `<line class="chart-gamma-flip" x1="${x}" x2="${x}" y1="${pad.top}" y2="${height - pad.bottom}"/>`;
  })() : "";
  // 右轴图表给出参考样式的 5 档刻度（±最大 / ±一半 / 0），Gamma 图保持 3 档。
  const halfTop = (scaleTop + baseline) / 2; const halfBottom = (scaleBottom + baseline) / 2;
  const halfLabel = formatChartValue(maxValue / 2, 2, unit);
  const axisLabels = axisOnRight
    ? `<text class="chart-label" x="${width - 6}" y="${scaleTop + 4}" text-anchor="end">${yLabel}</text><text class="chart-label" x="${width - 6}" y="${halfTop + 4}" text-anchor="end">${halfLabel}</text><text class="chart-label" x="${width - 6}" y="${baseline + 4}" text-anchor="end">0</text><text class="chart-label" x="${width - 6}" y="${halfBottom + 4}" text-anchor="end">-${halfLabel}</text><text class="chart-label" x="${width - 6}" y="${scaleBottom + 4}" text-anchor="end">-${yLabel}</text>`
    : `<text class="chart-label" x="4" y="${scaleTop + 4}">${yLabel}</text><text class="chart-label" x="4" y="${baseline + 4}">0</text><text class="chart-label" x="4" y="${scaleBottom + 4}">-${yLabel}</text>`;
  const gridLines = axisOnRight ? `<line class="chart-grid" x1="${pad.left}" x2="${width - pad.right}" y1="${halfTop}" y2="${halfTop}"/><line class="chart-grid" x1="${pad.left}" x2="${width - pad.right}" y1="${halfBottom}" y2="${halfBottom}"/>` : "";
  // 右轴图表不再画角落方向文字，方向改由图例说明，避免与右侧数值列抢位。
  const cornerLabels = axisOnRight ? "" : `<text class="chart-label" x="${width - 12}" y="${pad.top + 10}" text-anchor="end">看涨 ↑</text><text class="chart-label" x="${width - 12}" y="${height - pad.bottom - 3}" text-anchor="end">看跌 ↓</text>`;
  const tagElements = options.crosshairTags ? `<line class="chart-crosshair-h" x1="${pad.left}" x2="${width - pad.right}" y1="0" y2="0" visibility="hidden"/><g class="chart-tag chart-tag-x" visibility="hidden"><rect rx="2" height="16" width="0"/><text></text></g><g class="chart-tag chart-tag-y" visibility="hidden"><rect rx="2" height="16" width="0"/><text></text></g>` : "";
  const svg = `<svg viewBox="0 0 ${width} ${height}" role="img" tabindex="0" aria-label="${target.getAttribute("aria-label") || "期权分布图"}">${gridLines}<line class="chart-grid" x1="${pad.left}" x2="${width - pad.right}" y1="${scaleTop}" y2="${scaleTop}"/><line class="chart-grid" x1="${pad.left}" x2="${width - pad.right}" y1="${baseline}" y2="${baseline}"/><line class="chart-grid" x1="${pad.left}" x2="${width - pad.right}" y1="${scaleBottom}" y2="${scaleBottom}"/><line class="chart-zero" x1="${pad.left}" x2="${width - pad.right}" y1="${baseline}" y2="${baseline}"/>${axisLabels}${gammaFlipMarker}${bars}${markers}<line class="chart-crosshair" x1="0" x2="0" y1="${pad.top}" y2="${height - pad.bottom}" visibility="hidden"/>${tagElements}${cornerLabels}</svg><div class="chart-tooltip" hidden></div>`; target.innerHTML = svg;

  const chartSvg = target.querySelector("svg");
  const crosshair = target.querySelector(".chart-crosshair");
  const tooltip = target.querySelector(".chart-tooltip");
  const tooltipTitle = options.tooltipTitle || "行权价";
  // SVG 按等比缩放绘制，容器与绘图区之间可能有留白，坐标换算必须走屏幕矩阵，保证十字虚线与数据卡严格对齐。
  const viewBoxXToClient = (viewBoxX) => {
    const matrix = chartSvg.getScreenCTM();
    if (matrix && chartSvg.createSVGPoint) {
      const point = chartSvg.createSVGPoint();
      point.x = viewBoxX;
      point.y = 0;
      return point.matrixTransform(matrix).x;
    }
    const rect = chartSvg.getBoundingClientRect();
    return rect.left + (viewBoxX / width) * rect.width;
  };
  const clientToViewBox = (clientX, clientY) => {
    const matrix = chartSvg.getScreenCTM();
    if (matrix && chartSvg.createSVGPoint) {
      const point = chartSvg.createSVGPoint();
      point.x = clientX;
      point.y = clientY;
      return point.matrixTransform(matrix.inverse());
    }
    const rect = chartSvg.getBoundingClientRect();
    return { x: ((clientX - rect.left) / rect.width) * width, y: ((clientY - rect.top) / rect.height) * height };
  };
  const crosshairLine = target.querySelector(".chart-crosshair-h");
  const tagX = target.querySelector(".chart-tag-x");
  const tagY = target.querySelector(".chart-tag-y");
  // 十字光标取值标签（参考行情软件）：数值列一侧贴当前鼠标高度的取值，底部贴所在行权价。
  const updateTags = (index, clientY, centerX) => {
    if (!crosshairLine) return;
    const point = points[index];
    const hidden = clientY == null || !point;
    crosshairLine.setAttribute("visibility", hidden ? "hidden" : "visible");
    tagX.setAttribute("visibility", hidden ? "hidden" : "visible");
    tagY.setAttribute("visibility", hidden ? "hidden" : "visible");
    if (hidden) return;
    const localY = Math.min(Math.max(clientToViewBox(0, clientY).y, pad.top + 6), height - pad.bottom - 6);
    crosshairLine.setAttribute("y1", localY);
    crosshairLine.setAttribute("y2", localY);
    const value = (baseline - localY) / maxBarHeight * maxValue;
    const valueText = `${value < 0 ? "-" : ""}${formatChartValue(Math.abs(value), 2, unit)}`;
    const valueWidth = markerLabelWidth(valueText, 700) + 10;
    const valueLeft = axisOnRight ? width - valueWidth - 4 : 4;
    tagY.querySelector("rect").setAttribute("x", valueLeft);
    tagY.querySelector("rect").setAttribute("y", localY - 8);
    tagY.querySelector("rect").setAttribute("width", valueWidth);
    const valueNode = tagY.querySelector("text");
    valueNode.setAttribute("x", valueLeft + 5);
    valueNode.setAttribute("y", localY + 4);
    valueNode.textContent = valueText;
    const strikeText = formatMoney(point.strike);
    const strikeWidth = markerLabelWidth(strikeText, 700) + 10;
    const strikeLeft = Math.min(Math.max(centerX - strikeWidth / 2, 2), width - strikeWidth - 2);
    tagX.querySelector("rect").setAttribute("x", strikeLeft);
    tagX.querySelector("rect").setAttribute("y", height - 20);
    tagX.querySelector("rect").setAttribute("width", strikeWidth);
    const strikeNode = tagX.querySelector("text");
    strikeNode.setAttribute("x", strikeLeft + 5);
    strikeNode.setAttribute("y", height - 8);
    strikeNode.textContent = strikeText;
  };
  const showTooltip = (index, clientY) => {
    const point = points[index];
    if (!point) return;
    const positive = Number(point[positiveKey]) || 0;
    const negative = Number(point[negativeKey]) || 0;
    const isGamma = targetId === "gex-chart";
    const net = isGamma ? positive + negative : positive - negative;
    const valueLabel = isGamma ? " GEX" : (options.valueLabel || "");
    const rows = [
      `<div class="chart-tooltip-row"><span><i class="tooltip-dot call"></i>看涨${valueLabel}</span><strong>${formatChartValue(Math.abs(positive), 2, unit)}</strong></div>`,
      `<div class="chart-tooltip-row"><span><i class="tooltip-dot put"></i>看跌${valueLabel}</span><strong>${formatChartValue(Math.abs(negative), 2, unit)}</strong></div>`,
    ];
    if (isGamma) {
      rows.push(`<div class="chart-tooltip-row"><span><i class="tooltip-dot net"></i>净 GEX</span><strong>${formatChartValue(net, 2, unit)}</strong></div>`);
      if (options.spot != null) rows.unshift(`<div class="chart-tooltip-sub">行情价 ${formatMoney(options.spot)}</div>`);
    } else if (options.valueLabel) {
      rows.push(`<div class="chart-tooltip-row"><span><i class="tooltip-dot net"></i>总${options.valueLabel}</span><strong>${formatChartValue(Math.abs(positive) + Math.abs(negative), 2, unit)}</strong></div>`);
    }
    tooltip.innerHTML = `<div class="chart-tooltip-title">${tooltipTitle} ${formatMoney(point.strike)}</div>${rows.join("")}`;
    const x = pad.left + index * slot + slot / 2;
    crosshair.setAttribute("x1", x);
    crosshair.setAttribute("x2", x);
    crosshair.setAttribute("visibility", "visible");
    tooltip.hidden = false;
    const targetRect = target.getBoundingClientRect();
    const tooltipWidth = tooltip.offsetWidth || 180;
    // 数据卡固定悬浮在图表容器上沿之外（不进入图表内部），横向跟随十字虚线，纵向不跟随鼠标。
    const lineClientX = viewBoxXToClient(x);
    const left = Math.max(8, Math.min(targetRect.width - tooltipWidth - 8, lineClientX - targetRect.left - tooltipWidth / 2));
    tooltip.style.left = `${left}px`;
    tooltip.style.top = "auto";
    tooltip.style.bottom = `${targetRect.height + 4}px`;
    updateTags(index, clientY, x);
  };
  const hideTooltip = () => {
    crosshair.setAttribute("visibility", "hidden");
    tooltip.hidden = true;
    if (crosshairLine) crosshairLine.setAttribute("visibility", "hidden");
    if (tagX) tagX.setAttribute("visibility", "hidden");
    if (tagY) tagY.setAttribute("visibility", "hidden");
  };
  const pointFromEvent = (event) => {
    const local = clientToViewBox(event.clientX, event.clientY);
    return Math.max(0, Math.min(points.length - 1, Math.floor((local.x - pad.left) / slot)));
  };
  chartSvg.addEventListener("pointermove", (event) => showTooltip(pointFromEvent(event), event.clientY));
  chartSvg.addEventListener("pointerleave", hideTooltip);
  chartSvg.addEventListener("focus", () => showTooltip(0, null));
  chartSvg.addEventListener("blur", hideTooltip);
}

// 取指定字段数值最大的执行价，用于标注成交量/持仓量里的最高柱。
function maxPoint(points, key) {
  return points.reduce((best, point) => (Number(point[key]) || 0) > (Number(best?.[key]) || 0) ? point : best, null);
}

// 价内判断：优先按现价与行权价比较（与行情软件口径一致），缺少现价时退回数据源自带的标记。
function contractInTheMoney(row, spot) {
  const strike = Number(row.strike); const price = Number(spot);
  if (!Number.isFinite(strike) || !Number.isFinite(price) || price <= 0) return Boolean(row.in_the_money);
  return row.contract_type === "call" ? strike < price : strike > price;
}
// 按「全部 / 价内 / 价外」汇总看涨与看跌的成交量或持仓量。
function summarizeDistribution(rows, spot, key) {
  const summary = { call: { all: 0, itm: 0 }, put: { all: 0, itm: 0 } };
  for (const row of rows) {
    const bucket = summary[row.contract_type] || (summary[row.contract_type] = { all: 0, itm: 0 });
    const value = Number(row[key]) || 0;
    bucket.all += value;
    if (contractInTheMoney(row, spot)) bucket.itm += value;
  }
  return summary;
}
// 渲染图表下方的汇总小表（列：全部 / 价内 / 价外；行：看涨 / 看跌 / 合计）。
function renderDistributionSummary(target, rows, spot, key, totalLabel) {
  if (!target) return;
  const summary = summarizeDistribution(rows, spot, key);
  const line = (label, bucket) => `<tr><th>${label}</th><td>${formatCount(bucket.all)}</td><td>${formatCount(bucket.itm)}</td><td>${formatCount(bucket.all - bucket.itm)}</td></tr>`;
  const total = { all: summary.call.all + summary.put.all, itm: summary.call.itm + summary.put.itm };
  target.innerHTML = `<table><thead><tr><th></th><th>全部</th><th>价内</th><th>价外</th></tr></thead><tbody>${line("看涨", summary.call)}${line("看跌", summary.put)}${line(totalLabel, total)}</tbody></table>`;
}
// 压力位/支撑位的基准价：优先盘后价——盘后成交更接近当日结算价，盘前冲高与盘中回落常是「假突破」，
// 用它们当基准会把关键位算偏；没有盘后数据时依次回退盘前价、常规价。
function levelBasis(quote, fallbackPrice) {
  const sessions = quote?.sessions || {};
  for (const [key, label] of [["post", "盘后"], ["pre", "盘前"]]) {
    const price = Number(sessions[key]?.price);
    if (Number.isFinite(price) && price > 0) return { price, label };
  }
  return { price: Number(fallbackPrice), label: "常规" };
}

// 基准价开关的两个口径：live 实时价（默认）、close 盘后价（沿用旧口径）；常量用于遍历同步按钮状态。
const BASIS_MODES = { live: "实时价", close: "盘后价" };

// 实时价取当前生效的时段价格：盘中为最新成交价，盘前/盘后取该时段价格，跟随快照刷新；
// 盘后与夜盘时段两种口径取值相同；盘中则不同——旧口径锚定的是上一交易日的盘后价，突破行情下会明显滞后。
function activeBasis(quote) {
  if (state.levelBasisMode === "close") return levelBasis(quote, quote?.price);
  const price = Number(activeSessionQuote(quote)?.price);
  return Number.isFinite(price) && price > 0 ? { price, label: "实时" } : levelBasis(quote, quote?.price);
}

// 切换基准价口径：同步开关的按下状态，再用已有快照重算压力位/支撑位（不额外请求上游接口）。
// 支撑位/压力位表、交易计划与压力位/支撑位柱状图都由这一次重绘一起更新。
function applyBasisMode(mode) {
  state.levelBasisMode = mode === "close" ? "close" : "live";
  for (const key of Object.keys(BASIS_MODES)) {
    const button = $(key === "live" ? "basis-live" : "basis-close");
    if (button) button.setAttribute("aria-pressed", String(key === state.levelBasisMode));
  }
  const last = state.lastAnalysis;
  if (!last || !state.lastQuote) return;
  const basis = activeBasis(state.lastQuote);
  state.levelBasisLabel = basis.label;
  last.basis = basis;
  loadFactorLevels(last.points || [], basis.price);
}

// 压力位/支撑位：在现价上下各取 LEVEL_COUNT 个期权持仓最集中的行权价。
// 与 Gamma 敞口图同口径：有未平仓量时按 GEX 排序，盘前整链无持仓量时退回成交量分布。
function pickLevels(points, spot, side, valueOf, count = LEVEL_COUNT) {
  const minGap = spot * 0.005;
  const candidates = points
    .filter((point) => valueOf(point) > 0 && (side === "above" ? point.strike >= spot : point.strike <= spot))
    .sort((a, b) => valueOf(b) - valueOf(a));
  const picked = [];
  for (const point of candidates) {
    if (picked.length >= count) break;
    // 与已选价位过近的行权价跳过，避免 5 条挤在同一段价位上。
    if (picked.some((item) => Math.abs(item.strike - point.strike) < minGap)) continue;
    picked.push(point);
  }
  return picked.sort((a, b) => Math.abs(a.strike - spot) - Math.abs(b.strike - spot));
}

// 逐条渲染价位表（行权价 / 距现价 / 排序口径数值），离现价近的排在前面。
function renderLevelRows(target, levels, spot, valueOf, formatValue, metricLabel) {
  const head = `<div class="level-row level-head"><span>行权价</span><span>距现价</span><span>${metricLabel}</span></div>`;
  if (!levels.length) { target.innerHTML = head + '<div class="levels-empty">现价这一侧没有可用行权价</div>'; return; }
  target.innerHTML = head + levels.map((point) => {
    const gap = (point.strike / spot - 1) * 100;
    const gapText = `${gap >= 0 ? "+" : ""}${gap.toFixed(2)}%`;
    return `<div class="level-row"><span class="level-strike">${formatMoney(point.strike)}</span><span class="level-gap ${gap >= 0 ? "up" : "down"}">${gapText}</span><span class="level-value">${formatValue(valueOf(point))}</span></div>`;
  }).join("");
}

// 单因子回退时把选出的行权价转成柱状图口径：综合强度按本侧最大值归一（与多因子接口一致）。
function fallbackLevelSeries(picked, valueOf, metricLabel) {
  const peak = Math.max(0, ...picked.map(valueOf)) || 1;
  return picked.map((point) => ({ price: point.strike, score: valueOf(point) / peak, factors: [metricLabel], probability: null }));
}

function renderLevels(points, spot) {
  const price = Number(spot);
  const resistanceTarget = $("resistance-levels");
  const supportTarget = $("support-levels");
  if (!points.length || !Number.isFinite(price) || price <= 0) {
    resistanceTarget.innerHTML = '<div class="levels-empty">暂无数据</div>';
    supportTarget.innerHTML = '<div class="levels-empty">暂无数据</div>';
    $("resistance-note").textContent = `现价上方持仓最集中的 ${LEVEL_COUNT} 个价位`;
    $("support-note").textContent = `现价下方持仓最集中的 ${LEVEL_COUNT} 个价位`;
    renderLevelsChart(null);
    renderTrend(null);
    renderPlan(null, price);
    return;
  }
  const openInterest = points.reduce((sum, point) => sum + point.callOi + point.putOi, 0);
  const volume = points.reduce((sum, point) => sum + point.callVolume + point.putVolume, 0);
  // 未平仓量合计不足成交量 20% 时视为持仓数据不完整（上游盘前会整链返回 0），改用成交量口径。
  const byGex = openInterest > 0 && openInterest >= volume * 0.2;
  const callValue = byGex ? (point) => Math.max(point.callGex, 0) : (point) => point.callVolume;
  const putValue = byGex ? (point) => Math.max(-point.putGex, 0) : (point) => point.putVolume;
  const metricLabel = byGex ? "Gamma 敞口" : "成交量";
  const formatValue = byGex ? (value) => formatGex(value, 2) : (value) => formatCount(value, 2);
  const scope = `到期日 ${state.expiration || "--"} · 按${metricLabel}排序`;
  $("resistance-note").textContent = scope;
  $("support-note").textContent = scope;
  const resistance = pickLevels(points, price, "above", callValue);
  const support = pickLevels(points, price, "below", putValue);
  renderLevelRows(resistanceTarget, resistance, price, callValue, formatValue, metricLabel);
  renderLevelRows(supportTarget, support, price, putValue, formatValue, metricLabel);
  // 单因子回退时柱状图按同一批行权价绘制，综合强度按本侧最大值归一。
  const resistanceSeries = fallbackLevelSeries(resistance, callValue, metricLabel);
  const supportSeries = fallbackLevelSeries(support, putValue, metricLabel);
  renderLevelsChart({ spot: price, resistance: resistanceSeries, support: supportSeries });
  // 回退口径没有日线历史，趋势通道留空；交易计划按同一批支撑/压力价位切成三段。
  renderTrend(null);
  renderPlan({ buy: supportSeries.slice(0, PLAN_COUNT), add: supportSeries.slice(PLAN_COUNT, PLAN_COUNT * 2), sell: resistanceSeries.slice(0, PLAN_COUNT) }, price);
}

// 多因子压力位/支撑位：由后端按「斐波那契回撤 + 筹码密集 + 承接位 + 所选到期日期权持仓」合成，
// 前端只负责渲染；同一标的、同一到期日、同一快照只请求一次，图表尺寸变化时复用已有结果。
function loadFactorLevels(points, spot) {
  if (!points.length || !state.expiration) { renderLevels(points, spot); return; }
  // 缓存键带上基准价口径：两个口径取到同一价格时（盘后/夜盘时段）也各自成键，切换必然重绘一次。
  const key = `${state.symbol}|${state.expiration}|${state.chainFetchedAt || ""}|${Number(spot).toFixed(2)}|${state.levelBasisMode}`;
  // 已请求过：窗口尺寸变化时直接用缓存结果重绘，不再打接口。
  if (state.levelsKey === key) {
    if (state.levelsPayload) renderFactorLevels(state.levelsPayload);
    else renderLevels(points, spot);
    return;
  }
  state.levelsKey = key;
  state.levelsPayload = null;
  const encodedSymbol = encodeURIComponent(state.symbol);
  const encodedExpiration = encodeURIComponent(state.expiration);
  request(`/api/levels/${encodedSymbol}?expiration=${encodedExpiration}&spot=${encodeURIComponent(spot)}`)
    .then((payload) => {
      if (state.levelsKey !== key) return; // 期间切换了标的或到期日，丢弃过期结果
      state.levelsPayload = payload;
      renderFactorLevels(payload);
    })
    .catch(() => {
      if (state.levelsKey !== key) return;
      // 合成接口不可用时退回「按期权持仓」的单因子口径，表格与图表都不空着。
      renderLevels(points, spot);
    });
}

// 渲染多因子结果：价位 / 距现价 / 综合依据（组成该价位的因子标签）。
// 到达概率：0~1 的概率值转百分比，极小/极大用不等号，缺数据用占位符。
function formatProbability(value) {
  if (value == null || !Number.isFinite(Number(value))) return "--";
  const percent = Number(value) * 100;
  if (percent < 0.1) return "<0.1%";
  if (percent > 99.9) return ">99.9%";
  return `${percent.toFixed(1)}%`;
}

function renderFactorRows(target, levels, spot) {
  const head = `<div class="level-row level-head level-factor-row"><span>价位</span><span>距现价</span><span title="在所选到期日之前触及该价位的概率：按该到期日隐含波动率、零漂移的首次触及模型估算">到达概率</span><span>综合依据</span></div>`;
  if (!levels.length) { target.innerHTML = head + '<div class="levels-empty">现价这一侧暂无可用价位</div>'; return; }
  target.innerHTML = head + levels.map((level) => {
    const gap = Number.isFinite(spot) && spot > 0 ? (Number(level.price) / spot - 1) * 100 : null;
    const gapText = gap == null ? "--" : `${gap >= 0 ? "+" : ""}${gap.toFixed(2)}%`;
    const gapClass = gap == null ? "" : (gap >= 0 ? "up" : "down");
    const factors = (level.factors || []).join(" · ");
    return `<div class="level-row level-factor-row" title="综合强度 ${Number(level.score).toFixed(2)}（1 为最强）"><span class="level-strike">${formatMoney(level.price)}</span><span class="level-gap ${gapClass}">${gapText}</span><span class="level-prob">${formatProbability(level.probability)}</span><span class="level-factors">${factors}</span></div>`;
  }).join("");
}

// 高低点行：52 周与历史最高/最低价（取自日线最高/最低价；悬停显示发生日期与距现价）。
function trendExtremeRows(extremes, spot) {
  const price = Number(spot);
  return [
    ["52周最高", extremes?.week52?.high],
    ["52周最低", extremes?.week52?.low],
    ["历史最高", extremes?.all_time?.high],
    ["历史最低", extremes?.all_time?.low],
  ].map(([label, item]) => {
    const value = Number(item?.price);
    const valid = Number.isFinite(value) && value > 0;
    const gap = valid && Number.isFinite(price) && price > 0 ? (value / price - 1) * 100 : null;
    const gapText = gap == null ? "" : ` · 距现价 ${gap >= 0 ? "+" : ""}${gap.toFixed(2)}%`;
    const when = valid && item?.date ? `（${formatDay(item.date)}）` : "";
    const title = valid ? `${label} ${formatMoney(value)}${when}${gapText}` : `${label} 暂无数据`;
    return { valid, html: `<div class="trend-meta" title="${title}"><span>${label}</span><strong>${valid ? formatMoney(value) : "--"}</strong></div>` };
  });
}

// 趋势通道：展示日线线性回归得到的方向、上下轨与日均斜率，以及 52 周 / 历史最高最低价。
function renderTrend(trend, extremes, spot, historyMeta) {
  const target = $("trend-body");
  if (!target) return;
  const note = $("trend-note");
  const extremeRows = trendExtremeRows(extremes, spot);
  const hasExtremes = extremeRows.some((row) => row.valid);
  if (!trend && !hasExtremes) {
    target.innerHTML = '<div class="levels-empty">历史行情不足，暂无趋势判断</div>';
    if (note) note.textContent = "等待数据";
    return;
  }
  const className = trend?.direction === "up" ? "up" : (trend?.direction === "down" ? "down" : "range");
  const slope = Number(trend?.slope_percent) || 0;
  const rows = trend ? [
    ["通道上轨", formatMoney(trend.upper)],
    ["通道下轨", formatMoney(trend.lower)],
    ["日均斜率", `${slope >= 0 ? "+" : ""}${slope.toFixed(3)}%`],
    ["样本", `${Number(trend.bars) || 0} 根日线`],
  ] : [];
  const head = trend ? `<strong class="trend-label ${className}">${trend.label}</strong>` : '<div class="levels-empty">历史行情不足，暂无趋势判断</div>';
  target.innerHTML = head + rows.map(([label, value]) => `<div class="trend-meta"><span>${label}</span><strong>${value}</strong></div>`).join("") + (hasExtremes ? extremeRows.map((row) => row.html).join("") : "");
  if (note) {
    note.textContent = "按最近日线收盘价的线性回归通道；高低点取日线最高/最低价（历史极值用全量历史）";
    const meta = historyMeta || {};
    const parts = [
      meta.extremes_fetched_at ? `高低点快照 ${formatTime(meta.extremes_fetched_at)}` : null,
      meta.extremes_source ? `来源 ${meta.extremes_source}` : null,
      meta.extremes_warning ? `高低点降级：${meta.extremes_warning}` : null,
    ].filter(Boolean);
    if (parts.length) note.title = parts.join(" · ");
  }
}

// 交易计划价位表：价位 / 距现价 / 到达概率 / 综合依据；综合强度仍放在悬停提示里。
function renderPlanRows(target, levels, spot) {
  if (!target) return;
  const head = '<div class="level-row level-head level-plan-row"><span>价位</span><span>距现价</span><span title="在所选到期日之前触及该价位的概率：按该到期日隐含波动率、零漂移的首次触及模型估算">到达概率</span><span>综合依据</span></div>';
  if (!levels.length) { target.innerHTML = head + '<div class="levels-empty">暂无可用价位</div>'; return; }
  target.innerHTML = head + levels.map((level) => {
    const gap = Number.isFinite(spot) && spot > 0 ? (Number(level.price) / spot - 1) * 100 : null;
    const gapText = gap == null ? "--" : `${gap >= 0 ? "+" : ""}${gap.toFixed(2)}%`;
    const gapClass = gap == null ? "" : (gap >= 0 ? "up" : "down");
    const factors = (level.factors || []).join(" · ");
    return `<div class="level-row level-plan-row" title="综合强度 ${Number(level.score || 0).toFixed(2)}（1 为最强）"><span class="level-strike">${formatMoney(level.price)}</span><span class="level-gap ${gapClass}">${gapText}</span><span class="level-prob">${formatProbability(level.probability)}</span><span class="level-factors">${factors}</span></div>`;
  }).join("");
}

// 交易计划：买入 = 现价下方最近的 5 个支撑，加仓 = 更深一档的 5 个支撑，卖出 = 上方最近的 5 个压力。
function renderPlan(plan, spot) {
  const price = Number(spot);
  renderPlanRows($("buy-levels"), plan?.buy || [], price);
  renderPlanRows($("add-levels"), plan?.add || [], price);
  renderPlanRows($("sell-levels"), plan?.sell || [], price);
}

function renderFactorLevels(payload) {
  const spot = Number(payload?.spot);
  const expiration = payload?.expiration || state.expiration || "--";
  const metric = payload?.options_metric === "volume" ? "成交量" : "Gamma 敞口";
  const hasHistory = Number(payload?.history?.bars) > 0;
  const scope = hasHistory
    ? `斐波那契 · 筹码密集 · 承接位 · 期权持仓（${metric}）综合 · 到期日 ${expiration}`
    : `历史行情不可用，按期权持仓（${metric}）计算 · 到期日 ${expiration}`;
  const detail = [hasHistory ? `日线 ${payload.history.bars} 根` : null, payload?.history?.warning ? `历史行情降级：${payload.history.warning}` : null, "到达概率：按该到期日隐含波动率与剩余期限的零漂移首次触及概率"].filter(Boolean).join(" · ");
  const basisNote = Number.isFinite(spot) && spot > 0 ? ` · 基准 ${formatMoney(spot)}（${state.levelBasisLabel || "常规"}）` : "";
  for (const id of ["resistance-note", "support-note"]) { $(id).textContent = scope + basisNote; $(id).title = detail; }
  renderFactorRows($("resistance-levels"), payload?.resistance || [], spot);
  renderFactorRows($("support-levels"), payload?.support || [], spot);
  renderLevelsChart(payload);
  renderTrend(payload?.trend || null, payload?.extremes || null, spot, payload?.history || null);
  renderPlan(payload?.plan, spot);
}

// expirationRows 为上方所选到期日（期权链表格）的合约：Gamma 敞口与两张分布图都以它为唯一口径。
// 压力位/支撑位柱状图：横轴为价位（按价格线性排布），柱高为综合强度（1 为最强），
// 压力位向上（绿）、支撑位向下（红）；悬停显示价位、距现价、到达概率与综合依据。
function renderLevelsChart(payload) {
  const target = $("levels-chart");
  if (!target) return;
  const spot = Number(payload?.spot);
  const levels = [
    ...(payload?.resistance || []).map((item) => ({ ...item, side: "up" })),
    ...(payload?.support || []).map((item) => ({ ...item, side: "down" })),
  ]
    .map((item) => ({ price: Number(item.price), score: Math.max(0, Number(item.score) || 0), probability: item.probability, factors: item.factors || [], side: item.side }))
    .filter((item) => Number.isFinite(item.price) && item.price > 0);
  const basisBadge = $("levels-basis");
  if (basisBadge) basisBadge.textContent = Number.isFinite(spot) && spot > 0 ? `基准 ${formatMoney(spot)}` : "基准 --";
  if (!levels.length) { target.innerHTML = '<div class="chart-empty">暂无压力位/支撑位数据</div>'; return; }
  const { width, height } = chartContentBox(target);
  target.dataset.chartWidth = String(width); target.dataset.chartHeight = String(height);
  const pad = { left: 48, right: 12, top: 14, bottom: 26 };
  const innerWidth = width - pad.left - pad.right;
  const innerHeight = height - pad.top - pad.bottom;
  const baseline = pad.top + innerHeight / 2;
  // 上下各留一条「标签通道」，柱高最高只顶到通道内侧，柱端不会压到坐标刻度。
  const gutter = Math.min(44, Math.max(16, innerHeight / 2 - 16));
  const maxBarHeight = innerHeight / 2 - gutter;
  const maxScore = Math.max(0.01, ...levels.map((item) => item.score));
  const ordered = [...levels].sort((a, b) => a.price - b.price);
  const prices = ordered.map((item) => item.price);
  const rawMin = prices[0];
  const rawMax = prices[prices.length - 1];
  const span = rawMax - rawMin || Math.max(1, rawMax * 0.02);
  // 横轴按价格线性排布（真实反映价位间距），首尾各留 6% 余量避免柱子贴边。
  const domainMin = rawMin - span * 0.06;
  const domainMax = rawMax + span * 0.06;
  const scale = innerWidth / (domainMax - domainMin);
  const xOf = (price) => pad.left + (price - domainMin) * scale;
  // 柱宽取相邻价位最小间距的 60%，价位密集时柱子也不会互相压盖。
  const gaps = prices.slice(1).map((value, index) => value - prices[index]).filter((value) => value > 0);
  const minGap = gaps.length ? Math.min(...gaps) : span;
  const barWidth = Math.max(3, Math.min(26, minGap * scale * 0.6));
  const yLabel = maxScore.toFixed(2);
  // 价位刻度按像素间距抽样：横轴按价格线性排布、间距不均，必须按实际像素判断而不是按条数取样。
  const minLabelGap = 46;
  let lastLabelX = -Infinity;
  const barNodes = ordered.map((item) => {
    const centerX = xOf(item.price);
    const barHeight = item.score / maxScore * maxBarHeight;
    const y = item.side === "up" ? baseline - barHeight : baseline;
    const className = item.side === "up" ? "chart-call" : "chart-put";
    const title = `${item.side === "up" ? "压力位" : "支撑位"} ${formatMoney(item.price)} · 强度 ${item.score.toFixed(2)}`;
    const showLabel = centerX - lastLabelX >= minLabelGap;
    if (showLabel) lastLabelX = centerX;
    const label = showLabel ? `<text class="chart-label" x="${centerX}" y="${height - 8}" text-anchor="middle">${formatMoney(item.price)}</text>` : "";
    return `<rect class="${className}" x="${centerX - barWidth / 2}" y="${y}" width="${barWidth}" height="${barHeight}" rx="1"><title>${title}</title></rect>${label}`;
  }).join("");
  const gridLines = [baseline - maxBarHeight, baseline, baseline + maxBarHeight].map((line) => `<line class="chart-grid" x1="${pad.left}" x2="${width - pad.right}" y1="${line}" y2="${line}"/>`).join("");
  const axisLabels = `<text class="chart-label" x="4" y="${baseline - maxBarHeight + 4}">${yLabel}</text><text class="chart-label" x="4" y="${baseline + 4}">0</text><text class="chart-label" x="4" y="${baseline + maxBarHeight + 4}">-${yLabel}</text>`;
  const basisLine = Number.isFinite(spot) && spot > 0 && spot >= domainMin && spot <= domainMax
    ? `<line class="chart-level-basis" x1="${xOf(spot)}" x2="${xOf(spot)}" y1="${pad.top}" y2="${height - pad.bottom}"/><text class="chart-label" x="${Math.min(Math.max(xOf(spot), pad.left + 14), width - pad.right - 14)}" y="${pad.top + 10}" text-anchor="middle">基准</text>`
    : "";
  const cornerLabels = `<text class="chart-label" x="${width - 12}" y="${pad.top + 10}" text-anchor="end">压力 ↑</text><text class="chart-label" x="${width - 12}" y="${height - pad.bottom - 3}" text-anchor="end">支撑 ↓</text>`;
  // 与成交量/持仓量图一致的十字虚线：竖向跟随鼠标所在价位、横向贴鼠标高度，两端各带取值标签。
  const crosshairTags = `<line class="chart-crosshair-h" x1="${pad.left}" x2="${width - pad.right}" y1="0" y2="0" visibility="hidden"/><g class="chart-tag chart-tag-x" visibility="hidden"><rect rx="2" height="16" width="0"/><text></text></g><g class="chart-tag chart-tag-y" visibility="hidden"><rect rx="2" height="16" width="0"/><text></text></g>`;
  target.innerHTML = `<svg viewBox="0 0 ${width} ${height}" role="img" tabindex="0" aria-label="压力位/支撑位柱状图">${gridLines}${axisLabels}${basisLine}${barNodes}${cornerLabels}<line class="chart-crosshair" x1="0" x2="0" y1="${pad.top}" y2="${height - pad.bottom}" visibility="hidden"/>${crosshairTags}</svg><div class="chart-tooltip" hidden></div>`;

  const chartSvg = target.querySelector("svg");
  const crosshair = target.querySelector(".chart-crosshair");
  const crosshairLine = target.querySelector(".chart-crosshair-h");
  const tagX = target.querySelector(".chart-tag-x");
  const tagY = target.querySelector(".chart-tag-y");
  const tooltip = target.querySelector(".chart-tooltip");
  // SVG 与绘图区之间可能有留白，坐标换算走屏幕矩阵，保证十字虚线与数据卡严格对齐。
  const clientToViewBox = (clientX, clientY) => {
    const matrix = chartSvg.getScreenCTM();
    if (matrix && chartSvg.createSVGPoint) {
      const point = chartSvg.createSVGPoint();
      point.x = clientX; point.y = clientY;
      return point.matrixTransform(matrix.inverse());
    }
    const rect = chartSvg.getBoundingClientRect();
    return { x: ((clientX - rect.left) / rect.width) * width, y: ((clientY - rect.top) / rect.height) * height };
  };
  const viewBoxXToClient = (viewBoxX) => {
    const matrix = chartSvg.getScreenCTM();
    if (matrix && chartSvg.createSVGPoint) {
      const point = chartSvg.createSVGPoint();
      point.x = viewBoxX; point.y = 0;
      return point.matrixTransform(matrix).x;
    }
    const rect = chartSvg.getBoundingClientRect();
    return rect.left + (viewBoxX / width) * rect.width;
  };
  const nearestLevel = (clientX) => {
    const localX = clientToViewBox(clientX, 0).x;
    return ordered.reduce((best, item) => (best == null || Math.abs(xOf(item.price) - localX) < Math.abs(xOf(best.price) - localX) ? item : best), null);
  };
  // 十字光标取值标签：左侧贴当前鼠标高度的强度值，底部贴所在价位。
  const updateTags = (item, clientY) => {
    const hidden = clientY == null || !item;
    crosshairLine.setAttribute("visibility", hidden ? "hidden" : "visible");
    tagX.setAttribute("visibility", hidden ? "hidden" : "visible");
    tagY.setAttribute("visibility", hidden ? "hidden" : "visible");
    if (hidden) return;
    const localY = Math.min(Math.max(clientToViewBox(0, clientY).y, pad.top + 6), height - pad.bottom - 6);
    crosshairLine.setAttribute("y1", localY);
    crosshairLine.setAttribute("y2", localY);
    const value = (baseline - localY) / maxBarHeight * maxScore;
    const valueText = `${value < 0 ? "-" : ""}${Math.abs(value).toFixed(2)}`;
    const valueWidth = markerLabelWidth(valueText, 700) + 10;
    tagY.querySelector("rect").setAttribute("x", 4);
    tagY.querySelector("rect").setAttribute("y", localY - 8);
    tagY.querySelector("rect").setAttribute("width", valueWidth);
    const valueNode = tagY.querySelector("text");
    valueNode.setAttribute("x", 9);
    valueNode.setAttribute("y", localY + 4);
    valueNode.textContent = valueText;
    const priceText = formatMoney(item.price);
    const priceWidth = markerLabelWidth(priceText, 700) + 10;
    const priceLeft = Math.min(Math.max(xOf(item.price) - priceWidth / 2, 2), width - priceWidth - 2);
    tagX.querySelector("rect").setAttribute("x", priceLeft);
    tagX.querySelector("rect").setAttribute("y", height - 20);
    tagX.querySelector("rect").setAttribute("width", priceWidth);
    const priceNode = tagX.querySelector("text");
    priceNode.setAttribute("x", priceLeft + 5);
    priceNode.setAttribute("y", height - 8);
    priceNode.textContent = priceText;
  };
  const showTooltip = (item, clientY) => {
    if (!item) return;
    const gap = Number.isFinite(spot) && spot > 0 ? (item.price / spot - 1) * 100 : null;
    const gapText = gap == null ? "--" : `${gap >= 0 ? "+" : ""}${gap.toFixed(2)}%`;
    const sideLabel = item.side === "up" ? "压力位" : "支撑位";
    const rows = [
      `<div class="chart-tooltip-row"><span>距现价</span><strong>${gapText}</strong></div>`,
      `<div class="chart-tooltip-row"><span>到达概率</span><strong>${formatProbability(item.probability)}</strong></div>`,
      `<div class="chart-tooltip-row"><span>综合强度</span><strong>${item.score.toFixed(2)}</strong></div>`,
    ];
    const factors = (item.factors || []).join(" · ");
    tooltip.innerHTML = `<div class="chart-tooltip-title"><i class="tooltip-dot ${item.side === "up" ? "call" : "put"}"></i>${sideLabel} ${formatMoney(item.price)}</div>${rows.join("")}${factors ? `<div class="chart-tooltip-sub">${factors}</div>` : ""}`;
    crosshair.setAttribute("x1", xOf(item.price));
    crosshair.setAttribute("x2", xOf(item.price));
    crosshair.setAttribute("visibility", "visible");
    tooltip.hidden = false;
    const targetRect = target.getBoundingClientRect();
    const tooltipWidth = tooltip.offsetWidth || 180;
    const lineClientX = viewBoxXToClient(xOf(item.price));
    tooltip.style.left = `${Math.max(8, Math.min(targetRect.width - tooltipWidth - 8, lineClientX - targetRect.left - tooltipWidth / 2))}px`;
    tooltip.style.top = "auto";
    tooltip.style.bottom = `${targetRect.height + 4}px`;
    updateTags(item, clientY);
  };
  const hideTooltip = () => { crosshair.setAttribute("visibility", "hidden"); tooltip.hidden = true; crosshairLine.setAttribute("visibility", "hidden"); tagX.setAttribute("visibility", "hidden"); tagY.setAttribute("visibility", "hidden"); };
  chartSvg.addEventListener("pointermove", (event) => showTooltip(nearestLevel(event.clientX), event.clientY));
  chartSvg.addEventListener("pointerleave", hideTooltip);
  chartSvg.addEventListener("focus", () => showTooltip(ordered[0], null));
  chartSvg.addEventListener("blur", hideTooltip);
}

function renderAnalysis(rows, spot, analysisPayload, expirationRows = [], ivModel = {}, basis = null) {
  // 期权链聚合结果：Gamma 敞口、分布图与压力位/支撑位共用；切换基准价开关时直接复用，不重新聚合。
  const points = aggregateByStrike(expirationRows, spot);
  // 记录本次分析输入，窗口尺寸变化（含手机横竖屏切换）与基准价切换后都按这些输入重绘。
  state.lastAnalysis = { rows, spot, analysisPayload, expirationRows, ivModel, basis, points };
  // 压力位/支撑位用「基准价」（默认实时价，可切盘后价），图表仍用常规价。
  const levelSpot = Number(basis?.price) > 0 ? Number(basis.price) : spot;
  state.levelBasisLabel = basis?.label || "常规";
  const scopeText = expirationRows.length ? `到期日 ${state.expiration || "--"} · ${expirationRows.length} 个合约` : "当前到期日无数据";
  if ($("volume-scope")) $("volume-scope").textContent = scopeText;
  if ($("oi-scope")) $("oi-scope").textContent = scopeText;
  const modelIv = Number(ivModel?.[state.expiration]?.iv);
  const scopeSuffix = Number.isFinite(modelIv) && modelIv > 0 ? ` · 模型 IV ${(modelIv * 100).toFixed(1)}%` : "";
  const serverFlip = Number(analysisPayload?.zero_gamma?.price);
  // 零 Gamma 与柱状图同口径：先按所选到期日扫描，找不到穿越点再退回服务端结果。
  const gammaFlip = findGammaFlip(expirationRows, spot, state.expiration)
    || (Number.isFinite(serverFlip) ? { strike: serverFlip } : null);
  const callWall = points.reduce((best, point) => point.callGex > (best?.callGex || 0) ? point : best, null);
  const putWall = points.reduce((best, point) => Math.abs(point.putGex) > Math.abs(best?.putGex || 0) ? point : best, null);
  const volumeCallPeak = maxPoint(points, "callVolume");
  const volumePutPeak = maxPoint(points, "putVolume");
  const oiCallPeak = maxPoint(points, "callOi");
  const oiPutPeak = maxPoint(points, "putOi");
  $("net-gex").textContent = `净 Gamma ${formatGex(points.reduce((sum, point) => sum + point.callGex + point.putGex, 0), 2)}`;
  $("gamma-flip").textContent = `零 Gamma ${gammaFlip ? formatMoney(gammaFlip.strike) : "--"}`;
  $("gamma-scope").textContent = `柱状图 ${scopeText}${scopeSuffix}`;
  const analysisFallback = analysisPayload?.oi_fallback || {};
  if (analysisFallback.restored) $("gamma-scope").textContent += ` · 未平仓量回溯 ${formatDay(analysisFallback.as_of)}`;
  $("call-wall").textContent = `看涨墙 ${callWall?.callGex ? formatMoney(callWall.strike) : "--"}`;
  $("put-wall").textContent = `看跌墙 ${putWall?.putGex ? formatMoney(putWall.strike) : "--"}`;
  loadFactorLevels(points, levelSpot);
  renderSignedChart("gex-chart", points, "callGex", "putGex", "M", "当前期权链未提供 Gamma，暂无法估算 GEX", { spot, gammaFlip, crosshairTags: true, markers: [
    { point: callWall, className: "chart-wall-call", label: "看涨墙", position: "top" },
    { point: putWall, className: "chart-wall-put", label: "看跌墙", position: "bottom" },
  ] });
  renderSignedChart("volume-chart", points, "callVolume", "putVolume", "", "暂无成交量分布", { axis: "right", crosshairTags: true, valueLabel: "成交量", markers: [
    { point: volumeCallPeak, className: "chart-wall-call", label: "看涨", position: "top" },
    { point: volumePutPeak, className: "chart-wall-put", label: "看跌", position: "bottom" },
  ] });
  renderSignedChart("oi-chart", points, "callOi", "putOi", "", "暂无持仓量分布", { axis: "right", crosshairTags: true, valueLabel: "持仓量", markers: [
    { point: oiCallPeak, className: "chart-wall-call", label: "看涨", position: "top" },
    { point: oiPutPeak, className: "chart-wall-put", label: "看跌", position: "bottom" },
  ] });
  renderDistributionSummary($("volume-summary"), expirationRows, spot, "volume", "总成交量");
  renderDistributionSummary($("oi-summary"), expirationRows, spot, "open_interest", "总持仓量");
}

async function request(path, options = {}) {
  // 每次请求都重新读取 localStorage，确保用户在别处清空存储后立即触发 403，而不是继续使用内存里的旧值。
  const accessKey = readStoredAccessKey();
  if (accessKeyRequired() && !accessKey) {
    const message = "403 Forbidden";
    showAccessDenied();
    throw new Error(message);
  }
  const headers = new Headers(options.headers || {});
  if (accessKey) headers.set("X-Access-Key", accessKey);
  const response = await fetch(path, { ...options, headers });
  const body = await response.json().catch(() => ({}));
  if (response.status === 403) {
    const message = body.detail || "403 Forbidden";
    showAccessDenied();
    throw new Error(message);
  }
  if (!response.ok) throw new Error(body.detail || `请求失败 (${response.status})`);
  return body;
}

// 现价按时段动态取值：盘前显示盘前价，盘后/夜盘显示盘后价，盘中与休市显示常规价。
// 对应时段没有数据时回退常规价，避免整块行情空掉。
function activeSessionQuote(quote) {
  const sessions = quote?.sessions || {};
  const marketState = quote?.market_state;
  if (marketState === "PRE" && sessions.pre?.price != null) return sessions.pre;
  if ((marketState === "POST" || marketState === "OVERNIGHT") && sessions.post?.price != null) return sessions.post;
  return quote;
}

function renderQuote(quote) {
  const active = activeSessionQuote(quote);
  const price = active?.price ?? quote?.price;
  const change = active?.change_percent ?? quote?.change_percent;
  $("quote-symbol").textContent = quote?.symbol || state.symbol;
  $("quote-price").textContent = formatMoney(price);
  $("quote-change").textContent = change == null ? "涨跌 --" : `涨跌 ${change >= 0 ? "+" : ""}${Number(change).toFixed(2)}%`;
  // 涨跌色统一走 --up / --down：当前全局口径是绿涨红跌，变量名不再写死颜色。
  $("quote-change").style.color = change < 0 ? "var(--down)" : "var(--up)";
  $("quote-currency").textContent = quote?.currency || "USD";
  $("quote-market").textContent = marketStateLabel(quote?.market_state);
  $("market-state").textContent = marketStateLabel(quote?.market_state, "快照数据");
}

// 时段标签：数据源若返回未知取值就原样展示，避免换口径时把信息吞掉。
function marketStateLabel(value, fallback = "快照") {
  if (!value) return fallback;
  return MARKET_STATE_LABELS[value] || value;
}

// 期权链表格里的 Gamma 与 IV 同样优先展示价格反解出的模型值，避免展示数据源里的占位 IV。
function formatModelGamma(row) {
  const value = row.model_gamma ?? row.gamma;
  return value == null || !Number.isFinite(Number(value)) ? "--" : Number(value).toFixed(5);
}
function formatModelIv(row) {
  const value = row.model_iv ?? row.implied_volatility;
  return value == null || !Number.isFinite(Number(value)) ? "--" : `${(Number(value) * 100).toFixed(1)}%`;
}

// 热力强度：√ 缩放压缩极端值（同一列常跨三四个数量级），返回 0~100 的相对强度；0 值不着色。
function heatPercent(value, peak) {
  const number = Number(value) || 0;
  if (!(Number(peak) > 0) || number <= 0) return null;
  return Math.round(Math.sqrt(Math.min(number / Number(peak), 1)) * 100);
}

// 热力单元格：底色深浅表示该值在本列的相对强弱，悬停给出数值与本列最强值。
function heatCell(value, peak, label, hotLevel) {
  const number = Number(value) || 0;
  const percent = heatPercent(number, peak);
  const title = percent == null
    ? `${label} ${formatNumber(number)}`
    : `${label} ${formatNumber(number)} · 本屏最强 ${formatNumber(peak)}（占 ${Math.round((number / Number(peak)) * 100)}%）`;
  if (percent == null) return `<td class="num chain-heat" title="${title}">${formatNumber(number)}</td>`;
  const hot = percent >= hotLevel ? " chain-heat-hot" : "";
  return `<td class="num chain-heat${hot}" style="--heat:${percent}" title="${title}">${formatNumber(number)}</td>`;
}

// 单张合约的 GEX（美元 / 现价每变动 1%）：与 Gamma 敞口图同口径（模型 Gamma × 未平仓 × 100 × 现价² × 0.01），
// 方向也沿用图表约定——看涨为正、看跌为负，这样整列之和就是 Gamma 敞口面板上的「净 Gamma」。
function contractGex(row, spot) {
  const price = Number(spot);
  if (!Number.isFinite(price) || price <= 0) return null;
  const gamma = gammaAtSpot(price, row);
  const value = gamma == null ? Number(row.gamma) : Number(gamma);
  if (!Number.isFinite(value)) return null;
  const sign = row.contract_type === "call" ? 1 : -1;
  return sign * value * (Number(row.open_interest) || 0) * 100 * price ** 2 * 0.01;
}

// 图例：说明底色口径，并给出两列的归一基准（本屏最强值）。
function chainHeatNote(volumePeak, interestPeak) {
  if (!(volumePeak > 0) && !(interestPeak > 0)) return "行权价绿=看涨、红=看跌 · 本屏成交量与未平仓均为 0，暂无可比强度";
  return `行权价绿=看涨、红=看跌 · 底色深浅按该列本屏最强值归一（√ 缩放） · 成交量最强 ${formatNumber(volumePeak)} · 未平仓最强 ${formatNumber(interestPeak)}`;
}

// 期权链表格：行权价在最左，文字颜色即类型（看涨绿 / 看跌红，原先单独的「类型」列已并入行权价）；
// 成交量与未平仓两列按本屏强弱铺底色，另给出 GEX 估值列（与 Gamma 敞口图同口径）。
function renderChainRows(rows, spot, emptyLabel = "没有期权数据") {
  const body = $("chain-body");
  if (!rows.length) {
    body.innerHTML = `<tr><td colspan="7" class="empty">${emptyLabel}</td></tr>`;
    $("chain-heat-note").textContent = chainHeatNote(0, 0);
    return;
  }
  const volumePeak = Math.max(0, ...rows.map((row) => Number(row.volume) || 0));
  const interestPeak = Math.max(0, ...rows.map((row) => Number(row.open_interest) || 0));
  $("chain-heat-note").textContent = chainHeatNote(volumePeak, interestPeak);
  body.innerHTML = rows.map((row) => {
    const isCall = row.contract_type === "call";
    const gex = contractGex(row, spot);
    return [
      `<tr class="${isCall ? "chain-call" : "chain-put"}">`,
      `<td class="num chain-strike ${isCall ? "type-call" : "type-put"}">${formatMoney(row.strike)}</td>`,
      heatCell(row.volume, volumePeak, "成交量", isCall ? HEAT_HOT_LEVEL.call : HEAT_HOT_LEVEL.put),
      heatCell(row.open_interest, interestPeak, "未平仓", isCall ? HEAT_HOT_LEVEL.call : HEAT_HOT_LEVEL.put),
      `<td class="num">${formatModelGamma(row)}</td>`,
      `<td class="num" title="Gamma × 未平仓 × 100 × 现价² × 0.01，即现价每变动 1% 的美元敞口">${gex == null ? "--" : formatGex(gex / 1000000)}</td>`,
      `<td class="num">${formatModelIv(row)}</td>`,
      `<td class="${row.in_the_money ? "itm" : ""}">${row.in_the_money ? "价内" : "价外"}</td>`,
      "</tr>",
    ].join("");
  }).join("");
}

// 期权链筛选（全部 / 看涨 / 看跌）：切换筛选只重绘表格，不重新请求数据；
// 热力底色的归一基准跟着「当前显示的行」走（看涨视图里就以最强的看涨行为 100%）。
function renderChainTable() {
  const rows = state.chainRows || [];
  const shown = state.chainFilter === "all" ? rows : rows.filter((row) => row.contract_type === state.chainFilter);
  const emptyLabel = rows.length ? `当前筛选（${CHAIN_FILTERS[state.chainFilter] || "全部"}）下没有合约` : "没有期权数据";
  renderChainRows(shown, state.chainSpot, emptyLabel);
}

function renderChain(payload, quote, analysisPayload) {
  const rows = payload.data || []; state.expiration = payload.expiration;
  // 基准价开关切换时要用最近一次快照重算，这里留一份引用。
  state.lastQuote = quote || null;
  $("chain-title").textContent = `${payload.symbol} · ${payload.expiration}`;
  // 期权链始终从 SQLite 读取，这里按快照新鲜度标注来源，避免刚抓完还显示“缓存”造成误解。
  const snapshotAge = payload.fetched_at ? (Date.now() - new Date(payload.fetched_at).getTime()) / 1000 : null;
  // 上游在盘前/收盘后可能整链返回 0 未平仓量，读取层会用该合约最近一次有效值兜底，这里如实标注。
  const oiFallback = payload.oi_fallback || {};
  $("data-source").textContent = (snapshotAge != null && snapshotAge >= 0 && snapshotAge < 180 ? "上游新快照" : "SQLite 缓存") + (oiFallback.restored ? ` · 未平仓量回溯 ${formatDay(oiFallback.as_of)}` : "");
  $("fetched-at").textContent = `快照时间 ${formatTime(payload.fetched_at)}`;
  $("total-count").textContent = formatNumber(rows.length);
  const calls = rows.filter((row) => row.contract_type === "call"); const puts = rows.filter((row) => row.contract_type === "put");
  $("call-volume").textContent = formatNumber(calls.reduce((sum, row) => sum + (Number(row.volume) || 0), 0));
  $("put-volume").textContent = formatNumber(puts.reduce((sum, row) => sum + (Number(row.volume) || 0), 0));
  $("call-interest").textContent = `未平仓 ${formatNumber(calls.reduce((sum, row) => sum + (Number(row.open_interest) || 0), 0))}`;
  $("put-interest").textContent = `未平仓 ${formatNumber(puts.reduce((sum, row) => sum + (Number(row.open_interest) || 0), 0))}`;
  const analysisRows = analysisPayload?.data?.length ? analysisPayload.data : rows;
  // 记录本次快照时间：压力位/支撑位的合成接口按「标的 + 到期日 + 快照时间」去重请求。
  state.chainFetchedAt = payload.fetched_at || null;
  renderAnalysis(analysisRows, quote?.price, analysisPayload, rows, payload.iv_model || {}, activeBasis(quote));
  state.chainRows = rows; state.chainSpot = quote?.price ?? null;
  renderChainTable();
}

// emptyLabel：本地没有到期日时的占位文案，需要区分「还没抓过」（正在获取）和「该标的没有期权」。
function applyExpirations(dates, preferred, emptyLabel = "正在获取到期日…") {
  const unique = [...new Set((dates || []).filter(Boolean))];
  const select = $("expiration-select");
  if (!unique.length) {
    select.innerHTML = "<option>" + emptyLabel + "</option>";
    select.disabled = true;
    syncRefreshButton();
    return false;
  }
  select.innerHTML = unique.map((date) => `<option value="${date}">${date}</option>`).join("");
  const requested = preferred || parsePageQuery(location.search).expiration || state.expiration;
  state.expiration = unique.includes(requested) ? requested : unique[0];
  select.value = state.expiration;
  select.disabled = false;
  syncRefreshButton();
  return true;
}

function isCurrentLoad(loadId, expiration) {
  return state.loadId === loadId && (!expiration || state.expiration === expiration);
}

// 快照仍在新鲜期内时不再请求上游接口，只把本地缓存的状态回显给用户。
function isSnapshotFresh(snapshot) {
  const age = snapshotAgeSeconds(snapshot?.fetchedAt);
  return Boolean(snapshot?.shown) && age !== null && age < SNAPSHOT_FRESH_SECONDS;
}

function showFreshStatus(snapshot) {
  const age = snapshotAgeSeconds(snapshot?.fetchedAt);
  $("last-status").textContent = `本地快照 ${formatTime(snapshot?.fetchedAt)} 已是最新（${Math.round(age ?? 0)} 秒前）`;
}

function showPending(message) {
  $("chain-title").textContent = `${state.symbol}${state.expiration ? ` · ${state.expiration}` : ""}`;
  $("data-source").textContent = "后台刷新中";
  $("fetched-at").textContent = "快照时间 --";
  $("total-count").textContent = "--";
  $("call-volume").textContent = "--";
  $("put-volume").textContent = "--";
  $("call-interest").textContent = "未平仓 --";
  $("put-interest").textContent = "未平仓 --";
  $("net-gex").textContent = "净 Gamma --";
  $("gamma-flip").textContent = "零 Gamma --";
  $("call-wall").textContent = "看涨墙 --";
  $("put-wall").textContent = "看跌墙 --";
  $("gamma-scope").textContent = "Gamma 范围 --";
  $("gex-chart").innerHTML = `<div class="chart-empty">${message}</div>`;
  $("volume-chart").innerHTML = '<div class="chart-empty">正在后台加载</div>';
  $("oi-chart").innerHTML = '<div class="chart-empty">正在后台加载</div>';
  $("volume-summary").innerHTML = ""; $("oi-summary").innerHTML = "";
  // 压力位/支撑位会在新快照渲染后重新请求合成接口，这里先清空并解除去重键。
  state.levelsKey = "";
  state.levelsPayload = null;
  $("resistance-note").textContent = "等待数据";
  $("support-note").textContent = "等待数据";
  $("resistance-levels").innerHTML = `<div class="levels-empty">${message}</div>`;
  $("support-levels").innerHTML = `<div class="levels-empty">${message}</div>`;
  $("levels-chart").innerHTML = `<div class="chart-empty">${message}</div>`;
  $("levels-basis").textContent = "基准 --";
  $("trend-body").innerHTML = `<div class="levels-empty">${message}</div>`;
  $("trend-note").textContent = "等待数据";
  for (const id of ["buy-levels", "add-levels", "sell-levels"]) $(id).innerHTML = `<div class="levels-empty">${message}</div>`;
  $("chain-body").innerHTML = `<tr><td colspan="7" class="empty">${message}</td></tr>`;
  $("chain-heat-note").textContent = "等待数据";
}

function applyCachedQuote(quote) {
  if (!quote || quote.price == null) {
    $("quote-symbol").textContent = state.symbol;
    $("quote-price").textContent = "--";
    $("quote-change").textContent = "正在更新";
    $("quote-change").style.color = "var(--muted)";
    $("quote-currency").textContent = "USD";
    $("quote-market").textContent = "后台刷新";
    $("market-state").textContent = "后台刷新中";
    return;
  }
  renderQuote(quote);
}

async function renderSnapshot(loadId) {
  if (!isCurrentLoad(loadId)) return { shown: false, source: null };
  // 记住这次请求对应的到期日：响应回来时如果用户已经切走，整份数据作废（见 isCurrentLoad 的第二个参数），
  // 否则后到的旧响应会把新选择的数据覆盖掉。
  const expiration = state.expiration;
  const encodedSymbol = encodeURIComponent(state.symbol);
  const encodedExpiration = expiration ? encodeURIComponent(expiration) : "";
  const requests = [
    request(`/api/quote/${encodedSymbol}`).catch(() => null),
    expiration ? request(`/api/chain/${encodedSymbol}?expiration=${encodedExpiration}`).catch(() => null) : Promise.resolve(null),
    request(`/api/gamma/${encodedSymbol}?horizon_days=45`).catch(() => null),
  ];
  const [quote, payload, analysis] = await Promise.all(requests);
  if (!isCurrentLoad(loadId, expiration)) return { shown: false, source: null };
  applyCachedQuote(quote);
  if (!payload?.data?.length) return { shown: false, source: payload?.source || quote?.source || null, fetchedAt: payload?.fetched_at || null };
  state.analysisReady = Boolean(analysis?.data?.length) || state.analysisReady;
  renderChain(payload, quote, analysis);
  $("last-status").textContent = payload.source === "sqlite"
    ? `本地缓存 ${formatTime(payload.fetched_at)}`
    : `最近更新 ${formatTime(payload.fetched_at)}`;
  syncPageQuery();
  return { shown: true, source: payload.source || null, fetchedAt: payload.fetched_at || null };
}

// 跨期限 Gamma 窗口刷新最慢（SPY 需要串行拉取十余个到期日），放到后台执行：
// 表格与行情先落地，窗口数据回来后只重画分析区；同一标的只允许一个窗口刷新在飞，避免连点叠加请求。
function refreshAnalysisWindow(loadId, payload, quote) {
  const symbol = state.symbol;
  if (state.analysisRefreshSymbol === symbol) return;
  state.analysisRefreshSymbol = symbol;
  const encodedSymbol = encodeURIComponent(symbol);
  const pendingText = `快照已更新 ${formatTime(payload?.fetched_at)} · 正在后台刷新 Gamma 窗口…`;
  $("last-status").textContent = pendingText;
  request(`/api/gamma/${encodedSymbol}?horizon_days=45&refresh=true`)
    .then((analysis) => {
      if (!isCurrentLoad(loadId) || state.symbol !== symbol) return;
      if (!analysis?.data?.length) return;
      state.analysisReady = true;
      renderChain(payload, quote, analysis);
      // 窗口刷新期间用户可能又点了刷新：只在提示文案还属于本次窗口刷新时才改写，避免覆盖更新的状态。
      if ($("last-status").textContent === pendingText) $("last-status").textContent = `最近更新 ${formatTime(payload?.fetched_at)}`;
    })
    .catch((error) => {
      if (!isCurrentLoad(loadId) || state.symbol !== symbol) return;
      if ($("last-status").textContent === pendingText) $("last-status").textContent = `Gamma 窗口刷新失败，仍显示本地缓存（${error.message}）`;
    })
    .finally(() => { if (state.analysisRefreshSymbol === symbol) state.analysisRefreshSymbol = null; });
}

async function refreshInBackground(loadId) {
  const symbol = state.symbol;
  const expiration = state.expiration;
  const encodedSymbol = encodeURIComponent(symbol);
  if (!isCurrentLoad(loadId) || state.symbol !== symbol) return;
  // 同一标的只允许一条网络刷新链路：页面加载、手动点击、定时刷新共用这份互斥，避免叠加出多组请求。
  if (state.refreshInFlight === symbol) return;
  state.refreshInFlight = symbol;
  try {
    $("last-status").textContent = "正在请求上游快照…";
    // max_age 交给后端再兜底一次：本地快照仍在新鲜期内时后端会直接返回 skipped，不再打上游接口。
    const params = new URLSearchParams({ max_age: String(SNAPSHOT_FRESH_SECONDS) });
    if (expiration) params.set("expiration", expiration);
    const refreshResult = await request(`/api/refresh/${encodedSymbol}?${params}`, { method: "POST" });
    if (!isCurrentLoad(loadId) || state.symbol !== symbol) return;
    // 后端在标的没有挂牌期权时只写现货快照：走现货渲染分支，避免页面一直停在“后台刷新中”。
    if (refreshResult?.quote_only) { await loadQuoteOnly(loadId); return; }
    if (refreshResult?.skipped) {
      $("last-status").textContent = `本地快照 ${formatTime(refreshResult.fetched_at)} 已是最新（${Math.round(Number(refreshResult.age_seconds) || 0)} 秒前）`;
      return;
    }
    if (!state.expiration && refreshResult?.expiration) {
      applyExpirations([refreshResult.expiration], refreshResult.expiration);
    }
    const currentExpiration = state.expiration || refreshResult?.expiration;
    if (!currentExpiration) { await loadQuoteOnly(loadId); return; }
    const encodedExpiration = encodeURIComponent(currentExpiration);
    // 先取最新的期权链与现货：跨期限 Gamma 窗口刷新较慢，表格不能跟着一起等。
    const [payload, quote, expirations] = await Promise.all([
      request(`/api/chain/${encodedSymbol}?expiration=${encodedExpiration}`),
      request(`/api/quote/${encodedSymbol}`),
      request(`/api/expirations/${encodedSymbol}?refresh=true`).catch(() => null),
    ]);
    if (!isCurrentLoad(loadId) || state.symbol !== symbol) return;
    // 响应期间用户切了到期日：这批结果属于旧期限，整体作废并按新选择重来——
    // 否则既会把旧期限的链盖到新选择上，下面的 applyExpirations 还会把选择框拽回旧期限。
    // 本次调用还占着互斥标记，直接重入会被互斥挡住，先释放再重入。
    if (state.expiration !== currentExpiration) {
      state.refreshInFlight = null;
      await refreshInBackground(loadId);
      return;
    }
    if (expirations?.expirations?.length) applyExpirations(expirations.expirations, currentExpiration);
    // 选中的到期日已下架（例如当天盘后过期）时，切到最新到期日并重新加载；同样先释放互斥标记再重入。
    if (state.expiration && state.expiration !== currentExpiration) {
      state.refreshInFlight = null;
      await refreshInBackground(loadId);
      return;
    }
    renderQuote(quote);
    renderChain(payload, quote, state.lastAnalysis?.analysisPayload || null);
    $("last-status").textContent = `最近更新 ${formatTime(payload.fetched_at)}`;
    syncPageQuery();
    refreshAnalysisWindow(loadId, payload, quote);
  } catch (error) {
    if (!isCurrentLoad(loadId) || state.symbol !== symbol) return;
    $("last-status").textContent = `后台刷新失败，仍显示本地缓存（${error.message}）`;
    if (!$("chain-body").querySelector("td:not(.empty)")) setError(error.message);
    // 失败原因通常是所选到期日已过期下架：拉一次最新到期日，必要时自动切换到可刷新的期限。
    state.refreshInFlight = null;
    const fresh = await request(`/api/expirations/${encodedSymbol}?refresh=true`).catch(() => null);
    if (!isCurrentLoad(loadId) || state.symbol !== symbol) return;
    const dates = fresh?.expirations || [];
    if (dates.length && !dates.includes(state.expiration)) {
      applyExpirations(dates, null);
      await loadChain({ loadId });
    }
  } finally {
    if (state.refreshInFlight === symbol) state.refreshInFlight = null;
  }
}

// 标的没有挂牌期权（或可用期限已全部到期）时只渲染现货：现货与盘前盘后照常刷新，
// 期权相关面板统一提示没有期权数据，避免整页停在 “--”。
async function loadQuoteOnly(loadId) {
  const symbol = state.symbol;
  const encodedSymbol = encodeURIComponent(symbol);
  // 现货读本地快照（刷新接口刚写过），到期日必须回源：本地为空只能说明还没抓过，不等于没有期权。
  const [quote, expirations] = await Promise.all([
    request(`/api/quote/${encodedSymbol}`).catch(() => null),
    request(`/api/expirations/${encodedSymbol}?refresh=true`).catch(() => null),
  ]);
  if (!isCurrentLoad(loadId) || state.symbol !== symbol) return;
  const dates = expirations?.expirations || [];
  // 回源后拿到了期限：说明本地只是缺缓存，重走一次完整加载。
  if (dates.length) {
    applyExpirations(dates, null);
    state.refreshInFlight = null;
    await refreshInBackground(loadId);
    return;
  }
  applyCachedQuote(quote);
  // 所选期限可能刚刚全部到期：清掉失效的到期日，避免它继续留在 state 和地址栏里。
  state.expiration = null;
  showPending(symbol + " 没有挂牌期权（或可用期限已全部到期），仅显示现货行情");
  applyExpirations([], null, "无期权到期日");
  $("data-source").textContent = "上游无期权数据";
  $("last-status").textContent = "该标的没有期权合约，仅显示现货行情";
}

async function loadExpirations(loadId) {
  const encodedSymbol = encodeURIComponent(state.symbol);
  const payload = await request(`/api/expirations/${encodedSymbol}`).catch(() => ({ expirations: [] }));
  if (!isCurrentLoad(loadId)) return;
  // source=pending 表示本地还没缓存过（需要回源），与「该标的确实没有期权」是两回事。
  applyExpirations(payload.expirations, undefined, payload.source === "pending" ? "正在获取到期日…" : "该标的没有期权到期日");
  await loadChain({ loadId });
}

async function loadChain(options = {}) {
  const loadId = options.loadId || state.loadId;
  // 即使本地还没有到期日也要往下走：首次加载某个标的时后端需要回源才能拿到期限列表，
  // 提前 return 会让页面永远停在没有数据的状态。
  const snapshot = await renderSnapshot(loadId);
  if (!snapshot.shown) showPending("正在后台获取上游快照…");
  if (options.refresh === false) return;
  // 先读 SQLite 判断新鲜度：仍在新鲜期内直接复用，只有确认过期才请求上游接口。
  if (isSnapshotFresh(snapshot)) { showFreshStatus(snapshot); return; }
  await refreshInBackground(loadId);
}

async function loadSymbol() {
  const symbol = $("symbol-input").value.trim().toUpperCase();
  if (!symbol) return;
  $("symbol-input").value = symbol;
  state.symbol = symbol;
  state.expiration = parsePageQuery(location.search).expiration || null;
  state.analysisReady = false;
  const loadId = ++state.loadId;
  setError("");
  applyCachedQuote(null);
  $("last-status").textContent = "正在读取本地缓存…";
  syncPageQuery();
  try {
    await loadExpirations(loadId);
  } catch (error) {
    if (!isCurrentLoad(loadId)) return;
    $("expiration-select").innerHTML = "<option>加载失败</option>";
    showPending("暂无数据");
    setError(error.message);
  }
}

// 切换到期日：请求在飞时禁用下拉框（连点也切不出第二组请求），数据落地后再放开。
// 并发保护是两层：这一层防重复触发，下面 renderSnapshot / refreshInBackground 还会拿「请求时的到期日」校验，
// 响应回来时若选择已经变了就整份丢弃，杜绝旧数据覆盖新选择。
let expirationSwitchToken = 0;

// 切换到期日时下拉框的禁用兜底时间：超过这个时间还没拿到数据就放开，防止网络卡死导致控件永久不可用。
const EXPIRATION_SWITCH_TIMEOUT_MS = 20000;

async function switchExpiration(expiration) {
  const token = ++expirationSwitchToken;
  state.expiration = expiration;
  setError("");
  setBusy(true);
  // 兜底：请求万一一直不落地（例如上游卡住），20 秒后强制放开下拉框，避免控件被永久禁用。
  // 仍然按 token 校验，期间用户又切了一次就不动新的那次流程的禁用状态。
  const releaseTimer = setTimeout(() => { if (token === expirationSwitchToken) setBusy(false); }, EXPIRATION_SWITCH_TIMEOUT_MS);
  try {
    await loadChain({ loadId: state.loadId });
  } catch (error) {
    setError(error.message);
  } finally {
    clearTimeout(releaseTimer);
    // 正常情况下同一时刻只有一次切换（下拉框已禁用），这里再兜一层，避免旧流程提前放开选择框。
    if (token === expirationSwitchToken) setBusy(false);
  }
}


// 载入按钮与输入框回车都改为真实跳转：地址栏即状态，刷新、前进后退与分享链接都能复现同一视图。
function navigateToSymbol() {
  const symbol = $("symbol-input").value.trim().toUpperCase();
  if (!symbol) return;
  if (!/^[A-Z0-9][A-Z0-9.-]{0,9}$/.test(symbol)) { setError(`标的代码 ${symbol} 无效`); return; }
  setError("");
  // 换标的时清空 URL 里的到期日参数：各标的的到期日序列不同（有的每周五、有的每天、有的半月），
  // 统一回到列表第一个（最近）到期日；同一标的重复载入则保留当前选择。
  const expiration = symbol === state.symbol ? state.expiration : "";
  const target = buildPageQuery(symbol, expiration, location.pathname);
  if (target === `${location.pathname}${location.search}`) location.reload();
  else location.assign(target);
}

// 手动点击与 60 秒定时刷新共用入口：先读 SQLite（renderSnapshot 只读本地缓存），
// 快照仍在新鲜期内就直接复用，只有确认过期才请求上游接口；整段流程互斥，连点与定时器不会叠加请求。
async function refresh(silent = false) {
  if (state.refreshing) return;
  state.refreshing = true;
  syncRefreshButton();
  const loadId = state.loadId;
  try {
    if (!silent) setError("");
    await loadChain({ loadId, refresh: true });
  } catch (error) {
    if (isCurrentLoad(loadId)) setError(error.message);
  } finally {
    state.refreshing = false;
    syncRefreshButton();
  }
}

$("load-button").addEventListener("click", navigateToSymbol); $("refresh-button").addEventListener("click", () => refresh(false)); $("expiration-select").addEventListener("change", (event) => { switchExpiration(event.target.value); }); $("symbol-input").addEventListener("keydown", (event) => { if (event.key === "Enter") navigateToSymbol(); }); $("chain-type-filter").addEventListener("change", (event) => { state.chainFilter = event.target.value; renderChainTable(); });
initTheme();
initDetailGroup();
initChainGroup();
initChartGroup();
setInterval(() => { $("clock").textContent = new Date().toLocaleTimeString("zh-CN", { hour12: false }); }, 1000);
// 容器尺寸与上次绘制不一致时按新尺寸重绘图表：窗口缩放、图表折叠组展开后都走这里。
// 图表是按容器实际像素绘制的，隐藏状态下只能量到最小尺寸，所以展开后必须补一次重绘。
function redrawChartsIfResized() {
  if (!state.lastAnalysis) return;
  const charts = ["gex-chart", "levels-chart", "volume-chart", "oi-chart"].filter((id) => $(id).querySelector("svg"));
  const changed = charts.some((id) => {
    const box = chartContentBox($(id));
    return String(box.width) !== $(id).dataset.chartWidth || String(box.height) !== $(id).dataset.chartHeight;
  });
  if (!changed) return;
  const { rows, spot, analysisPayload, expirationRows, ivModel, basis } = state.lastAnalysis;
  renderAnalysis(rows, spot, analysisPayload, expirationRows || [], ivModel || {}, basis || null);
}

// 窗口尺寸变化后按新尺寸重绘图表；手机滚动时地址栏收起不会改变图表宽度，此时直接跳过重绘。
let chartResizeTimer = null;
window.addEventListener("resize", () => {
  clearTimeout(chartResizeTimer);
  chartResizeTimer = setTimeout(redrawChartsIfResized, 200);
});
const initialQuery = parsePageQuery(location.search);
const initialAccessKey = initializeAccessKey();
// 页面默认标的由服务端按 DEFAULT_SYMBOLS 注入到输入框，这里只在注入缺失时兜底。
state.symbol = initialQuery.symbol || $("symbol-input").value.trim().toUpperCase() || "QQQ";
$("symbol-input").value = state.symbol;
if (accessKeyRequired() && !initialAccessKey) {
  showAccessDenied();
} else {
  // 没有到期日（仅现货标的）也要走刷新链路：后端会返回 quote_only，只更新现货卡片。
  state.timer = setInterval(() => refresh(true), AUTO_REFRESH_SECONDS * 1000);
  loadSymbol();
}
