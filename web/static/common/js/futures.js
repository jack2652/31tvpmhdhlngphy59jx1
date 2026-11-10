/* 独立期货行情页：复用请求层与缠论 Canvas，不加载股票期权分析状态。 */
(function (window) {
  "use strict";

  const FUTURE_PATTERN = /^(?:[A-Z0-9]+(?:-[A-Z0-9]+)?=F)$/;
  const presets = [
    { symbol: "ES=F", label: "标普期货" }, { symbol: "NQ=F", label: "纳指期货" },
    { symbol: "YM=F", label: "道指期货" }, { symbol: "CL=F", label: "原油期货" },
    { symbol: "GC=F", label: "黄金期货" },
  ];
  const periods = [{ value: "daily", label: "日线" }, { value: "weekly", label: "周线" }, { value: "15m", label: "15分钟" }, { value: "1h", label: "1小时" }, { value: "2h", label: "2小时" }, { value: "4h", label: "4小时" }];
  const intradayPeriods = ["15m", "1h", "2h", "4h"];
  const intradayRetryLimit = 12;
  const indicators = [
    { value: "base", label: "缠论结构" }, { value: "afStochZ", label: "AF StochZ" },
    { value: "turtle", label: "海龟交易法" }, { value: "granville", label: "葛兰碧法则" },
    { value: "wyckoff", label: "威科夫阶段" }, { value: "supertrend", label: "超级趋势" },
    { value: "macdSma200", label: "异同移动平均线 + 200期简单均线" },
  ];
  const defaultLayers = { pens: true, penCenters: true, legacySignals: true, chanCandles: true, segments: false, segmentLevels: {}, recursiveCenters: {}, signals: true, supportResistance: true, afStochZ: false, trendlines: false, turtle: false, granville: false, macdSma200: false, supertrend: false, wyckoff: false, mfi: false };
  const defaultSubcharts = { volume: false, macd: false, dmi: false, mfi: false };
  const exclusiveIndicators = ["afStochZ", "turtle", "granville", "macdSma200", "supertrend", "wyckoff"];
  const chartPreferencesKey = "option-scope-futures-chart-preferences";
  const chartPreferencesVersion = 1;
  const scenarioVisibilityKey = "option-scope-chan-scenarios-visible";
  const foldStorageKeys = {
    panel: "option-scope-futures-chan-panel",
    stats: "option-scope-futures-chan-stats",
    more: "option-scope-futures-chan-more",
  };
  const query = new URLSearchParams(window.location.search);
  const initial = String(query.get("symbol") || "ES=F").trim().toUpperCase();
  const symbol = FUTURE_PATTERN.test(initial) ? initial : "ES=F";
  const accessKey = query.get("key") || readStoredKey();
  let clockTimer = null;
  let mfiBenchmarkRetryTimer = null;
  let quoteStream = null;
  let quoteStreamRetryTimer = null;

  function readStoredKey() {
    try { return localStorage.getItem("option-scope-access-key") || sessionStorage.getItem("option-scope-access-key") || ""; } catch (error) { return ""; }
  }
  function readScenarioVisibility() {
    try { return localStorage.getItem(scenarioVisibilityKey) === "1"; } catch (error) { return false; }
  }
  function readFoldState(key, defaultExpanded) {
    try {
      const stored = localStorage.getItem(key);
      if (stored === "1") return true;
      if (stored === "0") return false;
    } catch (error) { /* 浏览器存储不可用时使用默认状态。 */ }
    return defaultExpanded;
  }
  function rememberFoldState(key, expanded) {
    try { localStorage.setItem(key, expanded ? "1" : "0"); } catch (error) { /* 存储不可用时保留当前页面状态。 */ }
  }
  function withAccessKey(path) {
    if (!accessKey) return path;
    const url = new URL(path, window.location.origin);
    if (!url.searchParams.has("key")) url.searchParams.set("key", accessKey);
    return `${url.pathname}${url.search}`;
  }
  function request(path, options) {
    return OptionScopeRequest.request(path, options || {}, { accessKey, required: document.querySelector('meta[name="option-scope-access-required"]')?.content === "true", withAccessKey, onDenied: function () {} });
  }
  function finite(value) { return Number.isFinite(Number(value)) ? Number(value) : null; }
  function money(value) { const number = finite(value); return number == null ? "--" : number.toLocaleString("en-US", { minimumFractionDigits: 2, maximumFractionDigits: 2 }); }
  function formatTime(value) { const date = new Date(value || ""); return Number.isNaN(date.getTime()) ? "时间未知" : date.toLocaleString("zh-CN", { hour12: false }); }
  function displayPeriod(value) { return periods.find((item) => item.value === value)?.label || value; }
  function quoteIsNewer(incoming, current) {
    const incomingAt = Date.parse(incoming?.fetched_at || "");
    const currentAt = Date.parse(current?.fetched_at || "");
    return !Number.isFinite(currentAt) || !Number.isFinite(incomingAt) || incomingAt > currentAt;
  }
  function signalTimestamp(signal) {
    const timestamp = Date.parse(signal?.confirmed_at || signal?.observed_at || signal?.date || "");
    return Number.isFinite(timestamp) ? timestamp : 0;
  }
  function copyLayers(source) {
    const layers = { ...defaultLayers, segmentLevels: {}, recursiveCenters: {} };
    if (!source || typeof source !== "object") return layers;
    for (const key of Object.keys(defaultLayers)) {
      if (["segmentLevels", "recursiveCenters"].includes(key)) continue;
      if (typeof source[key] === "boolean") layers[key] = source[key];
    }
    for (const key of ["segmentLevels", "recursiveCenters"]) {
      const values = source[key];
      if (!values || typeof values !== "object" || Array.isArray(values)) continue;
      layers[key] = Object.fromEntries(Object.entries(values).filter(([level, visible]) => /^[0-7]$/.test(level) && typeof visible === "boolean"));
    }
    return layers;
  }
  function copySubcharts(source) {
    const subcharts = { ...defaultSubcharts };
    if (!source || typeof source !== "object") return subcharts;
    for (const key of Object.keys(defaultSubcharts)) if (typeof source[key] === "boolean") subcharts[key] = source[key];
    return subcharts;
  }
  function readChartPreferences() {
    try {
      const stored = JSON.parse(localStorage.getItem(chartPreferencesKey) || "null");
      const layers = copyLayers(stored?.layers);
      const subcharts = copySubcharts(stored?.subcharts);
      if (typeof stored?.subcharts?.mfi !== "boolean" && typeof stored?.layers?.mfi === "boolean") subcharts.mfi = stored.layers.mfi;
      layers.mfi = subcharts.mfi;
      const hasConflictingIndicators = exclusiveIndicators.filter((key) => layers[key]).length > 1;
      if (hasConflictingIndicators) exclusiveIndicators.forEach((key) => { layers[key] = false; });
      const preferences = { period: periods.some((item) => item.value === stored?.period) ? stored.period : "daily", layers, subcharts };
      if (Number(stored?.version) !== chartPreferencesVersion || hasConflictingIndicators) rememberChartPreferences(preferences.period, layers, subcharts);
      return preferences;
    } catch (error) {
      return { period: "daily", layers: copyLayers(), subcharts: copySubcharts() };
    }
  }
  function rememberChartPreferences(period, layers, subcharts) {
    try { localStorage.setItem(chartPreferencesKey, JSON.stringify({ version: chartPreferencesVersion, period, layers, subcharts })); } catch (error) { /* 存储不可用时保留当前页面选择。 */ }
  }
  function marketStateLabel(value) { return ({ REGULAR: "正常交易", PRE: "盘前", POST: "盘后", OVERNIGHT: "夜盘", CLOSED: "休市" })[String(value || "").toUpperCase()] || "行情状态未知"; }
  function sanitizeDisplayText(value, fallback) {
    const text = String(value == null ? (fallback || "") : value)
      .replace(/(?:https?:\/\/)?(?:query\d+\.)?finance\.yahoo\.com|(?:Yahoo(?:\s+Finance)?|Alpaca|Cboe|SQLite|go-yfinance)(?:[-_]\w+)?/gi, "行情服务")
      .replace(/可能(?:存在|有)?延迟|(?:存在|有)延迟/g, "")
      .replace(/\s*([·；，。,.;])\s*/g, "$1")
      .trim();
    return text || (fallback || "");
  }
  function mobileChartViewport() { return window.matchMedia ? window.matchMedia("(max-width: 600px)").matches : window.innerWidth <= 600; }
  function hasMfiBenchmarkHistory(payload) {
    const bars = payload?.market_history?.daily;
    return Array.isArray(bars) && bars.filter((bar) => Number(bar?.volume) > 0
      && [bar?.high, bar?.low, bar?.close].every((value) => Number.isFinite(Number(value)))).length >= 15;
  }
  function intradayFrameReady(payload) {
    return Number(payload?.frame?.bar_count) > 0 && Array.isArray(payload?.frame?.candles)
      && payload.frame.candles.length > 0;
  }
  function navigate(symbol) {
    const params = new URLSearchParams();
    if (accessKey) params.set("key", accessKey);
    params.set("symbol", symbol);
    window.location.assign(`/futures?${params.toString()}`);
  }

  function closeQuoteStream() {
    if (quoteStreamRetryTimer) window.clearTimeout(quoteStreamRetryTimer);
    quoteStreamRetryTimer = null;
    if (quoteStream) quoteStream.close();
    quoteStream = null;
  }

  if (window.Vue && window.OptionScopeCharts && window.OptionScopeChan) {
    Vue.directive("chan-chart-canvas", OptionScopeCharts.chanChartCanvas);
    Vue.directive("chan-chart-hover", OptionScopeCharts.chanChartHover);
    Vue.directive("chan-chart-interaction", OptionScopeCharts.chanChartInteraction);
  }

  const chartPreferences = readChartPreferences();

  const app = new Vue({
    el: "#futures-app",
    data: {
      symbol, symbolInput: symbol, presets, periods, indicators, period: chartPreferences.period,
      chartLayers: chartPreferences.layers, subcharts: chartPreferences.subcharts,
      quote: null, payload: null, intradayPayload: {}, intradayLoading: { "15m": false, "1h": false, "2h": false, "4h": false }, intradayRetryTimers: { "15m": null, "1h": null, "2h": null, "4h": null }, intradayRetryAttempts: { "15m": 0, "1h": 0, "2h": 0, "4h": 0 }, loading: false, error: "", mfiBenchmarkRetryCount: 0, clock: "--:--:--", theme: document.documentElement.dataset.theme || "light", mobileChart: mobileChartViewport(), chanExpanded: readFoldState(foldStorageKeys.panel, true), chanStatsExpanded: readFoldState(foldStorageKeys.stats, false), chanMoreExpanded: readFoldState(foldStorageKeys.more, false), chanScenariosVisible: readScenarioVisibility(), chanHoveredScenario: null, chanScenarioCardX: 0,
      viewport: { daily: { visibleBars: 0, endOffset: 0 }, weekly: { visibleBars: 0, endOffset: 0 }, "15m": { visibleBars: 0, endOffset: 0 }, "1h": { visibleBars: 0, endOffset: 0 }, "2h": { visibleBars: 0, endOffset: 0 }, "4h": { visibleBars: 0, endOffset: 0 } },
    },
    computed: {
      displaySymbol() { return this.symbol.replace("=F", ""); },
      priceLabel() { return money(this.quote?.price); },
      closeLabel() { return money(this.frame?.chart?.lastClose); },
      changeClass() { return finite(this.quote?.change_percent) >= 0 ? "futures-up" : "futures-down"; },
      changeLabel() { const value = finite(this.quote?.change_percent); return value == null ? "涨跌 --" : `${value >= 0 ? "+" : ""}${value.toFixed(2)}%`; },
      marketState() { return marketStateLabel(this.quote?.market_state); },
      marketLabel() { return this.loading ? "更新中" : this.marketState; },
      fetchedLabel() { return this.quote?.fetched_at ? `快照 ${formatTime(this.quote.fetched_at)}` : "等待快照"; },
      themeLabel() { return this.theme === "dark" ? "切换为浅色主题" : "切换为深色主题"; },
      periodLabel() { return displayPeriod(this.period); },
      dataSourceLabel() { return "行情数据"; },
      activeIndicator() {
        return exclusiveIndicators.find((key) => this.chartLayers[key]) || "base";
      },
      frame() {
        const layers = this.chartLayers;
        if (intradayPeriods.includes(this.period)) {
          const payload = this.intradayPayload[this.period];
          return payload ? OptionScopeChan.intradayView(payload, this.chanScenariosVisible, this.quote?.price, this.mobileChart, 50, this.viewport[this.period], layers, this.subcharts, { marketHistory: this.payload?.market_history }) : null;
        }
        const payload = this.payload && this.quote?.symbol === this.symbol
          ? OptionScopeChan.withSpot(this.payload, this.quote.price, null, this.quote)
          : this.payload;
        const view = payload ? OptionScopeChan.view(payload, this.chanScenariosVisible, this.mobileChart, 50, this.viewport, layers, this.subcharts) : null;
        return view ? view[this.period] : null;
      },
      guidanceTitle() { return this.frame?.guidance?.title || "等待结构形成"; },
      guidanceFocus() { return this.frame?.guidance?.focus || "载入行情后显示缠论位置和条件观察。"; },
      positionLabel() { return this.frame?.position || "等待中枢形成"; },
      centerLabel() { return this.frame?.center || "尚未形成"; },
      strokeLabel() { return this.frame?.stroke || "尚未成笔"; },
      structureLabel() { return this.frame?.structure || "等待结构"; },
      chartAsOf() { return this.frame?.asOf || "--"; },
      barCountLabel() { return this.frame ? `${this.frame.bars || 0} 根 K 线` : "等待数据"; },
      analysisNote() {
        const indicator = this.activeIndicator === "base" ? null : this.indicators.find((item) => item.value === this.activeIndicator);
        const subchartNames = Object.entries({ volume: "成交量", macd: "MACD", dmi: "DMI", mfi: "MFI" }).filter(([key]) => this.subcharts[key]).map(([, label]) => label);
        if (!indicator && !subchartNames.length) return "已收盘 K 线识别结构，现价仅用于定位观察。";
        const parts = [indicator?.label, subchartNames.length ? `副图：${subchartNames.join("、")}` : ""].filter(Boolean);
        return `${parts.join("；")}；规则信号仅供研究参考。`;
      },
      indicatorTitle() {
        const indicator = this.activeIndicator === "base" ? null : this.indicators.find((item) => item.value === this.activeIndicator);
        const subcharts = Object.entries({ volume: "成交量", macd: "MACD", dmi: "DMI", mfi: "MFI" }).filter(([key]) => this.subcharts[key]).map(([, label]) => label);
        return indicator?.label || (subcharts.length ? `副图：${subcharts.join("、")}` : "技术指标");
      },
      indicatorSummary() {
        if (this.activeIndicator === "base") return this.frame?.observation || "等待缠论结构分析。";
        const markerSignals = this.signals.slice(0, 4);
        return markerSignals.length ? markerSignals.map((item) => `${item.label} · ${item.dateLabel || item.date || "当前窗口"}`).join("；") : "当前窗口暂无已确认指标信号。";
      },
      signals() { return (this.frame?.signals || []).filter((item) => item.label).slice().sort((left, right) => signalTimestamp(right) - signalTimestamp(left)).slice(0, 8).map((item, index) => ({ key: `${item.key || item.label}-${index}`, label: item.label, dateLabel: item.dateLabel || item.date || "当前窗口", tone: /买|涨|多/.test(item.label) ? "futures-up" : /卖|跌|空/.test(item.label) ? "futures-down" : "" })); },
      canvasBinding() { return { chart: this.frame?.chart, layers: this.chartLayers, scenariosVisible: this.chanScenariosVisible }; },
      hoverBinding() {
        return {
          chart: this.frame?.chart,
          scenariosVisible: this.chanScenariosVisible,
          onScenarioHover: (key, event) => this.showChanScenario(key, event),
          onScenarioLeave: () => this.hideChanScenario(),
        };
      },
      interactionBinding() { return { chart: this.frame?.chart, scenariosVisible: this.chanScenariosVisible, onPan: (bars) => this.panViewport(bars), onZoom: (factor, anchor, ratio, baseVisible) => this.zoomViewport(factor, anchor, ratio, baseVisible) }; },
    },
    methods: {
      toggleTheme() { this.theme = this.theme === "dark" ? "light" : "dark"; document.documentElement.dataset.theme = this.theme; try { localStorage.setItem("option-scope-theme", this.theme); } catch (error) {} },
      toggleChanPanel() { this.chanExpanded = !this.chanExpanded; rememberFoldState(foldStorageKeys.panel, this.chanExpanded); },
      toggleChanStats() { this.chanStatsExpanded = !this.chanStatsExpanded; rememberFoldState(foldStorageKeys.stats, this.chanStatsExpanded); },
      toggleChanMore() { this.chanMoreExpanded = !this.chanMoreExpanded; rememberFoldState(foldStorageKeys.more, this.chanMoreExpanded); },
      toggleChanScenarios() {
        this.chanScenariosVisible = !this.chanScenariosVisible;
        if (!this.chanScenariosVisible) this.chanHoveredScenario = null;
        try { localStorage.setItem(scenarioVisibilityKey, this.chanScenariosVisible ? "1" : "0"); } catch (error) { /* 存储不可用时保留当前页面选择。 */ }
      },
      showChanScenario(key, event) {
        this.chanHoveredScenario = key;
        this.$nextTick(() => {
          if (this.chanHoveredScenario !== key || !event || !Number.isFinite(event.clientX)) return;
          const chart = document.querySelector(".futures-chart-workspace .chan-chart");
          const card = chart?.querySelector(`#chan-scenario-card-${key}`);
          if (!chart || !card) return;
          const chartRect = chart.getBoundingClientRect();
          const maxLeft = Math.max(0, chart.clientWidth - card.offsetWidth);
          this.chanScenarioCardX = Math.max(0, Math.min(maxLeft, event.clientX - chartRect.left - card.offsetWidth / 2));
        });
      },
      hideChanScenario() { this.chanHoveredScenario = null; },
      selectPreset(value) { this.symbolInput = value; navigate(value); },
      selectPeriod(value) {
        if (!periods.some((item) => item.value === value)) return;
        this.period = value;
        rememberChartPreferences(this.period, this.chartLayers, this.subcharts);
        if (intradayPeriods.includes(value)) this.loadIntraday(value);
      },
      setIndicator(value) {
        if (value === "base") {
          this.chartLayers = { ...this.chartLayers, ...Object.fromEntries(exclusiveIndicators.map((key) => [key, false])) };
        } else if (value === "mfi") {
          this.toggleSubchart("mfi");
          return;
        } else if (exclusiveIndicators.includes(value)) {
          const enabled = this.chartLayers[value] !== true;
          this.chartLayers = { ...this.chartLayers, ...Object.fromEntries(exclusiveIndicators.map((key) => [key, enabled && key === value])) };
        }
        rememberChartPreferences(this.period, this.chartLayers, this.subcharts);
      },
      closeChanLayerMenu(event) {
        const menu = event?.currentTarget?.closest(".chan-layer-menu, .chan-subchart-menu, .chan-period-menu, .chan-indicator-menu");
        if (!menu) return;
        menu.open = false;
        menu.querySelector("summary")?.focus();
      },
      toggleSubchart(name) {
        if (!Object.prototype.hasOwnProperty.call(defaultSubcharts, name)) return;
        const enabled = !this.subcharts[name];
        this.subcharts = { ...this.subcharts, [name]: enabled };
        if (name === "mfi") this.chartLayers = { ...this.chartLayers, mfi: enabled };
        rememberChartPreferences(this.period, this.chartLayers, this.subcharts);
        if (name === "mfi" && enabled) {
          this.mfiBenchmarkRetryCount = 0;
          if (!hasMfiBenchmarkHistory(this.payload)) this.loadMfiBenchmark();
        }
      },
      toggleChartLayer(layer) {
        if (layer === "mfi") return this.toggleSubchart("mfi");
        if (exclusiveIndicators.includes(layer)) {
          const enabled = !this.chartLayers[layer];
          this.chartLayers = { ...this.chartLayers, ...Object.fromEntries(exclusiveIndicators.map((key) => [key, enabled && key === layer])) };
        } else if (["pens", "penCenters", "legacySignals", "chanCandles", "segments", "signals", "supportResistance", "trendlines"].includes(layer)) {
          this.chartLayers = { ...this.chartLayers, [layer]: !this.chartLayers[layer] };
        } else {
          const level = Number(layer);
          if (!Number.isInteger(level) || level < 0 || level > 7) return;
          const centers = { ...(this.chartLayers.recursiveCenters || {}), [level]: !OptionScopeChan.isRecursiveCenterLevelVisible(this.chartLayers, level) };
          this.chartLayers = { ...this.chartLayers, recursiveCenters: centers };
        }
        rememberChartPreferences(this.period, this.chartLayers, this.subcharts);
      },
      toggleSegmentLevel(level) {
        this.chartLayers = OptionScopeChan.toggleSegmentLevel(this.chartLayers, level);
        rememberChartPreferences(this.period, this.chartLayers, this.subcharts);
      },
      isSegmentLevelVisible(level) { return OptionScopeChan.isSegmentLevelVisible(this.chartLayers, level); },
      isRecursiveCenterLevelVisible(level) { return OptionScopeChan.isRecursiveCenterLevelVisible(this.chartLayers, level); },
      changeViewport(action) {
        const chart = this.frame?.chart; if (!chart) return;
        if (action === "left") this.panViewport(-Math.max(1, Math.floor(chart.visibleBars * 0.25)));
        else if (action === "right") this.panViewport(Math.max(1, Math.floor(chart.visibleBars * 0.25)));
        else if (action === "reset") { this.viewport = { ...this.viewport, [this.period]: { visibleBars: 0, endOffset: 0 } }; }
        else this.zoomViewport(action === "zoom-in" ? 0.75 : 1.3, chart.windowStart + chart.visibleBars / 2, 0.5, chart.visibleBars);
      },
      panViewport(bars) {
        const chart = this.frame?.chart;
        if (!chart?.candles?.length || !Number.isFinite(Number(bars)) || !Number(bars)) return;
        const current = this.viewport[this.period] || {};
        const total = Number(chart.totalBars) || chart.candles.length;
        const visible = Number(chart.visibleBars) || chart.candles.length;
        const maxOffset = Number.isFinite(Number(chart.maxOffset)) ? Number(chart.maxOffset) : Math.max(0, total - visible);
        const maxFutureOffset = Number.isFinite(Number(chart.maxFutureOffset))
          ? Number(chart.maxFutureOffset)
          : OptionScopeChan.futureOffsetLimit(visible);
        const currentOffset = Number.isFinite(Number(current.endOffset)) ? Number(current.endOffset) : (Number(chart.endOffset) || 0);
        const endOffset = Math.max(-maxFutureOffset, Math.min(maxOffset, currentOffset + Math.round(Number(bars))));
        this.viewport = { ...this.viewport, [this.period]: { ...current, visibleBars: current.visibleBars || chart.visibleBars, endOffset } };
      },
      zoomViewport(factor, anchor, ratio, baseVisible) { const current = this.viewport[this.period] || {}; const visible = Math.max(20, Math.round(Number(baseVisible || current.visibleBars || 50) * Number(factor || 1))); this.viewport = { ...this.viewport, [this.period]: { ...current, visibleBars: visible } }; },
      scheduleIntradayRetry(value) {
        if (!intradayPeriods.includes(value) || this.intradayRetryTimers[value] || (this.intradayRetryAttempts[value] || 0) >= intradayRetryLimit) return;
        const attempt = this.intradayRetryAttempts[value] || 0;
        const delay = Math.min(15000, 1500 * (2 ** Math.min(attempt, 3)));
        this.$set(this.intradayRetryAttempts, value, attempt + 1);
        const timer = window.setTimeout(() => {
          this.$set(this.intradayRetryTimers, value, null);
          this.loadIntraday(value, true);
        }, delay);
        this.$set(this.intradayRetryTimers, value, timer);
      },
      async loadIntraday(value, refresh = false) {
        if (!intradayPeriods.includes(value) || this.intradayLoading[value]) return;
        const existing = this.intradayPayload[value];
        if (!refresh && intradayFrameReady(existing) && !existing.refreshing) return;
        if (!refresh && this.intradayRetryTimers[value]) return;
        const token = this.symbol;
        this.$set(this.intradayLoading, value, true);
        try {
          const spot = finite(this.quote?.price);
          const queryString = spot ? `?period=${value}&spot=${encodeURIComponent(spot)}` : `?period=${value}`;
          const payload = await request(`/api/chan/${encodeURIComponent(token)}/intraday${queryString}`, { timeout: 60000 });
          if (this.symbol !== token) return;
          if (intradayFrameReady(payload) || !intradayFrameReady(existing)) this.$set(this.intradayPayload, value, payload);
          if (intradayFrameReady(payload) && !payload.refreshing) {
            window.clearTimeout(this.intradayRetryTimers[value]);
            this.$set(this.intradayRetryTimers, value, null);
            this.$set(this.intradayRetryAttempts, value, 0);
          } else {
            this.scheduleIntradayRetry(value);
          }
        } catch (error) {
          if (this.symbol === token) this.scheduleIntradayRetry(value);
        } finally {
          if (this.symbol === token) this.$set(this.intradayLoading, value, false);
        }
      },
      scheduleMfiBenchmarkRetry() {
        if (mfiBenchmarkRetryTimer || !this.payload || hasMfiBenchmarkHistory(this.payload) || this.mfiBenchmarkRetryCount >= 12) return;
        mfiBenchmarkRetryTimer = window.setTimeout(() => {
          mfiBenchmarkRetryTimer = null;
          this.mfiBenchmarkRetryCount += 1;
          this.loadMfiBenchmark();
        }, 5000);
      },
      async loadMfiBenchmark() {
        const token = this.symbol;
        const params = new URLSearchParams({ include_mfi_market_history: "true", light: "true" });
        const spot = finite(this.quote?.price);
        if (spot) params.set("spot", spot);
        try {
          const payload = await request(`/api/chan/${encodeURIComponent(token)}?${params.toString()}`, { timeout: 60000 });
          if (this.symbol !== token) return;
          this.payload = { ...this.payload, market_history: payload.market_history };
          if (this.subcharts.mfi) this.scheduleMfiBenchmarkRetry();
        } catch (error) {
          if (this.symbol === token) this.scheduleMfiBenchmarkRetry();
        }
      },
      async loadSymbol() {
        const entered = String(this.symbolInput || "").trim().toUpperCase();
        if (!FUTURE_PATTERN.test(entered)) { this.error = "请输入期货代码，例如 ES=F"; return; }
        if (entered !== this.symbol) { navigate(entered); return; }
        this.loading = true; this.error = ""; this.mfiBenchmarkRetryCount = 0; const token = this.symbol;
        try {
          const quote = await request(`/api/quote/${encodeURIComponent(token)}?refresh=true`, { timeout: 30000 });
          if (this.symbol !== token) return;
          if (quoteIsNewer(quote, this.quote)) this.quote = quote;
          const spot = finite(this.quote?.price);
          const params = new URLSearchParams();
          if (this.subcharts.mfi) params.set("include_mfi_market_history", "true");
          if (spot) params.set("spot", spot);
          const path = `/api/chan/${encodeURIComponent(token)}?${params.toString()}`;
          const payload = await request(path, { timeout: 60000 });
          if (this.symbol !== token) return;
          this.payload = payload;
          if (this.subcharts.mfi) this.scheduleMfiBenchmarkRetry();
          if (payload?.ready === false || !payload?.daily?.bar_count) this.error = "历史结构正在后台准备，稍后会自动更新";
          if (intradayPeriods.includes(this.period)) this.loadIntraday(this.period);
        } catch (error) { if (this.symbol === token) this.error = sanitizeDisplayText(error.message, "期货行情加载失败"); }
        finally { if (this.symbol === token) this.loading = false; }
      },
      connectQuoteStream() {
        closeQuoteStream();
        if (!window.EventSource || !this.symbol) return;
        const source = new EventSource(withAccessKey(`/api/events/${encodeURIComponent(this.symbol)}`));
        quoteStream = source;
        source.addEventListener("update", (event) => {
          if (quoteStream !== source || this.symbol !== symbol) return;
          let message;
          try { message = JSON.parse(event.data || "{}"); } catch (error) { return; }
          if (message.symbol !== this.symbol || message.kind !== "quote") return;
          const incoming = message.payload?.quote;
          if (!incoming || incoming.symbol !== this.symbol || finite(incoming.price) == null) return;
          if (quoteIsNewer(incoming, this.quote)) this.quote = incoming;
        });
        source.addEventListener("open", () => {
          if (quoteStreamRetryTimer) window.clearTimeout(quoteStreamRetryTimer);
          quoteStreamRetryTimer = null;
        });
        source.addEventListener("error", () => {
          if (quoteStream !== source || quoteStreamRetryTimer) return;
          // EventSource 会先自动重连；连接持续关闭时再创建一个新的订阅。
          quoteStreamRetryTimer = window.setTimeout(() => {
            quoteStreamRetryTimer = null;
            if (quoteStream === source && source.readyState === EventSource.CLOSED) this.connectQuoteStream();
          }, 30000);
        });
      },
    },
    mounted() {
      this.updateMobileChart = () => { this.mobileChart = mobileChartViewport(); };
      window.addEventListener("resize", this.updateMobileChart, { passive: true });
      clockTimer = window.setInterval(() => { this.clock = new Date().toLocaleTimeString("zh-CN", { hour12: false }); }, 1000);
      this.connectQuoteStream();
      this.loadSymbol();
    },
    beforeDestroy() { closeQuoteStream(); if (clockTimer) window.clearInterval(clockTimer); if (mfiBenchmarkRetryTimer) window.clearTimeout(mfiBenchmarkRetryTimer); if (this.updateMobileChart) window.removeEventListener("resize", this.updateMobileChart); },
  });
}(window));
