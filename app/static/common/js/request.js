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
        timeout: config.timeout,
      }).then(function (body, textStatus, xhr) {
        rememberServerClock(xhr);
        return body;
      }, function (xhr) {
        rememberServerClock(xhr);
        var body = xhr && xhr.responseJSON ? xhr.responseJSON : {};
        var message = body.detail || (xhr && xhr.status ? "请求失败 (" + xhr.status + ")" : "网络请求失败");
        if (xhr && xhr.status === 403 && access.onDenied) access.onDenied();
        return $.Deferred().reject(new Error(message)).promise();
      });
    },
  };
}(window, window.jQuery));
