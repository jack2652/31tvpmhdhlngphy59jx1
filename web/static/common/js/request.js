/* jQuery 请求适配层：统一处理访问密钥、JSON 响应和错误，兼容不支持 fetch 的浏览器。 */
(function (window, $) {
  "use strict";

  // 服务端时钟减去本机时钟。快照年龄用这个差值，避免本机时钟偏快时倒计时提前打到刷新接口。
  var serverClockOffsetMs = 0;

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

      return $.ajax({
        url: url,
        type: config.method || config.type || "GET",
        dataType: "json",
        cache: false,
        headers: access.accessKey ? { "X-Access-Key": access.accessKey } : {},
        data: config.data,
        timeout: timeout,
      }).then(function (body, textStatus, xhr) {
        rememberServerClock(xhr);
        return body;
      }, function (xhr, textStatus) {
        rememberServerClock(xhr);
        var body = xhr && xhr.responseJSON ? xhr.responseJSON : {};
        var timedOut = textStatus === "timeout" || (xhr && xhr.statusText === "timeout");
        var message = body.detail || (timedOut ? "请求超时，请稍后重试" : (xhr && xhr.status ? "请求失败 (" + xhr.status + ")" : "网络请求失败"));
        if (xhr && xhr.status === 403 && access.onDenied) access.onDenied();
        var error = new Error(message);
        // 让上层在请求超时后跳过代价较高的“重新获取到期日”补救请求，
        // 先释放刷新状态，等待下一轮倒计时或用户手动重试。
        error.code = timedOut ? "timeout" : "http_error";
        error.timeout = timedOut;
        error.status = xhr && xhr.status;
        return $.Deferred().reject(error).promise();
      });
    },
  };
}(window, window.jQuery));
