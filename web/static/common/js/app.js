const defaultSymbolValue = (document.getElementById("symbol-input")?.getAttribute("value") || "").trim().toUpperCase();
const defaultSymbol = /^[A-Z0-9][A-Z0-9.-]{0,9}$/.test(defaultSymbolValue) ? defaultSymbolValue : "QQQ";
const GAMMA_ESTIMATE_TITLE = "Black-Scholes 估算：看涨为正、看跌为负，假设客户买入期权、做市商方向相反。不是真实做市商库存，也不是成交价。";
// 压力位/支撑位各展示的条数。骨架行数也用这个上限，避免加载时表格先塌掉。
const LEVEL_COUNT = 10;
// 交易计划（买入 / 加仓 / 卖出）各自最多展示的条数。
const PLAN_COUNT = 10;
// Gamma 轮询不阻塞实时快照推送，但页面已有计算任务时延后应用推送以避免重复分析。
const ANALYSIS_REFRESH_BLOCK_MS = 8000;
// Gamma 窗口远慢于单期限快照，不跟随每次行情更新重复下载；十分钟内复用上一份窗口。
const ANALYSIS_REFRESH_COOLDOWN_SECONDS = 600;
// 批次运行期间最多读取 90 次状态，每次间隔 3 秒；批次完成后再启动下一批。
const ANALYSIS_POLL_LIMIT = 90;
// 估值计算独立于现货/期权快照，首次进入新标的时只轮询轻量 quote 接口，不重复抓整份期权链。
// 财务时序和基准数据可能比行情慢；轮询只读轻量 quote，后端会做任务去重。
const FAIR_VALUE_POLL_INTERVAL_MS = 3000;
const FAIR_VALUE_POLL_LIMIT = 50;
const FAIR_VALUE_RETRY_INTERVAL_MS = 2500;

const TREND_SIGNAL_TITLE = "规则估算，不是下单指令";
const CHAN_FOLD_KEYS = {
  panel: "option-scope-chan",
  stats: "option-scope-chan-stats",
  plan: "option-scope-chan-plan",
  terms: "option-scope-chan-terms",
  evidence: "option-scope-chan-evidence",
  rules: "option-scope-chan-rules",
};
const CHAN_SCENARIOS_STORAGE_KEY = "option-scope-chan-scenarios-visible";
const CHAN_CHART_PREFERENCES_KEY = "option-scope-chan-chart-preferences";
const CHAN_INTRADAY_PERIODS = ["15m", "4h"];
const CHAN_CHART_PERIODS = ["daily", "weekly", "15m", "4h"];
const CHAN_ANALYSIS_RETRY_LIMIT = 60;
const CHAN_INTRADAY_RETRY_LIMIT = 60;
const CHAN_INTRADAY_REFRESH_INTERVAL_MS = 15 * 60 * 1000;
// Intraday data and both server/provider caches have a three-minute TTL. Wait
// just beyond it so each expected new 15m candle can trigger a real refresh.
const CHAN_INTRADAY_REFRESH_DELAY_MS = 3 * 60 * 1000 + 5000;
let chanIntradayBoundaryTimer = null;
let chanIntradayExpiryRetryTimer = null;
let chanIntradayLastBoundaryAttempt = "";
let chanIntradayExpiryRetryBoundary = "";
const NEW_YORK_INTRADAY_DATE_FORMATTER = new Intl.DateTimeFormat("en-US", {
  timeZone: "America/New_York", weekday: "short", year: "numeric", month: "2-digit", day: "2-digit",
  hour: "2-digit", minute: "2-digit", hourCycle: "h23",
});
const CHAN_CHART_PREFERENCES_VERSION = 4;
const DEFAULT_CHAN_CHART_LAYERS = { pens: true, penCenters: true, legacySignals: true, chanCandles: true, segments: false, segmentLevels: {}, recursiveCenters: {}, signals: true };
function copyDefaultChanChartLayers() {
  return { ...DEFAULT_CHAN_CHART_LAYERS, segmentLevels: {}, recursiveCenters: {} };
}
function matchesPreviousDefaultChanChartLayers(layers) {
  const previousDefaults = { pens: false, penCenters: false, legacySignals: false, chanCandles: true, segments: true, signals: true };
  return Object.entries(previousDefaults).every(([key, value]) => layers[key] === value)
    && Object.keys(layers.segmentLevels || {}).length === 0
    && Object.keys(layers.recursiveCenters || {}).length === 0;
}
function readChanScenarioVisibility() {
  try { return localStorage.getItem(CHAN_SCENARIOS_STORAGE_KEY) === "1"; } catch (error) { return false; }
}
function readChanChartPreferences() {
  try {
    const stored = JSON.parse(localStorage.getItem(CHAN_CHART_PREFERENCES_KEY) || "null");
    if (!stored || typeof stored !== "object") {
      const defaults = { period: "daily", layers: copyDefaultChanChartLayers() };
      rememberChanChartPreferences(defaults.period, defaults.layers);
      return defaults;
    }
    const sourceLayers = stored.layers && typeof stored.layers === "object" ? stored.layers : {};
    const layers = { ...DEFAULT_CHAN_CHART_LAYERS };
    for (const key of ["pens", "penCenters", "legacySignals", "chanCandles", "segments", "signals"]) {
      if (typeof sourceLayers[key] === "boolean") layers[key] = sourceLayers[key];
    }
    for (const key of ["segmentLevels", "recursiveCenters"]) {
      const values = sourceLayers[key];
      if (!values || typeof values !== "object" || Array.isArray(values)) continue;
      layers[key] = Object.fromEntries(Object.entries(values)
        .filter(([level, visible]) => /^[0-7]$/.test(level) && typeof visible === "boolean"));
    }
    const storedVersion = Number(stored.version) || 1;
    // Earlier builds persisted their defaults immediately, so upgrade an untouched
    // old default set while keeping any explicitly customized layer choices.
    if (storedVersion < CHAN_CHART_PREFERENCES_VERSION && matchesPreviousDefaultChanChartLayers(sourceLayers)) {
      Object.assign(layers, copyDefaultChanChartLayers());
    }
    // Persist the unified-menu preference format without resetting explicit
    // layer choices made before the menu redesign.
    const preferences = { period: CHAN_CHART_PERIODS.includes(stored.period) ? stored.period : "daily", layers };
    if (storedVersion < CHAN_CHART_PREFERENCES_VERSION) rememberChanChartPreferences(preferences.period, preferences.layers);
    return preferences;
  } catch (error) { return { period: "daily", layers: copyDefaultChanChartLayers() }; }
}
function rememberChanChartPreferences(period, layers) {
  try { localStorage.setItem(CHAN_CHART_PREFERENCES_KEY, JSON.stringify({ version: CHAN_CHART_PREFERENCES_VERSION, period, layers })); } catch (error) { /* 存储不可用时保留当前页面选择。 */ }
}
const initialChanChartPreferences = readChanChartPreferences();
const state = {
  symbol: defaultSymbol,
  symbolInput: defaultSymbol,
  expiration: null,
  expirationOptions: [],
  expirationPlaceholder: "先载入标的",
  analysisReady: false,
  loadId: 0,
  chanPeriod: initialChanChartPreferences.period,
  chanMobileChart: window.matchMedia ? window.matchMedia("(max-width: 600px)").matches : false,
  chanViewports: { daily: { visibleBars: 0, endOffset: 0 }, weekly: { visibleBars: 0, endOffset: 0 }, "15m": { visibleBars: 0, endOffset: 0 }, "4h": { visibleBars: 0, endOffset: 0 } },
  chanChartLayers: initialChanChartPreferences.layers,
  chanScenariosVisible: readChanScenarioVisibility(),
  chanIntradayPayload: { "15m": null, "4h": null },
  chanIntradayLoading: { "15m": false, "4h": false },
  chanIntradayRetryTimers: { "15m": null, "4h": null },
  chanIntradayRetryAttempts: { "15m": 0, "4h": 0 },
  chanIntradaySpot: null,
  chanHoveredScenario: null,
  chanScenarioCardX: 0,
  chanScenarioHideTimer: null,
  chanSpotGuideVisible: false,
  chanSpotGuideX: 0,
  chanSpotGuideY: 0,
  chanSpotGuideSide: "up",
  chanExpanded: readFoldState(CHAN_FOLD_KEYS.panel, false),
  chanFolds: {
    stats: readFoldState(CHAN_FOLD_KEYS.stats, false),
    plan: readFoldState(CHAN_FOLD_KEYS.plan, false),
    terms: readFoldState(CHAN_FOLD_KEYS.terms, false),
    evidence: readFoldState(CHAN_FOLD_KEYS.evidence, false),
    rules: readFoldState(CHAN_FOLD_KEYS.rules, false),
  },
  chanLoading: false,
  chanPayload: null,
  chanStale: false,
  chanRequestId: 0,
  chanInFlightSymbol: "",
  chanLastRequestedAt: 0,
  chanRetryTimer: null,
  chanRetryAttempts: 0,
  chanMarketRetryTimer: null,
  chanMarketRetryAttempts: 0,
  chanMarketInFlight: false,
  chanMarketLastRequestedAt: 0,
  chanMarketLastKey: "",
  chanMarketPayload: null,
  chanMarketRefreshPending: false,
  chanRefreshPending: false,
  trendHistorySymbol: "",
  loading: false,
  refreshInFlight: null,
  fairValuePollTimer: null,
  fairValuePollKey: "",
  fairValuePollAttempts: 0,
  fairValuePollInFlight: false,
  quoteRetryTimer: null,
  quoteRetryAttempts: 0,
  quoteRetryInFlightKey: "",
  quoteRecoveryTimer: null,
  quoteRecoveryKey: "",
  quoteRecoveryAttempts: 0,
  quoteRecoveryInFlightKey: "",
  snapshotRetryTimer: null,
  snapshotRetryAttempts: 0,
  pushSource: null,
  pushSymbol: "",
  pushReconnectTimer: null,
  pushRefreshTimer: null,
  analyticsRefreshTimer: null,
  analyticsRefreshInFlight: false,
  pendingAnalyticsRefresh: null,
  analyticsRefreshEventId: "",
  pushVisibilityHandler: null,
  pendingGammaRefresh: null,
  pendingSnapshotRefresh: false,
  analysisRefreshSymbol: null,
  analysisRefreshTimer: null,
  analysisBlockUntil: 0,
  chainFetchedAt: null,
  chainFetchedSymbol: "",
  chainFetchedExpiration: "",
  displayedChainSymbol: "",
  displayedChainExpiration: "",
  displayedChainFetchedAt: null,
  displayedLevelsContext: "",
  levelsKey: "",
  levelsPayload: null,
  // 最近一次服务端综合结果；服务端快照更新优先改用客户端 raw 计算时保留复杂字段。
  serverLevelsPayload: null,
  serverLevelsContext: "",
  serverLevelsRequestKey: "",
  serverLevelsPayloadKey: "",
  serverLevelsInFlightContext: "",
  serverLevelsInFlightKey: "",
  serverLevelsCooldownContext: "",
  serverLevelsCooldownUntil: 0,
  // 同一快照的实时价/盘后价综合结果分别缓存；切换口径时先复用已有结果，后台再补齐新口径。
  serverLevelsCache: new Map(),
  // 买方结构按快照和基准价缓存，避免 renderLevels/renderAnalysis/raw 重复反解 IV。
  clientBuyerCache: new Map(),
  buyerContext: "",
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
  knownETFSymbols: new Set(),
  trendMarket: null,
  trendMarketSymbol: "",
  accessKey: "",
  storageAvailable: false,
  view: {
    chan: OptionScopeChan.empty(),
    marketState: "等待数据",
    themeLabel: "白天",
    themeIcon: "el-icon-moon-night",
    quoteSymbol: defaultSymbol,
    quotePrice: "--",
    quoteChange: "涨跌 --",
    quoteChangeColor: "var(--muted)",
    quoteCurrency: "USD",
    quoteMarket: "--",
    quoteReference: "昨收 --",
    quoteReferenceTitle: "涨跌幅以昨收为基准",
    fairValue: "--",
    fairValueSource: "保守情景 -- · 乐观情景 --",
    fairValueTitle: "估值区间来自不同假设的情景模型，具体方法见估值卡片说明",
    fairDefensive: "--",
    fairDefensiveSource: "等待估值数据",
    fairDefensiveTitle: "保守情景估值",
    fairOptimistic: "--",
    fairOptimisticSource: "等待估值数据",
    fairOptimisticTitle: "乐观情景估值",
    totalCount: "--",
    callVolume: "--",
    putVolume: "--",
    callInterest: "未平仓 --",
    putInterest: "未平仓 --",
    flow: {
      available: false,
      signalLabel: "等待相邻快照",
      signalClass: "waiting",
      signalDetail: "需要两份相邻期权链快照才能估算流向",
      windowLabel: "快照窗口 --",
      methodLabel: "成交量增量 · 买卖价方向估算 · 中性单列",
      warning: "",
      summary: {
        totalVolume: "--",
        buyVolume: "--",
        sellVolume: "--",
        neutralVolume: "--",
      },
      call: {
        buyVolume: "--",
        sellVolume: "--",
        unknownVolume: "--",
        buyPremium: "权利金 --",
        sellPremium: "权利金 --",
        netVolume: "--",
        focusTitle: "集中执行价 --",
        focusSummary: "等待相邻快照",
        concentration: [],
        rows: [{ key: "call-empty", label: "--", direction: "等待数据", className: "unknown", volume: "--", share: "--", premium: "权利金 --" }],
        rowspan: 1,
      },
      put: {
        buyVolume: "--",
        sellVolume: "--",
        unknownVolume: "--",
        buyPremium: "权利金 --",
        sellPremium: "权利金 --",
        netVolume: "--",
        focusTitle: "集中执行价 --",
        focusSummary: "等待相邻快照",
        concentration: [],
        rows: [{ key: "put-empty", label: "--", direction: "等待数据", className: "unknown", volume: "--", share: "--", premium: "权利金 --" }],
        rowspan: 1,
      },
    },
    chainTitle: "期权链 · 选择到期日查看",
    dataSource: "尚未加载",
    fetchedAt: "快照时间 --",
    chainHeatNote: "等待数据",
    chainRows: [],
    chainEmpty: "输入标的并载入数据",
    chart: {
      netGex: "净 Gamma 0 · 估算",
      gammaFlip: "零 Gamma 0.00 · 估算",
      netGexTitle: GAMMA_ESTIMATE_TITLE,
      gammaFlipTitle: GAMMA_ESTIMATE_TITLE,
      callWall: "看涨墙 0.00",
      putWall: "看跌墙 0.00",
      gammaScopeDate: "柱状图 到期日 --",
      gammaScopeStats: "0 个合约",
      levelsBasis: "基准 0.00",
      volumeScope: "到期日 -- · 0 个合约",
      oiScope: "到期日 -- · 0 个合约",
    },
    levels: placeholderLevels(),
    buyer: {
      available: false,
      directionLabel: "",
      horizonLabel: "未来 5 个交易日",
      target: "",
      items: [],
      reason: "等待数据",
      note: "",
    },
    trend: placeholderTrend(),
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
const analysisWindowLastStartedAt = new Map();
// 最近一次已应用到页面的 Gamma 窗口时间；冷却期内只读任务状态，完成后立即更新图表。
const analysisWindowLastAppliedAt = new Map();
// SSE 生命周期驱动服务端市场时段调度；浏览器只接收变化通知，不运行轮询器。
const SNAPSHOT_FRESH_SECONDS = 15;
let expirationMenuOpen = false;
// 同一轮、同一到期日的快照读取共用一个 Promise，避免刷新和切换叠在一起时把 quote/chain/gamma 各打两遍。
let snapshotReadCache = null;
// 强化色由后端综合模型分数与历史回踩表现判定；严格识别的期权墙可在样本不足时标为重点位。
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

// Keep the last usable page snapshot outside the request lifecycle. An API
// outage must not turn an already rendered quote/chain into zero placeholders;
// the snapshot is also persisted for a normal page reload while the service is
// restarting.
const CLIENT_SNAPSHOT_STORAGE_KEY = "option-scope:client-snapshots:v1";
const CLIENT_SNAPSHOT_LIMIT = 6;
const clientSnapshotCache = new Map();
let clientSnapshotCacheLoaded = false;

function cloneClientSnapshot(value) {
  try {
    return JSON.parse(JSON.stringify(value));
  } catch (error) {
    return null;
  }
}

function clientSnapshotKey(symbol, expiration) {
  return `${String(symbol || "").toUpperCase()}|${expiration || ""}`;
}

function loadClientSnapshotCache() {
  if (clientSnapshotCacheLoaded) return;
  clientSnapshotCacheLoaded = true;
  try {
    const raw = sessionStorage.getItem(CLIENT_SNAPSHOT_STORAGE_KEY);
    const saved = raw ? JSON.parse(raw) : {};
    Object.entries(saved || {}).forEach(([key, value]) => {
      if (value && value.symbol && value.expiration && value.view && value.payload?.data?.length) {
        clientSnapshotCache.set(key, value);
      }
    });
  } catch (error) {
    // Private browsing and disabled storage are valid; the in-memory cache
    // still protects a page while the API process is being restarted.
  }
}

function persistClientSnapshotCache() {
  const values = [...clientSnapshotCache.entries()].slice(-CLIENT_SNAPSHOT_LIMIT);
  // Option chains can be large enough to exceed a browser's session storage
  // quota. Persist the newest subset that fits instead of abandoning reload
  // recovery for every cached symbol.
  for (let start = 0; start < values.length; start += 1) {
    try {
      sessionStorage.setItem(CLIENT_SNAPSHOT_STORAGE_KEY, JSON.stringify(Object.fromEntries(values.slice(start))));
      return;
    } catch (error) {
      // Retry with one fewer (oldest) snapshot.
    }
  }
  try { sessionStorage.removeItem(CLIENT_SNAPSHOT_STORAGE_KEY); } catch (error) { /* storage disabled */ }
}

function hasRenderableClientSnapshot() {
  return Boolean(
    state.symbol
    && state.expiration
    && state.chainFetchedExpiration === state.expiration
    && state.chainRows?.length
    && state.view.chainRows?.length
    && state.lastQuote?.symbol === state.symbol
    && quoteIsReady(state.lastQuote)
  );
}

// Keep the snapshot currently visible on screen even while the selector is
// pointing at a different expiration. `state.expiration` changes before the
// request starts, so it cannot identify the still-valid rows by itself.
function displayedClientSnapshotRef() {
  const symbol = state.chainFetchedSymbol || (state.lastQuote?.symbol === state.symbol ? state.symbol : "");
  const expiration = state.chainFetchedExpiration || "";
  if (!symbol || symbol !== state.symbol || !expiration) return null;
  if (!state.chainRows?.length || !state.view.chainRows?.length || !quoteIsReady(state.lastQuote)) return null;
  return { symbol, expiration };
}

function restoreDisplayedClientSnapshot(snapshot, message = "API 暂不可用", unavailable = true) {
  if (!snapshot || snapshot.symbol !== state.symbol) return false;
  if (!restoreClientSnapshot(snapshot.symbol, snapshot.expiration, true)) return false;
  if (unavailable) {
    markApiUnavailable(message);
  } else {
    state.view.lastStatus = `${message}，继续显示本地缓存`;
    setError("");
  }
  syncPageQuery();
  return true;
}

function rememberClientSnapshot() {
  if (!hasRenderableClientSnapshot()) return;
  loadClientSnapshotCache();
  const payload = {
    symbol: state.symbol,
    expiration: state.expiration,
    quote: cloneClientSnapshot(state.lastQuote),
    payload: cloneClientSnapshot({
      symbol: state.chainFetchedSymbol || state.symbol,
      expiration: state.chainFetchedExpiration || state.expiration,
      fetched_at: state.chainFetchedAt,
      source: "client-cache",
      data: state.chainRows,
    }),
    view: cloneClientSnapshot(state.view),
    expirationOptions: cloneClientSnapshot(state.expirationOptions || []),
    chainSpot: state.chainSpot,
    chainFetchedAt: state.chainFetchedAt,
    chainFetchedSymbol: state.chainFetchedSymbol,
    chainFetchedExpiration: state.chainFetchedExpiration,
    analysisReady: Boolean(state.analysisReady),
    savedAt: Date.now(),
  };
  if (!payload.quote || !payload.payload || !payload.view) return;
  // Chan is independent of option expiration and refreshes from its own API.
  delete payload.view.chan;
  const key = clientSnapshotKey(state.symbol, state.expiration);
  clientSnapshotCache.delete(key);
  clientSnapshotCache.set(key, payload);
  while (clientSnapshotCache.size > CLIENT_SNAPSHOT_LIMIT) {
    clientSnapshotCache.delete(clientSnapshotCache.keys().next().value);
  }
  persistClientSnapshotCache();
}

function restoreClientSnapshot(symbol, expiration = "", allowStale = false) {
  loadClientSnapshotCache();
  const exactKey = clientSnapshotKey(symbol, expiration);
  let cached = expiration ? clientSnapshotCache.get(exactKey) : null;
  if (!cached && !expiration) {
    const candidates = [...clientSnapshotCache.values()]
      .filter((item) => item.symbol === String(symbol || "").toUpperCase() && item.payload?.data?.length)
      .sort((a, b) => Number(b.savedAt || 0) - Number(a.savedAt || 0));
    cached = candidates[0] || null;
  }
  if (!cached || !cached.payload?.data?.length || !cached.view) return false;
  const quote = cloneClientSnapshot(cached.quote);
  const payload = cloneClientSnapshot(cached.payload);
  const view = cloneClientSnapshot(cached.view);
  if (!quote || !payload || !view) return false;
  // A persisted page view is only a startup shortcut while both market data
  // snapshots are recent. Stale data remains available to explicit outage
  // recovery paths, where the UI labels it as a local cache.
  if (!allowStale && (!quoteIsFresh(quote) || !snapshotTimestampIsFresh(payload.fetched_at))) return false;
  delete view.chan;
  state.expiration = cached.expiration;
  state.expirationOptions = cloneClientSnapshot(cached.expirationOptions || []);
  state.lastQuote = quote;
  state.chainRows = Object.freeze(payload.data);
  state.chainSpot = cached.chainSpot ?? quote.price ?? null;
  state.chainFetchedAt = cached.chainFetchedAt || payload.fetched_at || null;
  state.chainFetchedSymbol = cached.chainFetchedSymbol || symbol;
  state.chainFetchedExpiration = cached.chainFetchedExpiration || cached.expiration;
  state.displayedChainSymbol = symbol;
  state.displayedChainExpiration = cached.expiration || state.expiration;
  state.displayedChainFetchedAt = payload.fetched_at || state.chainFetchedAt || null;
  state.displayedLevelsContext = [symbol, state.expiration, state.levelBasisMode].join("|");
  state.analysisReady = Boolean(cached.analysisReady);
  Object.assign(state.view, view);
  if (quoteIsETF(quote)) applyETFValuationDefaults();
  state.lastAnalysis = null;
  // The persisted view is authoritative while offline. Keep the raw rows in
  // state so a later successful response can replace it without a blank pass.
  if (payload.data?.length) {
    state.lastAnalysis = {
      rows: payload.data,
      spot: state.chainSpot,
      analysisPayload: null,
      expirationRows: payload.data,
      ivModel: {},
      basis: activeBasis(quote),
      points: aggregateByStrike(payload.data, state.chainSpot),
    };
  }
  // Force the first successful API response after recovery to repaint charts
  // and analysis rather than being skipped by a stale render signature.
  chainRenderSignature = "";
  chainBodySignatureValue = "";
  analysisChartSignatureValue = "";
  return true;
}

function markApiUnavailable(message) {
  if (!hasRenderableClientSnapshot()) {
    if (!restoreClientSnapshot(state.symbol, state.expiration, true)) return false;
  }
  const detail = message || "API 暂不可用";
  state.view.lastStatus = `${detail}，保留本地缓存，等待 API 恢复`;
  const source = String(state.view.dataSource || "本地缓存");
  state.view.dataSource = source.includes("API 暂不可用") ? source : `${source} · API 暂不可用`;
  setError("");
  rememberClientSnapshot();
  return true;
}

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

// 主题：默认白天模式，用户可在右上角切换到黑夜；偏好写入 localStorage，刷新后保持。
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
  applyTheme(stored === "dark" ? "dark" : "light");
}

// 手动选择优先于默认值；跨标的、刷新页面及重新打开页面均保留。
function readFoldState(storageKey, defaultExpanded) {
  if (!storageKey) return defaultExpanded;
  try {
    const stored = localStorage.getItem(storageKey);
    if (stored === "1") return true;
    if (stored === "0") return false;
  } catch (error) { /* 浏览器禁用存储时使用默认值。 */ }
  // Migrate choices saved by earlier versions, which used tab-scoped storage.
  try {
    const stored = sessionStorage.getItem(storageKey);
    if (stored === "1" || stored === "0") {
      try { localStorage.setItem(storageKey, stored); } catch (error) { /* Keep the session value when persistent storage is unavailable. */ }
      return stored === "1";
    }
  } catch (error) { /* Browser storage is unavailable; use the default. */ }
  return defaultExpanded;
}

function rememberFoldState(storageKey, expanded) {
  if (!storageKey) return;
  try { localStorage.setItem(storageKey, expanded ? "1" : "0"); } catch (error) { /* 存储不可用时保留当前页面状态。 */ }
}

// 折叠组通用逻辑：内容体用 hidden 控制显隐（[hidden] 在 flex/grid 上下文里会被覆盖，样式里补了 [hidden]{display:none}），
// 按钮同步 aria-expanded 与「展开/收起」文案，并在 localStorage 中保存状态。
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
  apply(readFoldState(storageKey, defaultExpanded));
  header.addEventListener("click", (event) => {
    if (shouldIgnore && shouldIgnore(event)) return;
    const expanded = body.hidden;
    apply(expanded);
    rememberFoldState(storageKey, expanded);
  });
}

// 分析详情折叠组：默认展开，使用原存储键恢复已有的手动选择。
const DETAIL_KEY = "option-scope-detail-v2";

function initDetailGroup() {
  bindFoldGroup({
    headerId: "detail-header",
    toggleId: "detail-toggle",
    bodyId: "detail-body",
    actionId: "detail-action",
    storageKey: DETAIL_KEY,
    defaultExpanded: true,
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

// 期权链折叠组：表格与热力图例可折叠、默认收起；类型筛选放在折叠区里，收起时一并隐藏。
const CHAIN_KEY = "option-scope-chain";

function initChainGroup() {
  bindFoldGroup({
    headerId: "chain-header",
    toggleId: "chain-toggle",
    bodyId: "chain-fold",
    actionId: "chain-action",
    storageKey: CHAIN_KEY,
    defaultExpanded: false,
    // 标题行右侧是数据来源与快照时间，点它不折叠。
    shouldIgnore: (event) => Boolean(closestElement(event.target, ".panel-status")),
  });
}

// 图表折叠组：默认展开，位置变化不影响已保存的手动选择。
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

// 期权流向与买方结构共用一个默认折叠的分组，内部按钮只切换视图，不折叠外层面板。
const OPTION_ANALYSIS_KEY = "option-scope-option-analysis-v2";

function initOptionAnalysisGroup() {
  bindFoldGroup({
    headerId: "option-analysis-header",
    toggleId: "option-analysis-toggle",
    bodyId: "option-analysis-fold",
    actionId: "option-analysis-action",
    storageKey: OPTION_ANALYSIS_KEY,
    defaultExpanded: false,
    // 面板折叠时隐藏视图切换按钮，避免折叠标题栏仍显示内部操作控件。
    onChange: (expanded) => {
      const tabs = byId("option-analysis-tabs");
      if (tabs) tabs.hidden = !expanded;
    },
    shouldIgnore: (event) => {
      const tab = closestElement(event.target, "[data-option-view]");
      if (tab) {
        setOptionAnalysisView(tab.dataset.optionView);
        return true;
      }
      return Boolean(closestElement(event.target, "#option-analysis-tabs"));
    },
  });
  document.querySelectorAll("[data-option-view]").forEach((tab) => {
    tab.addEventListener("click", (event) => {
      event.preventDefault();
      event.stopPropagation();
      setOptionAnalysisView(tab.dataset.optionView);
    });
  });
  setOptionAnalysisView("flow");
}

function setOptionAnalysisView(view) {
  const next = view === "buyer" ? "buyer" : "flow";
  document.querySelectorAll("[data-option-view-panel]").forEach((panel) => {
    panel.hidden = panel.dataset.optionViewPanel !== next;
  });
  document.querySelectorAll("[data-option-view]").forEach((tab) => {
    tab.setAttribute("aria-pressed", tab.dataset.optionView === next ? "true" : "false");
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
// 快照年龄按服务端时钟计算。本机钟走得快时，不能提前把还新鲜的快照当成过期。
function syncedNow() {
  return window.OptionScopeRequest && typeof OptionScopeRequest.serverNow === "function"
    ? OptionScopeRequest.serverNow()
    : Date.now();
}

// 快照年龄（秒）：时间戳缺失或无法解析返回 null，时钟偏差导致的负值按 0 处理。
function snapshotAgeSeconds(fetchedAt) {
  if (!fetchedAt) return null;
  const time = new Date(fetchedAt).getTime();
  return Number.isNaN(time) ? null : Math.max((syncedNow() - time) / 1000, 0);
}

function snapshotTimestampIsFresh(fetchedAt, maxAgeSeconds = SNAPSHOT_FRESH_SECONDS) {
  const age = snapshotAgeSeconds(fetchedAt);
  return age !== null && age <= maxAgeSeconds;
}

function isTransientRequestError(error) {
  const status = Number(error?.status) || 0;
  return error?.timeout === true || status === 0 || status === 408 || status === 425
    || status === 429 || (status >= 500 && status < 600);
}

function quoteIsFresh(quote, maxAgeSeconds = SNAPSHOT_FRESH_SECONDS) {
  return quoteIsReady(quote) && snapshotTimestampIsFresh(quote?.fetched_at, maxAgeSeconds);
}

function clearQuoteRetry() {
  if (state.quoteRetryTimer) clearTimeout(state.quoteRetryTimer);
  state.quoteRetryTimer = null;
  state.quoteRetryAttempts = 0;
}

function clearQuoteRecovery(resetAttempts = true) {
  if (state.quoteRecoveryTimer) clearTimeout(state.quoteRecoveryTimer);
  state.quoteRecoveryTimer = null;
  state.quoteRecoveryKey = "";
  state.quoteRecoveryInFlightKey = "";
  if (resetAttempts) state.quoteRecoveryAttempts = 0;
}

function clearSnapshotRetry(resetAttempts = true) {
  if (state.snapshotRetryTimer) clearTimeout(state.snapshotRetryTimer);
  state.snapshotRetryTimer = null;
  if (resetAttempts) state.snapshotRetryAttempts = 0;
}

function scheduleSnapshotRetry(loadId = state.loadId, minimumDelay = 0) {
  if (!state.symbol || loadId !== state.loadId || state.snapshotRetryTimer) return;
    const delay = Math.min(300000, Math.max(minimumDelay, Math.min(300000, 5000 * (2 ** Math.min(state.snapshotRetryAttempts, 6)))));
  state.snapshotRetryAttempts = Math.min(20, state.snapshotRetryAttempts + 1);
  state.snapshotRetryTimer = setTimeout(() => {
    state.snapshotRetryTimer = null;
    if (loadId !== state.loadId || !state.symbol) return;
    if (document.hidden) {
      scheduleSnapshotRetry(loadId);
      return;
    }
    if (state.refreshInFlight || state.loading) {
      state.snapshotRetryTimer = setTimeout(() => {
        state.snapshotRetryTimer = null;
        scheduleSnapshotRetry(loadId);
      }, 5000);
      return;
    }
    if (state.expirationPlaceholder === "正在获取到期日…") {
      refreshInBackground(loadId);
      return;
    }
    if (!state.expiration) {
      if (state.expirationPlaceholder === "无期权缓存") return;
      refreshInBackground(loadId);
      return;
    }
    refreshInBackground(loadId);
  }, delay);
}

function quoteRecoveryThresholdSeconds(quote) {
  switch (String(quote?.market_state || "").toUpperCase()) {
    case "REGULAR": return 90;
    case "PRE": case "POST": case "OVERNIGHT": return 300;
    default: return 1200;
  }
}

function scheduleQuoteRecovery(quote = state.lastQuote, minimumDelay = 0) {
  const symbol = state.symbol;
  if (!symbol || quote?.symbol !== symbol || !quoteIsReady(quote)) {
    clearQuoteRecovery();
    return;
  }
  const key = `${state.loadId}|${symbol}|${quote.fetched_at || ""}`;
  if (state.quoteRecoveryInFlightKey) return;
  if (state.quoteRecoveryKey === key && state.quoteRecoveryTimer) return;
  clearTimeout(state.quoteRecoveryTimer);
  state.quoteRecoveryKey = key;
  const fetchedAt = Date.parse(quote.fetched_at || "");
  const thresholdMs = quoteRecoveryThresholdSeconds(quote) * 1000;
  const ageMs = Number.isFinite(fetchedAt) ? Math.max(0, syncedNow() - fetchedAt) : thresholdMs;
  const delay = Math.max(minimumDelay, ageMs >= thresholdMs
    ? Math.min(300000, 5000 * (2 ** Math.min(state.quoteRecoveryAttempts, 6)))
    : thresholdMs - ageMs);
  state.quoteRecoveryTimer = setTimeout(() => {
    state.quoteRecoveryTimer = null;
    if (key !== `${state.loadId}|${state.symbol}|${state.lastQuote?.fetched_at || ""}`) return;
    if (document.hidden) {
      state.quoteRecoveryKey = "";
      scheduleQuoteRecovery(quote);
      return;
    }
    refreshStaleQuote(symbol, state.loadId, key);
  }, delay);
}

function refreshStaleQuote(symbol, loadId, key) {
  if (state.quoteRecoveryInFlightKey || symbol !== state.symbol || loadId !== state.loadId) return;
  const quote = state.lastQuote;
  const fetchedAt = Date.parse(quote?.fetched_at || "");
  if (!quoteIsReady(quote) || quote.symbol !== symbol
    || (Number.isFinite(fetchedAt) && syncedNow() - fetchedAt < quoteRecoveryThresholdSeconds(quote) * 1000)) {
    state.quoteRecoveryAttempts = 0;
    scheduleQuoteRecovery(quote);
    return;
  }
  state.quoteRecoveryInFlightKey = key;
  state.quoteRecoveryAttempts += 1;
  let retryAfterFailure = true;
  request(`/api/quote/${encodeURIComponent(symbol)}?refresh=true`, { timeout: 20000 }).then((incoming) => {
    if (symbol !== state.symbol || loadId !== state.loadId) return;
    if (quoteIsFresh(incoming) && incoming?.symbol === symbol) {
      state.quoteRecoveryAttempts = 0;
      renderQuote(incoming);
      return;
    }
    state.quoteRecoveryKey = "";
    scheduleQuoteRecovery(state.lastQuote);
  }).catch((error) => {
    if (symbol !== state.symbol || loadId !== state.loadId) return;
    if (!isTransientRequestError(error)) {
      retryAfterFailure = false;
      state.quoteRecoveryKey = "";
      return;
    }
    state.quoteRecoveryKey = "";
    scheduleQuoteRecovery(state.lastQuote, error.retryAfterMs || 0);
  }).finally(() => {
    if (state.quoteRecoveryInFlightKey === key) state.quoteRecoveryInFlightKey = "";
    if (retryAfterFailure && !state.quoteRecoveryTimer && symbol === state.symbol && loadId === state.loadId) {
      state.quoteRecoveryKey = "";
      scheduleQuoteRecovery(state.lastQuote);
    }
  });
}

// A new symbol can briefly have no durable quote while the snapshot worker is
// still fetching upstream. Keep checking the lightweight quote endpoint until
// the first usable quote arrives; the SSE push remains the fast path.
function scheduleQuoteRetry(symbol, loadId) {
  if (!symbol || symbol !== state.symbol || loadId !== state.loadId
    || (state.lastQuote?.symbol === symbol && quoteIsReady(state.lastQuote))
    || state.quoteRetryTimer) return;
  const delay = Math.min(60000, 1000 * (2 ** Math.min(state.quoteRetryAttempts, 6)));
  state.quoteRetryAttempts += 1;
  state.quoteRetryTimer = setTimeout(() => {
    state.quoteRetryTimer = null;
    fetchQuoteForDisplay(symbol, loadId);
  }, delay);
}

function fetchQuoteForDisplay(symbol, loadId, allowStaleValuation = false) {
  const requestKey = `${loadId}|${symbol}`;
  if (!symbol || symbol !== state.symbol || loadId !== state.loadId || state.quoteRetryInFlightKey === requestKey) return;
  state.quoteRetryInFlightKey = requestKey;
  request(`/api/quote/${encodeURIComponent(symbol)}`).then((quote) => {
    if (symbol !== state.symbol || loadId !== state.loadId) return;
    const usableQuote = latestRenderableQuote(symbol, quote, state.lastQuote);
    if (usableQuote) renderQuote(usableQuote, allowStaleValuation, quoteIsUsableSQLiteSnapshot(usableQuote, symbol));
    if (!(state.lastQuote?.symbol === symbol && quoteIsReady(state.lastQuote))) scheduleQuoteRetry(symbol, loadId);
  }).catch(() => {
    scheduleQuoteRetry(symbol, loadId);
  }).finally(() => {
    if (state.quoteRetryInFlightKey === requestKey) state.quoteRetryInFlightKey = "";
  });
}

// The live snapshot cadence varies by session: regular hours refresh every
// 20 seconds, extended hours every minute, and closed markets every 5 minutes.
// Allow three expected refresh intervals before marking the displayed quote
// as cached so the label does not flash between normal refreshes.
function quoteCacheBadgeIsStale(quote) {
  const maxAgeBySession = { REGULAR: 60, PRE: 180, POST: 180, OVERNIGHT: 180, CLOSED: 900 };
  const maxAge = maxAgeBySession[quote?.market_state] || 900;
  return !snapshotTimestampIsFresh(quote?.fetched_at, maxAge);
}

function quoteIsUsableSQLiteSnapshot(quote, symbol = state.symbol) {
  const fetchedAt = quote?.fetched_at ? new Date(quote.fetched_at).getTime() : NaN;
  return quote?.source === "sqlite"
    && quoteIsReady(quote)
    && (!quote.symbol || quote.symbol === symbol)
    && Number.isFinite(fetchedAt);
}

function latestFreshQuote(symbol, ...quotes) {
  return quotes
    .filter((quote) => quoteIsFresh(quote) && (!quote.symbol || quote.symbol === symbol))
    .sort((left, right) => new Date(right.fetched_at).getTime() - new Date(left.fetched_at).getTime())[0] || null;
}

function latestRenderableQuote(symbol, ...quotes) {
  return latestFreshQuote(symbol, ...quotes)
    || quotes.filter((quote) => quoteIsUsableSQLiteSnapshot(quote, symbol))
      .sort((left, right) => new Date(right.fetched_at).getTime() - new Date(left.fetched_at).getTime())[0]
    || null;
}

function quoteAndChainAreAligned(quote, fetchedAt, allowStaleSQLite = false) {
  const cachedPair = allowStaleSQLite && quoteIsUsableSQLiteSnapshot(quote);
  if ((!cachedPair && !quoteIsFresh(quote)) || !fetchedAt) return false;
  if (!cachedPair && !snapshotTimestampIsFresh(fetchedAt)) return false;
  const quoteAt = new Date(quote.fetched_at).getTime();
  const chainAt = new Date(fetchedAt).getTime();
  if (!Number.isFinite(quoteAt) || !Number.isFinite(chainAt)) return false;
  // SQLite quote and option-chain batches are stored independently. A quote
  // refresh can be newer than the latest cached chain by much more than 15s;
  // both are still useful as a first paint while the coordinated refresh runs.
  if (cachedPair) return true;
  return Math.abs(quoteAt - chainAt) <= SNAPSHOT_FRESH_SECONDS * 1000;
}

function analysisRefreshBlocking(now = Date.now()) {
  return state.analysisBlockUntil > now;
}

function refreshWorkPending() {
  return Boolean(state.loading || state.refreshInFlight || analysisRefreshBlocking());
}

function setExpirationMenuOpen(open) {
  expirationMenuOpen = Boolean(open);
  if (!open) {
    if (state.pendingSnapshotRefresh) setTimeout(applyPendingSnapshotRefresh, 0);
    if (state.pendingGammaRefresh) setTimeout(applyPushedGammaUpdate, 0);
  }
}

function applyPendingSnapshotRefresh() {
  if (!state.pendingSnapshotRefresh || document.hidden || refreshWorkPending()) return;
  state.pendingSnapshotRefresh = false;
  const previousSnapshot = displayedClientSnapshotRef();
  state.chainFetchedAt = null;
  state.chainFetchedSymbol = "";
  loadChain({ loadId: state.loadId, refresh: false, fallbackSnapshot: previousSnapshot });
}

// Live snapshot frames may already contain the quote and selected chain that
// the refresh worker committed. Render them immediately; REST remains the
// fallback for replayed/legacy events that carry only an invalidation notice.
function applyPushedSnapshotPayload(message) {
  const payload = message?.payload || {};
  const quote = payload.quote;
  const chain = payload.chain;
  if (!quote || !chain?.data?.length || message.symbol !== state.symbol) return false;
  if (chain.expiration && state.expiration && chain.expiration !== state.expiration) return false;
  const loadId = state.loadId;
  if (!quoteAndChainAreAligned(quote, chain.fetched_at)) return false;
  applyCachedQuote(quote);
  renderQuote(quote);
  renderChain(chain, quote, state.lastAnalysis?.analysisPayload || null);
  if (chain.expiration && !state.expirationOptions.length) {
    // The quote and first chain can arrive before /api/expirations returns.
    // Show the selected expiry immediately, then fill the full menu from the
    // catalogue the refresh worker just persisted.
    applyExpirations([chain.expiration], chain.expiration);
    request(`/api/expirations/${encodeURIComponent(state.symbol)}`).then((catalog) => {
      if (loadId !== state.loadId || state.symbol !== message.symbol) return;
      const dates = [...new Set([...(catalog?.expirations || []), ...(catalog?.cached_expirations || [])])].sort();
      if (dates.length) applyExpirations(dates, state.expiration);
    }).catch(() => {});
  }
  if (!state.analysisReady && chain.data?.length) {
    // The first snapshot can arrive on SSE before expiry discovery completes.
    // Start the slow Gamma window only after this selected chain is visible.
    refreshAnalysisWindow(loadId, chain, quote);
  }
  state.view.lastStatus = `最近更新 ${formatTime(chain.fetched_at)}`;
  syncPageQuery();
  // Flow requires two adjacent snapshots and is intentionally kept as the
  // only small REST read on this push path.
  if (state.expiration) {
    request(`/api/flow/${encodeURIComponent(state.symbol)}?expiration=${encodeURIComponent(state.expiration)}`)
      .then((flow) => {
        if (loadId === state.loadId && state.symbol === message.symbol) renderOptionFlow(flow);
      }).catch(() => {});
  }
  return true;
}

async function applyPushedGammaUpdate() {
  const pending = state.pendingGammaRefresh;
  if (!pending || pending.symbol !== state.symbol || pending.loadId !== state.loadId || refreshWorkPending()) return;
  state.pendingGammaRefresh = null;
  const symbol = pending.symbol;
  const encodedSymbol = encodeURIComponent(symbol);
  const [analysis, chain] = await Promise.all([
    request(`/api/gamma/${encodedSymbol}?horizon_days=45&include_rows=false`).catch(() => null),
    state.expiration ? request(`/api/chain/${encodedSymbol}?expiration=${encodeURIComponent(state.expiration)}`).catch(() => null) : Promise.resolve(null),
  ]);
  if (symbol !== state.symbol || pending.loadId !== state.loadId) return;
  const payload = chain?.data?.length ? chain : state.lastAnalysis?.payload;
  if (payload?.data?.length && gammaProfileReady(analysis)) {
    state.analysisReady = true;
    renderChain(payload, state.lastQuote, analysis);
  }
}

function closePushStream() {
  if (state.pushReconnectTimer) clearTimeout(state.pushReconnectTimer);
  if (state.pushRefreshTimer) clearTimeout(state.pushRefreshTimer);
  state.pushReconnectTimer = null;
  state.pushRefreshTimer = null;
  if (state.pushVisibilityHandler) document.removeEventListener("visibilitychange", state.pushVisibilityHandler);
  state.pushVisibilityHandler = null;
  if (state.pushSource) state.pushSource.close();
  state.pushSource = null;
  state.pushSymbol = "";
}

function queuePushedAnalyticsRefresh(symbol, loadId, eventId, historyFetchedAt = null) {
  if (symbol !== state.symbol || loadId !== state.loadId) return;
  if (eventId && eventId === state.analyticsRefreshEventId) return;
  if (eventId) state.analyticsRefreshEventId = eventId;
  const adjustedCurrent = Boolean(historyFetchedAt)
    && state.chanPayload?.symbol === symbol
    && state.chanPayload?.history?.fetched_at === historyFetchedAt;
  if (!state.chanExpanded) {
    // Chart structures are lazy-loaded on expand; only the derived cards need
    // this event. Mark the chart stale so expanding fetches the new history.
    state.chanStale = true;
    refreshChanMarketAnalysis(true);
  } else if (adjustedCurrent) {
    // The adjusted structures for this history batch are already rendered;
    // refresh only the derived market cards/levels instead of downloading the
    // 1.5 MiB adjusted payload again.
    refreshChanMarketAnalysis(true);
  } else {
    refreshChanAnalysis(true);
  }
  state.pendingAnalyticsRefresh = { symbol, loadId, eventId, attempts: 0 };
  schedulePushedAnalyticsRefresh();
}

function schedulePushedAnalyticsRefresh() {
  if (!state.pendingAnalyticsRefresh || state.analyticsRefreshTimer || state.analyticsRefreshInFlight) return;
  const pending = state.pendingAnalyticsRefresh;
  const delay = pending.attempts
    ? Math.min(30000, 1000 * (2 ** Math.min(pending.attempts - 1, 5)))
    : 100;
  state.analyticsRefreshTimer = setTimeout(() => {
    state.analyticsRefreshTimer = null;
    const current = state.pendingAnalyticsRefresh;
    state.pendingAnalyticsRefresh = null;
    if (!current || current.symbol !== state.symbol || current.loadId !== state.loadId) return;
    state.analyticsRefreshInFlight = true;
    const encodedSymbol = encodeURIComponent(current.symbol);
    request(`/api/quote/${encodedSymbol}`).then((quote) => {
      if (current.symbol !== state.symbol || current.loadId !== state.loadId || quote?.symbol !== current.symbol) return;
      quote = latestFreshQuote(current.symbol, quote, state.lastQuote);
      if (!quote) return;
      applyCachedQuote(quote);
      renderQuote(quote);
      const points = state.lastAnalysis?.points;
      const expiration = state.expiration;
      const spot = Number(quote?.price ?? state.lastAnalysis?.basis?.price ?? state.lastAnalysis?.spot);
      if (!Array.isArray(points) || !points.length || !expiration || !Number.isFinite(spot) || spot <= 0) {
        // The SSE notice can arrive before the initial chain analysis is ready.
        // renderChain will resume this pending invalidation once its points exist.
        state.pendingAnalyticsRefresh = current;
        return;
      }

      // Analytics completion invalidates the current factor result even when
      // quote, expiry, and option snapshot are unchanged. Give this fetch a
      // distinct client key so an older in-flight response cannot win the race.
      const key = `${current.symbol}|${expiration}|${state.chainFetchedAt || ""}|${spotCacheBucket(spot)}|${state.levelBasisMode}|analytics:${current.eventId || Date.now()}`;
      const context = `${current.symbol}|${expiration}|${state.levelBasisMode}`;
      state.levelsKey = key;
      state.levelsPayload = null;
      state.serverLevelsPayload = null;
      state.serverLevelsRequestKey = "";
      state.serverLevelsPayloadKey = "";
      state.serverLevelsInFlightContext = "";
      state.serverLevelsInFlightKey = "";
      state.serverLevelsCache.delete(key);
      state.serverLevelsCooldownContext = "";
      state.serverLevelsCooldownUntil = 0;
      clearTimeout(state.levelsRetryTimer);
      state.levelsRetryTimer = null;
      requestFactorLevels(points, spot, key);
      state.serverLevelsContext = context;
    }).catch(() => {
      // Do not consume an analytics SSE event when the API is temporarily
      // unavailable. Keep it queued and retry with bounded backoff.
      if (current.symbol === state.symbol && current.loadId === state.loadId) {
        current.attempts = (current.attempts || 0) + 1;
        state.pendingAnalyticsRefresh = current;
      }
    }).finally(() => {
      state.analyticsRefreshInFlight = false;
      if (state.pendingAnalyticsRefresh && state.lastAnalysis?.points?.length) schedulePushedAnalyticsRefresh();
    });
  }, delay);
}

function connectPushStream(symbol = state.symbol) {
  if (!window.EventSource || !symbol || accessKeyRequired() && !currentAccessKey()) return;
  const subscriptionKey = `${symbol}|${state.expiration || ""}`;
  if (state.pushSource && state.pushSymbol === subscriptionKey) return;
  closePushStream();
  const params = new URLSearchParams();
  if (state.expiration) params.set("expiration", state.expiration);
  const eventPath = `/api/events/${encodeURIComponent(symbol)}${params.size ? `?${params}` : ""}`;
  const path = withAccessKey(eventPath, currentAccessKey());
  const source = new EventSource(path);
  state.pushSource = source;
  state.pushSymbol = subscriptionKey;
  const onUpdate = (event) => {
    let message = {};
    try { message = JSON.parse(event.data || "{}"); } catch (error) { return; }
    if (message.symbol !== state.symbol || symbol !== state.symbol) return;
    if (message.kind === "fair_value" || message.kind === "quote") {
      const pushedQuote = message.payload?.quote;
      if (pushedQuote?.symbol === symbol) {
        applyCachedQuote(pushedQuote);
        renderQuote(pushedQuote, message.kind === "fair_value");
        return;
      }
      const allowStaleValuation = message.kind === "fair_value";
      const loadId = state.loadId;
    request(`/api/quote/${encodeURIComponent(symbol)}`).then((quote) => {
        if (symbol === state.symbol && loadId === state.loadId && quote?.symbol === symbol) renderQuote(quote, allowStaleValuation);
      }).catch(() => {});
      return;
    }
    if (message.kind === "gamma") {
      state.pendingGammaRefresh = { symbol, loadId: state.loadId };
      if (!state.analysisRefreshSymbol && !refreshWorkPending()) applyPushedGammaUpdate();
      return;
    }
    if (message.kind === "analytics") {
      queuePushedAnalyticsRefresh(symbol, state.loadId, event.lastEventId || message.created_at || "", message.payload?.history_fetched_at || null);
      return;
    }
    if (message.kind !== "snapshot") return;
    if (applyPushedSnapshotPayload(message)) {
      state.pendingSnapshotRefresh = false;
      return;
    }
    state.pendingSnapshotRefresh = true;
    if (state.pushRefreshTimer) clearTimeout(state.pushRefreshTimer);
    // The worker that owns the refresh already wrote SQLite; read its result without another upstream fetch.
    state.pushRefreshTimer = setTimeout(() => {
      state.pushRefreshTimer = null;
      if (symbol !== state.symbol) return;
      applyPendingSnapshotRefresh();
    }, 250);
  };
  source.addEventListener("update", onUpdate);
  source.addEventListener("ready", () => {
    // A fresh SSE connection starts at the newest event instead of replaying
    // older events. Read the lightweight quote once so a fair_value event that
    // completed just before subscription cannot leave the valuation cards pending.
    const currentQuote = state.lastQuote;
    if (symbol === state.symbol
      && (currentQuote?.symbol !== symbol
        || currentQuote.fair_value == null
        || currentQuote.fair_value_status === "pending"
        || currentQuote.fair_value_status === "retry")) {
      const loadId = state.loadId;
      request(`/api/quote/${encodeURIComponent(symbol)}`).then((quote) => {
        if (symbol === state.symbol && loadId === state.loadId && quote?.symbol === symbol) renderQuote(quote, true);
      }).catch(() => {});
    }
    if (symbol === state.symbol && !document.hidden && !state.loading && !state.refreshInFlight) {
      const previousSnapshot = displayedClientSnapshotRef();
      state.chainFetchedAt = null;
      state.chainFetchedSymbol = "";
      loadChain({ loadId: state.loadId, fallbackSnapshot: previousSnapshot });
    }
  });
  state.pushVisibilityHandler = () => {
    if (!document.hidden && symbol === state.symbol && state.pushSource === source) {
      const previousSnapshot = displayedClientSnapshotRef();
      state.chainFetchedAt = null;
      state.chainFetchedSymbol = "";
      loadChain({ loadId: state.loadId, refresh: false, fallbackSnapshot: previousSnapshot });
    }
  };
  document.addEventListener("visibilitychange", state.pushVisibilityHandler);
  source.addEventListener("error", () => {
    if (state.pushSource !== source) return;
    // EventSource performs native reconnect; fallback REST refresh only while disconnected for extended periods.
    if (!state.pushReconnectTimer) state.pushReconnectTimer = setTimeout(() => {
      state.pushReconnectTimer = null;
      if (symbol === state.symbol && source.readyState === EventSource.CLOSED) {
        closePushStream();
        connectPushStream(symbol);
        if (!state.loading && !state.refreshInFlight) loadChain({ loadId: state.loadId });
      }
    }, 30000);
  });
}

function scheduleAutoRefresh() {
  if (document.body.classList.contains("access-denied-page")) return;
  connectPushStream(state.symbol);
  // Automatic snapshot refresh is server-side and active only while this SSE subscription exists.
  if (!expirationMenuOpen && !refreshWorkPending()) {
    if (state.pendingSnapshotRefresh) setTimeout(applyPendingSnapshotRefresh, 0);
    if (state.pendingGammaRefresh) setTimeout(applyPushedGammaUpdate, 0);
  }
}
// Intl.NumberFormat 构造开销远高于 format；期权链一次渲染会调用数千次，
// 按小数位数复用实例，避免每行都新建格式化器。
const numberFormatters = new Map();
function formatNumber(value, digits = 0) {
  if (value === null || value === undefined || value === "") return "--";
  let formatter = numberFormatters.get(digits);
  if (!formatter) {
    formatter = new Intl.NumberFormat("en-US", { maximumFractionDigits: digits });
    numberFormatters.set(digits, formatter);
  }
  return formatter.format(Number(value));
}
function moneyDecimals(value) {
  const number = Math.abs(Number(value));
  if (!Number.isFinite(number)) return 2;
  if (number >= 1) return 2;
  if (number >= 0.1) return 3;
  if (number >= 0.01) return 4;
  if (number >= 0.001) return 5;
  if (number >= 0.0001) return 6;
  if (number >= 0.00001) return 7;
  return 8;
}
function formatMoney(value) {
  if (value == null) return "--";
  const number = Number(value);
  if (!Number.isFinite(number)) return "--";
  const fixed = number.toFixed(moneyDecimals(number));
  if (!fixed.includes(".")) return fixed;
  const compact = fixed.replace(/(\.\d*?[1-9])0+$/, "$1").replace(/\.0+$/, "");
  return compact.includes(".") ? compact : `${compact}.00`;
}
// 买卖报价允许为 0（例如深度价外合约），只有缺失或非数字时显示占位符。
function formatOptionQuote(value) {
  if (value === null || value === undefined || value === "") return "--";
  const number = Number(value);
  return Number.isFinite(number) && number >= 0 ? number.toFixed(2) : "--";
}
function formatTime(value) { if (!value) return "--"; const date = new Date(value); return Number.isNaN(date.getTime()) ? value : date.toLocaleString("zh-CN", { hour12: false }); }
function formatFlowInterval(value) {
  const seconds = Number(value);
  if (!Number.isFinite(seconds) || seconds < 0) return "相邻快照";
  if (seconds >= 3600) return `相隔 ${Math.floor(seconds / 3600)} 小时`;
  if (seconds >= 60) return `相隔 ${Math.floor(seconds / 60)} 分钟`;
  return `相隔 ${Math.max(1, Math.round(seconds))} 秒`;
}
function formatFlowStrike(value) {
  const strike = Number(value);
  if (!Number.isFinite(strike)) return "--";
  return Number.isInteger(strike) ? String(strike) : strike.toFixed(2).replace(/0+$/, "").replace(/\.$/, "");
}
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
// 期权流向成交量带上方向：主动买入为正号，主动卖出为负号，中性成交不添加符号。
// 这里仅改变展示值，后端的买入/卖出/中性原始数量与净量计算保持不变。
function formatFlowVolume(value, direction) {
  const number = Number(value);
  if (!Number.isFinite(number)) return "--";
  const text = formatCount(Math.abs(number));
  if (direction === "buy") return `+${text}`;
  if (direction === "sell") return `-${text}`;
  return text;
}
function formatUsd(value) {
  if (value == null || !Number.isFinite(Number(value))) return "--";
  return `$${Number(value).toLocaleString("en-US", { minimumFractionDigits: 2, maximumFractionDigits: 2 })}`;
}
// 图表数值统一入口：GEX 走中文金额单位，成交量/持仓量走中文计数单位。
function formatChartValue(value, digits, unit) { return unit === "M" ? formatGex(value, digits) : formatCount(value, digits); }
function setBusy(busy) { state.loading = busy; }

function aggregateByStrike(rows, spot) {
  const grouped = new Map();
  const spotValue = Number(spot) || 0;
  const spotSquared = spotValue * spotValue;
  const spotKey = gammaSpotBucket(spotValue);
  for (const row of rows) {
    const strike = Number(row.strike);
    if (!Number.isFinite(strike)) continue;
    if (!grouped.has(strike)) grouped.set(strike, { strike, callVolume: 0, putVolume: 0, callOi: 0, putOi: 0, callGex: 0, putGex: 0 });
    const item = grouped.get(strike);
    const volume = Number(row.volume) || 0;
    const oi = Number(row.open_interest) || 0;
    const recalculatedGamma = gammaAtSpot(spotValue, row, undefined, spotKey);
    const gamma = recalculatedGamma == null ? (Number(row.gamma) || 0) : recalculatedGamma;
    const gex = gamma * oi * 100 * spotSquared * 0.01 / 1000000;
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

// Gamma 只需按细粒度现价档位复用：行情每跳动一分钱就重算整条链会让主线程持续繁忙。
// 0.05% 的档位对热力图和 GEX 列不可见，却能把同一轮渲染的重复计算全部命中缓存。
let gammaBucketSpot = NaN;
let gammaBucketKey = "";
function gammaSpotBucket(value) {
  const price = Number(value);
  if (!Number.isFinite(price) || price <= 0) return "";
  if (price === gammaBucketSpot) return gammaBucketKey;
  const magnitude = 10 ** Math.floor(Math.log10(price));
  const step = magnitude / 2000;
  gammaBucketSpot = price;
  gammaBucketKey = String(Math.round(price / step));
  return gammaBucketKey;
}

function gammaAtSpot(spot, row, expiration, spotKey) {
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
  const bucket = spotKey !== undefined ? spotKey : gammaSpotBucket(spotValue);
  const cacheKey = `${expiryKey}|${strike}|${row.contract_type || ""}|${volatility}|${bucket}|${timeBucket}`;
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
// 用它们当基准会把关键位算偏。盘后价只用「当前盘后」或「上一交易日盘后」，绝不回退盘前价/夜盘价：
// 后端在盘前、夜盘和休市时会从 Yahoo 1 分钟盘前盘后数据补出 sessions.previous_post；
// 若上一交易日确实没有盘后数据，退回该交易日收盘价，也不能拿盘前价冒充盘后价。
function levelBasis(quote, fallbackPrice) {
  const sessions = quote?.sessions || {};
  for (const key of ["post", "previous_post"]) {
    const price = Number(sessions[key]?.price);
    if (Number.isFinite(price) && price > 0) {
      const asOf = typeof sessions[key]?.date === "string"
        ? sessions[key].date
        : (typeof sessions[key]?.as_of === "string" ? sessions[key].as_of.slice(0, 10) : "");
      return { price, label: "盘后", asOf };
    }
  }
  // 盘中 sessions.post.reference_close 就是当前常规价，不能当成「昨收」；
  // 盘中仍沿用旧口径回退实时价，非盘中才用最近一个已完成交易日的收盘价兜底。
  if (quote?.market_state !== "REGULAR") {
    const reference = Number(sessions.post?.reference_close ?? quote?.previous_close);
    if (Number.isFinite(reference) && reference > 0) {
      const asOf = typeof sessions.post?.date === "string"
        ? sessions.post.date
        : (typeof sessions.post?.as_of === "string" ? sessions.post.as_of.slice(0, 10) : "");
      return { price: reference, label: "昨收", asOf };
    }
  }
  return { price: Number(fallbackPrice), label: "常规", asOf: "" };
}

// 基准价开关的两个口径：live 实时价（默认）、close 盘后价（沿用旧口径）；常量用于遍历同步按钮状态。
const BASIS_MODES = { live: "实时价", close: "盘后价" };

// 实时价取当前生效的时段价格：盘中为最新成交价，盘前/盘后取该时段价格；
// 夜盘没有可用的 Yahoo 夜盘价时改用最近一次正常交易日收盘价，避免把常规价误标成夜盘实时价。
function activeBasis(quote) {
  if (state.levelBasisMode === "close") return levelBasis(quote, quote?.price);
  // Alpaca 夜盘价是当前有效的夜盘参考；没有 Alpaca 时才回退最近一次正式收盘。
  if (quote?.market_state === "OVERNIGHT") {
    const close = overnightCloseQuote(quote);
    if (close) {
      const label = quote?.sessions?.overnight?.provider === "alpaca-overnight" ? "夜盘" : "收盘";
      return { price: close.price, label };
    }
  }
  const price = Number(activeSessionQuote(quote)?.price);
  return Number.isFinite(price) && price > 0 ? { price, label: "实时" } : levelBasis(quote, quote?.price);
}

// 切换基准价口径：同步开关的按下状态，再用已有快照重算压力位/支撑位（不额外请求上游接口）。
// 支撑位/压力位表、交易计划与压力位/支撑位柱状图都由这一次重绘一起更新。
function applyBasisMode(mode) {
  const previousMode = state.levelBasisMode;
  state.levelBasisMode = mode === "close" ? "close" : "live";
  if (state.levelBasisMode === previousMode) return;
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
  // 图表输入整体替换、不原地修改；冻结顶层避免 Vue 深观测几十兆的行情数组。
  state.lastAnalysis = Object.freeze({ ...last, basis });
  loadFactorLevels(last.points || [], basis.price);
}

// 压力位/支撑位：在现价上下各取 LEVEL_COUNT 个期权持仓最集中的行权价。
// 与 Gamma 敞口图同口径：有未平仓量时按 GEX 排序，盘前整链无持仓量时退回成交量分布。
function pickLevels(points, spot, side, valueOf, count = LEVEL_COUNT) {
  const minGap = spot * 0.005;
  // 先算一次权重再排序；原实现把 valueOf 放进比较器，排序时会重复调用。
  const candidates = [];
  for (const point of points) {
    const value = valueOf(point);
    if (!(value > 0)) continue;
    if (side === "above" ? point.strike < spot : point.strike > spot) continue;
    candidates.push({ point, value });
  }
  candidates.sort((left, right) => right.value - left.value);
  const picked = [];
  for (const candidate of candidates) {
    if (picked.length >= count) break;
    // 与已选价位过近的行权价跳过，避免 5 条挤在同一段价位上。
    if (picked.some((item) => Math.abs(item.point.strike - candidate.point.strike) < minGap)) continue;
    picked.push(candidate);
  }
  picked.sort((left, right) => Math.abs(left.point.strike - spot) - Math.abs(right.point.strike - spot));
  return picked.map((item) => item.point);
}

// 把单因子价位转换为 Vue 行数据（行权价 / 距现价 / 排序口径数值），离现价近的排在前面。
function levelScore(level) {
  const score = Number(level?.score);
  return Number.isFinite(score) ? Math.min(1, Math.max(0, score)) : 0;
}

function levelFactors(level) {
  if (!Array.isArray(level?.factors)) return [];
  return level.factors.map((factor) => {
    const text = String(factor);
    const match = text.match(/^斐波那契\s+([+-]?\d+(?:\.\d+)?)%$/);
    return match ? `斐波那契 ${formatPercentValue(Number(match[1]))}` : text;
  });
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

function commitLevelViews(next, preserveEmpty = false) {
  const context = [state.symbol, state.expiration, state.levelBasisMode].join("|");
  const sameContext = state.displayedLevelsContext === context;
  const previous = state.view.levels;
  const usableRows = (rows) => (Array.isArray(rows) ? rows : []).filter((row) =>
    row && !row.placeholder && Number.isFinite(Number(row.price)) && Number(row.price) > 0);
  for (const key of ["resistance", "support", "add"]) {
    if (!Array.isArray(next[key])) continue;
    const incoming = usableRows(next[key]);
    const keepPrevious = preserveEmpty && sameContext && incoming.length === 0 && usableRows(previous[key]).length > 0;
    state.view.levels[key] = keepPrevious ? previous[key] : incoming;
  }
  state.displayedLevelsContext = context;
  return state.view.levels;
}

function sameDisplayedLevelContext() {
  return state.displayedLevelsContext === [state.symbol, state.expiration, state.levelBasisMode].join("|");
}

function renderCommittedLevelsChart(spot) {
  const chartRows = (rows) => (rows || []).filter((row) =>
    row && !row.placeholder && Number.isFinite(Number(row.price)) && Number(row.price) > 0)
    .map((row) => ({
      price: Number(row.price),
      score: Number(row.score) || 0,
      probability: row.probability,
      factors: Array.isArray(row.factors) ? row.factors : String(row.factors || "").split(" · "),
    }));
  OptionScopeCharts.renderLevelsChart({
    spot,
    resistance: chartRows(state.view.levels.resistance),
    support: chartRows(state.view.levels.support),
  });
}

// 单因子回退时把选出的行权价转成柱状图口径：综合强度按本侧最大值归一（与多因子接口一致）。
function fallbackLevelSeries(picked, valueOf, metricLabel) {
  const peak = Math.max(0, ...picked.map(valueOf)) || 1;
  return picked.map((point) => ({ price: point.strike, score: valueOf(point) / peak, factors: [metricLabel], probability: null }));
}

function renderLevels(points, spot) {
  const price = Number(spot);
  // 刷新或切换基准价时先保留同一标的/期限的旧结构，避免整块闪回等待状态。
  // 没有旧结构时才显示轻量等待提示；raw/服务端结果到达后会立即覆盖。
  const previousBuyer = state.view.buyer;
  const currentBuyerContext = `${state.symbol}|${state.expiration}|${state.levelBasisMode}`;
  if (state.buyerContext !== currentBuyerContext || !previousBuyer?.available || !previousBuyer.items?.length) {
    state.view.buyer = {
      available: false,
      directionLabel: "",
      horizonLabel: "未来 5 个交易日",
      target: "",
      items: [],
      reason: "等待期权报价…",
      note: "",
    };
  } else {
    state.view.buyer = { ...previousBuyer, note: `${previousBuyer.note || ""}${previousBuyer.note ? " · " : ""}正在更新报价` };
  }
  state.view.chart.levelsBasis = Number.isFinite(price) && price > 0 ? `基准 ${formatMoney(price)}` : "基准 --";
  if (!points.length || !Number.isFinite(price) || price <= 0) {
    // SSE refresh temporarily clears fetched-snapshot validation fields, even
    // though valid rows for this selected symbol/expiry/basis remain visible.
    const preserveDisplayedLevels = sameDisplayedLevelContext() || displayedSnapshotContextMatches();
    if (!preserveDisplayedLevels) OptionScopeCharts.renderLevelsChart(null);
    const hasChanAnalysis = renderCurrentChanMarketAnalysis(price, preserveDisplayedLevels);
    if (!preserveDisplayedLevels && !hasChanAnalysis) {
      state.view.levels.resistance = [];
      state.view.levels.support = [];
      state.view.levels.add = [];
      state.view.levels.placeholder = false;
      state.view.levels.resistanceNote = `现价上方持仓最集中的 ${LEVEL_COUNT} 个价位`;
      state.view.levels.supportNote = `现价下方持仓最集中的 ${LEVEL_COUNT} 个价位`;
      state.view.levels.resistanceEmpty = "暂无数据";
      state.view.levels.supportEmpty = "暂无数据";
      state.view.levels.addEmpty = "暂无数据";
      if (state.trendHistorySymbol !== state.symbol) renderTrend(null);
      renderPlan(null, price);
    }
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
  const keepCurrentLevels = sameDisplayedLevelContext();
  const levelUpdates = {
    resistance: buildSimpleLevelViews(resistance, price, "resistance", callValue, formatValue, metricLabel),
    support: buildSimpleLevelViews(support, price, "support", putValue, formatValue, metricLabel),
  };
  // A new expiry/basis starts a new result set. Do not carry the prior add
  // prices into that context if the fallback has no add candidates.
  if (!keepCurrentLevels) levelUpdates.add = [];
  commitLevelViews(levelUpdates, true);
  state.view.levels.placeholder = false;
  state.view.levels.addEmpty = state.view.levels.add.length ? "" : "暂无多因子加仓数据";
  // 单因子回退时柱状图按同一批行权价绘制，综合强度按本侧最大值归一。
  const resistanceSeries = fallbackLevelSeries(resistance, callValue, metricLabel);
  const supportSeries = fallbackLevelSeries(support, putValue, metricLabel);
  if (resistance.length || support.length || !keepCurrentLevels) {
    renderCommittedLevelsChart(price);
  }
  // 回退口径没有日线历史，趋势通道留空；买卖点先展示期权候选的快速估算。
  const hasChanAnalysis = renderCurrentChanMarketAnalysis(price, true);
  if (!hasChanAnalysis) {
    const provisionalTradePoints = clientTradePoints(state.view.levels.support, state.view.levels.resistance, price);
    if (provisionalTradePoints.buy || provisionalTradePoints.sell) {
      renderTrend(null, null, price, null, null, provisionalTradePoints);
    } else {
      renderTrend(null);
    }
  }
  if (planSupport.length || !state.view.levels.add.some((level) => !level.placeholder)) {
    const planSupportSeries = fallbackLevelSeries(planSupport, putValue, metricLabel);
    const planSplit = Math.min(PLAN_COUNT, Math.ceil(planSupportSeries.length / 2));
    renderPlan({ add: planSupportSeries.slice(planSplit, planSplit + PLAN_COUNT) }, price);
  }
}

// 原始日线到达浏览器后，在本地完成不会产生副作用的计算。买方结构先使用报价回退，
// 服务端综合结果返回后再覆盖，避免低配服务器的计算延迟阻塞首屏。
function clientBars(payload) {
  return (payload?.bars || []).map((bar) => ({
    ...bar,
    high: Number(bar.high),
    low: Number(bar.low),
    close: Number(bar.close),
    volume: Number(bar.volume) || 0,
  })).filter((bar) => Number.isFinite(bar.high) && Number.isFinite(bar.low) && Number.isFinite(bar.close) && bar.close > 0);
}

function clientExtremes(bars) {
  if (!bars.length) return null;
  const highPoint = (items) => items.reduce((best, bar) => !best || bar.high > best.high ? bar : best, null);
  const lowPoint = (items) => items.reduce((best, bar) => !best || bar.low < best.low ? bar : best, null);
  const latestDate = String(bars[bars.length - 1].date || "").slice(0, 10);
  const cutoff = latestDate ? new Date(`${latestDate}T00:00:00Z`).getTime() - 365 * 86400000 : 0;
  const window = bars.filter((bar) => !cutoff || new Date(`${String(bar.date).slice(0, 10)}T00:00:00Z`).getTime() >= cutoff);
  const pack = (items) => {
    const high = highPoint(items); const low = lowPoint(items);
    return {
      high: high ? { price: high.high, date: String(high.date || "").slice(0, 10) } : null,
      low: low ? { price: low.low, date: String(low.date || "").slice(0, 10) } : null,
    };
  };
  return { week52: pack(window), all_time: pack(bars), window_days: 365, reference_date: latestDate, bars: bars.length, window_bars: window.length };
}

function clientRsi(values, period = 14) {
  if (values.length < period + 1) return null;
  let gain = 0; let loss = 0;
  for (let index = 1; index <= period; index += 1) {
    const delta = values[index] - values[index - 1];
    gain += Math.max(delta, 0); loss += Math.max(-delta, 0);
  }
  gain /= period; loss /= period;
  for (let index = period + 1; index < values.length; index += 1) {
    const delta = values[index] - values[index - 1];
    gain = (gain * (period - 1) + Math.max(delta, 0)) / period;
    loss = (loss * (period - 1) + Math.max(-delta, 0)) / period;
  }
  const value = gain === 0 && loss === 0 ? 50 : (loss === 0 ? 100 : 100 - 100 / (1 + gain / loss));
  const stateName = value >= 70 ? "overbought" : (value <= 30 ? "oversold" : "neutral");
  return { value: Number(value.toFixed(1)), period, state: stateName, label: stateName === "overbought" ? "超买" : (stateName === "oversold" ? "超卖" : "中性") };
}

function clientFitChannel(values) {
  if (values.length < 20) return null;
  const meanX = (values.length - 1) / 2;
  const meanY = values.reduce((sum, value) => sum + value, 0) / values.length;
  const variance = values.reduce((sum, _, index) => sum + (index - meanX) ** 2, 0);
  if (!(variance > 0) || !(meanY > 0)) return null;
  const slope = values.reduce((sum, value, index) => sum + (index - meanX) * (value - meanY), 0) / variance;
  const intercept = meanY - slope * meanX;
  const deviation = Math.sqrt(values.reduce((sum, value, index) => sum + (value - (intercept + slope * index)) ** 2, 0) / values.length);
  const fitted = intercept + slope * (values.length - 1);
  return { slope_percent: slope / meanY * 100, upper: Number((fitted + 1.5 * deviation).toFixed(4)), lower: Number((fitted - 1.5 * deviation).toFixed(4)), bars: values.length };
}

function clientTrend(bars) {
  const values = bars.map((bar) => bar.close).filter((value) => Number.isFinite(value) && value > 0);
  if (values.length < 20) return null;
  const longValues = values.slice(-60); const recentValues = values.slice(-20);
  const longFit = clientFitChannel(longValues); const recentFit = clientFitChannel(recentValues);
  if (!longFit || !recentFit) return null;
  const longDirection = longFit.slope_percent > 0.05 ? "up" : (longFit.slope_percent < -0.05 ? "down" : "range");
  const recentChange = recentValues[recentValues.length - 1] / recentValues[0] - 1;
  const rebound = recentValues[recentValues.length - 1] / Math.min(...recentValues) - 1;
  const pullback = Math.max(...recentValues) / recentValues[recentValues.length - 1] - 1;
  const recentUp = recentFit.slope_percent > 0.05; const recentDown = recentFit.slope_percent < -0.05;
  const reversalUp = recentUp && recentChange >= 0.05 && rebound >= 0.08;
  const reversalDown = recentDown && recentChange <= -0.05 && pullback >= 0.08;
  const selected = reversalUp && longDirection !== "up" ? recentFit : (reversalDown && longDirection !== "down" ? recentFit : longFit);
  const direction = reversalUp && longDirection !== "up" ? "up" : (reversalDown && longDirection !== "down" ? "down" : longDirection);
  const label = direction === "up" ? (selected === recentFit ? "反弹上行 · 上涨趋势" : "上行通道 · 上涨趋势") : (direction === "down" ? (selected === recentFit ? "短线转弱 · 下跌趋势" : "下行通道 · 下跌趋势") : "区间震荡 · 方向待定");
  return { direction, label, slope_percent: Number(selected.slope_percent.toFixed(3)), upper: selected.upper, lower: selected.lower, bars: selected.bars, background_direction: longDirection, reversal_confirmed: selected === recentFit, rsi: clientRsi(values) };
}

function clientTechnicalCandidates(bars, spot) {
  if (!bars.length || !(spot > 0)) return [];
  const window = bars.slice(-120); const high = Math.max(...window.map((bar) => bar.high)); const low = Math.min(...window.map((bar) => bar.low));
  const highIndex = window.map((bar) => bar.high).lastIndexOf(high); const lowIndex = window.map((bar) => bar.low).lastIndexOf(low);
  const result = [];
  if (high > low) {
    [[0.236, 0.6], [0.382, 0.9], [0.5, 1], [0.618, 1], [0.786, 0.6]].forEach(([ratio, score]) => {
      const price = lowIndex < highIndex ? high - (high - low) * ratio : low + (high - low) * ratio;
      result.push({ price, score, factors: [`斐波那契 ${formatPercentValue(ratio * 100)}`] });
    });
  }
  const weighted = window.filter((bar) => bar.volume > 0).reduce((sum, bar) => sum + bar.close * bar.volume, 0);
  const volume = window.reduce((sum, bar) => sum + Math.max(bar.volume, 0), 0);
  if (volume > 0 && Number.isFinite(weighted / volume)) result.push({ price: weighted / volume, score: 0.7, factors: ["筹码密集"] });
  for (let index = 2; index < window.length - 2; index += 1) {
    const bar = window[index];
    if (bar.low < spot && bar.low <= window[index - 1].low && bar.low <= window[index + 1].low) result.push({ price: bar.low, score: 0.62, factors: ["承接位"] });
  }
  return result;
}

function clientMergeLevels(points, bars, spot, side) {
  const openInterest = points.reduce((sum, point) => sum + point.callOi + point.putOi, 0);
  const volume = points.reduce((sum, point) => sum + point.callVolume + point.putVolume, 0);
  const byGex = openInterest > 0 && openInterest >= volume * 0.2;
  const valueOf = side === "resistance" ? (point) => byGex ? Math.max(point.callGex, 0) : point.callVolume : (point) => byGex ? Math.max(-point.putGex, 0) : point.putVolume;
  const metric = byGex ? "Gamma 敞口" : "成交量";
  const options = pickLevels(points, spot, side === "resistance" ? "above" : "below", valueOf, LEVEL_COUNT).map((point) => ({ price: point.strike, score: 0.65, factors: [metric] }));
  const technical = clientTechnicalCandidates(bars, spot).filter((item) => side === "resistance" ? item.price > spot : item.price < spot);
  const candidates = [...options, ...technical].filter((item) => Number.isFinite(item.price) && item.price > 0);
  const selected = [];
  for (const item of candidates.sort((a, b) => Math.abs(a.price - spot) - Math.abs(b.price - spot))) {
    if (selected.some((other) => Math.abs(other.price - item.price) < spot * 0.005)) {
      const existing = selected.find((other) => Math.abs(other.price - item.price) < spot * 0.005);
      existing.factors = [...new Set([...existing.factors, ...item.factors])]; existing.score = Math.min(1, existing.score + 0.08);
      continue;
    }
    selected.push({ ...item, zone_low: item.price * 0.9975, zone_high: item.price * 1.0025 });
    if (selected.length >= LEVEL_COUNT) break;
  }
  return { levels: selected, metric };
}

function serverLevelsHasCompleteFields(payload) {
  const levels = payload && Array.isArray(payload.resistance) && Array.isArray(payload.support)
    ? [...payload.resistance, ...payload.support]
    : [];
  return Boolean(
    payload
    && payload.trade_points
    && levels.some((level) => {
      const price = Number(level?.price ?? level?.strike);
      return Number.isFinite(price) && price > 0;
    })
    && levels.every((level) => Object.prototype.hasOwnProperty.call(level, "probability")
      && Object.prototype.hasOwnProperty.call(level, "strength_tier")),
  );
}

// The raw endpoint can return a fast levels shape before the selected
// expiration's IV model has been attached. A property containing `null` is
// structurally present but still renders as `--` for every touch probability.
// When raw data has a usable selected-expiry IV, require numeric probabilities
// before treating the fast payload as the final cached result.
function serverLevelsHasUsableProbability(payload, rawPayload = payload) {
  const levels = payload && Array.isArray(payload.resistance) && Array.isArray(payload.support)
    ? [...payload.resistance, ...payload.support]
    : [];
  if (!Array.isArray(payload?.resistance) || !Array.isArray(payload?.support)) return false;
  if (!levels.length) return true;
  const expiration = payload.expiration || state.expiration;
  const model = rawPayload?.iv_model?.[expiration];
  const volatility = Number(model?.iv);
  if (!Number.isFinite(volatility) || volatility <= 0) return true;
  return levels.every((level) => Number.isFinite(Number(level?.probability)));
}

function serverLevelsNeedsRetry(payload) {
  const rawPayload = state.levelsPayload?.raw ? state.levelsPayload : null;
  const warnings = [
    payload?.history?.warning,
    payload?.history?.extremes_warning,
    payload?.beta_meta?.warning,
  ].filter(Boolean).map(String);
  if (warnings.some((warning) => /不支持历史日线更新/.test(warning))) return false;
  const temporarilyBlocked = warnings.some((warning) => /内存保护|刷新正在进行|租约忙|等待超时|稍后重试/.test(warning));
  if (temporarilyBlocked) return true;
  if (rawPayload?.fast_levels
    && !hasUsableTrendHistory({ ...rawPayload, ...rawPayload.fast_levels })) return true;
  if (rawPayload && !serverLevelsHasUsableProbability(payload, rawPayload)) return true;
  if (!Array.isArray(payload?.resistance) || !Array.isArray(payload?.support) || !payload?.trade_points) return true;
  const levels = [...payload.resistance, ...payload.support];
  // Empty candidates can be a valid result for a quiet or illiquid chain.
  // Keep the client fallback instead of polling the same complete response.
  if (!levels.length) return false;
  return !serverLevelsHasCompleteFields(payload);
}

function hasUsableTrendHistory(payload) {
  const bars = Number(payload?.history?.bars || payload?.history_bars);
  const trend = payload?.trend;
  return bars > 0
    && Number.isFinite(Number(trend?.upper)) && Number(trend.upper) > 0
    && Number.isFinite(Number(trend?.lower)) && Number(trend.lower) > 0;
}

function levelsPayloadForBasis(payload, spot, staleBasis = false) {
  if (!payload) return null;
  return { ...payload, spot, _staleBasis: staleBasis };
}

function rememberServerLevels(key, payload) {
  if (!key || !serverLevelsHasCompleteFields(payload)) return;
  state.serverLevelsCache.set(key, payload);
  // 页面长期停留并切换多个期限时限制内存，保留最近的 12 份口径结果。
  while (state.serverLevelsCache.size > 12) {
    state.serverLevelsCache.delete(state.serverLevelsCache.keys().next().value);
  }
}

function clientTradePoints(support, resistance, spot) {
  const price = Number(spot);
  if (!Number.isFinite(price) || price <= 0) return { buy: null, sell: null };
  const provisional = (items, side) => {
    const candidates = (items || [])
      .filter((item) => Number.isFinite(Number(item?.price)) && Number(item.price) > 0)
      .map((item) => ({ ...item, distance: Math.abs(Number(item.price) - price) }))
      .sort((a, b) => (b.score || 0) - (a.score || 0) || a.distance - b.distance);
    const item = candidates[0];
    if (!item) return null;
    const score = Math.max(0, Math.min(1, Number(item.score) || 0));
    return {
      price: Number(item.price),
      zone_low: Number(item.zone_low) || Number(item.price) * 0.9975,
      zone_high: Number(item.zone_high) || Number(item.price) * 1.0025,
      confidence: score,
      model_confidence: score,
      history_samples: 0,
      factors: item.factors || [],
      reason: `客户端快速估算${side === "buy" ? "支撑" : "压力"}候选，服务端综合结果返回后更新`,
    };
  };
  return { buy: provisional(support, "buy"), sell: provisional(resistance, "sell") };
}

function renderClientRaw(payload, points, spot) {
  if (payload?.symbol && payload.symbol !== state.symbol) return;
  if (payload?.expiration && state.expiration && payload.expiration !== state.expiration) return;
  const serverPayload = state.serverLevelsPayload;
  const bars = clientBars(payload); const trend = clientTrend(bars); const extremes = payload?.extremes || clientExtremes(bars);
  const rawRows = Array.isArray(payload?.options) && payload.options.length ? payload.options : null;
  const optionPoints = rawRows ? aggregateByStrike(rawRows, Number(spot)) : points;
  const resistance = clientMergeLevels(optionPoints, bars, Number(spot), "resistance"); const support = clientMergeLevels(optionPoints, bars, Number(spot), "support");
  // raw 结果只包含浏览器可计算的基础价位。上一份同标的、同期限的服务端结果
  // 在新快照补全期间继续作为临时显示，避免概率、最佳买卖点和强化色闪回为空。
  const hasServerLevels = Boolean(serverLevelsHasCompleteFields(serverPayload)
    && serverPayload.expiration === state.expiration
    && state.serverLevelsContext === `${state.symbol}|${state.expiration}|${state.levelBasisMode}`);
  const displayedResistance = hasServerLevels ? serverPayload.resistance : resistance.levels;
  const displayedSupport = hasServerLevels ? serverPayload.support : support.levels;
  const displayedMetric = hasServerLevels
    ? (serverPayload.options_metric === "volume" ? "成交量" : "Gamma 敞口")
    : resistance.metric;
  const renderSide = (items, side, isAdd = false) => buildFactorViews(items, Number(spot), side, isAdd);
  state.levelsPayload = payload;
  state.view.chart.levelsBasis = Number.isFinite(Number(spot)) && Number(spot) > 0 ? `基准 ${formatMoney(Number(spot))}` : "基准 --";
  state.view.levels.placeholder = false;
  const addLevels = hasServerLevels && Array.isArray(serverPayload.plan?.add)
    ? serverPayload.plan.add
    : displayedSupport.slice(Math.ceil(displayedSupport.length / 2), Math.ceil(displayedSupport.length / 2) + PLAN_COUNT);
  const allSupport = displayedSupport.slice().sort((a, b) => a.price - b.price);
  const keepCurrentLevels = sameDisplayedLevelContext();
  commitLevelViews({
    resistance: renderSide(displayedResistance, "resistance"),
    support: renderSide(displayedSupport, "support"),
    add: renderSide(addLevels, "support", true),
  }, true);
  const serverNote = hasServerLevels
    ? (serverPayload?._staleBasis ? "服务端综合结果 · 正在更新基准价，暂用上一份口径" : "服务端综合结果 · 暂用上一份快照")
    : `客户端计算 · ${displayedMetric}`;
  state.view.levels.resistanceNote = serverNote;
  state.view.levels.supportNote = serverNote;
  state.view.levels.addEmpty = state.view.levels.add.length ? "" : (hasServerLevels ? "暂无可用价位" : "客户端暂无更深支撑");
  state.view.levels.resistanceEmpty = "现价这一侧暂无可用价位"; state.view.levels.supportEmpty = "现价这一侧暂无可用价位";
  const hasServerBuyer = Boolean(hasServerLevels && serverPayload?.buyer_structures?.available
    && Array.isArray(serverPayload.buyer_structures.items)
    && serverPayload.buyer_structures.items.length);
  if (hasServerBuyer) {
    renderBuyerStructures(serverPayload.buyer_structures, serverPayload.options_fetched_at || serverPayload.chain_fetched_at);
  } else {
    const sameServerContext = state.serverLevelsContext === `${state.symbol}|${state.expiration}|${state.levelBasisMode}`;
    const rawDirection = sameServerContext && serverPayload?.recommendation?.action === "buy"
      ? "call"
      : sameServerContext && serverPayload?.recommendation?.action === "sell"
        ? "put"
        : trend?.direction === "up"
          ? "call"
          : trend?.direction === "down"
            ? "put"
            : undefined;
    renderClientBuyerFallback(rawRows || payload?.options || [], Number(spot), displayedSupport, displayedResistance, rawDirection);
    if (!state.view.buyer.available) {
      state.view.buyer = {
        available: false,
        directionLabel: "",
        horizonLabel: "未来 5 个交易日",
        target: "",
        items: [],
        reason: "当前期权链缺少有效买卖报价",
        note: "基础趋势与价位已由客户端计算",
      };
    }
  }
  if (displayedResistance.length || displayedSupport.length || !keepCurrentLevels) {
    renderCommittedLevelsChart(spot);
  }
  const tradePointContext = `${state.symbol}|${state.expiration}|${state.levelBasisMode}`;
  const provisionalTradePoints = hasServerLevels
    ? serverPayload.trade_points
    : clientTradePoints(displayedSupport, displayedResistance, spot);
  const stableTradePoints = stabilizeTradePoints(provisionalTradePoints, tradePointContext);
  if ((hasServerLevels && serverPayload.trend) || !renderCurrentChanMarketAnalysis(spot, true)) {
    renderTrend(
      hasServerLevels && serverPayload.trend ? serverPayload.trend : trend,
      hasServerLevels && serverPayload.extremes ? serverPayload.extremes : extremes,
      spot,
      hasServerLevels && serverPayload.history ? serverPayload.history : payload.history,
      hasServerLevels ? serverPayload?.recommendation || null : null,
      stableTradePoints,
      serverPayload?.trade_points_horizon || null,
      payload.trend_market,
      payload.beta || (hasServerLevels ? serverPayload?.beta : null) || null,
      hasServerLevels ? serverPayload?.stop_loss || null : null,
      hasServerLevels ? (serverPayload?.support || []) : displayedSupport,
    );
  }
  if (hasServerLevels || state.chanPayload?.symbol !== state.symbol || !state.chanPayload?.market_analysis) {
    renderPlan({ add: addLevels }, spot);
  }
  refreshQuoteWithTrendClose();
  rememberClientSnapshot();
}

// 综合接口还在计算时，先用已经拿到的当前期限期权分布填充基础价位和图表，避免首屏整块留空。
// 综合结果回来后会覆盖这份临时结果；已有结果时保留旧值，避免刷新过程中闪回空状态。
function renderFactorFallback(points, spot) {
  // 0 值骨架也占着行数，但不能当成已经有综合结果，否则会跳过单因子回退。
  const levelRowsReady = !state.view.levels.placeholder && (
    state.view.levels.support.length
    || state.view.levels.resistance.length
    || state.view.levels.add.length
  );
  const hasVisibleLevels = Boolean(
    levelRowsReady
    || (state.view.trend.available && !state.view.trend.placeholder && !state.view.trend.partial)
  );
  if (!hasVisibleLevels) renderLevels(points, spot);
}

function requestServerFactorLevels(points, spot, key, attempt = 0) {
  const context = `${state.symbol}|${state.expiration}|${state.levelBasisMode}`;
  // 同一快照只补一次综合结果；新快照到来时允许更新，期间保留上一份
  // 同口径结果作为过渡，避免概率、最佳买卖点和强化色闪回为空。
  if (state.serverLevelsRequestKey === key
    || state.serverLevelsPayloadKey === key
    || state.serverLevelsInFlightContext === context
    || (state.serverLevelsCooldownContext === context && Date.now() < state.serverLevelsCooldownUntil)) return;
  // 先让正在进行的行情快照刷新完成，避免综合历史计算抢占首屏快照的重任务闸门。
  if (state.refreshInFlight === state.symbol) {
    if (!state.levelsRetryTimer) {
      state.levelsRetryTimer = setTimeout(() => {
        state.levelsRetryTimer = null;
        if (`${state.symbol}|${state.expiration}|${state.levelBasisMode}` !== context || !state.levelsKey) return;
        const latestSpot = Number(state.lastAnalysis?.basis?.price || state.lastAnalysis?.spot || spot);
        requestServerFactorLevels(state.lastAnalysis?.points || points, latestSpot, state.levelsKey, attempt);
      }, 1000);
    }
    return;
  }
  state.serverLevelsRequestKey = key;
  state.serverLevelsInFlightContext = context;
  state.serverLevelsInFlightKey = key;
  const encodedSymbol = encodeURIComponent(state.symbol);
  const encodedExpiration = encodeURIComponent(state.expiration);
  const numericSpot = Number(spot);
  const spotQuery = Number.isFinite(numericSpot) && numericSpot > 0
    ? `&spot=${encodeURIComponent(numericSpot)}`
    : "";
  request(`/api/levels/${encodedSymbol}?expiration=${encodedExpiration}${spotQuery}`)
    .then((payload) => {
      const currentContext = `${state.symbol}|${state.expiration}|${state.levelBasisMode}`;
      if (currentContext !== context || state.serverLevelsRequestKey !== key) {
        // A newer snapshot may have replaced this request. Clear only this
        // request's flight marker, then let the current snapshot start its
        // own request instead of leaving the context permanently locked.
        if (state.serverLevelsInFlightKey === key) {
          state.serverLevelsInFlightContext = "";
          state.serverLevelsInFlightKey = "";
          if (currentContext === context && state.levelsKey && state.levelsKey !== key) {
            const latestSpot = Number(state.lastAnalysis?.basis?.price || state.lastAnalysis?.spot || spot);
            requestServerFactorLevels(state.lastAnalysis?.points || points, latestSpot, state.levelsKey);
          }
        }
        return;
      }
      // 同一上下文可能在新快照到达后发起了更新请求；旧响应只能被丢弃，
      // 否则它会覆盖新结果并再次触发重复的综合计算。
      if (state.serverLevelsRequestKey !== key) return;
      const retry = serverLevelsNeedsRetry(payload);
      if (!retry) {
        state.serverLevelsPayload = payload;
        state.serverLevelsContext = context;
        state.serverLevelsPayloadKey = key;
        rememberServerLevels(key, payload);
      } else {
        // A partial response is useful only as a loading signal. Keep the last
        // complete same-context result (if any), and never make this payload
        // the cache hit that suppresses future attempts.
        state.serverLevelsPayloadKey = "";
      }
      state.serverLevelsInFlightContext = "";
      state.serverLevelsInFlightKey = "";
      if (retry) {
        // Resource protection can return HTTP 200 before selected-expiry IV,
        // history, or reinforcement metadata is ready. Retry with a bounded
        // backoff for as long as this snapshot remains selected.
        const delay = Math.max(Number(payload?.retry_after_ms) || 0, Math.min(300000, 3000 * (2 ** Math.min(attempt, 7))));
        state.serverLevelsRequestKey = "";
        state.serverLevelsCooldownContext = context;
        state.serverLevelsCooldownUntil = Date.now() + delay;
        clearTimeout(state.levelsRetryTimer);
        state.levelsRetryTimer = setTimeout(() => {
          state.levelsRetryTimer = null;
          if (`${state.symbol}|${state.expiration}|${state.levelBasisMode}` !== context || !state.levelsKey) return;
          state.serverLevelsCooldownUntil = 0;
          const latestSpot = Number(state.lastAnalysis?.basis?.price || state.lastAnalysis?.spot || spot);
          requestServerFactorLevels(state.lastAnalysis?.points || points, latestSpot, state.levelsKey, attempt + 1);
        }, delay);
      } else {
        state.serverLevelsCooldownContext = "";
        state.serverLevelsCooldownUntil = 0;
        state.serverLevelsRequestKey = key;
      }
      // 如果请求期间快照已更新，先保存结果，下一次 raw 重绘会复用；不强行覆盖最新现价。
      if (!retry && state.levelsKey === key) {
        state.levelsPayload = payload;
        renderFactorLevels(payload);
      } else if (state.levelsKey && state.levelsKey !== key
        && !retry
        && `${state.symbol}|${state.expiration}|${state.levelBasisMode}` === context) {
        // 当前快照在旧请求期间到达：旧请求完成后再串行补当前快照，避免两个综合计算并发。
        const latestSpot = Number(state.lastAnalysis?.basis?.price || state.lastAnalysis?.spot);
        requestServerFactorLevels(state.lastAnalysis?.points || [], latestSpot, state.levelsKey);
      }
    })
    .catch((error) => {
      // Leave the raw fallback visible and retry with capped backoff. A single
      // transient error must not disable strengthened levels for two minutes.
      const currentContext = `${state.symbol}|${state.expiration}|${state.levelBasisMode}`;
      if (!isTransientRequestError(error)) {
        state.serverLevelsRequestKey = "";
        state.serverLevelsInFlightContext = "";
        state.serverLevelsInFlightKey = "";
        markApiUnavailable(error.message || "API 暂不可用");
        return;
      }
      if (currentContext !== context || state.serverLevelsRequestKey !== key) {
        if (state.serverLevelsInFlightKey === key) {
          state.serverLevelsInFlightContext = "";
          state.serverLevelsInFlightKey = "";
          if (currentContext === context && state.levelsKey && state.levelsKey !== key) {
            const latestSpot = Number(state.lastAnalysis?.basis?.price || state.lastAnalysis?.spot || spot);
            requestServerFactorLevels(state.lastAnalysis?.points || points, latestSpot, state.levelsKey);
          }
        }
        return;
      }
      // A cached page can be kept visible while the service is restarting,
      // but the failed key must not remain marked as completed. Otherwise the
      // next attempt is rejected by the de-duplication guard forever.
      state.serverLevelsRequestKey = "";
      state.serverLevelsInFlightContext = "";
      state.serverLevelsInFlightKey = "";
      state.serverLevelsCooldownContext = context;
      const delay = Math.max(error?.retryAfterMs || 0, Math.min(300000, 3000 * (2 ** Math.min(attempt, 7))));
      state.serverLevelsCooldownUntil = Date.now() + delay;
      markApiUnavailable("API 暂不可用");
      clearTimeout(state.levelsRetryTimer);
      state.levelsRetryTimer = setTimeout(() => {
        state.levelsRetryTimer = null;
        if (`${state.symbol}|${state.expiration}|${state.levelBasisMode}` !== context || !state.levelsKey) return;
        state.serverLevelsCooldownUntil = 0;
        const latestSpot = Number(state.lastAnalysis?.basis?.price || state.lastAnalysis?.spot || spot);
        requestServerFactorLevels(state.lastAnalysis?.points || points, latestSpot, state.levelsKey, attempt + 1);
      }, delay);
    });
}

function requestFactorLevels(points, spot, key, attempt = 0) {
  const encodedSymbol = encodeURIComponent(state.symbol);
  const encodedExpiration = encodeURIComponent(state.expiration);
  const numericSpot = Number(spot);
  const spotQuery = Number.isFinite(numericSpot) && numericSpot > 0
    ? `&spot=${encodeURIComponent(numericSpot)}`
    : "";
  // 兼容旧版调用形式的检索锚点：request(`/api/levels/${encodedSymbol}?expiration=${encodedExpiration}${spotQuery}`)
  // 首选只读原始缓存。历史足够时由浏览器计算趋势、极值和基础价位；
  // 没有历史时先显示期权口径回退，再受控地异步请求服务端补全。
  request(`/api/levels/${encodedSymbol}?expiration=${encodedExpiration}${spotQuery}&raw=true`)
    .then((payload) => {
      if (state.levelsKey !== key) return;
      const rawBars = Array.isArray(payload?.bars) ? payload.bars.length : 0;
      const rawOptions = Array.isArray(payload?.options) ? payload.options.length : 0;
      // raw 只要带回期权行就先在浏览器计算；日线到达后再补趋势和历史因子。
      // 新标的常见情况是只有期权缓存、还没有日线缓存，不能因此跳过买方结构首屏回退。
	    if (payload?.raw && payload?.fast_levels) {
	      // Go 已经用同一批缓存输入完成趋势、强化等级、推荐、加仓价位和
	      // 买方结构计算。直接渲染服务端结果，避免浏览器先算一遍、随后再
	      // 等待完整 levels 请求覆盖临时结果。
      // Keep raw-only fields (bars/options/iv_model) alongside the server
      // decision fields, while letting the fast payload win for shared
      // metadata such as spot and generated_at.
      const fastPayload = { ...payload, ...payload.fast_levels, raw: true };
      const fastComplete = serverLevelsHasCompleteFields(fastPayload)
        && serverLevelsHasUsableProbability(fastPayload, payload)
        && hasUsableTrendHistory(fastPayload);
      const priorComplete = serverLevelsHasCompleteFields(state.serverLevelsPayload)
        && state.serverLevelsContext === `${state.symbol}|${state.expiration}|${state.levelBasisMode}`;
      if (fastComplete) {
        state.serverLevelsPayload = fastPayload;
        state.serverLevelsContext = `${state.symbol}|${state.expiration}|${state.levelBasisMode}`;
        state.serverLevelsPayloadKey = key;
        state.serverLevelsRequestKey = key;
        state.levelsPayload = fastPayload;
	        rememberServerLevels(key, fastPayload);
	        renderFactorLevels(fastPayload);
	      } else {
	        // History prefetch may still be in flight immediately after a new
	        // snapshot. Show the client fallback while the full server path fills
	        // the missing historical inputs.
        // Keep a complete same-context result available while the missing
        // probability fields are filled by the full endpoint. Do not let the
        // incomplete fast payload overwrite that fallback before rendering.
        if (!priorComplete) state.serverLevelsPayload = null;
        state.serverLevelsPayloadKey = "";
        state.serverLevelsRequestKey = "";
        state.levelsPayload = payload;
        renderClientRaw(payload, points, numericSpot);
	        requestServerFactorLevels(points, spot, key);
      }
      return;
    }
    if (payload?.raw && (rawBars > 0 || rawOptions > 0)) {
      state.levelsPayload = payload;
      renderClientRaw(payload, points, numericSpot);
      // 旧后端或缓存不足时继续使用完整接口回退。
      requestServerFactorLevels(points, spot, key);
      return;
    }
      // raw 模式没有日线时先显示期权口径的支撑/压力，再异步请求服务端补全。
      state.levelsPayload = payload?.raw ? payload : null;
      renderFactorFallback(points, numericSpot);
      if (payload?.raw) requestServerFactorLevels(points, spot, key);
    })
    .catch((error) => {
      if (state.levelsKey !== key) return;
      if (!isTransientRequestError(error)) {
        markApiUnavailable(error.message || "API 暂不可用");
        return;
      }
      // Keep the previous render, if any, and retry raw reads as the API
      // comes back. Without this path a failed raw request left the page on a
      // client fallback forever and the server result (including strength
      // tiers and probabilities) was never requested again.
      const context = `${state.symbol}|${state.expiration}|${state.levelBasisMode}`;
      const delay = Math.max(error?.retryAfterMs || 0, Math.min(300000, 3000 * (2 ** Math.min(attempt, 7))));
      markApiUnavailable("API 暂不可用");
      clearTimeout(state.levelsRetryTimer);
      state.levelsRetryTimer = setTimeout(() => {
        state.levelsRetryTimer = null;
        if (`${state.symbol}|${state.expiration}|${state.levelBasisMode}` !== context || state.levelsKey !== key) return;
        requestFactorLevels(state.lastAnalysis?.points || points, Number(state.lastAnalysis?.basis?.price || state.lastAnalysis?.spot || spot), key, attempt + 1);
      }, delay);
    });
  // raw 与综合接口都使用受控退避重试；同一快照不会并行发起重复请求。
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
  const hasSelectedChain = (state.chainFetchedSymbol === state.symbol && state.chainFetchedExpiration === state.expiration)
    || (state.displayedChainSymbol === state.symbol && state.displayedChainExpiration === state.expiration);
  // Chart resize/basis changes can fire while the selector already points at a
  // new expiry but lastAnalysis still belongs to the expiry that was just left.
  if (!hasSelectedChain) return;
  // 缓存键带上基准价口径：两个口径取到同一价格时（盘后/夜盘时段）也各自成键，切换必然重绘一次。
  // 现价按与后端相同的格子去重：格子内不再请求价位接口，跨出格子才用精确现价重算。
  // 不把 Gamma 窗口时间放进键。窗口完成后现价和选中期限都没变时，不再重算这组价位。
  const key = `${state.symbol}|${state.expiration}|${state.chainFetchedAt || ""}|${spotCacheBucket(spot)}|${state.levelBasisMode}`;
  // 同一快照已请求过：窗口尺寸变化时直接用缓存结果重绘，不再打接口。
  if (state.levelsKey === key) {
    if (state.levelsPayload?.raw) renderClientRaw(state.levelsPayload, points, spot);
    else if (state.levelsPayload) renderFactorLevels(state.levelsPayload);
    else renderLevels(points, spot);
    return;
  }
  const previousServerContext = state.serverLevelsContext;
  const previousRawPayload = state.levelsPayload?.raw
    && state.levelsPayload.symbol === state.symbol
    && state.levelsPayload.expiration === state.expiration
    ? state.levelsPayload
    : null;
  const previousContextParts = String(state.serverLevelsContext || "").split("|");
  const sameSymbolExpiration = previousContextParts[0] === state.symbol
    && previousContextParts[1] === state.expiration;
  const previousServerPayload = sameSymbolExpiration && serverLevelsHasCompleteFields(state.serverLevelsPayload)
    ? state.serverLevelsPayload
    : null;
  const cachedServerPayload = state.serverLevelsCache.get(key);
  state.levelsKey = key;
  const serverContext = `${state.symbol}|${state.expiration}|${state.levelBasisMode}`;
  const sameServerContext = previousServerContext === serverContext;
  state.serverLevelsContext = serverContext;
  if (!sameServerContext) {
    clearTimeout(state.levelsRetryTimer);
    state.levelsRetryTimer = null;
    state.serverLevelsInFlightContext = "";
    state.serverLevelsInFlightKey = "";
    state.serverLevelsRequestKey = "";
    state.serverLevelsPayloadKey = "";
    state.serverLevelsCooldownContext = "";
    state.serverLevelsCooldownUntil = 0;
  }
  if (cachedServerPayload) {
    state.serverLevelsPayload = cachedServerPayload;
    state.serverLevelsPayloadKey = key;
    state.levelsPayload = levelsPayloadForBasis(cachedServerPayload, spot);
    renderFactorLevels(state.levelsPayload);
    return;
  }
  if (previousServerPayload) {
    // 先显示上一口径的完整结果，避免最佳买卖点和强化色闪回空白；后台请求会替换它。
    const staleBasis = Boolean(previousServerPayload._staleBasis) || !sameServerContext;
    state.serverLevelsPayload = levelsPayloadForBasis(previousServerPayload, spot, staleBasis);
    state.serverLevelsPayloadKey = "";
    state.serverLevelsContext = serverContext;
    state.levelsPayload = levelsPayloadForBasis(previousServerPayload, spot, staleBasis);
    renderFactorLevels(state.levelsPayload);
    if (!staleBasis) {
      // 同一口径的新快照先沿用完整结果，再后台刷新当前快照的历史统计。
      requestServerFactorLevels(points, spot, key);
      return;
    }
    // 先读轻量 raw 快照，让趋势和极值跟上当前快照；raw 完成后再补一次综合结果。
    requestFactorLevels(points, spot, key);
    return;
  }
  if (previousRawPayload) {
    // raw 快照已经在浏览器中，切换口径直接重算临时价位，不再重复读取接口。
    state.levelsPayload = previousRawPayload;
    renderClientRaw(previousRawPayload, points, spot);
    requestServerFactorLevels(points, spot, key);
    return;
  }
  state.levelsPayload = null;
  renderFactorFallback(points, spot);
  requestFactorLevels(points, spot, key);
}

// 渲染多因子结果：价位 / 距现价 / 综合依据（组成该价位的因子标签）。
// 触及概率：0~1 的概率值转百分比，极小/极大用不等号，缺数据用占位符。
// 普通百分比统一保留指定小数位，并去掉尾随的 0，避免浮点计算结果直接暴露给用户。
function formatPercentValue(value, decimals = 1) {
  const number = Number(value);
  if (!Number.isFinite(number)) return "--";
  return `${number.toFixed(decimals).replace(/\.?(0+)$/, "")}%`;
}

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
  const rateLabel = "守住";
  const historyTitle = Number.isFinite(Number(level.history_samples)) && Number(level.history_samples) > 0
    ? ` · 历史触及 ${level.history_samples} 次，${rateLabel} ${(holdRate * 100).toFixed(1)}%，跌破 ${(breakRate * 100).toFixed(1)}%${Number.isFinite(Number(level.history_reaction_rate)) ? `，历史反应 ${(Number(level.history_reaction_rate) * 100).toFixed(1)}%` : ""}${Number(level.recent_samples) > 0 ? `；近期 ${level.recent_samples} 次反应，反弹 ${((Number(level.recent_reaction_rate) || 0) * 100).toFixed(1)}%` : ""}`
    : strengthTier === "reinforced"
      ? " · 历史触及样本不足，重点强化已启用"
      : " · 历史触及样本不足，未启用强化色";
  const recentReinforcement = Number(level.recent_samples) >= 2 && Number(level.recent_reactions) >= 2;
  const historySamples = Number(level.history_samples) || 0;
  const isOptionWall = levelFactors(level).some((factor) => factor.includes("看涨墙") || factor.includes("看跌墙"));
  const reinforcedReason = recentReinforcement
    ? "近期多次反应"
    : historySamples >= 5
      ? "历史反应与守住表现通过"
      : isOptionWall
        ? "高可信期权墙，历史样本较少"
        : "多因子信号，历史样本较少";
  const strengthTitle = strengthTag ? (strengthTier === "strong"
    ? ` · ${strengthTag}（历史回踩验证通过）`
    : ` · ${strengthTag}（${reinforcedReason}）`) : "";
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
  if (Number.isFinite(holdRate)) parts.push(`守住 ${(holdRate * 100).toFixed(1)}%`);
  if (Number.isFinite(breakRate)) parts.push(`跌破 ${(breakRate * 100).toFixed(1)}%`);
  const historicalReactionRate = Number(level.history_reaction_rate);
  if (Number.isFinite(historicalReactionRate)) parts.push(`历史反应 ${(historicalReactionRate * 100).toFixed(1)}%`);
  const recentSamples = Number(level.recent_samples);
  if (Number.isFinite(recentSamples) && recentSamples > 0) {
    parts.push(`近期 ${recentSamples} 次反应`);
    const reactionRate = Number(level.recent_reaction_rate);
    if (Number.isFinite(reactionRate)) parts.push(`反弹 ${(reactionRate * 100).toFixed(1)}%`);
  }
  return parts.join(" · ");
}

function buildFactorViews(levels, spot, side, isAdd = false) {
  return (levels || [])
    .map((level, index) => buildLevelView(level, index, spot, side, isAdd))
    .filter((level) => Number.isFinite(level.price) && level.price > 0);
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
    return {
      valid,
      label,
      value: valid ? formatMoney(value) : "--",
      title,
      className: valid ? "" : "trend-placeholder",
    };
  });
}

// 加仓、支撑、压力三张表在新数据回来前先铺满 0 值行，版式和最终表格一致。
function placeholderLevelRow(index, side, isAdd = false) {
  return {
    key: `placeholder-${side}-${isAdd ? "add" : "level"}-${index}`,
    range: "0.00",
    gap: "+0.00%",
    gapClass: "",
    probability: "0%",
    factors: "0",
    strengthTag: "",
    className: "",
    title: "正在加载",
    detail: "历史回踩：0 次",
    placeholder: true,
  };
}

function placeholderLevelRows(count, side, isAdd = false) {
  return Array.from({ length: count }, (_, index) => placeholderLevelRow(index, side, isAdd));
}

function placeholderLevels(note = "正在加载") {
  return {
    resistance: placeholderLevelRows(LEVEL_COUNT, "resistance"),
    support: placeholderLevelRows(LEVEL_COUNT, "support"),
    add: placeholderLevelRows(PLAN_COUNT, "support", true),
    resistanceNote: note,
    supportNote: note,
    resistanceEmpty: "现价这一侧暂无可用价位",
    supportEmpty: "现价这一侧暂无可用价位",
    addEmpty: "暂无可用价位",
    placeholder: true,
  };
}

// 新标的还没回到趋势数据时先铺完整版式。数值用 0，不给出买卖结论；真正的结果回来后整对象替换。
function placeholderTrend(status = "正在加载") {
  const loading = status === "正在加载";
  const rsiTitle = "相对强弱 RSI(14)，按日线收盘价。70 及以上为超买，30 及以下为超卖，其余为中性";
  const slopeTitle = "对样本区间的收盘价做线性回归，得到每个交易日相对均价的平均涨跌百分比。正数为上涨，负数为下跌";
  const betaTitle = "基准指数：标普500 · 时间跨度：2年 · Beta（β）衡量股票相对于整个股市的价格波动情况；高 Beta（>1.0）理论上风险更高但潜在回报更高，低 Beta（<1.0）理论上风险较低但潜在回报也较低";
  const meta = (label, value, title, rowClass = "") => ({
    label,
    value,
    title,
    className: rowClass || (String(label).startsWith("Beta") ? "trend-beta" : ""),
  });
  const rows = [
    meta("止损价", "0.00", "在近期买入区间下沿或通道下轨下方预留波动缓冲的参考价", "trend-stop-loss"),
    meta("昨开", "0.00", "最近一个已完成交易日的常规时段开盘价"),
    meta("昨收", "0.00", "昨日收盘价；非交易时段按最近一个已完成交易日的收盘价显示"),
    meta("Beta", "0.00", betaTitle),
    meta("相对强弱", "0.0 · 中性", rsiTitle, "trend-rsi neutral"),
    meta("日均斜率", "+0.000%", slopeTitle),
    meta("样本", "0 根日线", ""),
    meta("通道上轨", "0.00", ""),
    meta("通道下轨", "0.00", ""),
  ];
  const opportunities = [
    ["buy", "近期最佳买入点"],
    ["sell", "近期最佳卖出点"],
  ].map(([kind, label]) => ({
    kind,
    label,
    horizon: "未来 5 个交易日",
    range: "0.00",
    confidence: "0%",
    historySummary: "守住 0.0% · 0次",
    title: `${label}暂无可用数据 · 估算评分 0%，不是胜率 · 计算范围：未来 5 个交易日`,
  }));
  const extremes = ["52周最高", "52周最低", "历史最高", "历史最低"].map((label) => ({
    label,
    value: "0.00",
    title: `${label} 0.00`,
  }));
  return {
    available: true,
    placeholder: true,
    label: "趋势通道",
    signalTitle: TREND_SIGNAL_TITLE,
    directionClass: "range",
    action: "",
    actionClass: "hold",
    reason: loading ? "正在加载" : status,
    rows,
    leftRows: rows.slice(0, 7),
    rightRows: [...opportunities, ...rows.slice(7, 9), ...extremes],
    priceLabel: "实时价",
    price: "0.00",
    priceTitle: loading ? "正在加载" : "实时价暂无数据",
    opportunities,
    extremes,
    note: loading ? "正在加载" : status,
    noteTitle: "",
    empty: "历史行情不足，暂无趋势判断",
  };
}

// 趋势通道：展示方向、上下轨、日均斜率、RSI 超买超卖、今开或昨开/昨收、Beta，以及 52 周 / 历史最高最低价。
function renderTrend(trend, extremes, spot, historyMeta, recommendation = null, tradePoints = null, tradePointsHorizon = null, trendMarket = null, beta = null, serverStopLoss = null, stopLossSupports = []) {
  const trendClose = finitePrice(trendMarket?.previous_close);
  if (trendClose != null) {
    state.trendMarket = trendMarket;
    state.trendMarketSymbol = state.symbol;
  }
  const extremeRows = trendExtremeRows(extremes, spot);
  const hasExtremes = extremeRows.some((row) => row.valid);
  if (!trend && !hasExtremes && !tradePoints?.buy && !tradePoints?.sell) {
    state.view.trend = placeholderTrend("历史行情不足，暂无趋势判断");
    return;
  }
  const className = trend?.direction === "up" ? "up" : (trend?.direction === "down" ? "down" : "range");
  const slopeValue = trend?.slope_percent == null ? NaN : Number(trend.slope_percent);
  const slopeText = Number.isFinite(slopeValue) ? `${slopeValue >= 0 ? "+" : ""}${slopeValue.toFixed(3)}%` : "--";
  const rsiValue = trend?.rsi?.value == null ? NaN : Number(trend.rsi.value);
  const rsiState = ["overbought", "oversold", "neutral"].includes(trend?.rsi?.state) ? trend.rsi.state : "neutral";
  const rsiText = Number.isFinite(rsiValue) ? `${rsiValue.toFixed(1)} · ${trend.rsi.label || "中性"}` : "--";
  const rsiTitle = "相对强弱 RSI(14)，按日线收盘价。70 及以上为超买，30 及以下为超卖，其余为中性";
  const betaValue = beta?.value == null ? NaN : Number(beta.value);
  const betaText = Number.isFinite(betaValue) ? betaValue.toFixed(2) : "--";
  const betaTitle = "基准指数：标普500 · 时间跨度：2年 · Beta（β）衡量股票相对于整个股市的价格波动情况；高 Beta（>1.0）理论上风险更高但潜在回报更高，低 Beta（<1.0）理论上风险较低但潜在回报也较低";
  const selectedBasis = state.levelBasisMode === "close"
    ? levelBasis(state.lastQuote, spot)
    : activeBasis(state.lastQuote);
  const basisPrice = Number(selectedBasis?.price);
  const fallbackPrice = Number(spot);
  const displayedPrice = Number.isFinite(basisPrice) && basisPrice > 0 ? basisPrice : fallbackPrice;
  const validPrice = Number.isFinite(displayedPrice) && displayedPrice > 0;
  // 标签跟随实际取到的口径：夜盘/盘后数据缺失而回退到常规价时不能再写成「盘后价」。
  const priceLabel = state.levelBasisMode === "close"
    ? (selectedBasis?.label === "盘后" ? "盘后价" : (selectedBasis?.label === "昨收" ? "盘后价·昨收" : (selectedBasis?.label === "盘前" ? "盘前价" : "常规价")))
    : (selectedBasis?.label === "夜盘"
      ? "夜盘"
      : (selectedBasis?.label === "收盘" ? "收盘价" : "实时价"));
  const basisAsOf = typeof selectedBasis?.asOf === "string" && selectedBasis.asOf.length >= 10 ? selectedBasis.asOf : "";
  const priceTitle = validPrice
    ? `${priceLabel} ${formatMoney(displayedPrice)} · 数据来源：${selectedBasis?.label || "常规"}${basisAsOf ? " " + basisAsOf : ""}`
    : `${priceLabel}暂无数据`;
  // 夜盘、盘前还没进入新的常规交易，开盘价属于上一交易日，文案用昨开；盘中和盘后仍是今开。
  const priorSessionOpen = trendMarket?.market_state === "PRE" || trendMarket?.market_state === "OVERNIGHT";
  const openTitle = trendMarket?.previous_open_date
    ? trendMarket.previous_open_date + " 交易日的常规时段开盘价"
    : "最近一个已完成交易日的常规时段开盘价";
  const closeTitle = trendMarket?.previous_close_date
    ? trendMarket.previous_close_date + " 交易日的收盘价"
    : "昨日收盘价；非交易时段按最近一个已完成交易日的收盘价显示";
  const stopValue = Number(typeof serverStopLoss === "object" ? serverStopLoss?.price : serverStopLoss);
  const stopBasisPrice = validPrice ? displayedPrice : Number(spot);
  const buyZoneLow = Number(tradePoints?.buy?.zone_low) || Number(tradePoints?.buy?.price);
  const trendLower = Number(trend?.lower);
  const stopAnchors = [
    [buyZoneLow, "近期最佳买入点下沿"],
    [trendLower, "通道下轨"],
    ...(Array.isArray(stopLossSupports) ? stopLossSupports.map((level) => [Number(level?.zone_low) || Number(level?.price), "最近支撑位"]) : []),
  ].filter(([value]) => Number.isFinite(stopBasisPrice) && stopBasisPrice > 0 && Number.isFinite(value) && value > 0 && value < stopBasisPrice);
  const [fallbackAnchor, fallbackSource] = stopAnchors.sort((a, b) => b[0] - a[0])[0] || [];
  const stopPrice = Number.isFinite(stopValue) && stopValue > 0 && Number.isFinite(stopBasisPrice) && stopValue < stopBasisPrice
    ? stopValue
    : (Number.isFinite(fallbackAnchor) ? fallbackAnchor * 0.995 : NaN);
  const stopSource = typeof serverStopLoss === "object" && serverStopLoss?.source
    ? serverStopLoss.source
    : fallbackSource;
  const serverBuffer = Number(typeof serverStopLoss === "object" ? serverStopLoss?.buffer : NaN);
  const stopBufferNote = Number.isFinite(stopValue) && Number.isFinite(serverBuffer) && serverBuffer > 0
    ? ` · 波动缓冲 ${formatMoney(serverBuffer)}`
    : " · 本地估算在锚点下方预留 0.5% 缓冲";
  const stopRow = {
    label: "止损价",
    value: Number.isFinite(stopPrice) && stopPrice > 0 ? formatMoney(stopPrice) : "--",
    title: Number.isFinite(stopPrice) && stopPrice > 0
      ? `${Number.isFinite(stopValue) && stopValue > 0 ? "服务端参考止损价" : "本地临时估算止损价"} ${formatMoney(stopPrice)}${stopSource ? ` · 依据：${stopSource}` : ""}${stopBufferNote} · 请结合仓位与风险承受能力判断`
      : "暂无足够的支撑或趋势数据计算参考止损价",
    className: "trend-stop-loss",
    placeholder: !(Number.isFinite(stopPrice) && stopPrice > 0),
  };
  const rows = trend ? [
    ["通道上轨", formatMoney(trend.upper), ""],
    ["通道下轨", formatMoney(trend.lower), ""],
    ["相对强弱", rsiText, rsiTitle, `trend-rsi ${rsiState}`],
    ["日均斜率", slopeText, "对样本区间的收盘价做线性回归，得到每个交易日相对均价的平均涨跌百分比。正数为上涨，负数为下跌"],
    ["样本", `${Number(trend.bars) || 0} 根日线`, ""],
    ["昨开", formatMoney(trendMarket?.previous_open), openTitle],
    ["昨收", formatMoney(trendMarket?.previous_close), closeTitle],
    ["Beta", betaText, betaTitle],
  ].map(([label, value, title, rowClass]) => ({
    label,
    value,
    title,
    className: rowClass || (label.startsWith("Beta") ? "trend-beta" : ""),
    placeholder: value === "--",
  })) : placeholderTrend("趋势数据暂缺").rows.map((row) => {
    const values = {
      "通道上轨": "--",
      "通道下轨": "--",
      "相对强弱": "--",
      "日均斜率": "--",
      "样本": "等待日线",
      "昨开": formatMoney(trendMarket?.previous_open),
      "昨收": formatMoney(trendMarket?.previous_close),
      "Beta": betaText,
    };
    const value = values[row.label] ?? "--";
    const label = row.label;
    const title = row.label === "昨开" ? openTitle : (row.label === "昨收" ? closeTitle : row.title);
    const available = value !== "--" && value !== "等待日线";
    return {
      ...row,
      label,
      value,
      title,
      placeholder: !available,
    };
  });
  const byLabel = new Map(rows.map((row) => [row.label, row]));
  byLabel.set("止损价", stopRow);
  const leftRows = ["止损价", "昨开", "昨收", "Beta", "相对强弱", "日均斜率", "样本"]
    .map((label) => byLabel.get(label)).filter(Boolean);
  const rightRows = ["通道上轨", "通道下轨"].map((label) => byLabel.get(label)).filter(Boolean)
    .concat(extremeRows);
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
      placeholder: !point,
      title: `${title} · 模型评分 ${modelConfidence} · ${history} · 估算评分 ${confidence}，不是胜率 · 计算范围：${horizonLabel}`,
    };
  });
  rightRows.unshift(...opportunityRows);
  const meta = historyMeta || {};
  const parts = [
    meta.extremes_fetched_at ? `高低点快照 ${formatTime(meta.extremes_fetched_at)}` : null,
    meta.extremes_source ? `来源 ${meta.extremes_source}` : null,
    meta.extremes_warning ? `高低点降级：${meta.extremes_warning}` : null,
  ].filter(Boolean);
  state.view.trend = {
    available: true,
    placeholder: false,
    partial: !trend,
    label: trend?.label || "趋势通道",
    signalTitle: TREND_SIGNAL_TITLE,
    directionClass: className,
    action: actionLabel,
    actionClass: action || "hold",
    reason: actionReason || (!trend ? "趋势数据暂缺" : ""),
    rows,
    leftRows,
    rightRows,
    priceLabel,
    price: validPrice ? formatMoney(displayedPrice) : "--",
    priceTitle,
    opportunities: opportunityRows,
    extremes: extremeRows,
    note: trend ? "按最近日线收盘价的线性回归通道；相对强弱为 RSI(14)；高低点取日线最高/最低价（历史极值用全量历史）" : "日线趋势数据暂缺，缺失指标暂以占位显示",
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
function renderPlanRows(levels, spot, preserveEmpty = true) {
  commitLevelViews({ add: buildFactorViews(levels, spot, "support", true) }, preserveEmpty);
  state.view.levels.addEmpty = state.view.levels.add.length ? "" : "暂无可用价位";
  state.view.levels.placeholder = false;
}

// 交易计划：前端只展示更深一档的加仓支撑，推荐买入与卖出直接看支撑位/压力位面板。
function renderPlan(plan, spot, preserveEmpty = true) {
  const price = Number(spot);
  renderPlanRows(plan?.add || [], price, preserveEmpty);
}

// 期权报价可能在同一个 fetched_at 批次内被补写；缓存键不能只看行数，否则会继续展示旧的 IV/价差估算。
function clientBuyerRowsSignature(rows) {
  let hash = 2166136261;
  let quoted = 0;
  for (const row of rows || []) {
    const bid = Number(row?.bid); const ask = Number(row?.ask); const last = Number(row?.last_price);
    if ((Number.isFinite(bid) && bid > 0) || (Number.isFinite(ask) && ask > 0) || (Number.isFinite(last) && last > 0)) quoted += 1;
    const text = [row?.expiration, row?.contract_type, row?.strike, row?.bid, row?.ask, row?.last_price,
      row?.model_iv, row?.implied_volatility, row?.volume, row?.open_interest].join("|");
    for (let index = 0; index < text.length; index += 1) hash = Math.imul(hash ^ text.charCodeAt(index), 16777619);
  }
  return `${rows?.length || 0}:${quoted}:${hash >>> 0}`;
}

function clientBuyerLevelsSignature(support, resistance) {
  const prices = [...(support || []), ...(resistance || [])]
    .map((item) => Number(item?.price))
    .filter((value) => Number.isFinite(value) && value > 0)
    .sort((a, b) => a - b);
  return prices.map((value) => value.toFixed(2)).join(",");
}

function renderClientBuyerFallback(rows, spot, support = [], resistance = [], directionOverride = undefined) {
  const sameServerContext = state.serverLevelsContext === `${state.symbol}|${state.expiration}|${state.levelBasisMode}`;
  const recommendationAction = sameServerContext ? state.serverLevelsPayload?.recommendation?.action : null;
  const directionHint = directionOverride === "call" || directionOverride === "put"
    ? directionOverride
    : recommendationAction === "buy"
      ? "call"
      : recommendationAction === "sell"
        ? "put"
        : state.view.trend?.direction === "up"
          ? "call"
          : state.view.trend?.direction === "down"
            ? "put"
            : null;
  const priceBucket = Number.isFinite(Number(spot)) ? (Math.round(Number(spot) * 100) / 100).toFixed(2) : "";
  const key = `${state.symbol}|${state.expiration}|${state.chainFetchedAt || ""}|${priceBucket}|${state.levelBasisMode}|${clientBuyerRowsSignature(rows)}|${clientBuyerLevelsSignature(support, resistance)}|${directionHint || "neutral"}`;
  const cached = state.clientBuyerCache.get(key);
  const client = window.OptionScopeBuyerClient;
  const payload = cached
    || client?.buildStructures?.(rows, spot, support, resistance, directionHint)
    || client?.buildSkeleton?.(rows, spot, state.expiration);
  if (!cached && payload) {
    state.clientBuyerCache.set(key, payload);
    while (state.clientBuyerCache.size > 12) state.clientBuyerCache.delete(state.clientBuyerCache.keys().next().value);
  }
  if (payload) renderBuyerStructures(payload, null);
  return payload;
}

function renderFactorLevels(payload) {
  if (payload?.symbol && payload.symbol !== state.symbol) return;
  if (payload?.expiration && state.expiration && payload.expiration !== state.expiration) return;
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
  const basisNote = Number.isFinite(spot) && spot > 0 ? ` · 基准 ${formatMoney(spot)}` : "";
  state.view.levels.placeholder = false;
  state.view.levels.resistanceNote = scope + basisNote;
  state.view.levels.supportNote = scope + basisNote;
  const completeLevels = serverLevelsHasCompleteFields(payload);
  const keepCurrentLevels = sameDisplayedLevelContext();
  const resistanceViews = buildFactorViews(payload?.resistance || [], spot, "resistance");
  const supportViews = buildFactorViews(payload?.support || [], spot, "support");
  const addViews = buildFactorViews(payload?.plan?.add || [], spot, "support", true);
  commitLevelViews({ resistance: resistanceViews, support: supportViews, add: addViews }, !completeLevels);
  state.view.levels.resistanceEmpty = "现价这一侧暂无可用价位";
  state.view.levels.supportEmpty = "现价这一侧暂无可用价位";
  if (resistanceViews.length || supportViews.length || completeLevels || !keepCurrentLevels) {
    renderCommittedLevelsChart(spot);
  }
  // 服务端没有返回可用结构时保留 raw 首屏的客户端候选，避免综合接口的空结果把页面清空。
  if (payload?.buyer_structures?.available
    && Array.isArray(payload.buyer_structures.items)
    && payload.buyer_structures.items.length) {
    renderBuyerStructures(payload.buyer_structures, payload?.options_fetched_at || payload?.chain_fetched_at);
  }
  const tradePointContext = `${state.symbol}|${expiration}|${state.levelBasisMode}`;
  // 综合结果一旦返回，立即替换客户端临时候选；不要再等待两轮快照确认，
  // 否则用户会看到服务端已经返回但页面仍停留在旧的「--」或临时点。
  if (serverLevelsHasCompleteFields(payload)
    && state.tradePointStability.context === tradePointContext) {
    state.tradePointStability.stable = { buy: payload.trade_points?.buy || null, sell: payload.trade_points?.sell || null };
    state.tradePointStability.pending = { buy: null, sell: null };
  }
  const stableTradePoints = stabilizeTradePoints(payload?.trade_points, tradePointContext);
  const hasCachedHistoryAnalysis = Number(payload?.history?.bars) > 0 && payload?.trend;
  if (hasCachedHistoryAnalysis || !renderCurrentChanMarketAnalysis(spot)) {
    renderTrend(payload?.trend || null, payload?.extremes || null, spot, payload?.history || null, payload?.recommendation || null, stableTradePoints, payload?.trade_points_horizon || null, payload?.trend_market || null, payload?.beta || null, payload?.stop_loss || null, payload?.support || []);
  }
  if (completeLevels || addViews.length || !keepCurrentLevels) renderPlan(payload?.plan, spot, !completeLevels);
  refreshQuoteWithTrendClose();
  rememberClientSnapshot();
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

function buyerQuoteSourceLabel(value) {
  if (value === "last") return "最新成交价估算";
  if (value === "skeleton") return "结构骨架";
  if (value === "server") return "服务端综合";
  if (value === "bid_ask") return "Bid/Ask 中间价估算";
  return "买卖价中间价";
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
  const sourceLabel = buyerQuoteSourceLabel(payload.quote_source);
  const ivSource = String(payload.iv_source || "");
  const ivLabel = ivSource.includes("default")
    ? "默认 IV 粗略估算"
    : ivSource.includes("quote")
      ? "报价反解 IV"
      : ivSource.includes("price")
        ? "服务端报价反解 IV"
      : ivSource.includes("server") ? "服务端模型 IV" : "模型 IV";
  state.view.buyer = {
    available: true,
    directionLabel: payload.direction_label || (payload.direction === "call" ? "买入看涨" : "买入看跌"),
    horizonLabel: payload.horizon_label || "未来 5 个交易日",
    target: targetText || "--",
    reason: "",
    note: `${buyerMethod} · ${sourceLabel} · ${buyerQuoteMethod} · ${ivLabel} · ${fetchedAt ? `数据 ${formatTime(fetchedAt)}` : "数据时间未知"} · 预计盈利/亏损以当前买卖价中间价为成本基准；负数表示目标价虽达到，扣除时间价值后仍未覆盖成本。${payload.disclaimer || "综合评分不是历史胜率"}`,
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
        horizon: item.horizon || payload.horizon_label || "未来 5 个交易日",
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
  state.buyerContext = `${state.symbol}|${state.expiration}|${state.levelBasisMode}`;
}

function renderAnalysis(rows, spot, analysisPayload, expirationRows = [], ivModel = {}, basis = null) {
  // 期权链聚合结果：Gamma 敞口、分布图与压力位/支撑位共用；切换基准价开关时直接复用，不重新聚合。
  const points = aggregateByStrike(expirationRows, spot);
  // 记录本次分析输入，窗口尺寸变化（含手机横竖屏切换）与基准价切换后都按这些输入重绘。
  // 顶层冻结：这些数组只读，Vue 2 无需为每一行行情对象建立响应式观察器。
  state.lastAnalysis = Object.freeze({ rows, spot, analysisPayload, expirationRows, ivModel, basis, points });
  // 压力位/支撑位用「基准价」（默认实时价，可切盘后价），图表仍用常规价。
  const levelSpot = Number(basis?.price) > 0 ? Number(basis.price) : spot;
  state.levelBasisLabel = basis?.label || "常规";
  const scopeText = expirationRows.length ? `到期日 ${state.expiration || "--"} · ${expirationRows.length} 个合约` : "当前到期日无数据";
  state.view.chart.volumeScope = scopeText;
  state.view.chart.oiScope = scopeText;
  const modelIv = Number(ivModel?.[state.expiration]?.iv);
  const scopeSuffix = Number.isFinite(modelIv) && modelIv > 0 ? ` · 模型 IV ${(modelIv * 100).toFixed(1)}%` : "";
  const serverFlip = finitePrice(analysisPayload?.zero_gamma?.price);
  // 零 Gamma 等跨期限 Gamma 窗口全部到齐后再估算，避免首个到期日的局部结果
  // 被误认为完整窗口的零 Gamma。
  const gammaReady = state.analysisReady && gammaProfileReady(analysisPayload);
  const gammaFlip = gammaReady
    ? (serverFlip != null ? { strike: serverFlip } : null)
      || findGammaFlip(expirationRows, spot, state.expiration)
    : null;
  const callWall = points.reduce((best, point) => point.callGex > (best?.callGex || 0) ? point : best, null);
  const putWall = points.reduce((best, point) => Math.abs(point.putGex) > Math.abs(best?.putGex || 0) ? point : best, null);
  const volumeCallPeak = maxPoint(points, "callVolume");
  const volumePutPeak = maxPoint(points, "putVolume");
  const oiCallPeak = maxPoint(points, "callOi");
  const oiPutPeak = maxPoint(points, "putOi");
  state.view.chart.netGex = `净 Gamma ${formatGex(points.reduce((sum, point) => sum + point.callGex + point.putGex, 0), 2)} · 估算`;
  state.view.chart.gammaFlip = gammaReady
    ? `零 Gamma ${gammaFlip ? formatMoney(gammaFlip.strike) : "--"} · 估算`
    : "零 Gamma 等待窗口加载";
  state.view.chart.netGexTitle = GAMMA_ESTIMATE_TITLE;
  state.view.chart.gammaFlipTitle = GAMMA_ESTIMATE_TITLE;
  state.view.chart.gammaScopeDate = `柱状图 到期日 ${state.expiration || "--"}`;
  state.view.chart.gammaScopeStats = `${expirationRows.length} 个合约${scopeSuffix}`;
  const analysisFallback = analysisPayload?.oi_fallback || {};
  if (analysisFallback.restored) state.view.chart.gammaScopeStats += ` · 未平仓量回溯 ${formatDay(analysisFallback.as_of)}`;
  state.view.chart.callWall = `看涨墙 ${callWall?.callGex ? formatMoney(callWall.strike) : "--"}`;
  state.view.chart.putWall = `看跌墙 ${putWall?.putGex ? formatMoney(putWall.strike) : "--"}`;
  // 选中期限的综合价位与 Gamma 窗口并行请求，避免首次加载时趋势/支撑/压力面板长期空白。
  loadFactorLevels(points, levelSpot);
  if (!state.view.buyer.available && expirationRows.length) {
    renderClientBuyerFallback(expirationRows, levelSpot, state.view.levels.support, state.view.levels.resistance);
  }
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

// 夜盘优先展示 Alpaca 夜盘价；未配置或失效时沿用主行情适配器返回的价格。
function overnightCloseQuote(quote) {
  const sessions = quote?.sessions || {};
  const alpacaOvernight = sessions.overnight?.provider === "alpaca-overnight";
  // 未配置 Alpaca 时，主行情的 price/change_percent 才是原有行为。
  // 不能再用盘后 reference_close 覆盖它，否则 132.60 会被替换成 132.63，
  // 页面就会把昨收和当前价显示成同一个数，涨跌固定为 0.00%。
  if (!alpacaOvernight) {
    const price = finitePrice(quote?.price);
    return price == null ? null : { ...quote, price };
  }
  // Alpaca 夜盘快照提供参考价时优先展示它；没有夜盘价时才回退正式收盘。
  const overnightPrice = alpacaOvernight ? finitePrice(sessions.overnight?.price) : null;
  const reference = Number(sessions.post?.reference_close ?? sessions.overnight?.reference_close);
  const previous = Number(quote?.previous_close);
  const price = overnightPrice ?? (Number.isFinite(reference) && reference > 0
    ? reference
    : (Number.isFinite(previous) && previous > 0 ? previous : null));
  if (price == null) return null;
  // `Number(null)` 会得到 0；夜盘快照未提供涨跌时必须继续按夜盘价和昨收计算，不能误显示 +0.00%。
  const rawReportedChange = sessions.overnight?.change_percent;
  const reportedChange = rawReportedChange == null || rawReportedChange === ""
    ? null
    : Number(rawReportedChange);
  const change = Number.isFinite(reportedChange)
    ? reportedChange
    : (Number.isFinite(previous) && previous > 0 ? (price - previous) / previous * 100 : null);
  return { ...quote, price, change_percent: change };
}

// 现价按时段动态取值：盘前显示盘前价，盘后显示盘后价，夜盘优先显示 Alpaca 夜盘价，
// 盘中与休市显示常规价。对应时段没有数据时回退常规价，避免整块行情空掉。
function activeSessionQuote(quote) {
  const sessions = quote?.sessions || {};
  const marketState = quote?.market_state;
  if (marketState === "PRE" && sessions.pre?.price != null) return sessions.pre;
  if (marketState === "POST" && sessions.post?.price != null) return sessions.post;
  if (marketState === "OVERNIGHT") {
    // 配置 Alpaca 后，夜盘价优先级高于分析面板的盘后价基准开关；未配置时保留旧口径。
    if (sessions.overnight?.provider !== "alpaca-overnight"
      && state.levelBasisMode === "close" && sessions.post?.price != null) return sessions.post;
    return overnightCloseQuote(quote) || quote;
  }
  return quote;
}

function finitePrice(value) {
  const number = Number(value);
  return Number.isFinite(number) && number > 0 ? number : null;
}

// 涨跌幅旁边标出基准价。盘后和夜盘的盘后口径只用正式收盘，不拿昨收填空。
function quoteReference(quote) {
  const marketState = quote?.market_state;
  const sessions = quote?.sessions || {};
  const previous = alignedPreviousClose(quote);
  const previousCloseDate = state.trendMarketSymbol === quote?.symbol
    ? state.trendMarket?.previous_close_date : null;
  let label = "昨收";
  let price = previous;
  let title = "涨跌幅以最近一个已完成交易日的收盘价为基准";
  if (marketState === "PRE") {
    // 盘前现货卡片的昨收必须直接沿用趋势通道显示的历史日线收盘；
    // quote.previous_close 和 sessions.pre.reference_close 都可能与当前趋势结果不同步。
    label = "昨收";
    price = previous;
    title = "昨收与趋势通道一致，按最近一个已完成交易日的收盘价显示";
  } else if (marketState === "POST" || (marketState === "OVERNIGHT"
    && state.levelBasisMode === "close"
    && sessions.overnight?.provider !== "alpaca-overnight")) {
    label = "相对收盘";
    price = finitePrice(sessions.post?.reference_close);
    title = "盘后涨跌以最近一次正式盘中收盘为基准，不用昨收代替";
  } else if (marketState === "OVERNIGHT") {
    // 夜盘没有当前盘中报价时，涨跌基准是上一个交易日的收盘价。
    label = "昨收";
    price = previous;
    title = "夜盘涨跌以昨收为基准";
  }
  if (marketState === "CLOSED" && previousCloseDate) {
    title = "昨收与趋势通道一致，按 " + previousCloseDate + " 交易日的收盘价显示";
  }
  return {
    text: `${label} ${price == null ? "--" : formatMoney(price)}`,
    title,
    price,
  };
}

function alignedPreviousClose(quote) {
  const trendPrevious = state.trendMarketSymbol === quote?.symbol
    ? finitePrice(state.trendMarket?.previous_close)
    : null;
  return trendPrevious ?? finitePrice(quote?.previous_close);
}

function refreshQuoteWithTrendClose() {
  if (state.lastQuote?.symbol === state.symbol && state.trendMarketSymbol === state.symbol) {
    renderQuote(state.lastQuote);
  }
}

function hasFairValueSnapshot(quote) {
  return Boolean(quote
    && quote.fair_value != null
    && String(quote.fair_value_source || "").startsWith("valuation_"));
}

function mergeFairValueSnapshot(quote) {
  const previous = state.lastQuote;
  if (quote?.symbol && quote.symbol !== state.symbol) return null;
  if (!previous || previous.symbol !== quote?.symbol || !hasFairValueSnapshot(previous) || hasFairValueSnapshot(quote)) return quote;
  const merged = { ...previous, ...(quote || {}) };
  const fields = [
    "fair_value", "fair_value_low", "fair_value_high", "fair_value_buy_low", "fair_value_buy_high",
    "fair_value_source", "fair_value_model", "fair_value_forward_eps", "fair_value_forward_eps_source",
    "fair_value_safety_margin", "fair_value_confidence", "fair_value_confidence_score",
    "fair_value_interest_coverage", "fair_value_regime", "fair_value_regime_signals",
    "fair_value_model_under_regime", "fair_value_defensive", "fair_value_optimistic",
    "fair_value_normalized_eps_source", "fair_value_quarterly_momentum",
    "fair_value_historical_valuation_percentiles", "fair_value_shareholder_total_return_yield",
    "fair_value_market_cap_data_quality", "fair_value_data_quality_score",
    "fair_value_owner_earnings_maintenance_ratio", "fair_value_owner_earnings_ratio_source",
  ];
  for (const field of fields) {
    const value = quote?.[field];
    const emptyObject = value && typeof value === "object" && !Array.isArray(value) && Object.keys(value).length === 0;
    if (value == null || value === "" || emptyObject) merged[field] = previous[field];
  }
  if (hasFairValueSnapshot(previous) && quote?.fair_value_status === "pending") {
    merged.fair_value_status = previous.fair_value_status || "ready";
    merged.fair_value_warning = previous.fair_value_warning || null;
  }
  return merged;
}

function quoteIsETF(quote) {
  const symbol = String(quote?.symbol || state.symbol || "").trim().toUpperCase();
  const messages = [quote?.fair_value_warning, ...(Array.isArray(quote?.fair_value_warnings) ? quote.fair_value_warnings : [])];
  const classifiedAsETF = [quote?.instrument_type, quote?.quote_type, quote?.quoteType]
    .some((type) => String(type || "").trim().toUpperCase() === "ETF");
  const hasETFValuationWarning = messages.some((message) => /^\s*ETF\s+is not an operating company\b/i.test(String(message || "")));
  if (classifiedAsETF || hasETFValuationWarning) {
    if (quote && typeof quote === "object") quote.instrument_type = "ETF";
    state.knownETFSymbols.add(symbol);
    if (state.knownETFSymbols.size > 128) state.knownETFSymbols.delete(state.knownETFSymbols.values().next().value);
  }
  return Boolean(symbol && (classifiedAsETF || hasETFValuationWarning || state.knownETFSymbols.has(symbol)));
}

function applyETFValuationDefaults() {
  state.view.fairValue = "--";
  state.view.fairValueSource = "";
  state.view.fairValueTitle = "";
  state.view.fairDefensive = "--";
  state.view.fairDefensiveSource = "";
  state.view.fairDefensiveTitle = "";
  state.view.fairOptimistic = "--";
  state.view.fairOptimisticSource = "";
  state.view.fairOptimisticTitle = "";
  clearFairValuePoll();
}

// 缠论与趋势卡片共用 SQLite 日线。不要让趋势展示等待期权链的 renderChain 流程：
// 没有可用期权链时，缠论图表已有历史而趋势卡片会一直停在初始占位值。
function hydrateTrendFromChan(payload, preferredSpot = null, preserveOptionLevels = false) {
  if (!payload || payload.symbol !== state.symbol || !payload.market_analysis) return;
  const analysis = payload.market_analysis;
  const quote = state.lastQuote?.symbol === state.symbol ? state.lastQuote : null;
  const spot = finitePrice(preferredSpot)
    ?? finitePrice(activeSessionQuote(quote)?.price ?? quote?.price)
    ?? finitePrice(payload.spot);
  state.trendHistorySymbol = state.symbol;
  // Chan market_analysis is built from daily bars without option-chain rows, so
  // its stop loss, buy point and support list stay empty even when the levels
  // payload already produced them. Keep those option-derived values instead of
  // letting this faster response erase the trend card back to "--".
  const levels = state.serverLevelsPayload;
  const sameLevelsContext = serverLevelsHasCompleteFields(levels)
    && state.serverLevelsContext === `${state.symbol}|${state.expiration}|${state.levelBasisMode}`;
  const analysisBuy = Number(analysis.trade_points?.buy?.price);
  const levelsBuy = Number(levels?.trade_points?.buy?.price);
  const tradePoints = sameLevelsContext && levelsBuy > 0 && !(analysisBuy > 0)
    ? levels.trade_points
    : analysis.trade_points;
  const stopLoss = analysis.stop_loss || (sameLevelsContext ? levels.stop_loss : null);
  const stopSupports = Array.isArray(analysis.support) && analysis.support.length
    ? analysis.support
    : sameLevelsContext ? levels.support : analysis.support || [];
  renderTrend(analysis.trend || null, analysis.extremes || null, spot, payload.history || null,
    analysis.recommendation || null, tradePoints || null, analysis.trade_points_horizon || null,
    analysis.trend_market || null, analysis.beta || null, stopLoss || null, stopSupports);
  if (!preserveOptionLevels) {
    const resistance = analysis.resistance || [];
    const support = analysis.support || [];
    const addLevels = analysis.plan?.add || [];
    const keepCurrentLevels = sameDisplayedLevelContext();
    state.view.levels.placeholder = false;
    commitLevelViews({
      resistance: buildFactorViews(resistance, Number(spot), "resistance"),
      support: buildFactorViews(support, Number(spot), "support"),
      add: buildFactorViews(addLevels, Number(spot), "support", true),
    }, true);
    state.view.levels.resistanceEmpty = "现价上方暂无历史观察价位";
    state.view.levels.supportEmpty = "现价下方暂无历史观察价位";
    state.view.levels.addEmpty = addLevels.length ? "" : "暂无可用历史加仓价位";
    const source = "SQLite 日线计算 · 期权链尚不可用";
    state.view.levels.resistanceNote = source;
    state.view.levels.supportNote = source;
    if (resistance.length || support.length || !keepCurrentLevels) {
      OptionScopeCharts.renderLevelsChart({ spot, resistance, support });
    }
  }
}

function renderCurrentChanMarketAnalysis(spot, preserveOptionLevels = false) {
  const payload = state.chanMarketPayload?.symbol === state.symbol && state.chanMarketPayload?.market_analysis
    ? state.chanMarketPayload
    : state.chanPayload;
  if (payload?.symbol !== state.symbol || !payload.market_analysis) return false;
  hydrateTrendFromChan(payload, spot, preserveOptionLevels);
  return true;
}

function resetChanAnalysis() {
  Object.values(state.chanIntradayRetryTimers).forEach((timer) => clearTimeout(timer));
  clearTimeout(chanIntradayBoundaryTimer);
  chanIntradayBoundaryTimer = null;
  clearTimeout(chanIntradayExpiryRetryTimer);
  chanIntradayExpiryRetryTimer = null;
  chanIntradayLastBoundaryAttempt = "";
  chanIntradayExpiryRetryBoundary = "";
  clearTimeout(state.chanRetryTimer);
  state.chanRetryTimer = null;
  clearTimeout(state.chanMarketRetryTimer);
  state.chanMarketRetryTimer = null;
  state.chanRequestId += 1;
  state.chanInFlightSymbol = "";
  state.chanLastRequestedAt = 0;
  state.chanRetryAttempts = 0;
  state.chanMarketRetryAttempts = 0;
  state.chanMarketInFlight = false;
  state.chanMarketLastRequestedAt = 0;
  state.chanMarketLastKey = "";
  state.chanLoading = false;
  state.chanSpotGuideVisible = false;
  state.chanPayload = null;
  state.chanStale = false;
  state.chanMarketPayload = null;
  state.chanViewports = { daily: { visibleBars: 0, endOffset: 0 }, weekly: { visibleBars: 0, endOffset: 0 }, "15m": { visibleBars: 0, endOffset: 0 }, "4h": { visibleBars: 0, endOffset: 0 } };
  state.chanRefreshPending = false;
  state.view.chan = OptionScopeChan.empty();
  state.chanIntradayPayload = { "15m": null, "4h": null };
  state.chanIntradayLoading = { "15m": false, "4h": false };
  state.chanIntradayRetryTimers = { "15m": null, "4h": null };
  state.chanIntradayRetryAttempts = { "15m": 0, "4h": 0 };
  state.chanIntradaySpot = null;
}

function scheduleChanIntradayRetry(period, symbol, minimumDelay = 0) {
  const attempts = state.chanIntradayRetryAttempts[period] || 0;
  // Initial history may take a few minutes on a constrained host. Retry with
  // capped backoff so a missing cache recovers without a manual page reload.
  if (state.chanIntradayRetryTimers[period]) return;
  const delay = minimumDelay > 0 ? Math.min(300000, minimumDelay)
    : Math.min(300000, 3000 * (2 ** Math.min(attempts, 7)));
  state.chanIntradayRetryAttempts = { ...state.chanIntradayRetryAttempts, [period]: Math.min(CHAN_INTRADAY_RETRY_LIMIT, attempts + 1) };
  state.chanIntradayRetryTimers = { ...state.chanIntradayRetryTimers, [period]: setTimeout(() => {
    state.chanIntradayRetryTimers = { ...state.chanIntradayRetryTimers, [period]: null };
    if (state.symbol !== symbol || state.chanPeriod !== period) return;
    optionScopeApp?.ensureChanIntraday(period, true);
  }, delay) };
}

function newYorkIntradayParts(date = new Date()) {
  const values = Object.fromEntries(NEW_YORK_INTRADAY_DATE_FORMATTER
    .formatToParts(date).map((part) => [part.type, part.value]));
  return {
    date: `${values.year}-${values.month}-${values.day}`,
    minute: Number(values.hour) * 60 + Number(values.minute),
    weekday: values.weekday,
    year: Number(values.year),
    month: Number(values.month),
    day: Number(values.day),
  };
}

function previousIntradayCandleDate(parts) {
  const boundaryMinute = Math.floor(parts.minute / 15) * 15 - 15;
  const localStamp = new Date(Date.UTC(parts.year, parts.month - 1, parts.day, 0, boundaryMinute));
  return `${localStamp.toISOString().slice(0, 10)} ${localStamp.toISOString().slice(11, 16)}`;
}

function refreshChanIntradayAtBoundary(symbol) {
  if (state.symbol !== symbol || state.chanPeriod !== "15m") return;
  const candles = state.chanIntradayPayload["15m"]?.frame?.candles;
  if (!Array.isArray(candles) || candles.length === 0) return;

  const now = Date.now();
  const localClock = new Date(now);
  const parts = newYorkIntradayParts(localClock);
  const elapsedIntoQuarter = (parts.minute % 15) * 60 * 1000
    + localClock.getSeconds() * 1000 + localClock.getMilliseconds();
  if (elapsedIntoQuarter < CHAN_INTRADAY_REFRESH_DELAY_MS) return;
  if (parts.weekday === "Sat" || parts.weekday === "Sun") return;
  const latestDate = String(candles[candles.length - 1]?.date || "");

  const expectedCandleDate = previousIntradayCandleDate(parts);
  const expectedMinute = Number(expectedCandleDate.slice(11, 13)) * 60 + Number(expectedCandleDate.slice(14, 16));
  if (expectedCandleDate.slice(0, 10) !== parts.date || expectedMinute < 9 * 60 + 30 || latestDate >= expectedCandleDate) return;

  const marketState = String(state.lastQuote?.market_state || "").toUpperCase();
  const quoteIsFresh = quoteIsFreshForIntraday(state.lastQuote);
  const regularSession = parts.minute >= 9 * 60 + 30 && parts.minute < 16 * 60
    && (marketState === "REGULAR" && quoteIsFresh || latestDate.startsWith(parts.date));
  // These are the final candle starts for standard and 13:00 early closes.
  // Checking the expected candle prevents an extra request for a non-session
  // 13:00 or 16:00 bar after the close.
  const closingWindow = expectedMinute === 12 * 60 + 45 || expectedMinute === 15 * 60 + 45;
  const closingSession = closingWindow && latestDate.startsWith(parts.date);
  if (!regularSession && !closingSession) return;

  const attemptKey = `${symbol}:${expectedCandleDate}`;
  if (chanIntradayLastBoundaryAttempt === attemptKey) return;
  chanIntradayLastBoundaryAttempt = attemptKey;
  if (state.chanIntradayLoading["15m"]) return;
  window.optionScopeApp?.ensureChanIntraday("15m", true);
}

function quoteIsFreshForIntraday(quote) {
  return Boolean(quote && quoteIsFresh(quote, 180));
}

function scheduleChanIntradayCacheExpiryRetry(symbol, expectedCandleDate, fetchedAt, refreshing) {
  const retryBoundary = `${symbol}:${expectedCandleDate}`;
  if (chanIntradayExpiryRetryBoundary === retryBoundary) return;
  chanIntradayExpiryRetryBoundary = retryBoundary;
  clearTimeout(chanIntradayExpiryRetryTimer);
  const fetchedAtMs = Date.parse(fetchedAt || "");
  const expiresAt = Number.isFinite(fetchedAtMs)
    ? fetchedAtMs + 3 * 60 * 1000 + 5000
    : Date.now() + 3 * 60 * 1000 + 5000;
  const retryDelay = refreshing ? 60 * 1000 : Math.max(60 * 1000, expiresAt - Date.now());
  chanIntradayExpiryRetryTimer = setTimeout(() => {
    chanIntradayExpiryRetryTimer = null;
    if (state.symbol !== symbol || state.chanPeriod !== "15m") return;
    const candles = state.chanIntradayPayload["15m"]?.frame?.candles;
    const latestDate = String(candles?.[candles.length - 1]?.date || "");
    if (latestDate >= expectedCandleDate) {
      chanIntradayExpiryRetryBoundary = "";
      return;
    }
    chanIntradayLastBoundaryAttempt = "";
    chanIntradayExpiryRetryBoundary = "";
    refreshChanIntradayAtBoundary(symbol);
  }, retryDelay);
}

function scheduleChanIntradayBoundaryRefresh(period, symbol = state.symbol) {
  clearTimeout(chanIntradayBoundaryTimer);
  chanIntradayBoundaryTimer = null;
  if (period !== "15m") {
    clearTimeout(chanIntradayExpiryRetryTimer);
    chanIntradayExpiryRetryTimer = null;
    chanIntradayLastBoundaryAttempt = "";
    chanIntradayExpiryRetryBoundary = "";
    return;
  }
  if (!symbol) return;

  const now = Date.now();
  const date = new Date(now);
  const parts = newYorkIntradayParts(date);
  const elapsedIntoQuarter = (parts.minute % 15) * 60 * 1000
    + date.getSeconds() * 1000 + date.getMilliseconds();
  const delay = elapsedIntoQuarter < CHAN_INTRADAY_REFRESH_DELAY_MS
    ? CHAN_INTRADAY_REFRESH_DELAY_MS - elapsedIntoQuarter
    : CHAN_INTRADAY_REFRESH_INTERVAL_MS - elapsedIntoQuarter + CHAN_INTRADAY_REFRESH_DELAY_MS;
  chanIntradayBoundaryTimer = setTimeout(() => {
    chanIntradayBoundaryTimer = null;
    if (state.symbol !== symbol || state.chanPeriod !== "15m") return;
    scheduleChanIntradayBoundaryRefresh(period, symbol);
    refreshChanIntradayAtBoundary(symbol);
  }, delay);
}

document.addEventListener("visibilitychange", () => {
  if (document.visibilityState === "visible" && state.chanPeriod === "15m") {
    refreshChanIntradayAtBoundary(state.symbol);
  }
});

function scheduleChanAnalysisRetry(symbol, requestId, minimumDelay = 0) {
  if (state.chanRetryTimer) return;
  const attempts = state.chanRetryAttempts;
  state.chanRetryAttempts = Math.min(CHAN_ANALYSIS_RETRY_LIMIT, state.chanRetryAttempts + 1);
  const delay = minimumDelay > 0 ? Math.min(300000, minimumDelay)
    : Math.min(300000, 3000 * (2 ** Math.min(attempts, 7)));
  state.chanRetryTimer = setTimeout(() => {
    state.chanRetryTimer = null;
    if (state.symbol === symbol && state.chanRequestId === requestId) refreshChanAnalysis(true);
  }, delay);
}

function isTransientChanRequestError(error) {
  const status = Number(error?.status) || 0;
  return error?.timeout === true || status === 0 || status === 408 || status === 425 || status === 429 || (status >= 500 && status < 600);
}

function chanMarketPayloadNeedsRetry(payload) {
  const analysis = payload?.market_analysis;
  const trend = analysis?.trend;
  const hasTrend = Number(trend?.upper) > 0 && Number(trend?.lower) > 0;
  const hasHistory = Number(payload?.history?.bars) > 0;
  const hasLevels = Array.isArray(analysis?.resistance) && Array.isArray(analysis?.support);
  const warnings = [...(Array.isArray(payload?.warnings) ? payload.warnings : []), payload?.history?.warning,
    analysis?.history?.warning, analysis?.history?.extremes_warning, analysis?.beta_meta?.warning]
    .filter(Boolean).join(" ");
  const blocked = /正在后台|刷新正在进行|租约忙|等待超时|稍后重试/.test(warnings);
  const unsupportedHistory = /不支持历史日线更新/.test(warnings);
  return !unsupportedHistory && (!hasTrend || !hasHistory || !hasLevels || blocked);
}

function scheduleChanMarketRetry(symbol, loadId, minimumDelay = 0) {
  if (symbol !== state.symbol || loadId !== state.loadId || state.chanMarketRetryTimer) return;
  state.chanMarketRetryAttempts = Math.min(60, state.chanMarketRetryAttempts + 1);
  const delay = Math.max(minimumDelay, Math.min(300000, 3000 * (2 ** Math.min(state.chanMarketRetryAttempts - 1, 7))));
  state.chanMarketRetryTimer = setTimeout(() => {
    state.chanMarketRetryTimer = null;
    if (symbol !== state.symbol || loadId !== state.loadId) return;
    if (document.hidden) {
      scheduleChanMarketRetry(symbol, loadId);
      return;
    }
    refreshChanMarketAnalysis(true);
  }, delay);
}

function refreshChanMarketAnalysis(force = false) {
  const symbol = state.symbol;
  const loadId = state.loadId;
  if (!symbol) return;
  if (state.chanMarketInFlight) {
    if (force) state.chanMarketRefreshPending = true;
    return;
  }
  if (!force && Date.now() - state.chanMarketLastRequestedAt < 60000) return;
  const quote = state.lastQuote?.symbol === symbol ? state.lastQuote : null;
  const spot = finitePrice(activeSessionQuote(quote)?.price ?? quote?.price);
  // 首屏同一瞬间会有 quote/chain/analytics 多个回调强制刷新市场分析。
  // 相同快照与现价档位下 1 秒内只发一次，避免连续重复请求。
  const marketKey = `${symbol}|${spotCacheBucket(spot)}|${state.chainFetchedAt || ""}`;
  if (force && marketKey === state.chanMarketLastKey && Date.now() - state.chanMarketLastRequestedAt < 1000) return;
  state.chanMarketInFlight = true;
  state.chanMarketLastRequestedAt = Date.now();
  state.chanMarketLastKey = marketKey;
  const query = spot != null ? `?light=true&spot=${encodeURIComponent(spot)}` : "?light=true";
  request("/api/chan/" + encodeURIComponent(symbol) + query).then((payload) => {
    if (symbol !== state.symbol || loadId !== state.loadId) return;
    if (payload?.symbol !== symbol) {
      scheduleChanMarketRetry(symbol, loadId);
      return;
    }
    state.chanMarketPayload = payload;
    const latestQuote = state.lastQuote?.symbol === symbol ? state.lastQuote : null;
    const liveSpot = finitePrice(activeSessionQuote(latestQuote)?.price ?? latestQuote?.price) ?? spot;
    hydrateTrendFromChan(payload, liveSpot,
      displayedSnapshotContextMatches() || sameDisplayedLevelContext());
    if (chanMarketPayloadNeedsRetry(payload)) scheduleChanMarketRetry(symbol, loadId);
    else {
      state.chanMarketRetryAttempts = 0;
      if (state.chanMarketRetryTimer) clearTimeout(state.chanMarketRetryTimer);
      state.chanMarketRetryTimer = null;
    }
  }).catch((error) => {
    if (isTransientRequestError(error)) scheduleChanMarketRetry(symbol, loadId, error.retryAfterMs || 0);
  }).finally(() => {
    if (symbol === state.symbol && loadId === state.loadId) {
      state.chanMarketInFlight = false;
      if (state.chanMarketRefreshPending) {
        state.chanMarketRefreshPending = false;
        refreshChanMarketAnalysis(true);
      }
    }
  });
}

function refreshChanAnalysis(force = false) {
  const symbol = state.symbol;
  if (!symbol) return;
  if (!state.chanExpanded) {
    // The adjusted response is ~1.5 MiB and only feeds the collapsed chart.
    // Keep the trend/support cards fresh with the lightweight market endpoint
    // and fetch the chart structures when the user expands the panel.
    state.chanStale = true;
    state.chanLoading = false;
    refreshChanMarketAnalysis(true);
    return;
  }
  state.chanStale = false;
  if (state.chanInFlightSymbol === symbol) {
    if (force) state.chanRefreshPending = true;
    return;
  }
  if (!force && Date.now() - state.chanLastRequestedAt < 60000) return;
  const token = ++state.chanRequestId;
  state.chanInFlightSymbol = symbol;
  state.chanLastRequestedAt = Date.now();
  state.chanLoading = true;
  clearTimeout(state.chanRetryTimer);
  state.chanRetryTimer = null;
  const currentQuote = state.lastQuote?.symbol === symbol ? state.lastQuote : null;
  const currentSpot = finitePrice(activeSessionQuote(currentQuote)?.price ?? currentQuote?.price);
  const adjustedParams = new URLSearchParams({ compact: "true" });
  if (currentSpot != null) adjustedParams.set("spot", currentSpot);
  request(`/api/chan/${encodeURIComponent(symbol)}/adjusted?${adjustedParams}`, { timeout: 60000, retry: false }).then((payload) => {
    if (state.symbol !== symbol || state.chanRequestId !== token || payload?.symbol !== symbol) return;
    if (payload?.ready === false) {
      state.chanLoading = false;
      state.view.chan = Object.freeze({ ...state.view.chan, warnings: [...new Set([...(state.view.chan.warnings || []), ...(payload.warnings || [])])] });
      // An analytics event means the history prefetch just finished; bypass the
      // market-analysis throttle so the 0-bar trend/support cards refresh now
      // instead of waiting up to a minute or for the next retry backoff.
      refreshChanMarketAnalysis(force);
      scheduleChanAnalysisRetry(symbol, token, Number(payload.retry_after_ms) || 15000);
      return;
    }
    refreshChanMarketAnalysis(force);
    const liveQuote = state.lastQuote?.symbol === symbol ? state.lastQuote : null;
    const liveSpot = finitePrice(activeSessionQuote(liveQuote)?.price ?? liveQuote?.price);
    // Freeze the raw payload before it enters Vue's reactive tree: it holds
    // thousands of candles/segments that are only ever read, and the view is
    // rebuilt wholesale whenever anything changes.
    state.chanPayload = Object.freeze(OptionScopeChan.withSpot(payload, liveSpot, state.chanPayload));
    state.view.chan = OptionScopeChan.view(state.chanPayload, state.chanScenariosVisible, state.chanMobileChart, 30, state.chanViewports, state.chanChartLayers);
    // Adjusted Chan results intentionally omit market_analysis. The separate
    // /api/chan request feeds the trend and price-level cards above the chart.
    // SSE normally refreshes history completion immediately. Bounded retry
    // also covers symbols without options and browsers without EventSource.
    if (payload.history?.refreshing) scheduleChanAnalysisRetry(symbol, token, 15000);
    else state.chanRetryAttempts = 0;
  }).catch((error) => {
    if (state.symbol !== symbol || state.chanRequestId !== token) return;
    refreshChanMarketAnalysis(force);
    if (!state.view.chan.available) state.view.chan = OptionScopeChan.empty("缠论分析暂不可用，等待历史行情或下次刷新");
    const retrying = isTransientChanRequestError(error);
    state.view.chan = Object.freeze({ ...state.view.chan, warnings: [...new Set([...(state.view.chan.warnings || []), retrying
      ? `结构行情暂时获取失败（${error.message}），正在自动重试`
      : `结构更新失败：${error.message}`])] });
    if (isTransientChanRequestError(error)) scheduleChanAnalysisRetry(symbol, token, error?.retryAfterMs || 0);
  }).finally(() => {
    if (state.chanRequestId !== token) return;
    state.chanLoading = false;
    state.chanInFlightSymbol = "";
    if (state.chanRefreshPending) {
      state.chanRefreshPending = false;
      refreshChanAnalysis(true);
    }
  });
}

function renderQuote(quote, allowStaleValuation = false, allowStaleSQLiteMarket = false) {
  if (!quote || (quote.symbol && quote.symbol !== state.symbol)) return;
  const previous = state.lastQuote;
  let incomingAt = quote?.fetched_at ? new Date(quote.fetched_at).getTime() : null;
  const previousAt = previous?.fetched_at ? new Date(previous.fetched_at).getTime() : null;
  if (!quoteIsFresh(quote)) {
    // A saved SQLite quote is useful immediately on a cold page load even when
    // the provider refresh has not completed. It must never roll back a newer
    // quote already visible for the same symbol.
    if (allowStaleSQLiteMarket && quoteIsUsableSQLiteSnapshot(quote)) {
      // Continue through the normal render path below.
    } else {
      // Valuation events can carry the last market snapshot timestamp. Apply
      // their valuation fields to the latest fresh quote without rolling its
      // price, session label, or analysis back.
      if (!allowStaleValuation || previous?.symbol !== state.symbol || !quoteIsFresh(previous) || !hasFairValueSnapshot(quote)) return;
      const valuationFields = Object.fromEntries(Object.entries(quote).filter(([key]) =>
        key === "fair_value" || key.startsWith("fair_value_")));
      quote = { ...previous, ...valuationFields };
      incomingAt = previousAt;
    }
  }
  // Parallel REST reads can complete after a newer SSE quote. Never let an
  // older market snapshot roll the visible price, session label, or analysis back.
  if (previous?.symbol === state.symbol && Number.isFinite(incomingAt)
    && Number.isFinite(previousAt) && incomingAt < previousAt) return;
  if (quoteIsFresh(quote) && (!Number.isFinite(previousAt) || incomingAt > previousAt)) {
    state.quoteRecoveryAttempts = 0;
  }
  // The quote endpoint reads the persisted snapshot and marks it "sqlite",
  // while its SSE event retains the original provider. Those are the same
  // market data when their timestamps match, so do not flash the cache label
  // just because a later 3-second poll took the SQLite read path.
  if (previous?.symbol === state.symbol && Number.isFinite(incomingAt)
    && Number.isFinite(previousAt) && incomingAt === previousAt
    && previous.source !== "sqlite" && quote.source === "sqlite"
    && !quoteCacheBadgeIsStale(quote)) {
    quote = { ...quote, source: previous.source };
  }
  const mergedQuote = mergeFairValueSnapshot(quote);
  if (!mergedQuote) return;
  quote = mergedQuote;
  clearQuoteRetry();
  const active = activeSessionQuote(quote);
  const price = active?.price ?? quote?.price;
  const reference = quoteReference(quote);
  const referencePrice = finitePrice(reference.price);
  const change = referencePrice != null && finitePrice(price) != null
    ? (Number(price) - referencePrice) / referencePrice * 100
    : (active?.change_percent ?? quote?.change_percent);
  state.view.quoteSymbol = quote?.symbol || state.symbol;
  state.view.quotePrice = formatMoney(price);
  state.view.quoteChange = change == null ? "涨跌 --" : `涨跌 ${change >= 0 ? "+" : ""}${Number(change).toFixed(2)}%`;
  // 涨跌色统一走 --up / --down：当前全局口径是绿涨红跌，变量名不再写死颜色。
  state.view.quoteChangeColor = change == null ? "var(--muted)" : (change < 0 ? "var(--down)" : "var(--up)");
  state.view.quoteCurrency = quote?.currency || "USD";
  state.view.quoteMarket = quoteMarketLabel(quote);
  state.view.marketState = marketStateLabel(quote?.market_state, "快照数据");
  state.lastQuote = quote || null;
  scheduleQuoteRecovery(state.lastQuote);
  // 行情可能晚于期权链到达。现价档位变化后重新触发一次价位计算，否则
  // 触达概率、强化色、最佳买卖点和止损会一直停在无 IV 的客户端回退结果，
  // 只能等下一次快照或 analytics 事件。
  if (state.lastAnalysis?.points?.length && state.expiration) {
    const basis = activeBasis(quote);
    const levelSpot = finitePrice(basis?.price ?? quote?.price);
    if (levelSpot != null) {
      const levelsKey = `${state.symbol}|${state.expiration}|${state.chainFetchedAt || ""}|${spotCacheBucket(levelSpot)}|${state.levelBasisMode}`;
      if (state.levelsKey !== levelsKey) loadFactorLevels(state.lastAnalysis.points, levelSpot);
    }
  }
  if (state.chanPayload?.symbol === state.symbol) {
    state.chanPayload = Object.freeze(OptionScopeChan.withSpot(state.chanPayload, finitePrice(price)));
    state.view.chan = OptionScopeChan.view(state.chanPayload, state.chanScenariosVisible, state.chanMobileChart, 30, state.chanViewports, state.chanChartLayers);
  }
  if (CHAN_INTRADAY_PERIODS.some((period) => state.chanIntradayPayload[period])) {
    state.chanIntradaySpot = finitePrice(price);
  }
  refreshChanAnalysis();
  refreshChanMarketAnalysis();
  state.view.quoteReference = reference.text;
  state.view.quoteReferenceTitle = reference.title;
  if (quoteIsETF(quote)) {
    // Company valuation models do not apply to ETFs. Keep the cards at their
    // neutral defaults without exposing the backend diagnostic as UI copy.
    applyETFValuationDefaults();
    return;
  }
  const hasConservativeValue = quote?.fair_value_source?.startsWith("valuation_")
    && quote?.fair_value != null;
  const low = Number(quote?.fair_value_low);
  const high = Number(quote?.fair_value_high);
  const buyLow = Number(quote?.fair_value_buy_low);
  const buyHigh = Number(quote?.fair_value_buy_high);
  const defensive = quote?.fair_value_defensive;
  const optimistic = quote?.fair_value_optimistic;
  const fairValueStatus = String(quote?.fair_value_status || "pending");
  const fairValueUnavailable = ["unavailable", "failed"].includes(fairValueStatus);
  const fairValueRetrying = fairValueStatus === "retry";
  const fairValueWarning = quote?.fair_value_warning || "公开财务数据不足，暂不输出估值";
  const defensiveModel = defensive?.model || "保守情景估值";
  const optimisticModel = optimistic?.model || "乐观情景估值";
  const formatRange = (item) => item && Number.isFinite(Number(item.low)) && Number.isFinite(Number(item.high))
    ? `${formatMoney(item.low)}-${formatMoney(item.high)}` : "--";
  const formatRevenue = (value) => {
    const amount = Number(value);
    if (!Number.isFinite(amount)) return null;
    if (amount >= 1e12) return `$${(amount / 1e12).toFixed(2)}T`;
    if (amount >= 1e9) return `$${(amount / 1e9).toFixed(2)}B`;
    if (amount >= 1e6) return `$${(amount / 1e6).toFixed(1)}M`;
    return `$${formatMoney(amount)}`;
  };
  const scenarioInputs = (item) => {
    if (!item) return "";
    const parts = [];
    if (item.forward_eps != null) parts.push(`远期 EPS ${formatMoney(item.forward_eps)}`);
    if (item.growth_rate != null) parts.push(`增长率 ${(Number(item.growth_rate) * 100).toFixed(1)}%`);
    for (const [key, label] of [
      ["revenue", "已实现收入"],
      ["current_year_revenue_estimate", "本年收入预测"],
      ["next_year_revenue_estimate", "下一年收入预测"],
    ]) {
      const formatted = item[key] == null ? null : formatRevenue(item[key]);
      if (formatted) parts.push(`${label} ${formatted}`);
    }
    return parts.length ? `${parts.join("；")}。` : "";
  };
  state.view.fairValue = hasConservativeValue
    ? `保守 ${formatRange(defensive || { low, high })} · 乐观 ${formatRange(optimistic)}`
    : "--";
  const defensiveBuy = defensive ? formatRange({ low: defensive.buy_low, high: defensive.buy_high }) : "--";
  const optimisticBuy = optimistic ? formatRange({ low: optimistic.buy_low, high: optimistic.buy_high }) : "--";
  state.view.fairValueSource = !hasConservativeValue
    ? (fairValueRetrying ? (fairValueWarning || "Yahoo 数据源暂时限流，正在重试") : (fairValueUnavailable ? fairValueWarning : "暂无可靠估算"))
    : `保守买入 ${defensiveBuy} · 乐观买入 ${optimisticBuy}`;
  state.view.fairValueTitle = hasConservativeValue
    ? `模型 A：${defensiveModel}，防守区间 ${formatRange(defensive)}；模型 B：${optimisticModel}，进攻区间 ${formatRange(optimistic)}。远期 EPS 来源：${quote?.fair_value_forward_eps_source || "暂无"}。确定性：${quote?.fair_value_confidence_score != null ? `${quote.fair_value_confidence_score}%（${quote.fair_value_confidence || "中"}）` : (quote?.fair_value_confidence || "暂无")}。市场状态：${quote?.fair_value_regime || "NORMAL"}`
     : (fairValueRetrying ? (fairValueWarning || "Yahoo 数据源暂时限流，正在重试") : (fairValueUnavailable ? fairValueWarning : "公开财务数据不足，暂不输出保守估值"));
  state.view.fairDefensive = hasConservativeValue ? formatRange(defensive || { low, high }) : "--";
  state.view.fairDefensiveSource = hasConservativeValue && defensive
    ? `买入 ${defensiveBuy} · 安全边际 ${(Number(defensive.safety_margin || 0) * 100).toFixed(0)}%`
     : (fairValueRetrying ? (fairValueWarning || "Yahoo 数据源暂时限流，正在重试") : (fairValueUnavailable ? fairValueWarning : (hasConservativeValue ? "模型 A 数据不足" : "等待估值计算")));
  state.view.fairDefensiveTitle = hasConservativeValue
    ? `保守估值：${defensiveModel}。${scenarioInputs(defensive)}估值 ${state.view.fairDefensive}，${state.view.fairDefensiveSource}。确定性 ${quote?.fair_value_confidence_score != null ? `${quote.fair_value_confidence_score}%` : (quote?.fair_value_confidence || "暂无")}。${quote?.fair_value_regime === "WINTER" ? "AI 寒冬：禁止仅凭远期 EPS 抄底" : ""}`
    : (fairValueRetrying ? (fairValueWarning || "Yahoo 数据源暂时限流，正在重试") : (fairValueUnavailable ? fairValueWarning : "等待保守情景估值"));
  state.view.fairOptimistic = optimistic ? formatRange(optimistic) : "--";
  state.view.fairOptimisticSource = optimistic
    ? `买入 ${optimisticBuy} · 安全边际 ${(Number(optimistic.safety_margin || 0) * 100).toFixed(0)}%`
     : (fairValueRetrying ? (fairValueWarning || "Yahoo 数据源暂时限流，正在重试") : (fairValueUnavailable ? fairValueWarning : (hasConservativeValue ? "暂无可靠远期盈利依据" : "等待估值计算")));
  state.view.fairOptimisticTitle = optimistic
    ? `乐观估值：${optimisticModel}。${scenarioInputs(optimistic)}${state.view.fairOptimisticSource}`
    : (fairValueRetrying ? (fairValueWarning || "Yahoo 数据源暂时限流，正在重试") : "当前没有足够数据生成乐观情景估值");
  scheduleFairValuePoll(quote);
}

function clearFairValuePoll() {
  if (state.fairValuePollTimer) clearTimeout(state.fairValuePollTimer);
  state.fairValuePollTimer = null;
  state.fairValuePollKey = "";
  state.fairValuePollAttempts = 0;
  state.fairValuePollInFlight = false;
}

function scheduleFairValuePoll(quote) {
  if (quoteIsETF(quote)) {
    clearFairValuePoll();
    return;
  }
  const ready = quote?.fair_value_source?.startsWith("valuation_") && quote?.fair_value != null;
  const terminal = ["unavailable", "failed"].includes(String(quote?.fair_value_status || ""));
  if (ready || terminal) clearFairValuePoll();
  if (ready || terminal || !state.symbol || state.fairValuePollTimer || state.fairValuePollInFlight) return;
  if (state.fairValuePollAttempts >= FAIR_VALUE_POLL_LIMIT) return;
  const key = `${state.loadId}|${state.symbol}`;
  state.fairValuePollKey = key;
  state.fairValuePollTimer = setTimeout(() => pollFairValue(key), FAIR_VALUE_POLL_INTERVAL_MS);
}

async function pollFairValue(key) {
  state.fairValuePollTimer = null;
  if (key !== `${state.loadId}|${state.symbol}` || state.fairValuePollInFlight) return;
  state.fairValuePollInFlight = true;
  state.fairValuePollAttempts += 1;
  let resolved = false;
  try {
    const quote = await request(`/api/quote/${encodeURIComponent(state.symbol)}`).catch(() => null);
    if (key !== `${state.loadId}|${state.symbol}`) return;
    if ((quote?.fair_value_source?.startsWith("valuation_") && quote?.fair_value != null)
      || ["retry", "unavailable", "failed"].includes(String(quote?.fair_value_status || ""))) {
      resolved = true;
      renderQuote(quote, true);
      if (quote?.fair_value_status === "retry") {
        resolved = false;
        scheduleFairValuePoll(quote);
      }
      return;
    }
  } finally {
    state.fairValuePollInFlight = false;
    if (!resolved && key === `${state.loadId}|${state.symbol}` && state.fairValuePollAttempts < FAIR_VALUE_POLL_LIMIT) {
      scheduleFairValuePoll(null);
    }
  }
}

function emptyFlowSide(type) {
  return {
    buyVolume: "--",
    sellVolume: "--",
    unknownVolume: "--",
    buyPremium: "权利金 --",
    sellPremium: "权利金 --",
    netVolume: "--",
    focusTitle: "集中执行价 --",
    focusSummary: "等待相邻快照",
    concentration: [],
    rows: [{ key: `${type}-empty`, label: "--", direction: "等待数据", className: "unknown", volume: "--", share: "--", premium: "权利金 --" }],
    rowspan: 1,
  };
}

function renderFlowSide(side, type) {
  const source = side || {};
  const netVolume = Number(source.net_volume);
  const rawConcentration = Array.isArray(source.concentration) && source.concentration.length
    ? source.concentration
    : (Array.isArray(source.top_strikes) ? source.top_strikes : []).map((item) => ({
      strike: item.strike,
      volume: item.volume,
      dominant_direction: item.direction,
      buy_volume: item.direction === "buy" ? item.volume : 0,
      sell_volume: item.direction === "sell" ? item.volume : 0,
      unknown_volume: item.direction === "unknown" ? item.volume : 0,
    }));
  const directionLabel = (value) => value === "buy" ? "买入" : value === "sell" ? "卖出" : "中性";
  const typeLabel = type === "call" ? "CALL" : "PUT";
  const totalVolume = [source.buy_volume, source.sell_volume, source.unknown_volume].reduce((sum, value) => sum + (Number(value) || 0), 0);
  const concentration = rawConcentration.slice(0, 5).map((item, index) => ({
    key: `${type}-${item.strike}-${index}`,
    label: `${formatFlowStrike(item.strike)} ${typeLabel}`,
    detail: `${directionLabel(item.dominant_direction)} ${formatFlowVolume(item.volume, item.dominant_direction)}`,
    className: item.dominant_direction === "buy" ? "buy" : item.dominant_direction === "sell" ? "sell" : "unknown",
  }));
  const rows = rawConcentration.slice(0, 5).map((item, index) => {
    const volume = Number(item.volume) || 0;
    const dominantDirection = item.dominant_direction;
    return {
      key: `${type}-row-${item.strike}-${index}`,
      label: `${formatFlowStrike(item.strike)} ${typeLabel}`,
      direction: directionLabel(dominantDirection),
      className: dominantDirection === "buy" ? "buy" : dominantDirection === "sell" ? "sell" : "unknown",
      volume: formatFlowVolume(item.volume, dominantDirection),
      share: totalVolume > 0 ? `${Math.round(volume / totalVolume * 100)}%` : "--",
      premium: `权利金 ${formatUsd(item.premium)}`,
    };
  });
  if (!rows.length) rows.push({ key: `${type}-empty`, label: "--", direction: "等待数据", className: "unknown", volume: "--", share: "--", premium: "权利金 --" });
  const top = rawConcentration[0] || null;
  const focusPercent = top && totalVolume > 0 ? ` · 占 ${Math.round(Number(top.volume) / totalVolume * 100)}%` : "";
  return {
    buyVolume: formatFlowVolume(source.buy_volume, "buy"),
    sellVolume: formatFlowVolume(source.sell_volume, "sell"),
    unknownVolume: `中性 ${formatCount(source.unknown_volume)}`,
    buyPremium: `权利金 ${formatUsd(source.buy_premium)}`,
    sellPremium: `权利金 ${formatUsd(source.sell_premium)}`,
    netVolume: Number.isFinite(netVolume) ? `${netVolume >= 0 ? "+" : ""}${formatCount(netVolume)}` : "--",
    focusTitle: top ? `${formatFlowStrike(top.strike)} ${typeLabel}` : "集中执行价 --",
    focusSummary: top ? `${directionLabel(top.dominant_direction)} ${formatFlowVolume(top.volume, top.dominant_direction)}${focusPercent}` : "等待相邻快照",
    concentration,
    rows,
    rowspan: rows.length,
  };
}

function resetOptionFlow(reason = "需要两份相邻期权链快照才能估算流向") {
  state.view.flow = {
    available: false,
    signalLabel: "等待相邻快照",
    signalClass: "waiting",
    signalDetail: reason,
    windowLabel: "快照窗口 --",
    methodLabel: "成交量增量 · 买卖价方向估算 · 中性单列",
    warning: "",
    summary: {
      totalVolume: "--",
      buyVolume: "--",
      sellVolume: "--",
      neutralVolume: "--",
    },
    call: emptyFlowSide("call"),
    put: emptyFlowSide("put"),
  };
}

function hasOptionFlowData(payload) {
  if (!payload?.available) return false;
  return [payload.call, payload.put].some((side) => {
    if (!side) return false;
    const totals = [side.buy_volume, side.sell_volume, side.unknown_volume];
    if (totals.some((value) => Number.isFinite(Number(value)) && Number(value) > 0)) return true;
    return [side.concentration, side.top_strikes].some((rows) =>
      Array.isArray(rows) && rows.some((row) => Number.isFinite(Number(row?.volume)) && Number(row.volume) > 0));
  });
}

function renderOptionFlow(payload) {
  // 无新增成交、接口暂不可用或迟到的旧期限响应都不覆盖当前有效流向；
  // 换标的/期限时由相应加载流程显式 resetOptionFlow。
  if (!payload || (payload.expiration && state.expiration && payload.expiration !== state.expiration)
    || !hasOptionFlowData(payload)) return;
  const signal = payload.signal || {};
  const hasWindow = Boolean(payload.current_fetched_at && payload.previous_fetched_at);
  const windowLabel = hasWindow
    ? `${formatTime(payload.previous_fetched_at)} → ${formatTime(payload.current_fetched_at)} · ${formatFlowInterval(payload.interval_seconds)}`
    : payload.current_fetched_at ? `当前快照 ${formatTime(payload.current_fetched_at)} · 等待下一份` : "快照窗口 --";
  const call = payload.call || {};
  const put = payload.put || {};
  const sumVolume = (key) => (Number(call[key]) || 0) + (Number(put[key]) || 0);
  state.view.flow = {
    available: Boolean(payload.available),
    signalLabel: payload.available ? (signal.label || "暂无新增流向") : "等待相邻快照",
    signalClass: payload.available ? (signal.class_name || "neutral") : "waiting",
    signalDetail: payload.available ? (signal.detail || payload.reason || "暂无方向判断") : (payload.reason || "需要两份相邻期权链快照才能估算流向"),
    windowLabel,
    methodLabel: "成交量增量 · 买卖价方向估算 · 中性单列",
    warning: payload.warning || "",
    summary: {
      totalVolume: formatCount(sumVolume("buy_volume") + sumVolume("sell_volume") + sumVolume("unknown_volume")),
      buyVolume: formatFlowVolume(sumVolume("buy_volume"), "buy"),
      sellVolume: formatFlowVolume(sumVolume("sell_volume"), "sell"),
      neutralVolume: formatCount(sumVolume("unknown_volume")),
    },
    call: renderFlowSide(payload.call, "call"),
    put: renderFlowSide(payload.put, "put"),
  };
}

// 时段标签：数据源若返回未知取值就原样展示，避免换口径时把信息吞掉。
function marketStateLabel(value, fallback = "快照") {
  if (!value) return fallback;
  return MARKET_STATE_LABELS[value] || value;
}

// Alpaca 夜盘价可用时明确标注；没有夜盘价时沿用旧的收盘价回退标签。
function quoteMarketLabel(quote) {
  if (quote?.market_state !== "OVERNIGHT") return marketStateLabel(quote?.market_state);
  if (quote?.sessions?.overnight?.provider === "alpaca-overnight") return "夜盘";
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
function contractGex(row, price, priceSquared, spotKey) {
  if (!Number.isFinite(price) || price <= 0) return null;
  const gamma = gammaAtSpot(price, row, undefined, spotKey);
  const value = gamma == null ? Number(row.gamma) : Number(gamma);
  if (!Number.isFinite(value)) return null;
  const sign = row.contract_type === "call" ? 1 : -1;
  return sign * value * (Number(row.open_interest) || 0) * 100 * priceSquared * 0.01;
}

// 图例：说明底色口径，并给出两列的归一基准（本屏最强值）。
function chainHeatNote(volumePeak, interestPeak) {
  if (!(volumePeak > 0) && !(interestPeak > 0)) return "行权价绿=看涨、红=看跌 · 本屏成交量与未平仓均为 0，暂无可比强度";
  return `行权价绿=看涨、红=看跌 · 底色深浅按该列本屏最强值归一（√ 缩放） · 成交量最强 ${formatNumber(volumePeak)} · 未平仓最强 ${formatNumber(interestPeak)}`;
}

// 期权链表格：行权价在最左，文字颜色即类型（看涨绿 / 看跌红，原先单独的「类型」列已并入行权价）；
// 成交量与未平仓在报价前展示并按本屏强弱铺底色，买价/卖价直接展示当前期权报价，另给出 GEX 估值列（与 Gamma 敞口图同口径）。
function chainRowsFingerprint(rows, spot, emptyLabel) {
  // 32 位滚动哈希，避免为上千行构造 quoteSignature 字符串数组再 join。
  // bid/ask 的 null 用独立哨兵，保留与 0.00 的区别。
  let hash = rows.length >>> 0;
  for (let index = 0; index < rows.length; index += 1) {
    const row = rows[index];
    const bid = row.bid == null ? 0x7fffffff : Math.round((Number(row.bid) || 0) * 100);
    const ask = row.ask == null ? 0x7fffffff : Math.round((Number(row.ask) || 0) * 100);
    hash = (Math.imul(hash, 31)
      + (Number(row.volume) || 0)
      + (Number(row.open_interest) || 0)
      + Math.round((Number(row.strike) || 0) * 100)
      + bid + ask) >>> 0;
  }
  return `${state.chainFetchedAt}|${spot}|${state.chainFilter}|${emptyLabel}|${hash}`;
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
  let volumePeak = 0;
  let interestPeak = 0;
  for (const row of rows) {
    const volume = Number(row.volume) || 0;
    const interest = Number(row.open_interest) || 0;
    if (volume > volumePeak) volumePeak = volume;
    if (interest > interestPeak) interestPeak = interest;
  }
  const spotValue = Number(spot);
  const spotSquared = Number.isFinite(spotValue) && spotValue > 0 ? spotValue * spotValue : 0;
  const spotKey = gammaSpotBucket(spotValue);
  state.view.chainHeatNote = chainHeatNote(volumePeak, interestPeak);
  state.view.chainEmpty = emptyLabel;
  // 表格数据整体替换、从不原地修改；冻结数组可让 Vue 2 跳过上千行对象的深度观测。
  state.view.chainRows = Object.freeze(rows.map((row, index) => {
    const isCall = row.contract_type === "call";
    const gex = contractGex(row, spotValue, spotSquared, spotKey);
    const volume = heatCellModel(row.volume, volumePeak, "成交量", isCall ? HEAT_HOT_LEVEL.call : HEAT_HOT_LEVEL.put);
    const interest = heatCellModel(row.open_interest, interestPeak, "未平仓", isCall ? HEAT_HOT_LEVEL.call : HEAT_HOT_LEVEL.put);
    return {
      key: row.contract_symbol || `${row.contract_type || "option"}-${row.strike}-${index}`,
      rowClass: isCall ? "chain-call" : "chain-put",
      typeClass: isCall ? "type-call" : "type-put",
      strike: formatMoney(row.strike),
      bid: formatOptionQuote(row.bid),
      ask: formatOptionQuote(row.ask),
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
  }));
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
  if (payload?.symbol && payload.symbol !== state.symbol) return;
  // 晚到的旧期限不能把下拉框拽回去，也不能盖掉用户刚选的链。
  if (payload?.expiration && state.expiration && payload.expiration !== state.expiration) return;
  const rows = Array.isArray(payload?.data) ? payload.data : [];
  const incomingExpiration = payload?.expiration || state.expiration;
  // SQLite/upstream may briefly return an empty chain while another refresh is
  // replacing it. Do not let that transient response erase a visible snapshot;
  // explicit no-options handling clears the page through loadQuoteOnly instead.
  if (!rows.length
    && state.displayedChainSymbol === state.symbol
    && state.displayedChainExpiration === incomingExpiration) {
    state.view.lastStatus = "期权链暂时返回空数据，保留上一份显示并等待更新";
    scheduleSnapshotRetry(state.loadId);
    return;
  }
  const incomingFetchedAt = Date.parse(payload?.fetched_at || "");
  const displayedFetchedAt = Date.parse(state.displayedChainFetchedAt || "");
  if (rows.length
    && state.displayedChainSymbol === state.symbol
    && state.displayedChainExpiration === incomingExpiration
    && Number.isFinite(incomingFetchedAt)
    && Number.isFinite(displayedFetchedAt)
    && incomingFetchedAt < displayedFetchedAt) return;
  // The price-sensitive analysis needs a recent quote and a chain snapshot
  // from the same short refresh window. A stale response is refreshed by the
  // caller instead of briefly painting values that will immediately move.
  const cachedChain = payload?.source === "sqlite";
  quote = cachedChain
    ? latestRenderableQuote(state.symbol, quote, state.lastQuote)
    : latestFreshQuote(state.symbol, quote, state.lastQuote);
  if (!quoteAndChainAreAligned(quote, payload?.fetched_at, cachedChain)) {
    scheduleSnapshotRetry(state.loadId);
    return;
  }
  if (hasValidOptionQuotes(rows) && snapshotTimestampIsFresh(payload?.fetched_at)
    && state.expirationPlaceholder !== "正在获取到期日…") clearSnapshotRetry();
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
  if (signature === chainRenderSignature && state.view.chainRows.length) {
    if (state.pendingAnalyticsRefresh && state.lastAnalysis?.points?.length) schedulePushedAnalyticsRefresh();
    return;
  }
  state.expiration = payload.expiration;
  // 基准价开关切换时要用最近一次快照重算，这里留一份引用。
  state.lastQuote = quote || null;
  state.view.chainTitle = `期权链 · ${payload.symbol} · ${payload.expiration}`;
  // 期权链始终从本地缓存读取，这里按快照新鲜度标注来源，避免刚抓完还显示“缓存”造成误解。
  const snapshotAge = payload.fetched_at ? (Date.now() - new Date(payload.fetched_at).getTime()) / 1000 : null;
  // 上游在盘前/收盘后可能整链返回 0 未平仓量，读取层会用该合约最近一次有效值兜底，这里如实标注。
  const oiFallback = payload.oi_fallback || {};
  state.view.dataSource = (snapshotAge != null && snapshotAge >= 0 && snapshotAge < SNAPSHOT_FRESH_SECONDS ? "上游新快照" : "本地缓存") + (oiFallback.restored ? ` · 未平仓量回溯 ${formatDay(oiFallback.as_of)}` : "");
  state.view.fetchedAt = `快照时间 ${formatTime(payload.fetched_at)}`;
  state.view.totalCount = formatNumber(rows.length);
  let callVolume = 0, putVolume = 0, callInterest = 0, putInterest = 0;
  for (const row of rows) {
    const volume = Number(row.volume) || 0;
    const interest = Number(row.open_interest) || 0;
    if (row.contract_type === "call") { callVolume += volume; callInterest += interest; }
    else if (row.contract_type === "put") { putVolume += volume; putInterest += interest; }
  }
  state.view.callVolume = formatCount(callVolume);
  state.view.putVolume = formatCount(putVolume);
  state.view.callInterest = `未平仓 ${formatCount(callInterest)}`;
  state.view.putInterest = `未平仓 ${formatCount(putInterest)}`;
  const analysisRows = analysisPayload?.data?.length ? analysisPayload.data : rows;
  // 记录本次快照时间：压力位/支撑位的合成接口按「标的 + 到期日 + 快照时间」去重请求。
  state.chainFetchedAt = payload.fetched_at || null;
  state.chainFetchedSymbol = payload.symbol || state.symbol;
  state.chainFetchedExpiration = payload.expiration || state.expiration;
  state.displayedChainSymbol = payload.symbol || state.symbol;
  state.displayedChainExpiration = payload.expiration || state.expiration;
  state.displayedChainFetchedAt = payload.fetched_at || null;
  renderAnalysis(analysisRows, quote?.price, analysisPayload, rows, payload.iv_model || {}, activeBasis(quote));
  if (state.pendingAnalyticsRefresh && state.lastAnalysis?.points?.length) schedulePushedAnalyticsRefresh();
  state.chainRows = Object.freeze(rows); state.chainSpot = quote?.price ?? null;
  renderChainTable();
  chainRenderSignature = signature;
  rememberClientSnapshot();
}

// emptyLabel：本地没有到期日时的占位文案，需要区分「还没抓过」（正在获取）和「该标的没有期权」。
function applyExpirations(dates, preferred, emptyLabel = "正在获取到期日…") {
  const unique = [...new Set((dates || []).filter(Boolean))];
  state.expirationOptions = unique;
  state.expirationPlaceholder = emptyLabel;
  if (!unique.length) {
    return false;
  }
  const requested = preferred || parsePageQuery(location.search).expiration || state.expiration;
  state.expiration = unique.includes(requested) ? requested : unique[0];
  return true;
}

function preferredCachedExpiration(dates, preferred, cachedDates) {
  const ordered = [...new Set((dates || []).filter(Boolean))].sort();
  if (preferred && ordered.includes(preferred)) return preferred;
  // On a new symbol, start with the nearest active expiry even if only a
  // later expiry happens to be cached. The selected chain can then refresh
  // independently while the remaining expiries load in the Gamma queue.
  return ordered[0] || preferred || [...new Set((cachedDates || []).filter(Boolean))].sort()[0] || null;
}

async function renderCachedHistoryOnly(loadId, quote = null) {
  const symbol = state.symbol;
  const [quotePayload, cachedLevels] = await Promise.all([
    Promise.resolve(quote).then((value) => value || request(`/api/quote/${encodeURIComponent(symbol)}`).catch(() => null)),
    request(`/api/levels/${encodeURIComponent(symbol)}?raw=true`).catch(() => null),
  ]);
  if (!isCurrentLoad(loadId)) return;
  const usableQuote = latestRenderableQuote(symbol, quotePayload, state.lastQuote);
  if (usableQuote) renderQuote(usableQuote, false, quoteIsUsableSQLiteSnapshot(usableQuote));
  if (cachedLevels?.raw && cachedLevels?.fast_levels) {
    const levelsPayload = { ...cachedLevels, ...cachedLevels.fast_levels, raw: true };
    renderFactorLevels(levelsPayload);
    if (Number(levelsPayload.history?.bars) > 0) state.trendHistorySymbol = symbol;
  } else if (state.chanPayload?.symbol === symbol && state.chanPayload.market_analysis) {
    hydrateTrendFromChan(state.chanPayload);
  }
  // The queued snapshot refresh can finish while the slower daily-level read
  // is pending. Preserve both the rendered chain and history panels, and skip
  // only the history-only chain placeholder below.
  if (state.displayedChainSymbol === symbol
    && state.displayedChainExpiration === state.expiration
    && state.chainRows?.length) return;
  state.view.chainTitle = `期权链 · ${symbol}`;
  state.view.dataSource = "SQLite 行情/日线 · 无期权链缓存";
  state.view.fetchedAt = usableQuote?.fetched_at ? `行情 ${formatTime(usableQuote.fetched_at)}` : "SQLite 暂无行情缓存";
  state.view.totalCount = "0";
  state.view.callVolume = "0";
  state.view.putVolume = "0";
  state.view.callInterest = "未平仓 0";
  state.view.putInterest = "未平仓 0";
  state.view.chainRows = [];
  state.view.chainEmpty = "SQLite 目前没有期权合约缓存";
  state.view.lastStatus = "已读取 SQLite 行情与日线缓存；该标的暂无期权链缓存";
}

function isCurrentLoad(loadId, expiration) {
  return state.loadId === loadId && (!expiration || state.expiration === expiration);
}

function quoteIsReady(quote) {
  const price = Number(activeSessionQuote(quote)?.price ?? quote?.price);
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
  return Boolean(snapshot?.shown && snapshot?.quoteReady && snapshot?.optionsQuotesReady)
    && age !== null && age <= SNAPSHOT_FRESH_SECONDS
    && quoteAndChainAreAligned(snapshot?.quote, snapshot?.fetchedAt);
}

function showFreshStatus(snapshot) {
  const age = snapshotAgeSeconds(snapshot?.fetchedAt);
  state.view.lastStatus = `本地快照 ${formatTime(snapshot?.fetchedAt)} 已是最新（${Math.round(age ?? 0)} 秒前）`;
}

// 四张图在上游快照回来前先铺 0 值坐标和汇总表，数据落地后由正式绘制整图替换。
function showChartSkeletons() {
  state.view.chart.netGex = "净 Gamma 0 · 估算";
  state.view.chart.gammaFlip = "零 Gamma 0.00 · 估算";
  state.view.chart.netGexTitle = GAMMA_ESTIMATE_TITLE;
  state.view.chart.gammaFlipTitle = GAMMA_ESTIMATE_TITLE;
  state.view.chart.callWall = "看涨墙 0.00";
  state.view.chart.putWall = "看跌墙 0.00";
  state.view.chart.gammaScopeDate = "柱状图 到期日 --";
  state.view.chart.gammaScopeStats = "0 个合约";
  state.view.chart.levelsBasis = "基准 0.00";
  state.view.chart.volumeScope = "到期日 -- · 0 个合约";
  state.view.chart.oiScope = "到期日 -- · 0 个合约";
  OptionScopeCharts.renderChartSkeleton("gex-chart");
  OptionScopeCharts.renderChartSkeleton("volume-chart", { axis: "right" });
  OptionScopeCharts.renderChartSkeleton("oi-chart", { axis: "right" });
  OptionScopeCharts.renderChartSkeleton("levels-chart", { zeroLabel: "0.00", corners: "levels" });
  OptionScopeCharts.renderDistributionSummary(byId("volume-summary"), [], null, "volume", "总成交量");
  OptionScopeCharts.renderDistributionSummary(byId("oi-summary"), [], null, "open_interest", "总持仓量");
}

function showPending(message, preserveCache = false) {
  if (preserveCache && markApiUnavailable(message)) return;
  // 自动刷新会先清空 chainFetchedSymbol/chainFetchedAt 使 SSE 响应重新校验，
  // 但这不代表屏幕上的旧快照无效。同标的同期限仍有显示时只更新状态，不回退成 0 值骨架。
  if (state.displayedChainSymbol === state.symbol
    && state.displayedChainExpiration === state.expiration) {
    state.view.lastStatus = `${message} · 保留当前显示，等待更新`;
    return;
  }
  const keepFairValue = state.lastQuote
    && state.lastQuote.symbol === state.symbol
    && hasFairValueSnapshot(state.lastQuote);
  clearFairValuePoll();
  resetOptionFlow(message || "需要两份相邻期权链快照才能估算流向");
  if (!keepFairValue) {
    state.view.fairValue = "--";
    state.view.fairValueSource = "保守情景 -- · 乐观情景 --";
    state.view.fairValueTitle = "公开财务数据不足，暂不输出保守估值";
    state.view.fairDefensive = "--";
    state.view.fairDefensiveSource = "等待估值计算";
    state.view.fairOptimistic = "--";
    state.view.fairOptimisticSource = "等待估值数据";
    if (quoteIsETF(null)) applyETFValuationDefaults();
  }
  state.view.chainTitle = `期权链 · ${state.symbol}${state.expiration ? ` · ${state.expiration}` : ""}`;
  state.view.dataSource = "后台刷新中";
  state.view.fetchedAt = "快照时间 --";
  state.view.totalCount = "--";
  state.view.callVolume = "--";
  state.view.putVolume = "--";
  state.view.callInterest = "未平仓 --";
  state.view.putInterest = "未平仓 --";
  state.view.lastStatus = message;
  showChartSkeletons();
  // 压力位/支撑位会在新快照渲染后重新请求合成接口，这里先清空并解除去重键。
  clearTimeout(state.levelsRetryTimer);
  state.levelsRetryTimer = null;
  state.levelsKey = "";
  state.levelsPayload = null;
  state.serverLevelsPayload = null;
  state.serverLevelsContext = "";
  state.serverLevelsRequestKey = "";
  state.serverLevelsPayloadKey = "";
  state.serverLevelsInFlightContext = "";
  state.serverLevelsInFlightKey = "";
  state.serverLevelsCooldownUntil = 0;
  state.displayedLevelsContext = "";
  const cachedHistoryAdd = state.trendHistorySymbol === state.symbol
    ? state.view.levels.add.filter((level) => !level.placeholder) : [];
  state.view.levels = placeholderLevels(message || "正在加载");
  if (cachedHistoryAdd.length) {
    state.view.levels.add = cachedHistoryAdd;
    state.view.levels.addEmpty = "";
  }
  state.view.buyer = {
    available: false,
    directionLabel: "",
    horizonLabel: "未来 5 个交易日",
    target: "",
    items: [],
    reason: message,
    note: "",
  };
  state.buyerContext = `${state.symbol}|${state.expiration}|${state.levelBasisMode}`;
  // 压力位图已在 showChartSkeletons 里铺好 0 值坐标，这里不再改回一句加载提示。
  if (state.trendHistorySymbol !== state.symbol) state.view.trend = placeholderTrend(message || "正在加载");
  state.view.chainRows = [];
  state.displayedChainSymbol = "";
  state.displayedChainExpiration = "";
  state.displayedChainFetchedAt = null;
  state.view.chainEmpty = message;
  state.view.chainHeatNote = "等待数据";
  chainRenderSignature = "";
  chainBodySignatureValue = "";
  analysisChartSignatureValue = "";
  // 旧标的的分析不能在等待新快照时被窗口缩放画回来。
  state.lastAnalysis = null;
  cancelScopeCharts();
  // Keep the SQLite daily-history analysis visible while an option snapshot is
  // unavailable. It is independent of the selected expiry and can repopulate
  // the trend, add, support and resistance panels after their pending reset.
  if (state.chanPayload?.symbol === state.symbol && state.chanPayload.market_analysis) {
    hydrateTrendFromChan(state.chanPayload);
  }
}

function applyCachedQuote(quote) {
  if (!quote || !quoteIsReady(quote)) {
    if (state.lastQuote && state.lastQuote.symbol === state.symbol && quoteIsReady(state.lastQuote)) return;
    state.lastQuote = null;
    state.view.quoteSymbol = state.symbol;
    state.view.quotePrice = "--";
    state.view.quoteChange = "正在更新";
    state.view.quoteChangeColor = "var(--muted)";
    state.view.quoteCurrency = "USD";
    state.view.quoteMarket = "后台刷新";
    state.view.marketState = "后台刷新中";
    const reference = quoteReference(null);
    state.view.quoteReference = reference.text;
    state.view.quoteReferenceTitle = reference.title;
    state.view.fairValue = "--";
    state.view.fairValueSource = "保守情景 -- · 乐观情景 --";
    state.view.fairValueTitle = "公开财务数据不足，暂不输出保守估值";
    state.view.fairDefensive = "--";
    state.view.fairDefensiveSource = "等待估值计算";
    state.view.fairOptimistic = "--";
    state.view.fairOptimisticSource = "等待估值数据";
    if (quoteIsETF(null)) applyETFValuationDefaults();
    return;
  }
  renderQuote(quote, false, quoteIsUsableSQLiteSnapshot(quote));
}

async function renderSnapshot(loadId, fallbackQuote = null) {
  if (!isCurrentLoad(loadId)) return { shown: false, source: null, discarded: true };
  // 记住这次请求对应的到期日：响应回来时如果用户已经切走，整份数据作废（见 isCurrentLoad 的第二个参数），
  // 否则后到的旧响应会把新选择的数据覆盖掉。
  const expiration = state.expiration;
  const symbol = state.symbol;
  const reuseKey = `${loadId}|${symbol}|${expiration || ""}`;
  if (snapshotReadCache && snapshotReadCache.key === reuseKey && snapshotReadCache.promise) {
    const shared = await snapshotReadCache.promise;
    if (!isCurrentLoad(loadId, expiration)) return { shown: false, source: null, discarded: true };
    return shared;
  }
  const reading = loadSnapshotForRender(loadId, symbol, expiration, fallbackQuote);
  snapshotReadCache = { key: reuseKey, promise: reading };
  reading.finally(() => {
    // 下一拍再丢掉，同一轮里后启动的读取还能接上这次结果。回源开始时会主动清掉。
    setTimeout(() => {
      if (snapshotReadCache && snapshotReadCache.promise === reading) snapshotReadCache = null;
    }, 0);
  });
  return reading;
}

async function loadSnapshotForRender(loadId, symbol, expiration, fallbackQuote) {
  const encodedSymbol = encodeURIComponent(symbol);
  const encodedExpiration = expiration ? encodeURIComponent(expiration) : "";
  let quote = null;
  let payload = null;
  const analysis = reusableGammaProfile(symbol);
  const usableFallback = fallbackQuote && (!fallbackQuote.symbol || fallbackQuote.symbol === symbol)
    ? fallbackQuote
    : null;

  // quote 与链分别落地：其中一个 SQLite 读取较慢时，另一个仍可先展示。
  let quoteError = null;
  let chainError = null;
  const quoteRead = request(`/api/quote/${encodedSymbol}`).catch((error) => {
    quoteError = error;
    return null;
  }).then((result) => {
    if (!isCurrentLoad(loadId, expiration)) return null;
    const incomingQuote = (quoteIsFresh(result) || quoteIsUsableSQLiteSnapshot(result, symbol)) ? result : null;
    quote = latestRenderableQuote(symbol, incomingQuote, state.lastQuote, usableFallback);
    if (!quote && result?.warning) quoteError = new Error(result.warning);
    const resolvedQuote = latestRenderableQuote(symbol, quote, state.lastQuote, usableFallback);
    if (resolvedQuote) {
      applyCachedQuote(resolvedQuote);
      // 链先返回时先用缓存行情显示；quote 随后到达，再用准确现价重算分析区。
      if (payload?.data?.length) {
        state.analysisReady = gammaProfileReady(analysis) || state.analysisReady;
        renderChain(payload, resolvedQuote, analysis);
      }
    }
    return quote;
  });

  const chainRead = expiration
    ? request(`/api/chain/${encodedSymbol}?expiration=${encodedExpiration}`).catch((error) => {
      chainError = error;
      return null;
    }).then((result) => {
      if (!isCurrentLoad(loadId, expiration)) return null;
      if (result?.symbol && result.symbol !== symbol) return result;
      payload = result;
      if (!result?.data?.length) return result;
      const cachedChain = result.source === "sqlite";
      if (!cachedChain && !snapshotTimestampIsFresh(result.fetched_at)) return result;
      const resolvedQuote = latestRenderableQuote(symbol, quote, state.lastQuote, usableFallback);
      if (!quoteAndChainAreAligned(resolvedQuote, result.fetched_at, cachedChain && resolvedQuote?.source === "sqlite")) return result;
      state.analysisReady = gammaProfileReady(analysis) || state.analysisReady;
      renderChain(payload, resolvedQuote, analysis);
      state.view.lastStatus = payload.source === "sqlite"
        ? `本地缓存 ${formatTime(payload.fetched_at)}`
        : `最近更新 ${formatTime(payload.fetched_at)}`;
      syncPageQuery();
      return payload;
    })
    : Promise.resolve(null);

  // Flow 需要相邻快照计算，但不应阻塞报价和期权链首屏。
  if (expiration) {
    request(`/api/flow/${encodedSymbol}?expiration=${encodedExpiration}`).then((flow) => {
      if (flow && isCurrentLoad(loadId, expiration) && state.symbol === symbol) renderOptionFlow(flow);
    }).catch(() => {});
  }

  [quote, payload] = await Promise.all([quoteRead, chainRead]);
  if (!isCurrentLoad(loadId, expiration)) return { shown: false, source: null, discarded: true };
  const resolvedQuote = latestRenderableQuote(symbol, quote, state.lastQuote, usableFallback);
  const cachedPair = payload?.source === "sqlite" && resolvedQuote?.source === "sqlite";
  if (payload?.data?.length && !quoteAndChainAreAligned(resolvedQuote, payload.fetched_at, cachedPair)) {
    if (quoteError && markApiUnavailable(quoteError.message)) {
      return { shown: true, source: "client-cache", fetchedAt: state.chainFetchedAt, quote: state.lastQuote, payload: {
        data: state.chainRows, expiration: state.expiration, fetched_at: state.chainFetchedAt,
      }, analysisReady: state.analysisReady, quoteReady: true, optionsQuotesReady: hasValidOptionQuotes(state.chainRows), cached: true,
      };
    }
    return { shown: false, source: payload.source || null, fetchedAt: payload.fetched_at || null, quote: resolvedQuote, stale: true };
  }
  if (!payload?.data?.length) {
    if ((quoteError || chainError) && markApiUnavailable("API 暂不可用")) {
      return { shown: true, source: "client-cache", fetchedAt: state.chainFetchedAt, quote: state.lastQuote, payload: {
        data: state.chainRows,
        expiration: state.expiration,
        fetched_at: state.chainFetchedAt,
      }, analysisReady: state.analysisReady, quoteReady: true, optionsQuotesReady: hasValidOptionQuotes(state.chainRows), cached: true,
      };
    }
    // 暂时拿不到期权链时不清空已显示的流向；标的/期限切换会单独重置面板。
    return {
      shown: false,
      source: payload?.source || resolvedQuote?.source || null,
      fetchedAt: payload?.fetched_at || null,
      quote: resolvedQuote,
      apiError: Boolean(quoteError || chainError),
    };
  }
  if ((quoteError || chainError) && hasRenderableClientSnapshot()) markApiUnavailable("API 暂不可用");
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
  return refreshAnalysisWindowBatch(loadId, payload, quote, false);
}

function refreshAnalysisWindowBatch(loadId, payload, quote, continueBatch = false) {
  const symbol = state.symbol;
  if (state.analysisRefreshSymbol === symbol) return;
  const lastStartedAt = analysisWindowLastStartedAt.get(symbol) || 0;
  const lastAppliedAt = analysisWindowLastAppliedAt.get(symbol) || 0;
  const coolingDown = lastStartedAt > 0
    && Date.now() - lastStartedAt < ANALYSIS_REFRESH_COOLDOWN_SECONDS * 1000;
  // 冷却期内不启动新的跨期限抓取；每次正常快照只读一次状态，后台任务完成后
  // 立即读取已落库的 Gamma，避免图表继续显示旧窗口。
  if (!continueBatch && coolingDown && lastAppliedAt >= lastStartedAt) return;
  if (!continueBatch && !coolingDown) {
    analysisWindowLastStartedAt.set(symbol, Date.now());
    // 只在首次启动跨期限窗口任务时短暂延迟快照刷新；后续批次继续后台运行。
    state.analysisBlockUntil = Date.now() + ANALYSIS_REFRESH_BLOCK_MS;
  }
  state.analysisRefreshSymbol = symbol;
  const encodedSymbol = encodeURIComponent(symbol);
  const pendingText = continueBatch
    ? `Gamma 窗口继续加载 · 已完成上一批…`
    : coolingDown
    ? `快照已更新 ${formatTime(payload?.fetched_at)} · 检查后台 Gamma 窗口…`
    : `快照已更新 ${formatTime(payload?.fetched_at)} · 正在后台刷新 Gamma 窗口…`;
  state.view.lastStatus = pendingText;
  // attempt=1 只读状态；attempt=0 才会领取/启动新的后台窗口任务。
  pollGammaWindow(loadId, payload, quote, symbol, encodedSymbol, pendingText, continueBatch ? 0 : (coolingDown ? 1 : 0));
}

async function latestSelectedChain(loadId, symbol, fallbackPayload) {
  const expiration = state.expiration;
  if (!expiration) return fallbackPayload;
  // 选中期限刚由快照写入。Gamma 窗口即使重写 45 天里的其他期限，这一期也不用再读一次。
  if (
    fallbackPayload?.data?.length
    && fallbackPayload.expiration === expiration
    && (!fallbackPayload.symbol || fallbackPayload.symbol === symbol)
  ) {
    if (!isCurrentLoad(loadId, expiration) || state.symbol !== symbol) return null;
    return fallbackPayload;
  }
  const encodedSymbol = encodeURIComponent(symbol);
  const encodedExpiration = encodeURIComponent(expiration);
  const latest = await request(`/api/chain/${encodedSymbol}?expiration=${encodedExpiration}`).catch(() => null);
  if (!isCurrentLoad(loadId, expiration) || state.symbol !== symbol) return null;
  if (latest?.data?.length) return latest;
  // 回退载荷如果属于旧期限，不能拿来渲染，否则 renderChain 会把下拉框拽回去。
  if (fallbackPayload?.expiration && fallbackPayload.expiration !== expiration) return null;
  return fallbackPayload;
}

function reusableGammaProfile(symbol) {
  const analysis = state.lastAnalysis?.analysisPayload;
  if (!gammaProfileReady(analysis)) return null;
  if (analysis.symbol && analysis.symbol !== symbol) return null;
  if (state.chainFetchedSymbol && state.chainFetchedSymbol !== symbol) return null;
  return analysis;
}

function gammaProfileReady(analysis) {
  if (!analysis || analysis.status_only) return false;
  const count = Number(analysis.contract_count);
  if (Number.isFinite(count)) return count > 0;
  return Boolean(analysis.data?.length);
}

function releaseAnalysisRefresh(symbol) {
  if (state.analysisRefreshTimer || state.analysisRefreshSymbol !== symbol) return;
  state.analysisRefreshSymbol = null;
  state.analysisBlockUntil = 0;
  if (state.refreshInFlight || state.loading || expirationMenuOpen) return;
  scheduleAutoRefresh();
}

async function applyGammaPollResult(loadId, payload, quote, symbol, pendingText) {
  const encodedSymbol = encodeURIComponent(symbol);
  const [analysis, selectedPayload] = await Promise.all([
    request(`/api/gamma/${encodedSymbol}?horizon_days=45&include_rows=false`).catch(() => null),
    latestSelectedChain(loadId, symbol, payload),
  ]);
  if (!selectedPayload || !isCurrentLoad(loadId) || state.symbol !== symbol) return;
  if (!gammaProfileReady(analysis)) {
    // 没有可用的新窗口数据时，至少用旧分析完成一次价位刷新，保持价位与新快照同步。
    renderChain(selectedPayload, quote, state.lastAnalysis?.analysisPayload || null);
    return;
  }
  state.analysisReady = true;
  renderChain(selectedPayload, quote, analysis);
  analysisWindowLastAppliedAt.set(symbol, analysisWindowLastStartedAt.get(symbol) || Date.now());
  // 窗口刷新期间用户可能又点了刷新：只在提示文案还属于本次窗口刷新时才改写，避免覆盖更新的状态。
  if (state.view.lastStatus === pendingText) state.view.lastStatus = `最近更新 ${formatTime(payload?.fetched_at)}`;
}

function scheduleNextGammaBatch(loadId, payload, quote, symbol) {
  if (!isCurrentLoad(loadId) || state.symbol !== symbol) return;
  // 批次间隔期间仍保留互斥标记，防止快照刷新同时领取同一游标的下一批。
  // 继续加载时保留首次启动的刷新闸门。
  if (state.analysisRefreshTimer) clearTimeout(state.analysisRefreshTimer);
  // 批次之间让出事件循环和重任务闸门，避免连续到期日请求重新形成突发。
  state.analysisRefreshTimer = setTimeout(() => {
    state.analysisRefreshTimer = null;
    state.analysisRefreshSymbol = null;
    refreshAnalysisWindowBatch(loadId, payload, quote, true);
  }, 1200);
}

// 后端 Gamma 刷新改为 SQLite 任务协调的后台分批任务；前端只读取状态，期间继续展示当前期限分析。
// 最后一批完成后才读取展示用 Gamma，避免每批都把整个窗口序列化到浏览器。
function pollGammaWindow(loadId, payload, quote, symbol, encodedSymbol, pendingText, attempt) {
  // 任务没完成时只问状态，不把 45 天合约下载下来再解析。
  let continuePolling = false;
  // 只有第一请求负责领取后台任务；后续轮询只读状态，避免每秒重复 BEGIN IMMEDIATE
  // 和重复写入分析任务领取日志。
  const statusUrl = attempt === 0
    ? `/api/gamma/${encodedSymbol}?horizon_days=45&refresh=true&status_only=true`
    : `/api/gamma/${encodedSymbol}?horizon_days=45&status_only=true`;
  const task = request(statusUrl)
    .then(async (status) => {
      try {
        if (!isCurrentLoad(loadId) || state.symbol !== symbol) return;
        const running = Boolean(status && status.refresh && status.refresh.status === "running");
        if (!status || typeof status !== "object") {
          await applyGammaPollResult(loadId, payload, quote, symbol, pendingText);
          return;
        }
        if (running) {
          // 后台批次可能还在抓取；用 3 秒间隔读取一次状态，完成后才启动下一批，
          // 避免此前每秒请求造成的 /api/gamma 请求风暴。
          state.view.lastStatus = pendingText;
          continuePolling = true;
          if (state.analysisRefreshTimer) clearTimeout(state.analysisRefreshTimer);
          if (attempt < ANALYSIS_POLL_LIMIT) {
            state.analysisRefreshTimer = setTimeout(() => {
              state.analysisRefreshTimer = null;
              pollGammaWindow(loadId, payload, quote, symbol, encodedSymbol, pendingText, attempt + 1);
            }, 3000);
          } else {
            continuePolling = false;
            state.view.lastStatus = "Gamma 窗口状态读取超时，保留已加载数据";
            releaseAnalysisRefresh(symbol);
          }
          return;
        }
        const batchResult = status?.refresh?.result || {};
        if (batchResult.has_more) {
          // 中间批次只写入 SQLite 并显示进度，不读取/序列化整个 45 天窗口；
          // 等最后一批完成后再做一次完整零 Gamma 计算。
          continuePolling = true;
          const progress = batchResult;
          if (Number.isFinite(Number(progress.loaded)) && Number.isFinite(Number(progress.total))) {
            state.view.lastStatus = `Gamma 窗口已加载 ${progress.loaded}/${progress.total} 个到期日`;
          }
          scheduleNextGammaBatch(loadId, payload, quote, symbol);
          return;
        }
        await applyGammaPollResult(loadId, payload, quote, symbol, pendingText);
      } finally {
        if (!continuePolling) releaseAnalysisRefresh(symbol);
      }
    })
    .catch((error) => {
      try {
        if (!isCurrentLoad(loadId) || state.symbol !== symbol) return;
        if (payload?.expiration && state.expiration && payload.expiration !== state.expiration) return;
        // Gamma 窗口刷新失败时仍补做一次价位请求，避免快照已更新却继续显示旧的价位结果。
        renderChain(payload, quote, state.lastAnalysis?.analysisPayload || null);
        if (state.view.lastStatus === pendingText) state.view.lastStatus = `Gamma 窗口刷新失败，仍显示本地缓存（${error.message}）`;
      } finally {
        continuePolling = false;
        releaseAnalysisRefresh(symbol);
      }
    });
  // jQuery 的 Deferred 没有 Promise.finally。直接链式调用会抛 TypeError，
  // 被快照更新的 catch 接住后会再去回源到期日，页面状态就会一直停在「正在刷新」。
  Promise.resolve(task).then(() => {
    if (!continuePolling) releaseAnalysisRefresh(symbol);
  }, () => {
    if (!continuePolling) releaseAnalysisRefresh(symbol);
  });
}

async function refreshInBackground(loadId) {
  const symbol = state.symbol;
  const expiration = state.expiration;
  const encodedSymbol = encodeURIComponent(symbol);
  // 回源后的补读必须看到新快照，不能复用回源前那一次读取。
  snapshotReadCache = null;
  if (!isCurrentLoad(loadId) || state.symbol !== symbol) return;
  // 同一标的只允许一条网络刷新链路：页面加载与服务端通知后的读取共用这份互斥，避免叠加请求。
  if (state.refreshInFlight === symbol) return;
  state.refreshInFlight = symbol;
  // 到期日在 await 期间被换掉时，旧结果不能拿去渲染或拉新期限。先释放互斥再重入，第二轮请求等这一轮结束才发。
  async function restartIfExpirationChanged(captured) {
    if (state.expiration === captured || !isCurrentLoad(loadId) || state.symbol !== symbol) return false;
    state.refreshInFlight = null;
    await refreshInBackground(loadId);
    return true;
  }
  try {
    state.view.lastStatus = "正在请求上游快照…";
    // Begin the SSE subscription before the blocking refresh request. The
    // server runs a per-subscription snapshot worker, which can populate the
    // SQLite cache while this request is still waiting for upstream providers.
    connectPushStream(symbol);
    // 页面读取使用短缓存新鲜期，由服务端按交易时段调度活跃 SSE 标的的回源。
    const params = new URLSearchParams({ max_age: String(SNAPSHOT_FRESH_SECONDS), background: "true" });
    if (expiration) params.set("expiration", expiration);
    const refreshResult = await request(`/api/refresh/${encodedSymbol}?${params}`, { method: "POST" });
    if (!isCurrentLoad(loadId) || state.symbol !== symbol) return;
    // POST 还在飞时用户换了到期日：不能拿旧期限的结果去拉新期限，否则新期限没有回源，页面却当成已刷新。
    if (await restartIfExpirationChanged(expiration)) return;
    // 后端在标的没有挂牌期权时只写现货快照：走现货渲染分支，避免页面一直停在“后台刷新中”。
    if (refreshResult?.quote_only) {
      const restarted = await loadQuoteOnly(loadId);
      if (!restarted && await restartIfExpirationChanged(expiration)) return;
      return;
    }
    // 第一次刷新已经带回全部未过期到期日，立刻填进下拉框，不等期权链下载完，也不只放当前这一期。
    if (Array.isArray(refreshResult?.expirations) && refreshResult.expirations.length) {
      applyExpirations(refreshResult.expirations, state.expiration || refreshResult.expiration, "该标的没有期权到期日");
      clearSnapshotRetry();
    } else if (!state.expiration && refreshResult?.expiration) {
      applyExpirations([refreshResult.expiration], refreshResult.expiration, "该标的没有期权到期日");
      clearSnapshotRetry();
    }
    if (refreshResult?.skipped) {
      if (!state.expiration) {
        await renderCachedHistoryOnly(loadId, refreshResult.quote || null);
        if (!isCurrentLoad(loadId) || await restartIfExpirationChanged(expiration)) return;
        if (refreshResult.deferred && state.expirationPlaceholder !== "无期权缓存") scheduleSnapshotRetry(loadId, 15000);
        if (refreshResult.deferred) {
          state.view.lastStatus = "已读取 SQLite 行情/日线缓存；期权数据在后台检查";
        }
        return;
      }
      // 页面上已经是这份快照时不再读 quote、chain、levels。只有本地缺了有效报价才补读一次。
      if (!displayedSnapshotIsCurrent(refreshResult.fetched_at)) {
        // 首次读取时某个并发请求可能暂时失败，但后端已经确认 SQLite 快照新鲜；
        // 重新读取并使用刷新响应附带的 quote，避免页面停在“股价 -- / Gamma 0”。
        const snapshot = await renderSnapshot(loadId, refreshResult.quote || null);
        if (!isCurrentLoad(loadId) || state.symbol !== symbol) return;
        if (await restartIfExpirationChanged(expiration)) return;
        if (snapshot.shown && !snapshot.analysisReady && snapshot.payload?.data?.length && snapshot.quote) {
          refreshAnalysisWindow(loadId, snapshot.payload, snapshot.quote);
        }
        if (isSnapshotFresh(snapshot) && state.expirationPlaceholder !== "正在获取到期日…") clearSnapshotRetry();
        else scheduleSnapshotRetry(loadId, refreshResult.deferred ? 15000 : 0);
      } else {
        if (state.expirationPlaceholder !== "正在获取到期日…") clearSnapshotRetry();
      }
      if (refreshResult.deferred) {
        state.view.lastStatus = /内存保护/.test(String(refreshResult.warning || ""))
          ? "内存保护：本轮刷新已让路，继续使用本地快照"
          : "已有刷新进行中，本轮沿用本地快照";
        if (refreshResult.warning?.includes("SSE") && refreshResult.expiration) {
          // The subscription worker already owns the shared SQLite lease; do
          // not spin another request. A snapshot event will render its commit.
          state.view.lastStatus = "上游刷新仍在后台进行，正在等候推送";
        }
      } else {
        state.view.lastStatus = `本地快照 ${formatTime(refreshResult.fetched_at)} 已是最新（${Math.round(Number(refreshResult.age_seconds) || 0)} 秒前）`;
      }
      return;
    }
    const currentExpiration = state.expiration || refreshResult?.expiration;
    if (!currentExpiration) {
      await renderCachedHistoryOnly(loadId, refreshResult?.quote || null);
      if (await restartIfExpirationChanged(expiration)) return;
      return;
    }
    const encodedExpiration = encodeURIComponent(currentExpiration);
    // 新刷新响应通常已经带回选中期限的链；旧响应或降级响应再回读本地链。
    const expirationRequest = Array.isArray(refreshResult?.expirations)
      ? Promise.resolve({ expirations: refreshResult.expirations })
      : request(`/api/expirations/${encodedSymbol}?refresh=true`).catch(() => null);
    const quoteRequest = quoteIsFresh(refreshResult?.quote)
      ? Promise.resolve(refreshResult.quote)
      : request(`/api/quote/${encodedSymbol}`);
    const chainRequest = refreshResult?.chain?.data?.length
      ? Promise.resolve(refreshResult.chain)
      : request(`/api/chain/${encodedSymbol}?expiration=${encodedExpiration}`);
    const [payload, quote, expirations, flow] = await Promise.all([
      chainRequest,
      quoteRequest,
      expirationRequest,
      request(`/api/flow/${encodedSymbol}?expiration=${encodeURIComponent(currentExpiration)}`).catch(() => null),
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
    let resolvedQuote = quoteIsFresh(quote) ? quote : refreshResult?.quote;
    if (!quoteIsFresh(resolvedQuote)) {
      // 快照响应和 quote 查询都可能在低配服务器上错开；最后再主动读一次上游行情。
      const retryQuote = await request(`/api/quote/${encodedSymbol}?refresh=true`).catch(() => null);
      if (await restartIfExpirationChanged(currentExpiration)) return;
      if (quoteIsFresh(retryQuote)) resolvedQuote = retryQuote;
    }
    if (!quoteAndChainAreAligned(resolvedQuote, payload?.fetched_at)) {
      state.view.lastStatus = "行情或期权链仍在刷新，等待一份时间匹配的新快照";
      scheduleSnapshotRetry(loadId);
      return;
    }
    renderQuote(resolvedQuote);
    // Gamma 窗口继续后台刷新；选中期限的综合价位已在这里与窗口任务并行请求。
    renderChain(payload, resolvedQuote, state.lastAnalysis?.analysisPayload || null);
    if (!payload?.data?.length) scheduleSnapshotRetry(loadId);
    renderOptionFlow(flow);
    state.view.lastStatus = `最近更新 ${formatTime(payload.fetched_at)}`;
    syncPageQuery();
    // Gamma 轮询自己收尾。这里再抛错会被下面的 catch 当成快照失败，转去回源全部到期日。
    try {
      refreshAnalysisWindow(loadId, payload, resolvedQuote);
    } catch (error) {
      state.analysisRefreshSymbol = null;
      state.analysisBlockUntil = 0;
      state.view.lastStatus = `Gamma 窗口刷新失败，仍显示本地缓存（${error.message}）`;
    }
  } catch (error) {
    if (!isCurrentLoad(loadId) || state.symbol !== symbol) return;
    if (await restartIfExpirationChanged(expiration)) return;
    state.view.lastStatus = `后台刷新失败，仍显示本地缓存（${error.message}）`;
    if (!state.view.chainRows.length) setError(error.message);
    // 失败后不追加读请求：超时的服务端线程可能仍在抓取期权链，恢复读取
    // quote/chain 只会再占一轮连接。保留当前画面，等待 SSE 服务端调度下一轮重试。
    state.refreshInFlight = null;
    if (isTransientRequestError(error)) scheduleSnapshotRetry(loadId, error.retryAfterMs || 0);
  } finally {
    if (state.refreshInFlight === symbol) state.refreshInFlight = null;
  }
}

// 标的没有挂牌期权（或可用期限已全部到期）时只渲染现货：现货与盘前盘后照常刷新，
// 期权相关面板统一提示没有期权数据，避免整页停在 “--”。
async function loadQuoteOnly(loadId) {
  const symbol = state.symbol;
  const expiration = state.expiration;
  const encodedSymbol = encodeURIComponent(symbol);
  // 现货读本地快照（刷新接口刚写过），到期日必须回源：本地为空只能说明还没抓过，不等于没有期权。
  let requestError = null;
  const [quote, expirations] = await Promise.all([
    request(`/api/quote/${encodedSymbol}`).catch((error) => { requestError = error; return null; }),
    request(`/api/expirations/${encodedSymbol}`).catch((error) => { requestError = requestError || error; return null; }),
  ]);
  if (!isCurrentLoad(loadId) || state.symbol !== symbol) return;
  // 等待期间用户换了到期日：不要清掉新选择，交给调用方按新期限重入。
  if (state.expiration !== expiration) return;
  if ((requestError && isTransientRequestError(requestError)) || expirations?.source === "pending") {
    scheduleSnapshotRetry(loadId, Math.max(10000, requestError?.retryAfterMs || 0));
  }
  if (requestError && markApiUnavailable("API 暂不可用")) return;
  const dates = expirations?.expirations || [];
  // 回源后拿到了期限：说明本地只是缺缓存，重走一次完整加载。
  if (dates.length) {
      applyExpirations(dates, null);
      clearSnapshotRetry();
    state.refreshInFlight = null;
    await refreshInBackground(loadId);
    return true;
  }
  if (expirations?.cached_expirations?.length) {
    applyExpirations(expirations.cached_expirations, state.expiration);
    clearSnapshotRetry();
    state.refreshInFlight = null;
    await loadChain({ loadId });
    return true;
  }
  // 所选期限可能刚刚全部到期：清掉失效的到期日，避免它继续留在 state 和地址栏里。
  state.expiration = null;
  state.expirationOptions = [];
  state.expirationPlaceholder = expirations?.source === "pending" ? "正在获取到期日…" : "无期权缓存";
  syncPageQuery();
  await renderCachedHistoryOnly(loadId, quote);
  state.view.dataSource = "上游无期权数据";
  state.view.chainEmpty = "SQLite 目前没有期权合约缓存";
  state.view.lastStatus = expirations?.source === "pending"
    ? "已读取 SQLite 行情与日线缓存；正在检查期权期限"
    : "已读取 SQLite 行情与日线缓存；上游没有可用期权合约";
}

async function loadExpirations(loadId, fallbackSnapshot = null, refresh = false) {
  const encodedSymbol = encodeURIComponent(state.symbol);
  let requestError = null;
  const payload = await request(`/api/expirations/${encodedSymbol}${refresh ? "?refresh=true" : ""}`).catch((error) => {
    requestError = error;
    return { expirations: [], source: "pending" };
  });
  if (!isCurrentLoad(loadId)) return;
  const retryable = requestError
    ? isTransientRequestError(requestError)
    : payload.source === "pending";
  if (retryable) {
    scheduleSnapshotRetry(loadId, Math.max(10000, requestError?.retryAfterMs || 0));
  } else {
    clearSnapshotRetry();
  }
  if (requestError) {
    const restored = restoreClientSnapshot(state.symbol, state.expiration)
      || restoreDisplayedClientSnapshot(fallbackSnapshot);
    if (restored) {
      markApiUnavailable("API 暂不可用");
      syncPageQuery();
      return;
    }
  }
  // source=pending 表示本地还没缓存过（需要回源），与「该标的确实没有期权」是两回事。
  const dates = payload.expirations || [];
  const cachedDates = payload.cached_expirations || [];
  const allDates = [...new Set([...dates, ...cachedDates])].sort();
  const cachedExpiration = preferredCachedExpiration(allDates, state.expiration, cachedDates);
  if (cachedExpiration) {
    applyExpirations(allDates, cachedExpiration, retryable ? "正在获取到期日…" : "该标的没有期权到期日");
    if (!retryable) clearSnapshotRetry();
    else scheduleSnapshotRetry(loadId, Math.max(10000, requestError?.retryAfterMs || 0));
    await loadChain({ loadId });
    return;
  }
  // A live SSE refresh can deliver the nearest chain before this initial
  // expiry-catalog read completes. Keep that selected expiry visible instead
  // of interpreting an early `pending` catalog response as no options.
  if (state.displayedChainSymbol === state.symbol
    && state.displayedChainExpiration === state.expiration
    && state.expiration && state.chainRows?.length) {
    if (!retryable) clearSnapshotRetry();
    return;
  }
  state.expiration = null;
  state.expirationOptions = [];
  state.expirationPlaceholder = retryable ? "正在获取到期日…" : "无期权缓存";
  syncPageQuery();
  // A cached quote/history remains useful even when this symbol has no
  // cached option expirations. Render the durable daily-history analysis
  // independently, but do not make the quote or snapshot request wait for it.
  if (isCurrentLoad(loadId)) {
    if (retryable) scheduleSnapshotRetry(loadId, Math.max(10000, requestError?.retryAfterMs || 0));
    void refreshInBackground(loadId);
    void renderCachedHistoryOnly(loadId);
  }
}

function displayedSnapshotMatches() {
  return Boolean(
    state.chainFetchedSymbol
    && state.chainFetchedSymbol === state.symbol
    && state.expiration
    && state.chainFetchedExpiration === state.expiration
    && state.chainRows.length
  );
}

function displayedSnapshotContextMatches() {
  return Boolean(
    state.displayedChainSymbol === state.symbol
    && state.displayedChainExpiration
    && state.displayedChainExpiration === state.expiration
    && state.chainRows.length
  );
}

function displayedSnapshotIsFresh() {
  if (!displayedSnapshotMatches()) return false;
  if (!hasValidOptionQuotes(state.chainRows)) return false;
  const age = snapshotAgeSeconds(state.chainFetchedAt);
  return age != null && age < SNAPSHOT_FRESH_SECONDS
    && quoteAndChainAreAligned(state.lastQuote, state.chainFetchedAt);
}

// 后端跳过回源时，页面已经展示同一份快照并且报价有效，就不必再读一遍。
// 不看本地年龄：客户端时钟走得快时，服务端仍可能判定这份快照新鲜。
function displayedSnapshotIsCurrent(fetchedAt) {
  return displayedSnapshotMatches()
    && quoteAndChainAreAligned(state.lastQuote, state.chainFetchedAt)
    && snapshotTimestampIsFresh(state.chainFetchedAt)
    && hasValidOptionQuotes(state.chainRows)
    && Boolean(fetchedAt)
    && state.chainFetchedAt === fetchedAt;
}

async function loadChain(options = {}) {
  try {
    const loadId = options.loadId || state.loadId;
    if (!isCurrentLoad(loadId)) return;
    const previousSnapshot = options.fallbackSnapshot || displayedClientSnapshotRef();
    // 页面上已经是当前标的和到期日：新鲜时复用，过期后才请求上游。
    // 这样 SSE 触发的快照检查不会先把 quote、chain、gamma 读一遍，回源后再读一遍。
    if (displayedSnapshotMatches()) {
      if (displayedSnapshotIsFresh()) {
        showFreshStatus({ fetchedAt: state.chainFetchedAt });
        if (!state.analysisReady && state.lastQuote) {
          refreshAnalysisWindow(loadId, {
            symbol: state.chainFetchedSymbol,
            expiration: state.chainFetchedExpiration,
            fetched_at: state.chainFetchedAt,
            data: state.chainRows,
          }, state.lastQuote);
        }
        return;
      }
      await refreshInBackground(loadId);
      return;
    }
    // 即使本地还没有到期日也要往下走：首次加载某个标的时后端需要回源才能拿到期限列表，
    // 提前 return 会让页面永远停在没有数据的状态。
    const snapshot = await renderSnapshot(loadId);
    // 用户已经切走：不要把新期限的页面清成「正在后台获取」，也不要再为旧期限启动刷新。
    if (snapshot.discarded) return;
    if (!snapshot.shown) {
      scheduleSnapshotRetry(loadId);
      if (snapshot.apiError && restoreDisplayedClientSnapshot(previousSnapshot)) return;
      if (displayedSnapshotContextMatches()) {
        state.view.lastStatus = "期权链暂不可用，保留当前已显示数据并在后台重试";
      } else {
        showPending("正在后台获取上游快照…");
      }
    }
    if (options.refresh === false) {
      if (isSnapshotFresh(snapshot) && state.expirationPlaceholder !== "正在获取到期日…") clearSnapshotRetry();
      else scheduleSnapshotRetry(loadId);
      return;
    }
    // 先读 SQLite 判断新鲜度：仍在新鲜期内直接复用，只有确认过期才请求上游接口。
    if (isSnapshotFresh(snapshot)) {
      if (state.expirationPlaceholder !== "正在获取到期日…") clearSnapshotRetry();
      showFreshStatus(snapshot);
      // 首次访问可能只有选中期限的缓存，跨期限 Gamma 尚未生成；只补后台分析，不阻塞首屏。
      if (!snapshot.analysisReady && snapshot.payload?.data?.length && snapshot.quote) {
        refreshAnalysisWindow(loadId, snapshot.payload, snapshot.quote);
      }
      return;
    }
    await refreshInBackground(loadId);
  } catch (error) {
    if (isTransientRequestError(error)) scheduleSnapshotRetry(options.loadId || state.loadId, error.retryAfterMs || 0);
    throw error;
  } finally {
    scheduleAutoRefresh();
    if (state.pendingSnapshotRefresh) setTimeout(applyPendingSnapshotRefresh, 0);
  }
}

async function loadSymbol() {
  const previousSnapshot = displayedClientSnapshotRef();
  const symbol = (state.symbolInput || byId("symbol-input").value).trim().toUpperCase();
  if (!symbol) {
    state.loading = false;
    scheduleAutoRefresh();
    return;
  }
  state.loading = true;
  state.symbolInput = symbol;
  state.symbol = symbol;
  clearQuoteRecovery();
  clearSnapshotRetry();
  resetChanAnalysis();
  state.trendMarket = null;
  state.trendMarketSymbol = "";
  state.trendHistorySymbol = "";
  closePushStream();
  clearFairValuePoll();
  state.expiration = parsePageQuery(location.search).expiration || null;
  state.expirationOptions = [];
  state.expirationPlaceholder = "正在获取到期日…";
  // 切换标的时取消旧页面的 Gamma 状态轮询；服务端任务可继续，但旧响应不能再驱动新页面。
  if (state.analysisRefreshTimer) clearTimeout(state.analysisRefreshTimer);
  state.analysisRefreshTimer = null;
  state.analysisRefreshSymbol = null;
  state.analysisBlockUntil = 0;
  state.analysisReady = false;
  const loadId = ++state.loadId;
  if (window.optionScopeApp && state.chanExpanded) window.optionScopeApp.ensureChanIntraday(state.chanPeriod);
  clearQuoteRetry();
  state.quoteRetryInFlightKey = "";
  setError("");
  // Do not paint an old full-page snapshot during normal navigation. A fresh
  // quote is requested below; client snapshots remain an explicit API-outage
  // fallback through markApiUnavailable/restoreDisplayedClientSnapshot.
  state.lastQuote = null;
  applyCachedQuote(null);
  showPending("正在获取最新行情并核对期权链…");
  syncPageQuery();
  // Quote delivery must not wait for expiry discovery or the historical-levels
  // read. The cache read gives an immediate first paint when available; the
  // SSE subscription and snapshot refresh then publish a live session quote.
  connectPushStream(symbol);
  fetchQuoteForDisplay(symbol, loadId);
  if (state.chanExpanded) refreshChanAnalysis();
  else refreshChanMarketAnalysis(true);
  // Opening the stream starts the server-side snapshot worker immediately;
  // loadChain owns any follow-up read after expiry discovery.
  try {
    await loadExpirations(loadId, previousSnapshot);
  } catch (error) {
    if (!isCurrentLoad(loadId)) return;
    state.expirationOptions = [];
    state.expirationPlaceholder = "加载失败";
    if (!markApiUnavailable(error.message) && !restoreDisplayedClientSnapshot(previousSnapshot, error.message)) {
      showPending("暂无数据");
      setError(error.message);
    }
  } finally {
    state.loading = false;
    // 首次进入新标的时行情/期权链可能先完成，估值仍在后端线程计算；
    // 在 loading 解除后启动独立轻量轮询，避免必须手动刷新整页才能看到估值。
    if (state.lastQuote) scheduleFairValuePoll(state.lastQuote);
    scheduleAutoRefresh();
  }
}

// 切换到期日：请求在飞时禁用下拉框（连点也切不出第二组请求），数据落地后再放开。
// 并发保护是三层：菜单打开时暂停自动刷新；这一层防重复触发；
// renderSnapshot / refreshInBackground 还会拿「请求时的到期日」校验，响应回来时若选择已经变了就整份丢弃并按新期限重入。
let expirationSwitchToken = 0;

// 切换到期日时下拉框的禁用兜底时间：超过这个时间还没拿到数据就放开，防止网络卡死导致控件永久不可用。
const EXPIRATION_SWITCH_TIMEOUT_MS = 20000;

async function switchExpiration(expiration) {
  const token = ++expirationSwitchToken;
  const previousSnapshot = displayedClientSnapshotRef();
  state.expiration = expiration;
  connectPushStream(state.symbol);
  resetOptionFlow("正在读取当前期限的相邻快照…");
  setError("");
  setBusy(true);
  // 兜底：请求万一一直不落地（例如上游卡住），20 秒后强制放开下拉框，避免控件被永久禁用。
  // 仍然按 token 校验，期间用户又切了一次就不动新的那次流程的禁用状态。
  const releaseTimer = setTimeout(() => { if (token === expirationSwitchToken) setBusy(false); }, EXPIRATION_SWITCH_TIMEOUT_MS);
  try {
    await loadChain({ loadId: state.loadId });
  } catch (error) {
    if (!markApiUnavailable(error.message) && !restoreDisplayedClientSnapshot(previousSnapshot, error.message)) {
      setError(error.message);
    }
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

initTheme();
// Element UI 2.x 基于 Vue 2，必须在根实例创建前注册；静态库已经由 index.html 按依赖顺序加载。
if (window.Vue && window.ELEMENT) Vue.use(ELEMENT);
// 期权链单独成组件：父页面刷新报价或状态时，行数组引用不变就不重绘这张表。
if (window.Vue) {
  Vue.directive("chan-chart-hover", OptionScopeCharts.chanChartHover);
  Vue.directive("chan-chart-canvas", OptionScopeCharts.chanChartCanvas);
  Vue.directive("chan-chart-interaction", OptionScopeCharts.chanChartInteraction);
  Vue.component("chain-table-body", {
    props: {
      rows: { type: Array, default: () => [] },
      empty: { type: String, default: "" },
    },
    template: '<tbody id="chain-body"><tr v-if="!rows.length"><td colspan="9" class="empty">{{ empty }}</td></tr><tr v-for="row in rows" :key="row.key" :class="row.rowClass"><td class="num chain-strike" :class="row.typeClass">{{ row.strike }}</td><td class="num chain-heat" :class="row.volumeClass" :style="row.volumeStyle" :title="row.volumeTitle">{{ row.volume }}</td><td class="num chain-heat" :class="row.interestClass" :style="row.interestStyle" :title="row.interestTitle">{{ row.interest }}</td><td class="num">{{ row.bid }}</td><td class="num">{{ row.ask }}</td><td class="num">{{ row.gamma }}</td><td class="num" :title="row.gexTitle">{{ row.gex }}</td><td class="num">{{ row.iv }}</td><td :class="row.itmClass">{{ row.itm }}</td></tr></tbody>',
  });
}
const optionScopeApp = new Vue({
  el: "#app",
  data: state,
  computed: {
    busy() { return this.loading; },
    chanFrame() {
      if (CHAN_INTRADAY_PERIODS.includes(this.chanPeriod)) {
        const payload = this.chanIntradayPayload[this.chanPeriod];
        return payload
          ? OptionScopeChan.intradayView(payload, this.chanScenariosVisible, this.chanIntradaySpot, this.chanMobileChart, 30, this.chanViewports[this.chanPeriod], this.chanChartLayers)
          : OptionScopeChan.emptyFrame(({ "15m": "15分钟", "4h": "4小时" })[this.chanPeriod]);
      }
      return this.view.chan[this.chanPeriod] || this.view.chan.daily;
    },
    chanChartInteraction() {
      return {
        chart: this.chanFrame?.chart,
        scenariosVisible: this.chanScenariosVisible,
        onPan: (bars) => this.panChanChart(bars),
        onZoom: (factor, anchor, ratio, baseVisible) => this.zoomChanChart(factor, anchor, ratio, baseVisible),
        onScenarioClick: (key, event) => this.toggleChanScenario(key, event),
      };
    },
    chanLoadStatus() {
      if (!CHAN_INTRADAY_PERIODS.includes(this.chanPeriod) && this.chanLoading) return "缠论行情加载中…";
      if (CHAN_INTRADAY_PERIODS.includes(this.chanPeriod)) {
        const attempts = this.chanIntradayRetryAttempts[this.chanPeriod] || 0;
        if (this.chanIntradayLoading[this.chanPeriod]) return `${this.chanPeriod === "15m" ? "15分钟" : "4小时"}行情加载中…`;
        if (attempts >= CHAN_INTRADAY_RETRY_LIMIT && !this.chanIntradayRetryTimers[this.chanPeriod]) return "盘中行情失败，继续低频自动重试";
        if (attempts > 0) return `盘中行情正在自动重试（${attempts}/${CHAN_INTRADAY_RETRY_LIMIT}）`;
        return "";
      }
      if (this.chanRetryAttempts >= CHAN_ANALYSIS_RETRY_LIMIT && !this.chanRetryTimer) return "缠论行情失败，继续低频自动重试";
      if (this.chanRetryAttempts > 0) return `缠论行情正在自动重试（${this.chanRetryAttempts}/${CHAN_ANALYSIS_RETRY_LIMIT}）`;
      return "";
    },
    chanLoadRetryAvailable() {
      if (CHAN_INTRADAY_PERIODS.includes(this.chanPeriod)) return !this.chanIntradayLoading[this.chanPeriod] && !this.chanIntradayRetryTimers[this.chanPeriod] && (this.chanIntradayRetryAttempts[this.chanPeriod] || 0) >= CHAN_INTRADAY_RETRY_LIMIT;
      return !this.chanLoading && !this.chanRetryTimer && this.chanRetryAttempts >= CHAN_ANALYSIS_RETRY_LIMIT;
    },
    chanCanvasBinding() { return { chart: this.chanFrame?.chart, layers: this.chanChartLayers, scenariosVisible: this.chanScenariosVisible }; },
    chanHoverBinding() {
      return {
        chart: this.chanFrame?.chart,
        scenariosVisible: this.chanScenariosVisible,
        onScenarioHover: (key, event) => this.showChanScenario(key, event),
        onScenarioLeave: (event) => this.hideChanScenario(event),
      };
    },
    chanLayerLevels() {
      return this.chanFrame?.chart?.layerLevels || [];
    },
    chanSegmentLevels() {
      return this.chanFrame?.chart?.segmentLevels || [];
    },
  },
  watch: {
    chanPeriod() {
      rememberChanChartPreferences(this.chanPeriod, this.chanChartLayers);
      this.chanHoveredScenario = null;
      this.chanSpotGuideVisible = false;
      scheduleChanIntradayBoundaryRefresh(this.chanPeriod);
      this.ensureChanIntraday(this.chanPeriod);
    },
  },
  methods: {
    closeChanLayerMenu(event) {
      const menu = event?.currentTarget?.closest(".chan-layer-menu");
      if (!menu) return;
      menu.open = false;
      menu.querySelector("summary")?.focus();
    },
    chanTurningStatus(index) {
      const turning = this.chanFrame?.turning;
      const item = Array.isArray(turning) ? turning[index] : null;
      return item?.status || "等待信号";
    },
    navigateToSymbol() { return navigateToSymbol(); },
    retryChanChartLoad() {
      if (CHAN_INTRADAY_PERIODS.includes(this.chanPeriod)) {
        const period = this.chanPeriod;
        clearTimeout(this.chanIntradayRetryTimers[period]);
        this.chanIntradayRetryTimers = { ...this.chanIntradayRetryTimers, [period]: null };
        this.chanIntradayRetryAttempts = { ...this.chanIntradayRetryAttempts, [period]: 0 };
        this.ensureChanIntraday(period, true);
        return;
      }
      clearTimeout(state.chanRetryTimer);
      state.chanRetryTimer = null;
      state.chanRetryAttempts = 0;
      refreshChanAnalysis(true);
    },
    ensureChanIntraday(period, refresh = false) {
      if (!CHAN_INTRADAY_PERIODS.includes(period)) return;
      if (period === "15m" && !chanIntradayBoundaryTimer) scheduleChanIntradayBoundaryRefresh(period);
      if (period !== "15m" && chanIntradayBoundaryTimer) {
        clearTimeout(chanIntradayBoundaryTimer);
        chanIntradayBoundaryTimer = null;
      }
      if (period !== "15m" && chanIntradayExpiryRetryTimer) {
        clearTimeout(chanIntradayExpiryRetryTimer);
        chanIntradayExpiryRetryTimer = null;
      }
      if (this.chanIntradayLoading[period]) return;
      if (!refresh && this.chanIntradayPayload[period]?.frame?.bar_count > 0 && !this.chanIntradayPayload[period]?.refreshing) {
        if (period === "15m") refreshChanIntradayAtBoundary(state.symbol);
        return;
      }
      const symbol = state.symbol;
      const generation = state.loadId;
      if (!symbol) return;
      const quote = state.lastQuote?.symbol === symbol ? state.lastQuote : null;
      const spot = finitePrice(activeSessionQuote(quote)?.price ?? quote?.price);
      const query = spot != null ? `?period=${period}&spot=${encodeURIComponent(spot)}` : `?period=${period}`;
      this.chanIntradayLoading = { ...this.chanIntradayLoading, [period]: true };
      request(`/api/chan/${encodeURIComponent(symbol)}/intraday${query}`, { timeout: 60000 }).then((payload) => {
        if (state.symbol !== symbol || state.loadId !== generation) return;
        const previousBars = this.chanIntradayPayload[period]?.frame?.bar_count || 0;
        const nextBars = payload?.frame?.bar_count || 0;
        const viewport = state.chanViewports[period];
        if (previousBars > 0 && nextBars > previousBars && viewport?.visibleBars > 0) {
          const addedBars = nextBars - previousBars;
          const maxOffset = Math.max(0, nextBars - viewport.visibleBars);
          const endOffset = Math.max(0, Math.min(maxOffset, (viewport.endOffset || 0) + (viewport.endOffset > 0 ? addedBars : 0)));
          state.chanViewports = { ...state.chanViewports, [period]: { ...viewport, endOffset } };
        }
        this.chanIntradayPayload = { ...this.chanIntradayPayload, [period]: Object.freeze(payload) };
        const boundaryPrefix = `${symbol}:`;
        const expectedCandleDate = period === "15m" && chanIntradayLastBoundaryAttempt.startsWith(boundaryPrefix)
          ? chanIntradayLastBoundaryAttempt.slice(boundaryPrefix.length)
          : "";
        const candles = payload?.frame?.candles;
        const latestCandleDate = String(candles?.[candles.length - 1]?.date || "");
        if (expectedCandleDate && latestCandleDate < expectedCandleDate) {
          scheduleChanIntradayCacheExpiryRetry(symbol, expectedCandleDate, payload?.fetched_at, payload?.refreshing);
        } else if (expectedCandleDate && latestCandleDate >= expectedCandleDate) {
          clearTimeout(chanIntradayExpiryRetryTimer);
          chanIntradayExpiryRetryTimer = null;
          chanIntradayExpiryRetryBoundary = "";
        }
        if (payload?.refreshing || !nextBars) {
          if (!expectedCandleDate) scheduleChanIntradayRetry(period, symbol);
        }
        else {
          clearTimeout(state.chanIntradayRetryTimers[period]);
          state.chanIntradayRetryTimers = { ...state.chanIntradayRetryTimers, [period]: null };
          state.chanIntradayRetryAttempts = { ...state.chanIntradayRetryAttempts, [period]: 0 };
        }
      }).catch((error) => {
        if (state.symbol === symbol && state.loadId === generation) {
          if (!isTransientChanRequestError(error)) setError(error.message || "盘中周期加载失败");
          if (isTransientChanRequestError(error)) scheduleChanIntradayRetry(period, symbol, error?.retryAfterMs || 0);
        }
      }).then(() => {
        if (state.loadId === generation && state.symbol === symbol) {
          this.chanIntradayLoading = { ...this.chanIntradayLoading, [period]: false };
        }
      });
    },
    isChanTouchInput() {
      const coarsePointer = window.matchMedia && window.matchMedia("(hover: none), (pointer: coarse)").matches;
      const compactTouchScreen = Number(navigator.maxTouchPoints) > 0
        && window.matchMedia && window.matchMedia("(max-width: 600px)").matches;
      return Boolean(coarsePointer || compactTouchScreen);
    },
    showChanScenario(key, event) {
      if (this.isChanTouchInput() && ["mouseenter", "focusin"].includes(event?.type)) return;
      this.chanSpotGuideVisible = false;
      if (this.chanScenarioHideTimer) window.clearTimeout(this.chanScenarioHideTimer);
      this.chanScenarioHideTimer = null;
      this.chanHoveredScenario = key;
      this.positionChanScenarioCard(key, event);
    },
    positionChanScenarioCard(key, event) {
      if (!event || !Number.isFinite(event.clientX)) return;
      this.$nextTick(() => {
        if (this.chanHoveredScenario !== key) return;
        const chart = document.querySelector(".chan-chart");
        const card = document.getElementById(`chan-scenario-card-${key}`);
        if (!chart || !card) return;
        const chartRect = chart.getBoundingClientRect();
        const maxLeft = Math.max(0, chart.clientWidth - card.offsetWidth);
        this.chanScenarioCardX = Math.max(0, Math.min(maxLeft, event.clientX - chartRect.left - card.offsetWidth / 2));
      });
    },
    hideChanScenario(event) {
      if (this.isChanTouchInput() && ["mouseleave", "focusout"].includes(event?.type)) return;
      if (this.chanScenarioHideTimer) window.clearTimeout(this.chanScenarioHideTimer);
      this.chanScenarioHideTimer = null;
      this.chanHoveredScenario = null;
    },
    toggleChanScenario(key, event) {
      if (!this.isChanTouchInput()) return;
      event?.preventDefault();
      this.chanHoveredScenario = this.chanHoveredScenario === key ? null : key;
      if (this.chanHoveredScenario) this.chanSpotGuideVisible = false;
      if (this.chanHoveredScenario) this.positionChanScenarioCard(key, event);
    },
    dismissChanScenarioOnTouch(event) {
      if (!this.isChanTouchInput() || event.target.closest(".chan-projection-up, .chan-projection-down, .chan-projection-range, .chan-projection-float-card, .chan-spot-canvas-hitbox, .chan-spot-float-card")) return;
      this.chanHoveredScenario = null;
      this.chanSpotGuideVisible = false;
    },
    preferredChanSpotGuideSide() {
      const score = (value) => Number(String(value || "").match(/\d+(?:\.\d+)?/)?.[0] || 0);
      return score(this.chanFrame?.spotGuide?.bearishTilt) > score(this.chanFrame?.spotGuide?.bullishTilt) ? "down" : "up";
    },
    setChanSpotGuideSide(side) {
      if (side === "up" || side === "down") this.chanSpotGuideSide = side;
    },
    dismissChanSpotGuide() {
      this.chanSpotGuideVisible = false;
    },
    positionChanSpotGuide() {
      this.$nextTick(() => {
        if (!this.chanSpotGuideVisible) return;
        const card = document.getElementById("chan-spot-guide-card");
        if (!card) return;
        const viewportWidth = document.documentElement.clientWidth || window.innerWidth;
        const viewportHeight = window.visualViewport?.height || document.documentElement.clientHeight || window.innerHeight;
        const margin = viewportWidth <= 600 ? 8 : 16;
        const maxLeft = Math.max(margin, viewportWidth - card.offsetWidth - margin);
        const spotHitbox = document.querySelector(".chan-chart .chan-spot-canvas-hitbox");
        const hitboxRect = spotHitbox?.getBoundingClientRect();
        const mobileViewport = viewportWidth <= 600;
        if (mobileViewport) {
          this.chanSpotGuideX = viewportWidth / 2;
          this.chanSpotGuideY = null;
          return;
        }
        card.style.maxHeight = Math.max(120, viewportHeight - 32) + "px";
        // Anchor the popup just to the right of the canvas live-price label;
        // keep it fixed there instead of following the pointer along the line.
        const preferredLeft = hitboxRect
          ? hitboxRect.right + 12
          : (viewportWidth - card.offsetWidth) / 2;
        this.chanSpotGuideX = Math.max(margin, Math.min(maxLeft, preferredLeft));
        this.chanSpotGuideY = Math.max(16, Math.min(viewportHeight - card.offsetHeight - 16, (viewportHeight - card.offsetHeight) / 2));
      });
    },
    toggleChanSpotGuide(event) {
      this.chanSpotGuideVisible = !this.chanSpotGuideVisible;
      event?.preventDefault();
      if (this.chanSpotGuideVisible) this.chanHoveredScenario = null;
      if (this.chanSpotGuideVisible) {
        this.chanSpotGuideSide = this.preferredChanSpotGuideSide();
        this.positionChanSpotGuide();
      }
    },
    toggleChanScenarios() {
      this.chanScenariosVisible = !this.chanScenariosVisible;
      this.chanHoveredScenario = null;
      if (this.chanScenarioHideTimer) window.clearTimeout(this.chanScenarioHideTimer);
      this.chanScenarioHideTimer = null;
      try { localStorage.setItem(CHAN_SCENARIOS_STORAGE_KEY, this.chanScenariosVisible ? "1" : "0"); } catch (error) { /* 存储不可用时保留当前页面状态。 */ }
      if (this.chanPayload) this.view.chan = OptionScopeChan.view(this.chanPayload, this.chanScenariosVisible, this.chanMobileChart, 30, this.chanViewports, this.chanChartLayers);
    },
    toggleChanChartLayer(layer) {
      if (layer === "pens" || layer === "penCenters" || layer === "legacySignals" || layer === "chanCandles" || layer === "signals") {
        this.chanChartLayers = { ...this.chanChartLayers, [layer]: !this.chanChartLayers[layer] };
      } else {
        const level = Number(layer);
        if (!Number.isInteger(level) || level < 0 || level > 7) return;
        const recursiveCenters = this.chanChartLayers?.recursiveCenters || {};
        const centers = { ...recursiveCenters, [level]: !OptionScopeChan.isRecursiveCenterLevelVisible(this.chanChartLayers, level) };
        this.chanChartLayers = { ...this.chanChartLayers, recursiveCenters: centers };
      }
      rememberChanChartPreferences(this.chanPeriod, this.chanChartLayers);
      if (this.chanPayload) this.view.chan = OptionScopeChan.view(this.chanPayload, this.chanScenariosVisible, this.chanMobileChart, 30, this.chanViewports, this.chanChartLayers);
    },
    toggleChanSegmentLevel(level) {
      this.chanChartLayers = OptionScopeChan.toggleSegmentLevel(this.chanChartLayers, level);
      rememberChanChartPreferences(this.chanPeriod, this.chanChartLayers);
      if (this.chanPayload) this.view.chan = OptionScopeChan.view(this.chanPayload, this.chanScenariosVisible, this.chanMobileChart, 30, this.chanViewports, this.chanChartLayers);
    },
    isChanSegmentLevelVisible(level) {
      return OptionScopeChan.isSegmentLevelVisible(this.chanChartLayers, level);
    },
    isChanRecursiveCenterLevelVisible(level) {
      return OptionScopeChan.isRecursiveCenterLevelVisible(this.chanChartLayers, level);
    },
    changeChanViewport(action) {
      const chart = this.chanFrame?.chart;
      if (!chart?.candles?.length) return;
      const viewport = this.chanViewports[this.chanPeriod];
      const total = chart.totalBars || chart.candles.length;
      const current = chart.visibleBars || chart.candles.length;
      const currentOffset = chart.endOffset || 0;
      const maxVisible = total;
      if (action === "reset") { viewport.visibleBars = 0; viewport.endOffset = 0; }
      else if (action === "zoom-in" || action === "zoom-out") {
        const next = Math.max(Math.min(12, total), Math.min(maxVisible, Math.round(current * (action === "zoom-in" ? 0.75 : 1.35))));
        const center = total - currentOffset - current / 2;
        viewport.visibleBars = next;
        viewport.endOffset = Math.max(0, Math.min(total - next, Math.round(total - center - next / 2)));
      } else {
        const step = Math.max(1, Math.round(current * 0.7));
        if (action === "left") viewport.endOffset = Math.max(0, Math.min(total - current, currentOffset + step));
        if (action === "right") viewport.endOffset = Math.max(0, currentOffset - step);
      }
      this.commitChanViewport(viewport);
    },
    commitChanViewport(viewport) {
      const period = this.chanPeriod;
      this.chanViewports = { ...this.chanViewports, [period]: { visibleBars: viewport.visibleBars, endOffset: viewport.endOffset } };
      if ((period === "daily" || period === "weekly") && this.chanPayload) {
        this.view.chan = OptionScopeChan.view(this.chanPayload, this.chanScenariosVisible, this.chanMobileChart, 30, this.chanViewports, this.chanChartLayers);
      }
    },
    panChanChart(deltaBars) {
      const chart = this.chanFrame?.chart;
      if (!chart?.candles?.length || !Number.isFinite(deltaBars) || !deltaBars) return;
      const viewport = { ...this.chanViewports[this.chanPeriod] };
      const maxOffset = Math.max(0, chart.totalBars - chart.visibleBars);
      viewport.visibleBars = chart.visibleBars;
      viewport.endOffset = Math.max(0, Math.min(maxOffset, (viewport.endOffset || chart.endOffset) + Math.round(deltaBars)));
      this.commitChanViewport(viewport);
    },
    zoomChanChart(factor, anchorIndex = null, anchorRatio = 0.5, baseVisible = null) {
      const chart = this.chanFrame?.chart;
      if (!chart?.candles?.length) return;
      const total = chart.totalBars || chart.candles.length;
      const minVisible = Math.min(12, total);
      const viewport = this.chanViewports[this.chanPeriod];
      const visible = Number.isFinite(baseVisible) ? baseVisible : (viewport.visibleBars || chart.visibleBars);
      const next = Math.max(minVisible, Math.min(total, Math.round(visible * factor)));
      const ratioValue = Math.max(0, Math.min(1, Number(anchorRatio) || 0));
      const currentStart = Math.max(0, total - (viewport.endOffset || chart.endOffset) - visible);
      const anchor = Number.isFinite(anchorIndex) ? anchorIndex : currentStart + ratioValue * Math.max(0, visible - 1);
      const start = Math.max(0, Math.min(total - next, Math.round(anchor - ratioValue * (next - 1))));
      this.commitChanViewport({ visibleBars: next, endOffset: Math.max(0, total - start - next) });
    },
    toggleChanPanel() {
      this.chanExpanded = !this.chanExpanded;
      rememberFoldState(CHAN_FOLD_KEYS.panel, this.chanExpanded);
      if (!this.chanExpanded) return;
      if (CHAN_INTRADAY_PERIODS.includes(this.chanPeriod)) this.ensureChanIntraday(this.chanPeriod);
      if (state.chanStale || state.chanPayload?.symbol !== state.symbol) refreshChanAnalysis(true);
    },
    toggleChanStats() {
      this.chanFolds.stats = !this.chanFolds.stats;
      rememberFoldState(CHAN_FOLD_KEYS.stats, this.chanFolds.stats);
    },
    rememberChanFold(name, event) {
      if (!Object.prototype.hasOwnProperty.call(this.chanFolds, name)) return;
      const expanded = event.currentTarget.open;
      this.chanFolds[name] = expanded;
      rememberFoldState(CHAN_FOLD_KEYS[name], expanded);
    },
    expirationChanged() { return switchExpiration(this.expiration); },
    expirationMenuChanged(open) { return setExpirationMenuOpen(open); },
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
initOptionAnalysisGroup();
// 时钟是独立的高频显示，不进入 Vue 响应式树，避免每秒遍历整张期权链的虚拟 DOM。
function updateClock() {
  const clock = byId("clock");
  if (clock) clock.textContent = clockFormatter.format(new Date());
  // 状态跟随时钟更新，不写 Vue 数据。
}
const clockFormatter = new Intl.DateTimeFormat("zh-CN", { hour: "2-digit", minute: "2-digit", second: "2-digit", hour12: false });
updateClock();
setInterval(updateClock, 1000);
// 容器尺寸与上次绘制不一致时按新尺寸重绘图表：窗口缩放、图表折叠组展开后都走这里。
// 图表是按容器实际像素绘制的，隐藏状态下只能量到最小尺寸，所以展开后必须补一次重绘。
function redrawChartsIfResized() {
  if (!state.lastAnalysis) {
    if (byId("gex-chart")?.querySelector("svg[data-skeleton]")) showChartSkeletons();
    return;
  }
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
// The Chan chart uses a taller coordinate system on phones so the candles
// remain legible without clipping history or shrinking the scenario lane.
let chanViewportTimer = null;
window.addEventListener("resize", () => {
  clearTimeout(chanViewportTimer);
  chanViewportTimer = setTimeout(() => {
    const mobileChart = Boolean(window.matchMedia && window.matchMedia("(max-width: 600px)").matches);
    if (state.chanMobileChart === mobileChart) return;
    state.chanMobileChart = mobileChart;
    if (state.chanPayload) state.view.chan = OptionScopeChan.view(state.chanPayload, state.chanScenariosVisible, mobileChart, 30, state.chanViewports, state.chanChartLayers);
  }, 120);
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
  // 首屏加载完成后由活跃 SSE 订阅启动服务端更新调度。
  state.loading = true;
  scheduleAutoRefresh();
  showChartSkeletons();
  loadSymbol();
}
