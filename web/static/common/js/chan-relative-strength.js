/* 相对强弱按同周期同时间价比计算，基准请求在两张页面上复用。 */
(function (window) {
  "use strict";

  const cache = new Map();
  const pendingRequests = new Map();
  const preferenceKey = "option-scope-chan-rs-benchmark";
  const benchmarkChoices = ["QQQ", "SPY"];

  function defaultBenchmark(symbol) {
    if (symbol === "QQQ") return "SPY";
    if (symbol === "SPY" || /^(\^NDX|\^IXIC|(?:MNQ|NQ)=F|TQQQ|SQQQ|QQQM)$/.test(symbol)) return "QQQ";
    return "SPY";
  }

  function readPreference() {
    try {
      const stored = window.localStorage.getItem(preferenceKey);
      return benchmarkChoices.includes(stored) ? stored : "auto";
    } catch (error) { return "auto"; }
  }

  function benchmarkRequest(enabled, symbol, period, basis, preference) {
    if (!enabled || !symbol) return "";
    const benchmark = benchmarkChoices.includes(preference) ? preference : defaultBenchmark(symbol);
    const timeframe = { daily: "1d", weekly: "1wk" }[period] || period;
    const priceBasis = timeframe === "1d" || timeframe === "1wk" ? basis : "raw";
    const self = benchmark === symbol ? "&self=true" : "";
    return `/api/chan-benchmark/${benchmark}?period=${timeframe}&basis=${priceBasis}${self}`;
  }

  function benchmarkConfig(path, history) {
    if (!path) return null;
    const url = new URL(path, "http://localhost");
    return {
      symbol: url.pathname.split("/").at(-1),
      self: url.searchParams.has("self"),
      history: history?.path === path ? history : null,
    };
  }

  function benchmarkMixin(request) {
    return {
      data() { return { rsPreference: readPreference(), rsHistory: null }; },
      computed: {
        rsBenchmark() { return benchmarkConfig(this.rsRequest, this.rsHistory); },
      },
      watch: {
        rsRequest: { immediate: true, handler: "refreshRsHistory" },
        rsPreference(value) {
          try { window.localStorage.setItem(preferenceKey, value); } catch (error) { /* 存储不可用时保留当前选择。 */ }
        },
      },
      beforeDestroy() {
        this.rsRequestGeneration += 1;
        window.clearTimeout(this.rsTimer);
      },
      methods: {
        async refreshRsHistory(path = this.rsRequest) {
          window.clearTimeout(this.rsTimer);
          const generation = (this.rsRequestGeneration || 0) + 1;
          this.rsRequestGeneration = generation;
          if (!path || path.endsWith("&self=true")) {
            this.rsHistory = null;
            return;
          }
          const existing = cache.get(path);
          this.rsHistory = existing?.data || null;
          let nextDelay = 30000;
          try {
            if (!window.document.hidden || !existing) {
              let data = existing?.data;
              if (!data || Date.now() - existing.at >= (data.refreshing ? 3000 : 15000)) {
                if (!pendingRequests.has(path)) {
                  const pending = Promise.resolve(request(path, { timeout: 10000, retry: false })).then((payload) => {
                    const data = Object.freeze({ ...payload, path });
                    cache.set(path, { data, at: Date.now() });
                    return data;
                  }).finally(() => pendingRequests.delete(path));
                  pendingRequests.set(path, pending);
                }
                data = await pendingRequests.get(path);
              }
              if (this.rsRequestGeneration !== generation || this.rsRequest !== path) return;
              this.rsHistory = data;
              if (data.refreshing) nextDelay = 3000;
              else if (!data.bars?.length) nextDelay = 15000;
            }
          } catch (error) {
            nextDelay = 15000;
          }
          if (this.rsRequestGeneration === generation && this.rsRequest === path) {
            this.rsTimer = window.setTimeout(() => this.refreshRsHistory(), nextDelay);
          }
        },
      },
    };
  }

  function calculate(candles, benchmark) {
    const reference = new Map((benchmark?.history?.bars || []).map((bar) => [bar.date, bar.close]));
    const live = benchmark?.history?.live_bar;
    let base = null, side = null;
    return candles.map((bar) => {
      const matchingLive = live?.is_forming === true && live.date === bar.date
        && bar.source_date && live.source_date === bar.source_date;
      const price = benchmark?.self ? bar.close
        : bar.is_forming ? (matchingLive ? live.close : null) : reference.get(bar.date);
      if (!Number.isFinite(price) || price <= 0 || !Number.isFinite(bar.close) || bar.close <= 0) return null;
      const ratio = bar.close / price;
      base ??= ratio;
      const value = ratio / base * 100;
      const state = Math.abs(value - 100) < 1e-8 ? "flat" : value > 100 ? "outperform" : "underperform";
      const changed = !bar.is_forming && state !== "flat" && side !== null && state !== side;
      if (!bar.is_forming && state !== "flat") side = state;
      return { value, state, changed, live: Boolean(bar.is_forming) };
    });
  }

  window.OptionScopeRelativeStrength = { benchmarkRequest, benchmarkMixin, calculate };
})(window);
