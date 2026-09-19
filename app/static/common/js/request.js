/* jQuery 请求适配层：统一处理访问密钥、JSON 响应和错误，兼容不支持 fetch 的浏览器。 */
(function (window, $) {
  "use strict";

  window.OptionScopeRequest = {
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
        headers: access.accessKey ? { "X-Access-Key": access.accessKey } : {},
        data: config.data,
        timeout: config.timeout,
      }).then(function (body) {
        return body;
      }, function (xhr) {
        var body = xhr && xhr.responseJSON ? xhr.responseJSON : {};
        var message = body.detail || (xhr && xhr.status ? "请求失败 (" + xhr.status + ")" : "网络请求失败");
        if (xhr && xhr.status === 403 && access.onDenied) access.onDenied();
        return $.Deferred().reject(new Error(message)).promise();
      });
    },
  };
}(window, window.jQuery));
