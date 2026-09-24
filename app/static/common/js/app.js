const defaultSymbolValue = (document.getElementById("symbol-input")?.getAttribute("value") || "").trim().toUpperCase();
const defaultSymbol = /^[A-Z0-9][A-Z0-9.-]{0,9}$/.test(defaultSymbolValue) ? defaultSymbolValue : "QQQ";
const state = {
  symbol: defaultSymbol,
  symbolInput: defaultSymbol,
  expiration: null,
  expirationOptions: [],
  expirationPlaceholder: "先载入标的",
  timer: null,
  analysisReady: false,
  loadId: 0,
  refreshing: false,
  loading: false,
  refreshInFlight: null,
  analysisRefreshSymbol: null,
  analysisRefreshTimer: null,
  chainFetchedAt: null,
  levelsWindowFetchedAt: null,
  levelsKey: "",
  levelsPayload: null,
  levelsRetryTimer: null,
  tradePointStability: {
    context: "",
    stable: { buy: null, sell: null },
    pending: { buy: null, sell: null },
  },
  chainFilter: "all",
  chainRows: [],
  chainSpot: null,
  levelBasisMode: "live",
  lastQuote: null,
  accessKey: "",
  storageAvailable: false,
  view: {
    marketState: "等待数据",
    themeLabel: "白天",
    themeIcon: "el-icon-moon-night",
    refreshNote: "每 60 秒自动更新",
    quoteSymbol: defaultSymbol,
    quotePrice: "--",
    quoteChange: "涨跌 --",
    quoteChangeColor: "var(--muted)",
    quoteCurrency: "USD",
    quoteMarket: "--",
    totalCount: "--",
    callVolume: "--",
    putVolume: "--",
    callInterest: "未平仓 --",
    putInterest: "未平仓 --",
    chainTitle: "选择到期日查看期权链",
    dataSource: "尚未加载",
    fetchedAt: "快照时间 --",
    chainHeatNote: "等待数据",
    chainRows: [],
    chainEmpty: "输入标的并载入数据",
    chart: {
      netGex: "净 Gamma --",
      gammaFlip: "零 Gamma --",
      callWall: "看涨墙 --",
      putWall: "看跌墙 --",
      gammaScope: "Gamma 范围 --",
      levelsBasis: "基准 --",
      volumeScope: "",
      oiScope: "",
    },
    levels: {
      resistance: [],
      support: [],
      add: [],
      resistanceNote: "等待数据",
      supportNote: "等待数据",
      resistanceEmpty: "暂无数据",
      supportEmpty: "暂无数据",
      addEmpty: "暂无数据",
    },
    buyer: {
      available: false,
      directionLabel: "",
      horizonLabel: "未来 5 个交易日",
      target: "",
      items: [],
      reason: "等待数据",
      note: "",
    },
    trend: {
      available: false,
      label: "",
      directionClass: "range",
      action: "",
      actionClass: "hold",
      reason: "",
      rows: [],
      priceLabel: "实时价",
      price: "--",
      priceTitle: "等待数据",
      opportunities: [],
      extremes: [],
      note: "等待数据",
      empty: "历史行情不足，暂无趋势判断",
    },
    error: "",
    lastStatus: "系统就绪",
  },
};
const GAMMA_MIN_MINUTES = 30;
// Gamma 与图表复用：同一现价、同一分钟内的合约不重复做指数运算；图表绘制延后到下一帧。
const gammaValueCache = new Map();
const GAMMA_VALUE_CACHE_LIMIT = 4000;
const gammaFlipCache = new Map();
let chainRenderSignature = "";
let chainBodySignatureValue = "";
let analysisChartSignatureValue = "";
let forceChartRedraw = false;
let scopeChartTimer = null;
let scopeChartToken = 0;
// 自动刷新间隔（秒）：页面提示文案与定时器共用同一个值。
const AUTO_REFRESH_SECONDS = 60;
// 快照已过期但上一轮没写成新数据时的重试间隔，避免再空等一个完整周期。
const AUTO_REFRESH_RETRY_SECONDS = 15;
// 本地快照新鲜期（秒）：SQLite 里的快照比它更新时直接复用，不再请求上游接口。
const SNAPSHOT_FRESH_SECONDS = 60;
// 压力位/支撑位各展示的条数。
const LEVEL_COUNT = 10;
// 交易计划（买入 / 加仓 / 卖出）各自最多展示的条数。
const PLAN_COUNT = 10;
// 强化色只显示后端同时通过模型强度、独立证据和历史回踩验证的价位。
const STRONG_LEVEL_SCORE = 0.7;
// 新候选连续两次快照确认后才替换，避免期权链短暂波动造成最佳点闪烁。
const TRADE_POINT_CONFIRMATIONS = 2;
// 期权链热力底色：成交量与未平仓各自按本屏最大值归一，得到 0~100 的相对强度；
// 底色深浅（含白天/黑夜各自的透明度区间）交给 styles.css 的 --heat-floor / --heat-gain 换算。
// 达到这个强度的格子算「热点」：底色已经很亮，文字换成深色墨色，避免亮底浅字看不清。
// 阈值按方向分开——绿底比红底亮得多，绿色格子更早需要换深色字（阈值取自两种字色对比度的交叉点）。
const HEAT_HOT_LEVEL = { call: 68, put: 80 };
// 期权链筛选下拉框的取值与中文标签。
const CHAIN_FILTERS = { all: "全部", call: "看涨", put: "看跌" };
// 时段标签：数据源给的是上游的 marketState 口径（PRE/REGULAR/POST/CLOSED），夜盘由本地时钟补充。
const MARKET_STATE_LABELS = { PRE: "盘前", REGULAR: "正常交易", POST: "盘后", OVERNIGHT: "夜盘", CLOSED: "休市" };
// DOM 访问只保留给折叠交互和图表容器；业务展示数据统一交给 Vue 模板。
const byId = (id) => document.getElementById(id);

// Vue 挂载后折叠标题中的事件目标仍可能来自文本节点；同时兼容没有 Element.closest 的旧浏览器。
function closestElement(target, selector) {
  let node = target && target.nodeType === 1 ? target : target?.parentElement;
  while (node && node !== document) {
    const matches = node.matches || node.msMatchesSelector || node.webkitMatchesSelector;
    if (matches && matches.call(node, selector)) return node;
    node = node.parentElement;
  }
  return null;
}

// 主题：默认黑夜模式，用户可在右上角切换到白天；偏好写入 localStorage，刷新后保持。
const THEME_KEY = "option-scope-theme";
const ACCESS_KEY_STORAGE = "option-scope-access-key";
// 按钮文案展示当前生效的主题名称。
const THEME_LABELS = { light: "白天", dark: "黑夜" };

// 应用主题：切换根节点 data-theme，并同步按钮文案、提示与无障碍状态。
function applyTheme(theme) {
  const next = theme === "light" ? "light" : "dark";
  document.documentElement.dataset.theme = next;
  state.view.themeLabel = THEME_LABELS[next];
  state.view.themeIcon = next === "light" ? "el-icon-sunny" : "el-icon-moon-night";
  const button = byId("theme-toggle");
  if (!button) return;
  button.title = next === "dark" ? "切换到白天模式" : "切换到黑夜模式";
  button.setAttribute("aria-pressed", String(next === "dark"));
}

// 初始化主题：读取本地偏好并绑定切换按钮；localStorage 不可用时静默降级为白天。
function initTheme() {
  let stored = null;
  try { stored = localStorage.getItem(THEME_KEY); } catch (error) { stored = null; }
  applyTheme(stored === "light" ? "light" : "dark");
}

// 折叠组通用逻辑：内容体用 hidden 控制显隐（[hidden] 在 flex/grid 上下文里会被覆盖，样式里补了 [hidden]{display:none}），
// 按钮同步 aria-expanded 与「展开/收起」文案，展开状态记在 sessionStorage——同一标签页里换标的、跳 URL 不必重复展开，
// 关掉标签页就回到各自默认状态（分析详情与期权链默认折叠、图表默认展开）。
function bindFoldGroup({ headerId, toggleId, bodyId, actionId, storageKey, defaultExpanded, onChange, shouldIgnore }) {
  const header = byId(headerId);
  const body = byId(bodyId);
  const toggle = byId(toggleId);
  if (!header || !body || !toggle) return;
  const apply = (expanded) => {
    body.hidden = !expanded;
    toggle.setAttribute("aria-expanded", expanded ? "true" : "false");
    const action = actionId ? byId(actionId) : null;
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

// 分析详情折叠组：趋势通道、加仓价位与压力位/支撑位四张明细表，默认折叠。
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
    onChange: (expanded) => { const modes = byId("detail-modes"); if (modes) modes.hidden = !expanded; },
    // 标题栏里混着「基准价」开关：命中开关就切口径，开关容器里的空白则不改折叠状态，避免贴着按钮点空时把面板收了。
    shouldIgnore: (event) => {
      const basisButton = closestElement(event.target, "[data-basis]");
      if (basisButton) { applyBasisMode(basisButton.dataset.basis); return true; }
      return Boolean(closestElement(event.target, "#detail-modes"));
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
    shouldIgnore: (event) => Boolean(closestElement(event.target, ".panel-status")),
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

// 买方结构默认折叠，展开状态仅在当前标签页内记忆，避免首屏占用过多空间。
const BUYER_STRUCTURE_KEY = "option-scope-buyer-structure";

function initBuyerStructureGroup() {
  bindFoldGroup({
    headerId: "buyer-structure-header",
    toggleId: "buyer-structure-toggle",
    bodyId: "buyer-structure-fold",
    actionId: "buyer-structure-action",
    storageKey: BUYER_STRUCTURE_KEY,
    defaultExpanded: false,
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
  try {
    const value = (localStorage.getItem(ACCESS_KEY_STORAGE) || "").trim();
    if (value) return value;
  } catch (error) { /* 隐私模式或浏览器策略可能禁用 localStorage，继续尝试 sessionStorage。 */ }
  try { return (sessionStorage.getItem(ACCESS_KEY_STORAGE) || "").trim(); } catch (error) { return ""; }
}

// 存储探测只判断浏览器是否允许写入，不依赖 key 是否已经存在；探测失败时页面改用内存值。
function storageWritable() {
  const probe = `${ACCESS_KEY_STORAGE}-probe`;
  try {
    localStorage.setItem(probe, "1");
    localStorage.removeItem(probe);
    return true;
  } catch (error) { /* 继续尝试会话存储。 */ }
  try {
    sessionStorage.setItem(probe, "1");
    sessionStorage.removeItem(probe);
    return true;
  } catch (error) { return false; }
}

function rememberAccessKey(key) {
  let remembered = false;
  try { localStorage.setItem(ACCESS_KEY_STORAGE, key); remembered = true; } catch (error) { /* 忽略并回退。 */ }
  try { sessionStorage.setItem(ACCESS_KEY_STORAGE, key); remembered = true; } catch (error) { /* 忽略并回退。 */ }
  return remembered;
}

function currentAccessKey() {
  // URL 是入口凭证：即使浏览器完全禁用本地存储，也必须优先使用它，不能被内存状态覆盖。
  const queryKey = parsePageQuery(location.search).key;
  if (queryKey) { state.accessKey = queryKey; return queryKey; }
  const stored = readStoredAccessKey();
  if (stored) { state.accessKey = stored; return stored; }
  // 存储完全不可用时允许当前页面继续使用内存里的 key；刷新后 URL 仍会带着它重新初始化。
  if (!state.storageAvailable && state.accessKey) return state.accessKey;
  return "";
}

function initializeAccessKey() {
  const queryKey = parsePageQuery(location.search).key;
  state.storageAvailable = storageWritable();
  if (queryKey) {
    // 先尝试持久化；哪怕写入失败也保留 URL 里的 key，避免后续导航或刷新丢掉访问凭证。
    rememberAccessKey(queryKey);
    state.storageAvailable = state.storageAvailable || readStoredAccessKey() === queryKey;
    state.accessKey = queryKey;
    return queryKey;
  }
  state.accessKey = readStoredAccessKey();
  return state.accessKey;
}

function showAccessDenied() {
  clearAutoRefresh();
  document.body.classList.add("access-denied-page");
  const view = byId("access-denied-view");
  if (view) view.hidden = false;
}

function syncPageQuery() {
  const next = buildPageQuery(state.symbol, state.expiration, location.pathname);
  const current = `${location.pathname}${location.search}`;
  if (next === current) return;
  history.replaceState(null, "", next);
}

function setError(message) { state.view.error = message || ""; }
// 快照年龄（秒）：时间戳缺失或无法解析返回 null，时钟偏差导致的负值按 0 处理。
function snapshotAgeSeconds(fetchedAt) {
  if (!fetchedAt) return null;
  const time = new Date(fetchedAt).getTime();
  return Number.isNaN(time) ? null : Math.max((Date.now() - time) / 1000, 0);
}

function clearAutoRefresh() {
  if (state.timer) clearTimeout(state.timer);
  state.timer = null;
}

// 下一次自动刷新对准快照年龄，而不是页面打开后的固定节拍。
// 新鲜快照等到刚好过期；过期却没更新成功时短间隔重试。
function scheduleAutoRefresh() {
  clearAutoRefresh();
  if (document.body.classList.contains("access-denied-page")) return;
  const age = snapshotAgeSeconds(state.chainFetchedAt);
  const remaining = age == null ? AUTO_REFRESH_SECONDS : AUTO_REFRESH_SECONDS - age;
  const delaySeconds = remaining > 1 ? remaining : (remaining > 0 ? 1 : AUTO_REFRESH_RETRY_SECONDS);
  state.timer = setTimeout(() => { refresh(true); }, delaySeconds * 1000);
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
function formatUsd(value) {
  if (value == null || !Number.isFinite(Number(value))) return "--";
  return `$${Number(value).toLocaleString("en-US", { minimumFractionDigits: 2, maximumFractionDigits: 2 })}`;
}
// 图表数值统一入口：GEX 走中文金额单位，成交量/持仓量走中文计数单位。
function formatChartValue(value, digits, unit) { return unit === "M" ? formatGex(value, digits) : formatCount(value, digits); }
// 刷新按钮只由「是否正在刷新」决定，避免多条并发路径各自改写 disabled 后被误启用；
// 没有到期日（标的没有挂牌期权）时同样允许手动刷新现货快照。
function syncRefreshButton() {
  state.view.refreshNote = state.refreshing ? "正在刷新…" : `每 ${AUTO_REFRESH_SECONDS} 秒自动更新`;
}
function setBusy(busy) { state.loading = busy; syncRefreshButton(); }

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

function rememberGammaValue(cacheKey, value) {
  gammaValueCache.set(cacheKey, value);
  if (gammaValueCache.size > GAMMA_VALUE_CACHE_LIMIT) gammaValueCache.delete(gammaValueCache.keys().next().value);
  return value;
}

function gammaAtSpot(spot, row, expiration) {
  const spotValue = Number(spot);
  const strike = Number(row.strike);
  // 模型 IV 由后端按合约价格反解，优先于数据源里常为占位值的 implied_volatility。
  const volatility = Number(row.model_iv ?? row.implied_volatility);
  if (!Number.isFinite(spotValue) || spotValue <= 0 || !Number.isFinite(strike) || strike <= 0) return null;
  if (!Number.isFinite(volatility) || volatility <= 0) return null;
  const expiryKey = row.expiration || expiration || "";
  const expiry = optionExpiry(expiryKey);
  if (!expiry) return null;
  // 30 秒一档：同一次渲染里的柱状图和期权链表共用结果，刷新时也不把同一批合约再算一遍。
  const timeBucket = Math.floor(Date.now() / 30000);
  const cacheKey = `${expiryKey}|${strike}|${row.contract_type || ""}|${volatility}|${spotValue}|${timeBucket}`;
  if (gammaValueCache.has(cacheKey)) return gammaValueCache.get(cacheKey);
  const timeYears = Math.max((expiry.getTime() - Date.now()) / (365 * 24 * 60 * 60 * 1000), GAMMA_MIN_MINUTES / (365 * 24 * 60));
  const volatilityTime = volatility * Math.sqrt(timeYears);
  const d1 = (Math.log(spotValue / strike) + (0.005 + 0.5 * volatility ** 2) * timeYears) / volatilityTime;
  const gamma = Math.exp(-0.5 * d1 ** 2) / (spotValue * volatilityTime * Math.sqrt(2 * Math.PI));
  return rememberGammaValue(cacheKey, gamma);
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

function gammaFlipCacheKey(rows, spot, expiration) {
  const timeBucket = Math.floor(Date.now() / 30000);
  let checksum = rows.length;
  for (let index = 0; index < rows.length; index += 1) {
    const row = rows[index];
    checksum = (checksum + Math.round((Number(row.strike) || 0) * 100) + (Number(row.open_interest) || 0) + Math.round((Number(row.model_iv ?? row.implied_volatility) || 0) * 100000)) % 1000000007;
  }
  return `${expiration}|${spot}|${timeBucket}|${checksum}`;
}

function rememberGammaFlip(cacheKey, value) {
  gammaFlipCache.set(cacheKey, { value });
  if (gammaFlipCache.size > 24) gammaFlipCache.delete(gammaFlipCache.keys().next().value);
  return value;
}

function findGammaFlip(rows, currentSpot, expiration) {
  const strikes = rows.map((row) => Number(row.strike)).filter((strike) => Number.isFinite(strike) && strike > 0);
  const spot = Number(currentSpot);
  if (!strikes.length || !Number.isFinite(spot) || spot <= 0 || !expiration) return null;
  const cacheKey = gammaFlipCacheKey(rows, spot, expiration);
  if (gammaFlipCache.has(cacheKey)) return gammaFlipCache.get(cacheKey).value;
  const prepared = prepareGexRows(rows, expiration);
  if (!prepared.length) return rememberGammaFlip(cacheKey, null);
  // 公开 GEX 实现常用现价 ±15% 的扫描带，避免把远端低信号根误当成交易区间的 Flip。
  const lower = Math.max(Math.min(...strikes), spot * 0.85);
  const upper = Math.min(Math.max(...strikes), spot * 1.15);
  if (!(upper > lower)) return rememberGammaFlip(cacheKey, null);
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
  if (!roots.length) return rememberGammaFlip(cacheKey, null);
  return rememberGammaFlip(cacheKey, { strike: roots.reduce((nearest, root) => Math.abs(root - spot) < Math.abs(nearest - spot) ? root : nearest) });
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

// 实时价取当前生效的时段价格：盘中为最新成交价，盘前/盘后取该时段价格；
// 夜盘没有可用的 Yahoo 夜盘价时改用最近一次正常交易日收盘价，避免把常规价误标成夜盘实时价。
function activeBasis(quote) {
  if (state.levelBasisMode === "close") return levelBasis(quote, quote?.price);
  // 夜盘没有 Yahoo 的实时标的价时，实时价口径改用最近一次正常交易日收盘价。
  if (quote?.market_state === "OVERNIGHT") {
    const close = overnightCloseQuote(quote);
    if (close) return { price: close.price, label: "收盘" };
  }
  const price = Number(activeSessionQuote(quote)?.price);
  return Number.isFinite(price) && price > 0 ? { price, label: "实时" } : levelBasis(quote, quote?.price);
}

// 切换基准价口径：同步开关的按下状态，再用已有快照重算压力位/支撑位（不额外请求上游接口）。
// 支撑位/压力位表、交易计划与压力位/支撑位柱状图都由这一次重绘一起更新。
function applyBasisMode(mode) {
  state.levelBasisMode = mode === "close" ? "close" : "live";
  // 基准价切换也要立即重绘现货卡片，否则标签变了但价格仍停留在上一次口径。
  if (state.lastQuote) renderQuote(state.lastQuote);
  for (const key of Object.keys(BASIS_MODES)) {
    const button = byId(key === "live" ? "basis-live" : "basis-close");
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

// 把单因子价位转换为 Vue 行数据（行权价 / 距现价 / 排序口径数值），离现价近的排在前面。
function levelScore(level) {
  const score = Number(level?.score);
  return Number.isFinite(score) ? Math.min(1, Math.max(0, score)) : 0;
}

function levelFactors(level) {
  return Array.isArray(level?.factors) ? level.factors.map((factor) => String(factor)) : [];
}

function hasStrongLevelEvidence(factors) {
  const groups = new Set();
  let hasWall = false;
  let hasAbsorption = false;
  for (const factor of factors) {
    if (factor.includes("看涨墙") || factor.includes("看跌墙")) hasWall = true;
    if (factor.includes("看涨") || factor.includes("看跌")) groups.add("options");
    else if (factor.includes("斐波那契")) groups.add("fibonacci");
    else if (factor.includes("筹码密集")) groups.add("chips");
    else if (factor.includes("承接位")) { groups.add("absorption"); hasAbsorption = true; }
    else groups.add(`other:${factor}`);
  }
  return hasWall || hasAbsorption || groups.size >= 2;
}

function levelStrengthTag(level, side, isAdd = false) {
  const factors = levelFactors(level);
  const tier = String(level?.strength_tier || "");
  if (!['strong', 'reinforced'].includes(tier) || !hasStrongLevelEvidence(factors)) return "";
  const prefix = tier === "strong" ? "强" : "重点";
  if (side === "support" && isAdd && factors.some((factor) => factor.includes("承接位"))) return tier === "strong" ? "强承接" : "重点承接";
  if (side === "support") return `${prefix}支撑`;
  return factors.some((factor) => factor.includes("看涨墙")) ? (tier === "strong" ? "集中抛压" : "重点抛压") : `${prefix}压力`;
}

// 强化色按最终综合分数分五档；strong 额外包含历史验证，因此视觉上高半档。
function levelStrengthIntensity(level) {
  const score = levelScore(level) + (level?.strength_tier === "strong" ? 0.1 : 0);
  if (score >= 0.9) return 5;
  if (score >= 0.8) return 4;
  if (score >= 0.68) return 3;
  if (score >= 0.56) return 2;
  return 1;
}

function levelStrengthClass(level, side, isAdd = false) {
  const tag = levelStrengthTag(level, side, isAdd);
  const tier = String(level?.strength_tier || "");
  const intensity = tag ? ` level-strength-${levelStrengthIntensity(level)}` : "";
  return tag ? `level-strong level-${tier} level-strong-${side}${isAdd ? " level-strong-add" : ""}${intensity}` : "";
}

function buildLevelView(level, index, spot, side, isAdd = false) {
  const price = Number(level?.price ?? level?.strike);
  const normalized = { ...level, price };
  const gap = Number.isFinite(spot) && spot > 0 && Number.isFinite(price) ? (price / spot - 1) * 100 : null;
  const strengthTag = levelStrengthTag(normalized, side, isAdd);
  const factors = levelFactors(normalized).join(" · ") || "--";
  return {
    ...normalized,
    key: `${side}-${isAdd ? "add" : "level"}-${Number.isFinite(price) ? price.toFixed(4) : index}`,
    range: formatLevelRange(normalized),
    gap: gap == null ? "--" : `${gap >= 0 ? "+" : ""}${gap.toFixed(2)}%`,
    gapClass: gap == null ? "" : (gap >= 0 ? "up" : "down"),
    probability: formatProbability(normalized.probability),
    factors,
    strengthTag,
    className: levelStrengthClass(normalized, side, isAdd),
    title: levelTooltipText(normalized, levelScore(normalized), strengthTag),
    detail: levelDetailText(normalized),
  };
}

function buildSimpleLevelViews(levels, spot, side, valueOf, formatValue, metricLabel) {
  const peak = Math.max(0, ...levels.map(valueOf)) || 1;
  return levels.map((point, index) => buildLevelView({
    price: point.strike,
    score: valueOf(point) / peak,
    factors: [metricLabel],
    displayValue: formatValue(valueOf(point)),
  }, index, spot, side));
}

// 单因子回退时把选出的行权价转成柱状图口径：综合强度按本侧最大值归一（与多因子接口一致）。
function fallbackLevelSeries(picked, valueOf, metricLabel) {
  const peak = Math.max(0, ...picked.map(valueOf)) || 1;
  return picked.map((point) => ({ price: point.strike, score: valueOf(point) / peak, factors: [metricLabel], probability: null }));
}

function renderLevels(points, spot) {
  const price = Number(spot);
  state.view.buyer = {
    available: false,
    directionLabel: "",
    horizonLabel: "未来 5 个交易日",
    target: "",
    items: [],
    reason: "正在计算买方结构…",
    note: "",
  };
  state.view.chart.levelsBasis = Number.isFinite(price) && price > 0 ? `基准 ${formatMoney(price)}` : "基准 --";
  if (!points.length || !Number.isFinite(price) || price <= 0) {
    state.view.levels.resistance = [];
    state.view.levels.support = [];
    state.view.levels.add = [];
    state.view.levels.resistanceNote = `现价上方持仓最集中的 ${LEVEL_COUNT} 个价位`;
    state.view.levels.supportNote = `现价下方持仓最集中的 ${LEVEL_COUNT} 个价位`;
    state.view.levels.resistanceEmpty = "暂无数据";
    state.view.levels.supportEmpty = "暂无数据";
    state.view.levels.addEmpty = "暂无数据";
    OptionScopeCharts.renderLevelsChart(null);
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
  state.view.levels.resistanceNote = scope;
  state.view.levels.supportNote = scope;
  const resistance = pickLevels(points, price, "above", callValue);
  const support = pickLevels(points, price, "below", putValue);
  const planSupport = pickLevels(points, price, "below", putValue, PLAN_COUNT * 2);
  state.view.levels.resistance = buildSimpleLevelViews(resistance, price, "resistance", callValue, formatValue, metricLabel);
  state.view.levels.support = buildSimpleLevelViews(support, price, "support", putValue, formatValue, metricLabel);
  state.view.levels.add = [];
  state.view.levels.addEmpty = "暂无多因子加仓数据";
  // 单因子回退时柱状图按同一批行权价绘制，综合强度按本侧最大值归一。
  const resistanceSeries = fallbackLevelSeries(resistance, callValue, metricLabel);
  const supportSeries = fallbackLevelSeries(support, putValue, metricLabel);
  OptionScopeCharts.renderLevelsChart({ spot: price, resistance: resistanceSeries, support: supportSeries });
  // 回退口径没有日线历史，趋势通道留空；交易计划按同一批支撑/压力价位切成三段。
  renderTrend(null);
  const planSupportSeries = fallbackLevelSeries(planSupport, putValue, metricLabel);
  const planSplit = Math.min(PLAN_COUNT, Math.ceil(planSupportSeries.length / 2));
  renderPlan({ add: planSupportSeries.slice(planSplit, planSplit + PLAN_COUNT) }, price);
}

// 综合接口还在计算时，先用已经拿到的当前期限期权分布填充基础价位和图表，避免首屏整块留空。
// 综合结果回来后会覆盖这份临时结果；已有结果时保留旧值，避免刷新过程中闪回空状态。
function renderFactorFallback(points, spot) {
  const hasVisibleLevels = Boolean(
    state.view.levels.support.length
    || state.view.levels.resistance.length
    || state.view.levels.add.length
    || state.view.trend.available
  );
  if (!hasVisibleLevels) renderLevels(points, spot);
}

function requestFactorLevels(points, spot, key, attempt = 0) {
  const encodedSymbol = encodeURIComponent(state.symbol);
  const encodedExpiration = encodeURIComponent(state.expiration);
  const numericSpot = Number(spot);
  const spotQuery = Number.isFinite(numericSpot) && numericSpot > 0
    ? `&spot=${encodeURIComponent(numericSpot)}`
    : "";
  request(`/api/levels/${encodedSymbol}?expiration=${encodedExpiration}${spotQuery}`)
    .then((payload) => {
      if (state.levelsKey !== key) return; // 期间切换了标的、期限或快照，丢弃过期结果
      state.levelsPayload = payload;
      renderFactorLevels(payload);
    })
    .catch(() => {
      if (state.levelsKey !== key) return;
      state.levelsPayload = null;
      renderFactorFallback(points, spot);
      // 首次历史数据可能仍在上游或 SQLite 写入链路中，短暂失败时只补一次，避免反复请求。
      if (attempt >= 1) return;
      clearTimeout(state.levelsRetryTimer);
      state.levelsRetryTimer = setTimeout(() => {
        state.levelsRetryTimer = null;
        if (state.levelsKey === key) requestFactorLevels(points, spot, key, attempt + 1);
      }, 1200);
    });
}

// 与后端 spot_cache_bucket 同一套半入规则，避免前后端格子边界不一致。
function spotCacheBucket(value) {
  const price = Number(value);
  if (!Number.isFinite(price) || price <= 0) return "";
  const magnitude = 10 ** Math.floor(Math.log10(price));
  const step = magnitude / 500;
  const units = Math.round(price / step);
  return (units * step).toFixed(6);
}

// 多因子压力位/支撑位：由后端按「斐波那契回撤 + 筹码密集 + 承接位 + 所选到期日期权持仓」合成，
// 前端只负责渲染；同一标的、同一到期日、同一快照只请求一次，图表尺寸变化时复用已有结果。
function loadFactorLevels(points, spot) {
  if (!points.length || !state.expiration) { renderLevels(points, spot); return; }
  // 缓存键带上基准价口径：两个口径取到同一价格时（盘后/夜盘时段）也各自成键，切换必然重绘一次。
  // 现价按与后端相同的格子去重：格子内不再请求价位接口，跨出格子才用精确现价重算。
  const key = `${state.symbol}|${state.expiration}|${state.chainFetchedAt || ""}|${state.levelsWindowFetchedAt || ""}|${spotCacheBucket(spot)}|${state.levelBasisMode}`;
  // 已请求过：窗口尺寸变化时直接用缓存结果重绘，不再打接口。
  if (state.levelsKey === key) {
    if (state.levelsPayload) renderFactorLevels(state.levelsPayload);
    else renderLevels(points, spot);
    return;
  }
  clearTimeout(state.levelsRetryTimer);
  state.levelsRetryTimer = null;
  state.levelsKey = key;
  state.levelsPayload = null;
  renderFactorFallback(points, spot);
  requestFactorLevels(points, spot, key);
}

// 渲染多因子结果：价位 / 距现价 / 综合依据（组成该价位的因子标签）。
// 触及概率：0~1 的概率值转百分比，极小/极大用不等号，缺数据用占位符。
function formatProbability(value) {
  if (value == null || !Number.isFinite(Number(value))) return "--";
  const percent = Number(value) * 100;
  if (percent < 0.1) return "<0.1%";
  if (percent > 99.9) return ">99.9%";
  return `${percent.toFixed(1)}%`;
}

function formatLevelRange(level) {
  const center = Number(level?.price);
  const low = Number(level?.zone_low);
  const high = Number(level?.zone_high);
  if (!Number.isFinite(center) || center <= 0) return "--";
  if (!Number.isFinite(low) || !Number.isFinite(high) || high <= low) return formatMoney(center);
  return `${formatMoney(low)}-${formatMoney(high)}`;
}

// 桌面端悬停提示保留完整信息，避免说明行精简后丢失详情。
function levelTooltipText(level, score, strengthTag) {
  const adjustedHoldRate = Number(level.history_adjusted_hold_rate);
  const adjustedBreakRate = Number(level.history_adjusted_break_rate);
  const holdRate = Number.isFinite(adjustedHoldRate) ? adjustedHoldRate : Number(level.history_hold_rate);
  const breakRate = Number.isFinite(adjustedBreakRate) ? adjustedBreakRate : Number(level.history_break_rate);
  const strengthTier = String(level?.strength_tier || "");
  const rateLabel = Number.isFinite(adjustedHoldRate) ? "校准守住" : "守住";
  const historyTitle = Number.isFinite(Number(level.history_samples)) && Number(level.history_samples) > 0
    ? ` · 历史触及 ${level.history_samples} 次，${rateLabel} ${(holdRate * 100).toFixed(1)}%，跌破 ${(breakRate * 100).toFixed(1)}%${Number(level.recent_samples) > 0 ? `；近期 ${level.recent_samples} 次反应，反弹 ${((Number(level.recent_reaction_rate) || 0) * 100).toFixed(1)}%` : ""}`
    : strengthTier === "reinforced"
      ? " · 历史触及样本不足，重点强化已启用"
      : " · 历史触及样本不足，未启用强化色";
  const recentReinforcement = Number(level.recent_samples) >= 2 && Number(level.recent_reactions) >= 2;
  const strengthTitle = strengthTag ? (strengthTier === "strong"
    ? ` · ${strengthTag}（历史回踩验证通过）`
    : ` · ${strengthTag}（${recentReinforcement ? "近期多次反应" : "多因子共振，历史样本不足"}）`) : "";
  return `代表价 ${formatMoney(level.price)} · 综合强度 ${score.toFixed(2)}（1 为最强）${strengthTitle}${historyTitle}`;
}

// 手机端说明只保留历史验证摘要，主数据行已有代表价、强度和综合依据。
function levelDetailText(level) {
  const samples = Number(level?.history_samples);
  const representative = Number(level?.price);
  const prefix = Number.isFinite(representative) && representative > 0 ? `代表价 ${formatMoney(representative)} · ` : "";
  const strengthTier = String(level?.strength_tier || "");
  if (!Number.isFinite(samples) || samples <= 0) {
    const reinforcement = strengthTier === "reinforced" ? "重点强化已启用 · " : "";
    return `${prefix}${reinforcement}历史回踩：暂无样本`;
  }
  const adjustedHoldRate = Number(level.history_adjusted_hold_rate);
  const adjustedBreakRate = Number(level.history_adjusted_break_rate);
  const holdRate = Number.isFinite(adjustedHoldRate) ? adjustedHoldRate : Number(level.history_hold_rate);
  const breakRate = Number.isFinite(adjustedBreakRate) ? adjustedBreakRate : Number(level.history_break_rate);
  const parts = [`${prefix}历史回踩：${samples} 次`];
  if (Number.isFinite(holdRate)) parts.push(`${Number.isFinite(adjustedHoldRate) ? "校准守住" : "守住"} ${(holdRate * 100).toFixed(1)}%`);
  if (Number.isFinite(breakRate)) parts.push(`跌破 ${(breakRate * 100).toFixed(1)}%`);
  const recentSamples = Number(level.recent_samples);
  if (Number.isFinite(recentSamples) && recentSamples > 0) {
    parts.push(`近期 ${recentSamples} 次反应`);
    const reactionRate = Number(level.recent_reaction_rate);
    if (Number.isFinite(reactionRate)) parts.push(`反弹 ${(reactionRate * 100).toFixed(1)}%`);
  }
  return parts.join(" · ");
}

function buildFactorViews(levels, spot, side, isAdd = false) {
  return (levels || []).map((level, index) => buildLevelView(level, index, spot, side, isAdd));
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
    return { valid, label, value: valid ? formatMoney(value) : "--", title };
  });
}

// 趋势通道：展示方向、上下轨、日均斜率、今开/昨收、Beta，以及 52 周 / 历史最高最低价。
function renderTrend(trend, extremes, spot, historyMeta, recommendation = null, tradePoints = null, tradePointsHorizon = null, trendMarket = null, beta = null) {
  const extremeRows = trendExtremeRows(extremes, spot);
  const hasExtremes = extremeRows.some((row) => row.valid);
  if (!trend && !hasExtremes) {
    state.view.trend = { ...state.view.trend, available: false, rows: [], opportunities: [], extremes: [], note: "等待数据" };
    return;
  }
  const className = trend?.direction === "up" ? "up" : (trend?.direction === "down" ? "down" : "range");
  const slope = Number(trend?.slope_percent) || 0;
  const betaValue = Number(beta?.value);
  const betaText = Number.isFinite(betaValue) ? betaValue.toFixed(2) : "--";
  const betaTitle = "基准指数：标普500 · 时间跨度：2年 · Beta（β）衡量股票相对于整个股市的价格波动情况；高 Beta（>1.0）理论上风险更高但潜在回报更高，低 Beta（<1.0）理论上风险较低但潜在回报也较低";
  const selectedBasis = state.levelBasisMode === "close"
    ? levelBasis(state.lastQuote, spot)
    : activeBasis(state.lastQuote);
  const basisPrice = Number(selectedBasis?.price);
  const fallbackPrice = Number(spot);
  const displayedPrice = Number.isFinite(basisPrice) && basisPrice > 0 ? basisPrice : fallbackPrice;
  const validPrice = Number.isFinite(displayedPrice) && displayedPrice > 0;
  const priceLabel = state.levelBasisMode === "live" && selectedBasis?.label === "收盘"
    ? "收盘价"
    : (BASIS_MODES[state.levelBasisMode] || BASIS_MODES.live);
  const priceTitle = validPrice
    ? `${priceLabel} ${formatMoney(displayedPrice)} · 数据来源：${selectedBasis?.label || "常规"}`
    : `${priceLabel}暂无数据`;
  const rows = trend ? [
    ["通道上轨", formatMoney(trend.upper), ""],
    ["通道下轨", formatMoney(trend.lower), ""],
    ["日均斜率", `${slope >= 0 ? "+" : ""}${slope.toFixed(3)}%`, ""],
    ["样本", `${Number(trend.bars) || 0} 根日线`, ""],
    ["今开", formatMoney(trendMarket?.today_open), "今日开盘价；盘前、盘后和夜盘缺少当日开盘价时，使用前一个交易日的开盘价"],
    ["昨收", formatMoney(trendMarket?.previous_close), "昨日收盘价；非交易时段按最近一个已完成交易日的收盘价显示"],
    ["Beta", betaText, betaTitle],
  ].map(([label, value, title]) => ({ label, value, title, className: label.startsWith("Beta") ? "trend-beta" : "" })) : [];
  const action = ["buy", "sell", "hold"].includes(recommendation?.action) ? recommendation.action : null;
  const actionLabel = action ? (recommendation.label || "继续持有") : "";
  const actionReason = action ? (recommendation.reason || "结合当前趋势与价位综合判断") : "";
  const horizonLabel = tradePointsHorizon?.label || "未来 5 个交易日";
  const opportunityRows = [
    ["buy", "近期最佳买入点"],
    ["sell", "近期最佳卖出点"],
  ].map(([kind, label]) => {
    const point = tradePoints?.[kind];
    const range = point ? formatLevelRange(point) : "--";
    const confidence = point ? formatProbability(point.confidence) : "--";
    const modelConfidence = point ? formatProbability(point.model_confidence) : "--";
    const samples = Number(point?.history_samples);
    const holdRate = Number(point?.history_hold_rate);
    const breakRate = Number(point?.history_break_rate);
    const history = Number.isFinite(holdRate) && samples > 0
      ? `历史守住 ${(holdRate * 100).toFixed(1)}%（${samples}次）${Number.isFinite(breakRate) ? `，跌破 ${(breakRate * 100).toFixed(1)}%` : ""}`
      : "历史样本不足";
    const historySummary = Number.isFinite(holdRate) && samples > 0
      ? `守住 ${(holdRate * 100).toFixed(1)}% · ${samples}次`
      : "暂无历史样本";
    const title = point?.reason ? `${label}：${point.reason}` : `${label}暂无可用数据`;
    return {
      kind,
      label,
      horizon: horizonLabel,
      range,
      confidence,
      historySummary,
      title: `${title} · 模型评分 ${modelConfidence} · ${history} · 综合评分 ${confidence} · 计算范围：${horizonLabel}`,
    };
  });
  const meta = historyMeta || {};
  const parts = [
    meta.extremes_fetched_at ? `高低点快照 ${formatTime(meta.extremes_fetched_at)}` : null,
    meta.extremes_source ? `来源 ${meta.extremes_source}` : null,
    meta.extremes_warning ? `高低点降级：${meta.extremes_warning}` : null,
  ].filter(Boolean);
  state.view.trend = {
    available: true,
    label: trend?.label || "趋势通道",
    directionClass: className,
    action: actionLabel,
    actionClass: action || "hold",
    reason: actionReason,
    rows,
    priceLabel,
    price: validPrice ? formatMoney(displayedPrice) : "--",
    priceTitle,
    opportunities: opportunityRows,
    extremes: hasExtremes ? extremeRows : [],
    note: "按最近日线收盘价的线性回归通道；高低点取日线最高/最低价（历史极值用全量历史）",
    noteTitle: parts.join(" · "),
    empty: "历史行情不足，暂无趋势判断",
  };
}

function tradePointIdentity(point) {
  const price = Number(point?.price);
  return Number.isFinite(price) && price > 0 ? price.toFixed(4) : "";
}

// 价位候选需要连续两次快照确认；同一代表价只更新评分和说明，不阻塞最新的历史统计。
function stabilizeTradePoints(points, context) {
  const incoming = { buy: points?.buy || null, sell: points?.sell || null };
  const stability = state.tradePointStability;
  if (stability.context !== context) {
    stability.context = context;
    stability.stable = incoming;
    stability.pending = { buy: null, sell: null };
    return incoming;
  }
  for (const side of ["buy", "sell"]) {
    const incomingId = tradePointIdentity(incoming[side]);
    const stableId = tradePointIdentity(stability.stable[side]);
    if (incomingId === stableId) {
      stability.stable[side] = incoming[side];
      stability.pending[side] = null;
      continue;
    }
    const pending = stability.pending[side];
    const nextCount = pending && pending.id === incomingId ? pending.count + 1 : 1;
    stability.pending[side] = { id: incomingId, count: nextCount };
    if (nextCount >= TRADE_POINT_CONFIRMATIONS) {
      stability.stable[side] = incoming[side];
      stability.pending[side] = null;
    }
  }
  return { ...stability.stable };
}

// 交易计划价位表：价位区间 / 距现价 / 触及概率 / 综合依据；强化标签放在说明行。
function renderPlanRows(levels, spot) {
  state.view.levels.add = buildFactorViews(levels, spot, "support", true);
  state.view.levels.addEmpty = levels?.length ? "" : "暂无可用价位";
}

// 交易计划：前端只展示更深一档的加仓支撑，推荐买入与卖出直接看支撑位/压力位面板。
function renderPlan(plan, spot) {
  const price = Number(spot);
  renderPlanRows(plan?.add || [], price);
}

function renderFactorLevels(payload) {
  const spot = Number(payload?.spot);
  state.view.chart.levelsBasis = Number.isFinite(spot) && spot > 0 ? `基准 ${formatMoney(spot)}` : "基准 --";
  const expiration = payload?.expiration || state.expiration || "--";
  const metric = payload?.options_metric === "volume" ? "成交量" : "Gamma 敞口";
  const hasHistory = Number(payload?.history?.bars) > 0;
  const optionExpirations = payload?.options_expirations || [];
  const optionScope = optionExpirations.length ? `多期限 ${optionExpirations.length} 个到期日` : "多期限暂无数据";
  const scope = hasHistory
    ? `斐波那契 · 筹码密集 · 承接位 · 期权持仓（${metric}，${optionScope}）综合 · 选中期限 ${expiration}`
    : `历史行情不可用，按期权持仓（${metric}，${optionScope}）计算 · 选中期限 ${expiration}`;
  const detail = [hasHistory ? `日线 ${payload.history.bars} 根` : null, payload?.history?.warning ? `历史行情降级：${payload.history.warning}` : null, "触及概率：按选中期限隐含波动率与剩余期限的零漂移首次触及概率", "Gamma、成交量和持仓量图表仍按当前选中期限绘制"].filter(Boolean).join(" · ");
  const basisNote = Number.isFinite(spot) && spot > 0 ? ` · 基准 ${formatMoney(spot)}（${state.levelBasisLabel || "常规"}）` : "";
  state.view.levels.resistanceNote = scope + basisNote;
  state.view.levels.supportNote = scope + basisNote;
  state.view.levels.resistance = buildFactorViews(payload?.resistance || [], spot, "resistance");
  state.view.levels.support = buildFactorViews(payload?.support || [], spot, "support");
  state.view.levels.resistanceEmpty = "现价这一侧暂无可用价位";
  state.view.levels.supportEmpty = "现价这一侧暂无可用价位";
  OptionScopeCharts.renderLevelsChart(payload);
  renderBuyerStructures(payload?.buyer_structures, payload?.options_fetched_at || payload?.chain_fetched_at);
  const tradePointContext = `${state.symbol}|${expiration}|${state.levelBasisMode}`;
  const stableTradePoints = stabilizeTradePoints(payload?.trade_points, tradePointContext);
  renderTrend(payload?.trend || null, payload?.extremes || null, spot, payload?.history || null, payload?.recommendation || null, stableTradePoints, payload?.trade_points_horizon || null, payload?.trend_market || null, payload?.beta || null);
  renderPlan(payload?.plan, spot);
}

function formatStructureDelta(value) {
  if (value == null || !Number.isFinite(Number(value))) return "--";
  const number = Number(value);
  return `${number >= 0 ? "+" : ""}${number.toFixed(2)}`;
}

function formatStructureReturn(value) {
  if (value == null || !Number.isFinite(Number(value))) return "--";
  const number = Number(value) * 100;
  return `${number >= 0 ? "+" : ""}${number.toFixed(1)}%`;
}

function formatStructureScenarioAmount(value, cost) {
  const returnValue = Number(value);
  const costValue = Number(cost);
  if (!Number.isFinite(returnValue) || !Number.isFinite(costValue)) return "--";
  const amount = Math.abs(returnValue * costValue);
  return `${returnValue > 0 ? "+" : returnValue < 0 ? "-" : ""}${formatUsd(amount)}`;
}

function formatStructureExpiration(value) {
  const match = String(value || "").match(/^\d{4}-(\d{2})-(\d{2})$/);
  return match ? `${match[1]}/${match[2]}` : String(value || "--");
}

function structureContractCode(contractType) {
  return contractType === "put" ? "P" : "C";
}

function formatStructureStrike(value) {
  if (value == null || !Number.isFinite(Number(value))) return "--";
  return Number(value).toFixed(2).replace(/\.00$/, "");
}

function structureLegSummary(item) {
  const legs = Array.isArray(item.legs) ? item.legs : [];
  return legs.map((leg) => `${leg.action === "sell" ? "卖出" : "买入"}${formatStructureStrike(leg.strike)}${structureContractCode(leg.contract_type)}`).join("，");
}

function structureNotation(item, strikes) {
  const expiration = formatStructureExpiration(item.expiration);
  const suffix = structureContractCode(item.direction);
  return `${expiration} ${strikes}${suffix}`;
}

function formatBuyerMethod(value) {
  return String(value || "模型估算").replace(/Black-Scholes/g, "布莱克-斯科尔斯");
}

function formatBuyerQuoteMethod(value) {
  return String(value || "买卖价中间价")
    .replace(/Bid\/Ask Mid/g, "买卖价中间价")
    .replace(/Bid\/Ask/g, "买卖价")
    .replace(/Last/g, "最新成交价");
}

function renderBuyerStructures(payload, fetchedAt = null) {
  const empty = {
    available: false,
    directionLabel: "",
    horizonLabel: payload?.horizon_label || "未来 5 个交易日",
    target: "",
    items: [],
    reason: payload?.reason || "等待数据",
    note: "",
  };
  if (!payload?.available || !Array.isArray(payload.items) || !payload.items.length) {
    state.view.buyer = empty;
    return;
  }
  const targets = payload.targets || {};
  const callTarget = Number(targets.call?.price);
  const putTarget = Number(targets.put?.price);
  const targetText = [
    Number.isFinite(callTarget) ? `看涨 ${formatMoney(callTarget)}` : null,
    Number.isFinite(putTarget) ? `看跌 ${formatMoney(putTarget)}` : null,
  ].filter(Boolean).join(" · ");
  const buyerMethod = formatBuyerMethod(payload.method);
  const buyerQuoteMethod = formatBuyerQuoteMethod(payload.quote_method);
  state.view.buyer = {
    available: true,
    directionLabel: payload.direction_label || (payload.direction === "call" ? "买入看涨" : "买入看跌"),
    horizonLabel: payload.horizon_label || "未来 5 个交易日",
    target: targetText || "--",
    reason: "",
    note: `${buyerMethod} · ${buyerQuoteMethod} · ${fetchedAt ? `数据 ${formatTime(fetchedAt)}` : "数据时间未知"} · 预计盈利/亏损以当前买卖价中间价为成本基准；负数表示目标价虽达到，扣除时间价值后仍未覆盖成本。${payload.disclaimer || "综合评分不是历史胜率"}`,
    items: payload.items.map((item, index) => {
      const strikes = (item.strikes || []).map((strike) => formatMoney(strike)).join(" / ");
      const compactStrikes = (item.strikes || []).map((strike) => formatStructureStrike(strike)).join("/");
      const scenarioTarget = Number(item.target_price);
      const scenarioReturn = Number(item.scenario_return);
      const scenario = formatStructureReturn(scenarioReturn);
      const hasScenario = Number.isFinite(scenarioReturn);
      const scenarioAmount = formatStructureScenarioAmount(scenarioReturn, item.cost);
      const scenarioState = !hasScenario ? "unknown" : scenarioReturn > 0 ? "positive" : scenarioReturn < 0 ? "negative" : "neutral";
      const scenarioLabel = scenarioState === "positive" ? "预计盈利" : scenarioState === "negative" ? "预计亏损" : scenarioState === "neutral" ? "预计持平" : "无法估算";
      const spread = Number(item.spread_ratio);
      const legs = structureLegSummary(item);
      const isVertical = item.kind === "vertical";
      const directionLabel = item.direction === "put" ? "看跌" : "看涨";
      const notation = structureNotation(item, compactStrikes);
      const planLabel = item.direction_label || `${directionLabel}方案`;
      const verticalExplanation = item.direction === "put"
        ? "即买入较高执行价期权、卖出较低执行价期权，收益封顶但权利金较低。"
        : "即买入较低执行价期权、卖出较高执行价期权，收益封顶但权利金较低。";
      const title = isVertical
        ? `${notation}：${legs}。${verticalExplanation}`
        : `${notation}：${item.label || "买方期权"}，到期日 ${item.expiration}，执行价 ${strikes}。`;
      return {
        ...item,
        // 同一期限可能同时出现多个相同执行价的候选方案，方向与序号一起纳入 key，避免 Vue 2 列表重排时出现重复 key。
        key: `${item.kind || "structure"}-${item.direction || "unknown"}-${item.expiration || "unknown"}-${strikes || "unknown"}-${index}`,
        structure: notation,
        rowClass: item.direction === "put" ? "buyer-structure-put" : "buyer-structure-call",
        subtitle: `${planLabel}${item.is_primary ? " · 主方向" : " · 对比方案"} · ${isVertical ? `${directionLabel}价差 · ${legs}` : `${item.label || "买方期权"} · ${item.style || "单腿"}`} · 剩余 ${item.dte} 天`,
        title: title.replace(/。$/, ""),
        iv: Number.isFinite(Number(item.iv)) ? `${(Number(item.iv) * 100).toFixed(1)}%` : "--",
        delta: formatStructureDelta(item.delta),
        exposure: formatUsd(item.delta_exposure),
        leverage: Number.isFinite(Number(item.effective_leverage)) ? `${Number(item.effective_leverage).toFixed(1)} 倍` : "--",
        maxLoss: formatUsd(item.max_loss),
        score: Number.isFinite(Number(item.score)) ? `${Math.round(Number(item.score) * 100)}/100` : "--",
        detail: `成本 ${formatUsd(item.cost)} · 盈亏平衡 ${formatMoney(item.breakeven)} · 目标 ${Number.isFinite(scenarioTarget) ? formatMoney(scenarioTarget) : "--"} · 目标价下${scenarioLabel} ${scenario}${hasScenario ? `（约 ${scenarioAmount}）` : ""} · 买卖价差 ${Number.isFinite(spread) ? `${(spread * 100).toFixed(1)}%` : "--"}`,
        scenarioStatus: scenarioState === "positive"
          ? "预计盈利 · 已覆盖成本"
          : scenarioState === "negative"
            ? "预计亏损 · 未覆盖成本"
            : scenarioState === "neutral"
              ? "预计持平 · 接近成本线"
              : "无法判断 · 缺少有效数据",
        scenarioClass: `buyer-structure-scenario-${scenarioState}`,
        risk: item.risk || "请结合报价和到期时间确认风险",
      };
    }),
  };
}

function renderAnalysis(rows, spot, analysisPayload, expirationRows = [], ivModel = {}, basis = null) {
  // 期权链聚合结果：Gamma 敞口、分布图与压力位/支撑位共用；切换基准价开关时直接复用，不重新聚合。
  const points = aggregateByStrike(expirationRows, spot);
  state.levelsWindowFetchedAt = analysisPayload?.fetched_at || null;
  // 记录本次分析输入，窗口尺寸变化（含手机横竖屏切换）与基准价切换后都按这些输入重绘。
  state.lastAnalysis = { rows, spot, analysisPayload, expirationRows, ivModel, basis, points };
  // 压力位/支撑位用「基准价」（默认实时价，可切盘后价），图表仍用常规价。
  const levelSpot = Number(basis?.price) > 0 ? Number(basis.price) : spot;
  state.levelBasisLabel = basis?.label || "常规";
  const scopeText = expirationRows.length ? `到期日 ${state.expiration || "--"} · ${expirationRows.length} 个合约` : "当前到期日无数据";
  state.view.chart.volumeScope = scopeText;
  state.view.chart.oiScope = scopeText;
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
  state.view.chart.netGex = `净 Gamma ${formatGex(points.reduce((sum, point) => sum + point.callGex + point.putGex, 0), 2)}`;
  state.view.chart.gammaFlip = `零 Gamma ${gammaFlip ? formatMoney(gammaFlip.strike) : "--"}`;
  state.view.chart.gammaScope = `柱状图 ${scopeText}${scopeSuffix}`;
  const analysisFallback = analysisPayload?.oi_fallback || {};
  if (analysisFallback.restored) state.view.chart.gammaScope += ` · 未平仓量回溯 ${formatDay(analysisFallback.as_of)}`;
  state.view.chart.callWall = `看涨墙 ${callWall?.callGex ? formatMoney(callWall.strike) : "--"}`;
  state.view.chart.putWall = `看跌墙 ${putWall?.putGex ? formatMoney(putWall.strike) : "--"}`;
  // 选中期限的综合价位与 Gamma 窗口并行请求，避免首次加载时趋势/支撑/压力面板长期空白。
  loadFactorLevels(points, levelSpot);
  const chartChecksum = points.reduce((sum, point) => sum + point.callGex + point.putGex + point.callVolume + point.putVolume + point.callOi + point.putOi, 0);
  const chartSignature = `${points.length}|${spot}|${gammaFlip ? gammaFlip.strike : ""}|${scopeText}|${chartChecksum}`;
  if (!forceChartRedraw && chartSignature === analysisChartSignatureValue) return;
  analysisChartSignatureValue = chartSignature;
  // 先让行情和期权链完成绘制，再在下一拍重建 SVG，避免和表格挤在同一次长任务里。
  scheduleScopeCharts(() => {
    OptionScopeCharts.renderSignedChart("gex-chart", points, "callGex", "putGex", "M", "当前期权链未提供 Gamma，暂无法估算 GEX", { spot, gammaFlip, crosshairTags: true, markers: [
      { point: callWall, className: "chart-wall-call", label: "看涨墙", position: "top" },
      { point: putWall, className: "chart-wall-put", label: "看跌墙", position: "bottom" },
    ] });
    OptionScopeCharts.renderSignedChart("volume-chart", points, "callVolume", "putVolume", "", "暂无成交量分布", { axis: "right", crosshairTags: true, valueLabel: "成交量", markers: [
      { point: volumeCallPeak, className: "chart-wall-call", label: "看涨", position: "top" },
      { point: volumePutPeak, className: "chart-wall-put", label: "看跌", position: "bottom" },
    ] });
    OptionScopeCharts.renderSignedChart("oi-chart", points, "callOi", "putOi", "", "暂无持仓量分布", { axis: "right", crosshairTags: true, valueLabel: "持仓量", markers: [
      { point: oiCallPeak, className: "chart-wall-call", label: "看涨", position: "top" },
      { point: oiPutPeak, className: "chart-wall-put", label: "看跌", position: "bottom" },
    ] });
    OptionScopeCharts.renderDistributionSummary(byId("volume-summary"), expirationRows, spot, "volume", "总成交量");
    OptionScopeCharts.renderDistributionSummary(byId("oi-summary"), expirationRows, spot, "open_interest", "总持仓量");
  });
}

function scheduleScopeCharts(draw) {
  const token = ++scopeChartToken;
  if (scopeChartTimer) clearTimeout(scopeChartTimer);
  scopeChartTimer = setTimeout(() => {
    scopeChartTimer = null;
    if (token !== scopeChartToken) return;
    draw();
  }, 0);
}

function cancelScopeCharts() {
  scopeChartToken += 1;
  if (scopeChartTimer) {
    clearTimeout(scopeChartTimer);
    scopeChartTimer = null;
  }
}

// 所有 AJAX 请求都优先把 key 放进 URL 查询参数，避免浏览器存储策略影响鉴权。
function withAccessKey(path, accessKey) {
  if (!accessKey) return path;
  const url = new URL(path, location.origin);
  if (!url.searchParams.has("key")) url.searchParams.set("key", accessKey);
  return `${url.pathname}${url.search}${url.hash}`;
}

async function request(path, options = {}) {
  // URL key、localStorage/sessionStorage 和内存回退按优先级逐层读取，兼容禁用存储的浏览器。
  const accessKey = currentAccessKey();
  if (accessKeyRequired() && !accessKey) {
    const message = "403 Forbidden";
    showAccessDenied();
    throw new Error(message);
  }
  return OptionScopeRequest.request(path, options, {
    accessKey,
    required: accessKeyRequired(),
    withAccessKey,
    onDenied: showAccessDenied,
  });
}

// 夜盘正式收盘价优先取盘后摘要里的 reference_close；它代表最近一次正常盘收盘，
// 不能误用 sessions.post.price（那是盘后最新价）。没有扩展时段摘要时再回退行情源的 previous_close。
function overnightCloseQuote(quote) {
  const sessions = quote?.sessions || {};
  const reference = Number(sessions.post?.reference_close ?? sessions.overnight?.reference_close);
  const previous = Number(quote?.previous_close);
  const price = Number.isFinite(reference) && reference > 0
    ? reference
    : (Number.isFinite(previous) && previous > 0 ? previous : null);
  if (price == null) return null;
  const change = Number.isFinite(previous) && previous > 0 ? (price - previous) / previous * 100 : null;
  return { ...quote, price, change_percent: change };
}

// 现价按时段动态取值：盘前显示盘前价，盘后显示盘后价，夜盘显示最近正常盘收盘价，
// 盘中与休市显示常规价。对应时段没有数据时回退常规价，避免整块行情空掉。
function activeSessionQuote(quote) {
  const sessions = quote?.sessions || {};
  const marketState = quote?.market_state;
  if (marketState === "PRE" && sessions.pre?.price != null) return sessions.pre;
  if (marketState === "POST" && sessions.post?.price != null) return sessions.post;
  if (marketState === "OVERNIGHT") {
    // 盘后价基准明确要求显示盘后最新价；实时价基准仍显示正式收盘价。
    if (state.levelBasisMode === "close" && sessions.post?.price != null) return sessions.post;
    return overnightCloseQuote(quote) || quote;
  }
  return quote;
}

function renderQuote(quote) {
  const active = activeSessionQuote(quote);
  const price = active?.price ?? quote?.price;
  const change = active?.change_percent ?? quote?.change_percent;
  state.view.quoteSymbol = quote?.symbol || state.symbol;
  state.view.quotePrice = formatMoney(price);
  state.view.quoteChange = change == null ? "涨跌 --" : `涨跌 ${change >= 0 ? "+" : ""}${Number(change).toFixed(2)}%`;
  // 涨跌色统一走 --up / --down：当前全局口径是绿涨红跌，变量名不再写死颜色。
  state.view.quoteChangeColor = change == null ? "var(--muted)" : (change < 0 ? "var(--down)" : "var(--up)");
  state.view.quoteCurrency = quote?.currency || "USD";
  state.view.quoteMarket = quoteMarketLabel(quote);
  state.view.marketState = marketStateLabel(quote?.market_state, "快照数据");
  state.lastQuote = quote || null;
}

// 时段标签：数据源若返回未知取值就原样展示，避免换口径时把信息吞掉。
function marketStateLabel(value, fallback = "快照") {
  if (!value) return fallback;
  return MARKET_STATE_LABELS[value] || value;
}

// 夜盘没有实时价：现货卡片的标签跟随分析基准，避免把收盘价继续标成「夜盘」。
function quoteMarketLabel(quote) {
  if (quote?.market_state !== "OVERNIGHT") return marketStateLabel(quote?.market_state);
  return state.levelBasisMode === "close" ? "盘后" : "收盘";
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

// 热力单元格模型：底色深浅表示该值在本列的相对强弱，悬停给出数值与本列最强值。
function heatCellModel(value, peak, label, hotLevel) {
  const number = Number(value) || 0;
  const percent = heatPercent(number, peak);
  const title = percent == null
    ? `${label} ${formatNumber(number)}`
    : `${label} ${formatNumber(number)} · 本屏最强 ${formatNumber(peak)}（占 ${Math.round((number / Number(peak)) * 100)}%）`;
  return {
    value: formatNumber(number),
    className: percent != null && percent >= hotLevel ? "chain-heat-hot" : "",
    style: percent == null ? "" : `--heat:${percent}`,
    title,
  };
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
function chainRowsFingerprint(rows, spot, emptyLabel) {
  let checksum = rows.length;
  for (let index = 0; index < rows.length; index += 1) {
    const row = rows[index];
    checksum = (checksum + (Number(row.volume) || 0) + (Number(row.open_interest) || 0) + Math.round((Number(row.strike) || 0) * 100)) % 1000000007;
  }
  return `${state.chainFetchedAt}|${spot}|${state.chainFilter}|${emptyLabel}|${checksum}`;
}

function renderChainRows(rows, spot, emptyLabel = "没有期权数据") {
  const fingerprint = chainRowsFingerprint(rows, spot, emptyLabel);
  // 快照和现价都没变时不替换行数组，子组件就不会因为状态文字刷新而重绘整表。
  if (fingerprint === chainBodySignatureValue) return;
  chainBodySignatureValue = fingerprint;
  if (!rows.length) {
    state.view.chainRows = [];
    state.view.chainEmpty = emptyLabel;
    state.view.chainHeatNote = chainHeatNote(0, 0);
    return;
  }
  const volumePeak = Math.max(0, ...rows.map((row) => Number(row.volume) || 0));
  const interestPeak = Math.max(0, ...rows.map((row) => Number(row.open_interest) || 0));
  state.view.chainHeatNote = chainHeatNote(volumePeak, interestPeak);
  state.view.chainEmpty = emptyLabel;
  state.view.chainRows = rows.map((row, index) => {
    const isCall = row.contract_type === "call";
    const gex = contractGex(row, spot);
    const volume = heatCellModel(row.volume, volumePeak, "成交量", isCall ? HEAT_HOT_LEVEL.call : HEAT_HOT_LEVEL.put);
    const interest = heatCellModel(row.open_interest, interestPeak, "未平仓", isCall ? HEAT_HOT_LEVEL.call : HEAT_HOT_LEVEL.put);
    return {
      key: row.contract_symbol || `${row.contract_type || "option"}-${row.strike}-${index}`,
      rowClass: isCall ? "chain-call" : "chain-put",
      typeClass: isCall ? "type-call" : "type-put",
      strike: formatMoney(row.strike),
      volume: volume.value,
      volumeClass: volume.className,
      volumeStyle: volume.style,
      volumeTitle: volume.title,
      interest: interest.value,
      interestClass: interest.className,
      interestStyle: interest.style,
      interestTitle: interest.title,
      gamma: formatModelGamma(row),
      gex: gex == null ? "--" : formatGex(gex / 1000000),
      gexTitle: "Gamma × 未平仓 × 100 × 现价² × 0.01，即现价每变动 1% 的美元敞口",
      iv: formatModelIv(row),
      itm: row.in_the_money ? "价内" : "价外",
      itmClass: row.in_the_money ? "itm" : "",
    };
  });
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
  const rows = payload.data || [];
  const basis = activeBasis(quote);
  const signature = [
    payload?.symbol,
    payload?.expiration,
    payload?.fetched_at,
    rows.length,
    payload?.oi_fallback?.as_of,
    payload?.oi_fallback?.restored,
    quote?.price,
    quote?.change_percent,
    quote?.market_state,
    state.levelBasisMode,
    basis?.price,
    basis?.label,
    analysisPayload?.fetched_at,
    analysisPayload?.zero_gamma?.price,
    analysisPayload?.contract_count,
    analysisPayload?.oi_fallback?.as_of,
  ].join("|");
  // 自动刷新经常拿到同一份快照。签名一致时跳过聚合、图表和表格。
  if (signature === chainRenderSignature && state.view.chainRows.length) return;
  state.expiration = payload.expiration;
  // 基准价开关切换时要用最近一次快照重算，这里留一份引用。
  state.lastQuote = quote || null;
  state.view.chainTitle = `${payload.symbol} · ${payload.expiration}`;
  // 期权链始终从 SQLite 读取，这里按快照新鲜度标注来源，避免刚抓完还显示“缓存”造成误解。
  const snapshotAge = payload.fetched_at ? (Date.now() - new Date(payload.fetched_at).getTime()) / 1000 : null;
  // 上游在盘前/收盘后可能整链返回 0 未平仓量，读取层会用该合约最近一次有效值兜底，这里如实标注。
  const oiFallback = payload.oi_fallback || {};
  state.view.dataSource = (snapshotAge != null && snapshotAge >= 0 && snapshotAge < AUTO_REFRESH_SECONDS ? "上游新快照" : "SQLite 缓存") + (oiFallback.restored ? ` · 未平仓量回溯 ${formatDay(oiFallback.as_of)}` : "");
  state.view.fetchedAt = `快照时间 ${formatTime(payload.fetched_at)}`;
  state.view.totalCount = formatNumber(rows.length);
  const calls = rows.filter((row) => row.contract_type === "call"); const puts = rows.filter((row) => row.contract_type === "put");
  state.view.callVolume = formatNumber(calls.reduce((sum, row) => sum + (Number(row.volume) || 0), 0));
  state.view.putVolume = formatNumber(puts.reduce((sum, row) => sum + (Number(row.volume) || 0), 0));
  state.view.callInterest = `未平仓 ${formatNumber(calls.reduce((sum, row) => sum + (Number(row.open_interest) || 0), 0))}`;
  state.view.putInterest = `未平仓 ${formatNumber(puts.reduce((sum, row) => sum + (Number(row.open_interest) || 0), 0))}`;
  const analysisRows = analysisPayload?.data?.length ? analysisPayload.data : rows;
  // 记录本次快照时间：压力位/支撑位的合成接口按「标的 + 到期日 + 快照时间」去重请求。
  state.chainFetchedAt = payload.fetched_at || null;
  renderAnalysis(analysisRows, quote?.price, analysisPayload, rows, payload.iv_model || {}, activeBasis(quote));
  state.chainRows = rows; state.chainSpot = quote?.price ?? null;
  renderChainTable();
  chainRenderSignature = signature;
}

// emptyLabel：本地没有到期日时的占位文案，需要区分「还没抓过」（正在获取）和「该标的没有期权」。
function applyExpirations(dates, preferred, emptyLabel = "正在获取到期日…") {
  const unique = [...new Set((dates || []).filter(Boolean))];
  state.expirationOptions = unique;
  state.expirationPlaceholder = emptyLabel;
  if (!unique.length) {
    syncRefreshButton();
    return false;
  }
  const requested = preferred || parsePageQuery(location.search).expiration || state.expiration;
  state.expiration = unique.includes(requested) ? requested : unique[0];
  syncRefreshButton();
  return true;
}

function isCurrentLoad(loadId, expiration) {
  return state.loadId === loadId && (!expiration || state.expiration === expiration);
}

function quoteIsReady(quote) {
  const price = Number(quote?.price);
  return Number.isFinite(price) && price > 0;
}

function hasValidOptionQuotes(rows) {
  return (rows || []).some((row) => {
    const bid = Number(row?.bid);
    const ask = Number(row?.ask);
    return Number.isFinite(bid) && Number.isFinite(ask) && bid > 0 && ask >= bid;
  });
}

// 快照仍在新鲜期内时不再请求上游接口，只把本地缓存的状态回显给用户。
function isSnapshotFresh(snapshot) {
  const age = snapshotAgeSeconds(snapshot?.fetchedAt);
  return Boolean(snapshot?.shown && snapshot?.quoteReady && snapshot?.optionsQuotesReady) && age !== null && age < SNAPSHOT_FRESH_SECONDS;
}

function showFreshStatus(snapshot) {
  const age = snapshotAgeSeconds(snapshot?.fetchedAt);
  state.view.lastStatus = `本地快照 ${formatTime(snapshot?.fetchedAt)} 已是最新（${Math.round(age ?? 0)} 秒前）`;
}

function showPending(message) {
  state.view.chainTitle = `${state.symbol}${state.expiration ? ` · ${state.expiration}` : ""}`;
  state.view.dataSource = "后台刷新中";
  state.view.fetchedAt = "快照时间 --";
  state.view.totalCount = "--";
  state.view.callVolume = "--";
  state.view.putVolume = "--";
  state.view.callInterest = "未平仓 --";
  state.view.putInterest = "未平仓 --";
  state.view.lastStatus = message;
  state.view.chart.netGex = "净 Gamma --";
  state.view.chart.gammaFlip = "零 Gamma --";
  state.view.chart.callWall = "看涨墙 --";
  state.view.chart.putWall = "看跌墙 --";
  state.view.chart.gammaScope = "Gamma 范围 --";
  state.view.chart.levelsBasis = "基准 --";
  state.view.chart.volumeScope = "";
  state.view.chart.oiScope = "";
  OptionScopeCharts.showEmpty("gex-chart", message);
  OptionScopeCharts.showEmpty("volume-chart", "正在后台加载");
  OptionScopeCharts.showEmpty("oi-chart", "正在后台加载");
  OptionScopeCharts.clearSummary("volume-summary");
  OptionScopeCharts.clearSummary("oi-summary");
  // 压力位/支撑位会在新快照渲染后重新请求合成接口，这里先清空并解除去重键。
  clearTimeout(state.levelsRetryTimer);
  state.levelsRetryTimer = null;
  state.levelsKey = "";
  state.levelsPayload = null;
  state.view.levels.resistance = [];
  state.view.levels.support = [];
  state.view.levels.add = [];
  state.view.levels.resistanceNote = "等待数据";
  state.view.levels.supportNote = "等待数据";
  state.view.levels.resistanceEmpty = message;
  state.view.levels.supportEmpty = message;
  state.view.levels.addEmpty = message;
  state.view.buyer = {
    available: false,
    directionLabel: "",
    horizonLabel: "未来 5 个交易日",
    target: "",
    items: [],
    reason: message,
    note: "",
  };
  OptionScopeCharts.showEmpty("levels-chart", message);
  state.view.trend = { ...state.view.trend, available: false, rows: [], opportunities: [], extremes: [], note: "等待数据", empty: message };
  state.view.chainRows = [];
  state.view.chainEmpty = message;
  state.view.chainHeatNote = "等待数据";
  chainRenderSignature = "";
  chainBodySignatureValue = "";
  analysisChartSignatureValue = "";
  cancelScopeCharts();
}

function applyCachedQuote(quote) {
  if (!quote || quote.price == null) {
    state.view.quoteSymbol = state.symbol;
    state.view.quotePrice = "--";
    state.view.quoteChange = "正在更新";
    state.view.quoteChangeColor = "var(--muted)";
    state.view.quoteCurrency = "USD";
    state.view.quoteMarket = "后台刷新";
    state.view.marketState = "后台刷新中";
    return;
  }
  renderQuote(quote);
}

async function renderSnapshot(loadId, fallbackQuote = null) {
  if (!isCurrentLoad(loadId)) return { shown: false, source: null };
  // 记住这次请求对应的到期日：响应回来时如果用户已经切走，整份数据作废（见 isCurrentLoad 的第二个参数），
  // 否则后到的旧响应会把新选择的数据覆盖掉。
  const expiration = state.expiration;
  const encodedSymbol = encodeURIComponent(state.symbol);
  const encodedExpiration = expiration ? encodeURIComponent(expiration) : "";
  const requests = [
    request(`/api/quote/${encodedSymbol}`).catch(() => null),
    expiration ? request(`/api/chain/${encodedSymbol}?expiration=${encodedExpiration}`).catch(() => null) : Promise.resolve(null),
    request(`/api/gamma/${encodedSymbol}?horizon_days=45&include_rows=false`).catch(() => null),
  ];
  const [quote, payload, analysis] = await Promise.all(requests);
  if (!isCurrentLoad(loadId, expiration)) return { shown: false, source: null };
  const resolvedQuote = quoteIsReady(quote) ? quote : fallbackQuote;
  applyCachedQuote(resolvedQuote);
  if (!payload?.data?.length) return { shown: false, source: payload?.source || resolvedQuote?.source || null, fetchedAt: payload?.fetched_at || null, quote: resolvedQuote };
  state.analysisReady = gammaProfileReady(analysis) || state.analysisReady;
  renderChain(payload, resolvedQuote, analysis);
  state.view.lastStatus = payload.source === "sqlite"
    ? `本地缓存 ${formatTime(payload.fetched_at)}`
    : `最近更新 ${formatTime(payload.fetched_at)}`;
  syncPageQuery();
  return {
    shown: true,
    source: payload.source || null,
    fetchedAt: payload.fetched_at || null,
    quote: resolvedQuote,
    payload,
    analysisReady: gammaProfileReady(analysis),
    quoteReady: quoteIsReady(resolvedQuote),
    optionsQuotesReady: hasValidOptionQuotes(payload.data),
  };
}

// 跨期限 Gamma 窗口刷新最慢（SPY 需要串行拉取十余个到期日），放到后台执行：
// 表格与行情先落地，窗口数据回来后只重画分析区；同一标的只允许一个窗口刷新在飞，避免连点叠加请求。
function refreshAnalysisWindow(loadId, payload, quote) {
  const symbol = state.symbol;
  if (state.analysisRefreshSymbol === symbol) return;
  state.analysisRefreshSymbol = symbol;
  const encodedSymbol = encodeURIComponent(symbol);
  const pendingText = `快照已更新 ${formatTime(payload?.fetched_at)} · 正在后台刷新 Gamma 窗口…`;
  state.view.lastStatus = pendingText;
  pollGammaWindow(loadId, payload, quote, symbol, encodedSymbol, pendingText, 0);
}

async function latestSelectedChain(loadId, symbol, fallbackPayload) {
  const expiration = state.expiration;
  if (!expiration) return fallbackPayload;
  const encodedSymbol = encodeURIComponent(symbol);
  const encodedExpiration = encodeURIComponent(expiration);
  const latest = await request(`/api/chain/${encodedSymbol}?expiration=${encodedExpiration}`).catch(() => null);
  if (!isCurrentLoad(loadId, expiration) || state.symbol !== symbol) return null;
  return latest?.data?.length ? latest : fallbackPayload;
}

function gammaProfileReady(analysis) {
  if (!analysis || analysis.status_only) return false;
  const count = Number(analysis.contract_count);
  if (Number.isFinite(count)) return count > 0;
  return Boolean(analysis.data?.length);
}

// 后端 Gamma 刷新改为 SQLite 任务协调的后台任务；前端轮询任务状态，期间继续展示旧分析。
function pollGammaWindow(loadId, payload, quote, symbol, encodedSymbol, pendingText, attempt) {
  // 任务没完成时只问状态，不把 45 天合约下载下来再解析。
  request(`/api/gamma/${encodedSymbol}?horizon_days=45&refresh=true&status_only=true`)
    .then(async (status) => {
      if (!isCurrentLoad(loadId) || state.symbol !== symbol) return;
      if (status?.refresh?.status === "running" && attempt < 30) {
        state.analysisRefreshTimer = setTimeout(() => {
          state.analysisRefreshTimer = null;
          pollGammaWindow(loadId, payload, quote, symbol, encodedSymbol, pendingText, attempt + 1);
        }, 1000);
        return;
      }
      const [analysis, selectedPayload] = await Promise.all([
        request(`/api/gamma/${encodedSymbol}?horizon_days=45&include_rows=false`).catch(() => null),
        latestSelectedChain(loadId, symbol, payload),
      ]);
      if (!selectedPayload) return;
      if (!gammaProfileReady(analysis)) {
        // 没有可用的新窗口数据时，至少用旧分析完成一次价位刷新，保持价位与新快照同步。
        renderChain(selectedPayload, quote, state.lastAnalysis?.analysisPayload || null);
        return;
      }
      state.analysisReady = true;
      renderChain(selectedPayload, quote, analysis);
      // 窗口刷新期间用户可能又点了刷新：只在提示文案还属于本次窗口刷新时才改写，避免覆盖更新的状态。
      if (state.view.lastStatus === pendingText) state.view.lastStatus = `最近更新 ${formatTime(payload?.fetched_at)}`;
    })
    .catch((error) => {
      if (!isCurrentLoad(loadId) || state.symbol !== symbol) return;
      // Gamma 窗口刷新失败时仍补做一次价位请求，避免快照已更新却继续显示旧的价位结果。
      renderChain(payload, quote, state.lastAnalysis?.analysisPayload || null);
      if (state.view.lastStatus === pendingText) state.view.lastStatus = `Gamma 窗口刷新失败，仍显示本地缓存（${error.message}）`;
    })
    .finally(() => {
      if (state.analysisRefreshTimer || (state.analysisRefreshSymbol !== symbol)) return;
      if (attempt >= 30) state.analysisRefreshSymbol = null;
      else state.analysisRefreshSymbol = null;
    });
}

async function refreshInBackground(loadId, force = false) {
  const symbol = state.symbol;
  const expiration = state.expiration;
  const encodedSymbol = encodeURIComponent(symbol);
  if (!isCurrentLoad(loadId) || state.symbol !== symbol) return;
  // 同一标的只允许一条网络刷新链路：页面加载、手动点击、定时刷新共用这份互斥，避免叠加出多组请求。
  if (state.refreshInFlight === symbol) return;
  state.refreshInFlight = symbol;
  try {
    state.view.lastStatus = "正在请求上游快照…";
    // 手动刷新必须强制回源；自动刷新仍复用 60 秒新鲜期，避免定时器重复请求上游。
    const params = new URLSearchParams({ max_age: String(force ? 0 : SNAPSHOT_FRESH_SECONDS) });
    if (expiration) params.set("expiration", expiration);
    const refreshResult = await request(`/api/refresh/${encodedSymbol}?${params}`, { method: "POST" });
    if (!isCurrentLoad(loadId) || state.symbol !== symbol) return;
    // 后端在标的没有挂牌期权时只写现货快照：走现货渲染分支，避免页面一直停在“后台刷新中”。
    if (refreshResult?.quote_only) { await loadQuoteOnly(loadId, force); return; }
    if (refreshResult?.skipped) {
      // 首次读取时某个并发请求可能暂时失败，但后端已经确认 SQLite 快照新鲜；
      // 重新读取并使用刷新响应附带的 quote，避免页面停在“股价 -- / Gamma 0”。
      const snapshot = await renderSnapshot(loadId, refreshResult.quote || null);
      if (snapshot.shown && !snapshot.analysisReady && snapshot.payload?.data?.length && snapshot.quote) {
        refreshAnalysisWindow(loadId, snapshot.payload, snapshot.quote);
      }
      state.view.lastStatus = `本地快照 ${formatTime(refreshResult.fetched_at)} 已是最新（${Math.round(Number(refreshResult.age_seconds) || 0)} 秒前）`;
      return;
    }
    if (!state.expiration && refreshResult?.expiration) {
      applyExpirations([refreshResult.expiration], refreshResult.expiration);
    }
    const currentExpiration = state.expiration || refreshResult?.expiration;
    if (!currentExpiration) { await loadQuoteOnly(loadId, force); return; }
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
      await refreshInBackground(loadId, force);
      return;
    }
    if (expirations?.expirations?.length) applyExpirations(expirations.expirations, currentExpiration);
    // 选中的到期日已下架（例如当天盘后过期）时，切到最新到期日并重新加载；同样先释放互斥标记再重入。
    if (state.expiration && state.expiration !== currentExpiration) {
      state.refreshInFlight = null;
      await refreshInBackground(loadId, force);
      return;
    }
    let resolvedQuote = quoteIsReady(quote) ? quote : refreshResult?.quote;
    if (!quoteIsReady(resolvedQuote)) {
      // 快照响应和 quote 查询都可能在低配服务器上错开；最后再主动读一次上游行情。
      const retryQuote = await request(`/api/quote/${encodedSymbol}?refresh=true`).catch(() => null);
      if (quoteIsReady(retryQuote)) resolvedQuote = retryQuote;
    }
    renderQuote(resolvedQuote);
    // Gamma 窗口继续后台刷新；选中期限的综合价位已在这里与窗口任务并行请求。
    renderChain(payload, resolvedQuote, state.lastAnalysis?.analysisPayload || null);
    state.view.lastStatus = `最近更新 ${formatTime(payload.fetched_at)}`;
    syncPageQuery();
    refreshAnalysisWindow(loadId, payload, resolvedQuote);
  } catch (error) {
    if (!isCurrentLoad(loadId) || state.symbol !== symbol) return;
    state.view.lastStatus = `后台刷新失败，仍显示本地缓存（${error.message}）`;
    if (!state.view.chainRows.length) setError(error.message);
    // 失败原因通常是所选到期日已过期下架：拉一次最新到期日，必要时自动切换到可刷新的期限。
    state.refreshInFlight = null;
    const fresh = await request(`/api/expirations/${encodedSymbol}?refresh=true`).catch(() => null);
    if (!isCurrentLoad(loadId) || state.symbol !== symbol) return;
    const dates = fresh?.expirations || [];
    if (dates.length && !dates.includes(state.expiration)) {
      applyExpirations(dates, null);
      await loadChain({ loadId, force });
    }
  } finally {
    if (state.refreshInFlight === symbol) state.refreshInFlight = null;
  }
}

// 标的没有挂牌期权（或可用期限已全部到期）时只渲染现货：现货与盘前盘后照常刷新，
// 期权相关面板统一提示没有期权数据，避免整页停在 “--”。
async function loadQuoteOnly(loadId, force = false) {
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
    await refreshInBackground(loadId, force);
    return;
  }
  applyCachedQuote(quote);
  // 所选期限可能刚刚全部到期：清掉失效的到期日，避免它继续留在 state 和地址栏里。
  state.expiration = null;
  showPending(symbol + " 没有挂牌期权（或可用期限已全部到期），仅显示现货行情");
  applyExpirations([], null, "无期权到期日");
  state.view.dataSource = "上游无期权数据";
  state.view.lastStatus = "该标的没有期权合约，仅显示现货行情";
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
  try {
    const loadId = options.loadId || state.loadId;
    const force = options.force === true;
    // 即使本地还没有到期日也要往下走：首次加载某个标的时后端需要回源才能拿到期限列表，
    // 提前 return 会让页面永远停在没有数据的状态。
    const snapshot = await renderSnapshot(loadId);
    if (!snapshot.shown) showPending("正在后台获取上游快照…");
    if (options.refresh === false) return;
    // 先读 SQLite 判断新鲜度：仍在新鲜期内直接复用，只有确认过期才请求上游接口。
    if (!force && isSnapshotFresh(snapshot)) {
      showFreshStatus(snapshot);
      // 首次访问可能只有选中期限的缓存，跨期限 Gamma 尚未生成；只补后台分析，不阻塞首屏。
      if (!snapshot.analysisReady && snapshot.payload?.data?.length && snapshot.quote) {
        refreshAnalysisWindow(loadId, snapshot.payload, snapshot.quote);
      }
      return;
    }
    await refreshInBackground(loadId, force);
  } finally {
    scheduleAutoRefresh();
  }
}

async function loadSymbol() {
  const symbol = (state.symbolInput || byId("symbol-input").value).trim().toUpperCase();
  if (!symbol) return;
  state.symbolInput = symbol;
  state.symbol = symbol;
  state.expiration = parsePageQuery(location.search).expiration || null;
  state.analysisReady = false;
  const loadId = ++state.loadId;
  setError("");
  applyCachedQuote(null);
  state.view.lastStatus = "正在读取本地缓存…";
  syncPageQuery();
  try {
    await loadExpirations(loadId);
  } catch (error) {
    if (!isCurrentLoad(loadId)) return;
    state.expirationOptions = [];
    state.expirationPlaceholder = "加载失败";
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
  const symbol = (state.symbolInput || byId("symbol-input").value).trim().toUpperCase();
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

// 手动点击强制刷新上游快照；60 秒定时刷新仍优先复用 SQLite 新鲜缓存，整段流程互斥。
async function refresh(silent = false) {
  if (state.refreshing) return;
  state.refreshing = true;
  syncRefreshButton();
  const loadId = state.loadId;
  try {
    if (!silent) setError("");
    await loadChain({ loadId, force: !silent });
  } catch (error) {
    if (isCurrentLoad(loadId)) setError(error.message);
  } finally {
    state.refreshing = false;
    syncRefreshButton();
    scheduleAutoRefresh();
  }
}

initTheme();
// Element UI 2.x 基于 Vue 2，必须在根实例创建前注册；静态库已经由 index.html 按依赖顺序加载。
if (window.Vue && window.ELEMENT) Vue.use(ELEMENT);
// 期权链单独成组件：父页面刷新报价或状态时，行数组引用不变就不重绘这张表。
if (window.Vue) {
  Vue.component("chain-table-body", {
    props: {
      rows: { type: Array, default: () => [] },
      empty: { type: String, default: "" },
    },
    template: '<tbody id="chain-body"><tr v-if="!rows.length"><td colspan="7" class="empty">{{ empty }}</td></tr><tr v-for="row in rows" :key="row.key" :class="row.rowClass"><td class="num chain-strike" :class="row.typeClass">{{ row.strike }}</td><td class="num chain-heat" :class="row.volumeClass" :style="row.volumeStyle" :title="row.volumeTitle">{{ row.volume }}</td><td class="num chain-heat" :class="row.interestClass" :style="row.interestStyle" :title="row.interestTitle">{{ row.interest }}</td><td class="num">{{ row.gamma }}</td><td class="num" :title="row.gexTitle">{{ row.gex }}</td><td class="num">{{ row.iv }}</td><td :class="row.itmClass">{{ row.itm }}</td></tr></tbody>',
  });
}
const optionScopeApp = new Vue({
  el: "#app",
  data: state,
  computed: {
    busy() { return this.loading || this.refreshing; },
  },
  methods: {
    navigateToSymbol() { return navigateToSymbol(); },
    refreshNow() { return refresh(false); },
    expirationChanged() { return switchExpiration(this.expiration); },
    filterChanged() { return renderChainTable(); },
    toggleTheme() {
      const next = document.documentElement.dataset.theme === "dark" ? "light" : "dark";
      applyTheme(next);
      try { localStorage.setItem(THEME_KEY, next); } catch (error) { /* 存储被禁用时保留当前页面主题。 */ }
    },
  },
});
window.optionScopeApp = optionScopeApp;
// Vue 挂载会重建模板中的后代节点，折叠事件必须在根实例创建后绑定到最终 DOM 节点。
initDetailGroup();
initChainGroup();
initChartGroup();
initBuyerStructureGroup();
// 时钟是独立的高频显示，不进入 Vue 响应式树，避免每秒遍历整张期权链的虚拟 DOM。
function updateClock() {
  const clock = byId("clock");
  if (clock) clock.textContent = new Date().toLocaleTimeString("zh-CN", { hour12: false });
}
updateClock();
setInterval(updateClock, 1000);
// 容器尺寸与上次绘制不一致时按新尺寸重绘图表：窗口缩放、图表折叠组展开后都走这里。
// 图表是按容器实际像素绘制的，隐藏状态下只能量到最小尺寸，所以展开后必须补一次重绘。
function redrawChartsIfResized() {
  if (!state.lastAnalysis) return;
  const charts = ["gex-chart", "levels-chart", "volume-chart", "oi-chart"].filter((id) => byId(id).querySelector("svg"));
  const changed = charts.some((id) => {
    const box = OptionScopeCharts.chartContentBox(byId(id));
    return String(box.width) !== byId(id).dataset.chartWidth || String(box.height) !== byId(id).dataset.chartHeight;
  });
  if (!changed) return;
  const { rows, spot, analysisPayload, expirationRows, ivModel, basis } = state.lastAnalysis;
  forceChartRedraw = true;
  renderAnalysis(rows, spot, analysisPayload, expirationRows || [], ivModel || {}, basis || null);
  forceChartRedraw = false;
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
state.symbol = initialQuery.symbol || byId("symbol-input").value.trim().toUpperCase() || "QQQ";
state.symbolInput = state.symbol;
if (accessKeyRequired() && !initialAccessKey) {
  showAccessDenied();
} else {
  // 没有到期日（仅现货标的）也要走刷新链路：后端会返回 quote_only，只更新现货卡片。
  // 首次按 60 秒排程；首屏读完快照后会按真实年龄提前或推后。
  scheduleAutoRefresh();
  loadSymbol();
}
