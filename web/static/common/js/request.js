/* jQuery 请求适配层：统一处理访问密钥、JSON 响应和错误，兼容不支持 fetch 的浏览器。 */
(function (window, $) {
  "use strict";

  // 服务端时钟减去本机时钟。快照年龄用这个差值，避免本机时钟偏快时倒计时提前打到刷新接口。
  var serverClockOffsetMs = 0;
  // 同一 URL 的 GET 同时在飞时共享一个请求。首屏、SSE 通知和重试定时器
  // 经常在同一瞬间读取同一份快照；没有这层合并会把相同 JSON 下载多次。
  var inFlightGets = Object.create(null);
  // 轻量只读接口的 1 秒复用：行情事件、到期日发现和状态检查会在同一次
  // 首屏里连续触发同一 URL。并发合并管不到“上一个刚返回”的连续调用。
  var hotGetCache = Object.create(null);
  var HOT_GET_CACHE_MS = 1000;
  var HOT_GET_CACHE_LIMIT = 64;
  function hotGetCacheable(path) {
    if (/^\/api\/(quote|expirations|status|flow)\//.test(path)) return !/[?&]refresh=true/.test(path);
    // Gamma 状态轮询可能被多个首屏回调同时触发；只缓存只读状态查询，
    // 启动/刷新任务的 refresh=true 请求必须透传。
    if (/^\/api\/gamma\//.test(path)) return /[?&]status_only=true/.test(path) && !/[?&]refresh=true/.test(path);
    return false;
  }
  function rememberHotGet(key, value) {
    hotGetCache[key] = { at: Date.now(), value: value };
    var keys = Object.keys(hotGetCache);
    if (keys.length > HOT_GET_CACHE_LIMIT) delete hotGetCache[keys[0]];
  }
  function forgetHotGet(prefix) {
    for (var key in hotGetCache) {
      if (key.indexOf(prefix) === 0) delete hotGetCache[key];
    }
  }

  function noteServerNow(serverNowMs) {
    if (!isFinite(serverNowMs)) return;
    serverClockOffsetMs = serverNowMs - Date.now();
  }

  function rememberServerClock(xhr) {
    if (!xhr || !xhr.getResponseHeader) return;
    var raw = xhr.getResponseHeader("Date");
    if (!raw) return;
    var serverNow = Date.parse(raw);
    if (isNaN(serverNow)) return;
    noteServerNow(serverNow);
  }

  function transientRequestFailure(xhr, textStatus) {
    var status = Number(xhr && xhr.status) || 0;
    return textStatus === "timeout" || status === 0 || status === 408 || status === 425 || status === 429 || (status >= 500 && status < 600);
  }

  function retryAfterMilliseconds(xhr) {
    var retryAfter = xhr && xhr.getResponseHeader && xhr.getResponseHeader("Retry-After");
    var seconds = Number(retryAfter);
    var retryAfterMs = isFinite(seconds) && seconds > 0 ? seconds * 1000 : 0;
    if (!retryAfterMs && retryAfter) {
      // HTTP-date form is common on 429 responses. Use the response Date as
      // the reference when available so a skewed client clock does not retry
      // earlier than the server requested.
      var retryAt = Date.parse(retryAfter);
      var serverDate = Date.parse(xhr.getResponseHeader("Date") || "");
      var reference = isFinite(serverDate) ? serverDate : Date.now();
      if (isFinite(retryAt) && retryAt > reference) retryAfterMs = retryAt - reference;
    }
    return Math.min(300000, retryAfterMs);
  }

  function retryDelay(xhr, attempt) {
    var retryAfterMs = retryAfterMilliseconds(xhr);
    // Honor Retry-After up to five minutes. Without it, wait longer for 429s
    // to avoid adding pressure to a rate-limited service.
    var status = Number(xhr && xhr.status) || 0;
    var fallbackDelay = status === 429
      ? Math.min(20000, 5000 * Math.pow(2, attempt))
      : Math.min(8000, 1500 * Math.pow(2, attempt));
    var delay = retryAfterMs > 0
      ? Math.min(300000, retryAfterMs)
      : fallbackDelay;
    // 并发失败的浏览器错开重试，避免同一秒再次集中打到已过载的源站。
    return delay + Math.floor(Math.random() * Math.min(500, delay / 4));
  }

  window.OptionScopeRequest = {
    serverNow: function () { return Date.now() + serverClockOffsetMs; },
    noteServerNow: noteServerNow,
    request: function (path, options, context) {
      var config = options || {};
      var access = context || {};
      var url = access.withAccessKey(path, access.accessKey || "");

      // 所有页面请求都必须有上限。服务器上游或 SQLite 锁等待时，jQuery 默认会
      // 无限等待，页面的 refreshInFlight/loading 就永远不释放，倒计时会一直显示
      // “正在刷新”。刷新接口允许服务端完成一次较慢的上游回源，其余读接口应更快
      // 失败并回退到本地快照；调用方仍可用 options.timeout 覆盖单个请求。
      var defaultTimeout = /^\/api\/refresh\//.test(path) ? 45000 : 20000;
      var timeout = Number(config.timeout);
      if (!isFinite(timeout) || timeout <= 0) timeout = defaultTimeout;

      if (access.required && !access.accessKey) {
        if (access.onDenied) access.onDenied();
        return $.Deferred().reject({ status: 403, responseJSON: { detail: "403 Forbidden" } }).promise();
      }

      var method = String(config.method || config.type || "GET").toUpperCase();
      // POST 会改变服务端状态，不能合并。带 data 的 GET 也跳过，避免把不同
      // 查询体误当成同一请求。
      var dedupeKey = method === "GET" && !config.data
        ? method + " " + url + "|" + (access.accessKey || "")
        : "";
      if (dedupeKey && inFlightGets[dedupeKey]) return inFlightGets[dedupeKey];
      var cacheable = method === "GET" && hotGetCacheable(url);
      if (cacheable) {
        var cached = hotGetCache[dedupeKey];
        if (cached && Date.now() - cached.at < HOT_GET_CACHE_MS) {
          return $.Deferred().resolve(cached.value).promise();
        }
      }
      // GET 请求是只读的，可以安全地恢复 Cloudflare 520/521、上游 5xx、429
      // 及短暂网络断开。最多额外请求两次，避免一个页面在故障期间放大流量。
      var maxRetries = method === "GET" && config.retry !== false ? 2 : 0;

      function attemptRequest(attempt) {
        return $.ajax({
          url: url,
          type: method,
          dataType: "json",
          cache: false,
          headers: access.accessKey ? { "X-Access-Key": access.accessKey } : {},
          data: config.data,
          timeout: timeout,
        }).then(function (body, textStatus, xhr) {
          rememberServerClock(xhr);
          if (cacheable) rememberHotGet(dedupeKey, body);
          // 主动回源返回的新行情/期限必须立即盖掉短 TTL 缓存，否则后续读取
          // 会在 1 秒内又拿到刷新前的旧响应。
          if (method === "GET" && /^\/api\/quote\//.test(path) && /[?&]refresh=true/.test(path)) {
            forgetHotGet("GET " + path.split("?")[0]);
          } else if (method === "POST" && /^\/api\/refresh\//.test(path)) {
            var refreshed = path.split("?")[0].split("/")[3];
            if (refreshed) {
              forgetHotGet("GET /api/quote/" + refreshed);
              forgetHotGet("GET /api/expirations/" + refreshed);
            }
          }
          return body;
        }, function (xhr, textStatus) {
          rememberServerClock(xhr);
          if (attempt < maxRetries && transientRequestFailure(xhr, textStatus)) {
            var deferred = $.Deferred();
            setTimeout(function () {
              attemptRequest(attempt + 1).then(deferred.resolve, deferred.reject);
            }, retryDelay(xhr, attempt));
            return deferred.promise();
          }
          var body = xhr && xhr.responseJSON ? xhr.responseJSON : {};
          var timedOut = textStatus === "timeout" || (xhr && xhr.statusText === "timeout");
          var message = body.detail || (timedOut ? "请求超时，请稍后重试" : (xhr && xhr.status ? "请求失败 (" + xhr.status + ")" : "网络请求失败"));
          if (xhr && xhr.status === 403 && access.onDenied) access.onDenied();
          var error = new Error(message);
          error.code = timedOut ? "timeout" : "http_error";
          error.timeout = timedOut;
          error.status = xhr && xhr.status;
          error.retryAfterMs = retryAfterMilliseconds(xhr);
          return $.Deferred().reject(error).promise();
        });
      }

      var pending = attemptRequest(0);
      if (dedupeKey) {
        inFlightGets[dedupeKey] = pending;
        var release = function () {
          if (inFlightGets[dedupeKey] === pending) delete inFlightGets[dedupeKey];
        };
        pending.then(release, release);
      }
      return pending;
    },
  };
}(window, window.jQuery));
