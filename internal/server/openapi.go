package server

import (
	"encoding/json"
	"html"
	"net/url"
	"strings"

	"github.com/gin-gonic/gin"
)

func (s *Service) openAPIDocument(c *gin.Context) {
	c.Header("Content-Type", "application/json; charset=utf-8")
	c.JSON(200, fastAPIOpenAPIDocument())
}

func (s *Service) swaggerDocs(c *gin.Context) {
	openAPIURL := docsOpenAPIURL(c)
	encodedURL, _ := json.Marshal(openAPIURL)
	c.Header("Content-Type", "text/html; charset=utf-8")
	c.String(200, `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Option Scope - Swagger UI</title>
<link rel="stylesheet" href="https://cdn.jsdelivr.net/npm/swagger-ui-dist@5/swagger-ui.css">
</head>
<body>
<div id="swagger-ui"></div>
<script src="https://cdn.jsdelivr.net/npm/swagger-ui-dist@5/swagger-ui-bundle.js"></script>
<script>
window.onload = function() {
  window.ui = SwaggerUIBundle({url: `+string(encodedURL)+`, dom_id: '#swagger-ui'});
};
</script>
</body>
</html>`)
}

func (s *Service) redocDocs(c *gin.Context) {
	openAPIURL := html.EscapeString(docsOpenAPIURL(c))
	c.Header("Content-Type", "text/html; charset=utf-8")
	c.String(200, `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Option Scope - ReDoc</title>
</head>
<body>
<redoc spec-url="`+openAPIURL+`"></redoc>
<script src="https://cdn.jsdelivr.net/npm/redoc@2/bundles/redoc.standalone.js"></script>
</body>
</html>`)
}

func docsOpenAPIURL(c *gin.Context) string {
	provided, _ := queryValue(c, "key")
	if provided == "" {
		provided = c.GetHeader("X-Access-Key")
	}
	if provided == "" {
		return "/openapi.json"
	}
	return "/openapi.json?key=" + url.QueryEscape(provided)
}

func fastAPIOpenAPIDocument() map[string]any {
	pathParam := func() map[string]any {
		return map[string]any{"name": "stock_symbol", "in": "path", "required": true, "schema": map[string]any{"type": "string", "title": "Stock Symbol"}}
	}
	stringQuery := func(name, title string, required bool, pattern string) map[string]any {
		schema := map[string]any{"type": "string", "title": title}
		if pattern != "" {
			schema["pattern"] = pattern
		}
		return map[string]any{"name": name, "in": "query", "required": required, "schema": schema}
	}
	optionalStringQuery := func(name, title, pattern string) map[string]any {
		return map[string]any{
			"name": name, "in": "query", "required": false,
			"schema": map[string]any{"anyOf": []any{map[string]any{"type": "string", "pattern": pattern}, map[string]any{"type": "null"}}, "title": title},
		}
	}
	boolQuery := func(name, title string, defaultValue bool) map[string]any {
		return map[string]any{"name": name, "in": "query", "required": false, "schema": map[string]any{"type": "boolean", "default": defaultValue, "title": title}}
	}
	intQuery := func(name, title string, defaultValue, minimum, maximum *int) map[string]any {
		schema := map[string]any{"type": "integer", "default": *defaultValue, "title": title}
		if minimum != nil {
			schema["minimum"] = *minimum
		}
		if maximum != nil {
			schema["maximum"] = *maximum
		}
		return map[string]any{"name": name, "in": "query", "required": false, "schema": schema}
	}
	optionalIntQuery := func(name, title string, minimum *int) map[string]any {
		integer := map[string]any{"type": "integer"}
		if minimum != nil {
			integer["minimum"] = *minimum
		}
		return map[string]any{
			"name": name, "in": "query", "required": false,
			"schema": map[string]any{"anyOf": []any{integer, map[string]any{"type": "null"}}, "title": title},
		}
	}
	optionalFloatQuery := map[string]any{
		"name": "spot", "in": "query", "required": false,
		"schema": map[string]any{"anyOf": []any{map[string]any{"type": "number", "exclusiveMinimum": 0}, map[string]any{"type": "null"}}, "title": "Spot"},
	}

	zero, one, four, eight, sixty, fortyFive, threeSixtyFive, oneHour := 0, 1, 4, 8, 60, 45, 365, 3600
	objectResponse := func(title string) map[string]any {
		return map[string]any{"type": "object", "additionalProperties": true, "title": title}
	}
	paths := map[string]any{
		"/api/events/{stock_symbol}":      openAPIGet("events_api_events__stock_symbol__get", "Events", "SSE update feed. Initial data stays on REST; frames only invalidate changed symbol snapshots.", []map[string]any{pathParam(), optionalStringQuery("expiration", "Expiration", `^\d{4}-\d{2}-\d{2}$`), optionalIntQuery("last_event_id", "Last Event Id", &zero)}, true),
		"/api/quote/{stock_symbol}":       openAPIGet("quote_api_quote__stock_symbol__get", "Quote", "", []map[string]any{pathParam(), boolQuery("refresh", "Refresh", false)}, true),
		"/api/expirations/{stock_symbol}": openAPIGet("expirations_api_expirations__stock_symbol__get", "Expirations", "", []map[string]any{pathParam(), boolQuery("refresh", "Refresh", false)}, true),
		"/api/chain/{stock_symbol}":       openAPIGet("chain_api_chain__stock_symbol__get", "Chain", "", []map[string]any{pathParam(), stringQuery("expiration", "Expiration", true, `^\d{4}-\d{2}-\d{2}$`)}, true),
		"/api/flow/{stock_symbol}":        openAPIGet("option_flow_api_flow__stock_symbol__get", "Option Flow", "比较同一期限最近两次快照，返回估算的 Call/Put 买卖流向。", []map[string]any{pathParam(), stringQuery("expiration", "Expiration", true, `^\d{4}-\d{2}-\d{2}$`)}, true),
		"/api/gamma/{stock_symbol}":       openAPIGet("gamma_profile_api_gamma__stock_symbol__get", "Gamma Profile", "返回近期期限的链，供 Zero Gamma/Gamma Flip 曲线使用。\n\n轮询传 status_only 时只回任务状态，避免每秒读取并序列化全部合约。\n页面展示传 include_rows=false：仍计算零 Gamma 和模型 IV，但不把合约行送到浏览器。", []map[string]any{pathParam(), boolQuery("refresh", "Refresh", false), intQuery("horizon_days", "Horizon Days", &fortyFive, &one, &threeSixtyFive), boolQuery("status_only", "Status Only", false), boolQuery("include_rows", "Include Rows", true)}, true),
		"/api/refresh/{stock_symbol}":     openAPIPost("refresh_api_refresh__stock_symbol__post", "Refresh", "刷新一次快照；调用方可指定缓存新鲜期，SSE 自动调度由订阅服务独立控制。", []map[string]any{pathParam(), optionalStringQuery("expiration", "Expiration", `^\d{4}-\d{2}-\d{2}$`), intQuery("max_age", "Max Age", &sixty, &zero, &oneHour)}, true),
		"/api/levels/{stock_symbol}":      openAPIGet("levels_api_levels__stock_symbol__get", "Levels", "压力位/支撑位：技术面与近 45 天多期限期权持仓综合。\n\n``raw=true`` 是轻量客户端计算模式。它只读取已经落库的日线和分析快照，\n不触发历史或 Beta 回源，也不执行 ``build_levels``；浏览器可以用这些\n原始数据计算趋势、极值和基础价位。原始数据不足时，前端显示期权分布回退，\n不在倒计时刷新链路里启动默认综合模式。\n\n`expiration` 可省略；省略时仅按已缓存的日线与分析快照计算，不筛选选中期限的期权链。\n`spot` 是前端传入的当前展示基准价；候选池优先使用快照中的前一交易日收盘价作为\n日内稳定锚点，避免实时价变化或实时价与盘后价切换时反复生成不同的价位簇。", []map[string]any{pathParam(), optionalStringQuery("expiration", "Expiration", `^\d{4}-\d{2}-\d{2}$`), optionalFloatQuery, boolQuery("raw", "Raw", false), intQuery("raw_expirations", "Raw Expirations", &four, &one, &eight)}, true),
		"/api/status/{stock_symbol}":      openAPIGet("status_api_status__stock_symbol__get", "Status", "", []map[string]any{pathParam()}, true),
		"/api/chan/{stock_symbol}":        openAPIGet("chan_api_chan__stock_symbol__get", "Chan Structure", "日线与周线缠论笔结构：严格旧笔、笔中枢、当前位置、力度衰减线索和条件式走势推演。只读取历史缓存，缺失或过期时后台更新；spot 可传页面现价。未收盘日线、未完成周线和形成中的笔不用于中枢确认。笔中枢不代表递归级别；第三类位置只标记候选。", []map[string]any{pathParam(), optionalFloatQuery}, true),
		"/api/cleanup":                    map[string]any{"post": openAPIOperation("cleanup_api_cleanup_post", "Cleanup", "", nil, false)},
	}
	responseTitles := map[string]string{
		"/api/chain/{stock_symbol}":       "Response Chain Api Chain  Stock Symbol  Get",
		"/api/cleanup":                    "Response Cleanup Api Cleanup Post",
		"/api/expirations/{stock_symbol}": "Response Expirations Api Expirations  Stock Symbol  Get",
		"/api/flow/{stock_symbol}":        "Response Option Flow Api Flow  Stock Symbol  Get",
		"/api/gamma/{stock_symbol}":       "Response Gamma Profile Api Gamma  Stock Symbol  Get",
		"/api/levels/{stock_symbol}":      "Response Levels Api Levels  Stock Symbol  Get",
		"/api/quote/{stock_symbol}":       "Response Quote Api Quote  Stock Symbol  Get",
		"/api/refresh/{stock_symbol}":     "Response Refresh Api Refresh  Stock Symbol  Post",
		"/api/status/{stock_symbol}":      "Response Status Api Status  Stock Symbol  Get",
		"/api/chan/{stock_symbol}":        "Response Chan Api Chan  Stock Symbol  Get",
	}
	for path, operations := range paths {
		for _, operation := range operations.(map[string]any) {
			op := operation.(map[string]any)
			responses := op["responses"].(map[string]any)
			content := responses["200"].(map[string]any)["content"].(map[string]any)
			mediaType := content["application/json"].(map[string]any)
			if title, ok := responseTitles[path]; ok {
				mediaType["schema"] = objectResponse(title)
			}
		}
	}
	return map[string]any{
		"openapi": "3.1.0",
		"info":    map[string]any{"title": "Option Scope", "version": "0.1.0"},
		"paths":   paths,
		"components": map[string]any{"schemas": map[string]any{
			"HTTPValidationError": map[string]any{"properties": map[string]any{"detail": map[string]any{"items": map[string]any{"$ref": "#/components/schemas/ValidationError"}, "type": "array", "title": "Detail"}}, "type": "object", "title": "HTTPValidationError"},
			"ValidationError": map[string]any{"properties": map[string]any{
				"loc": map[string]any{"items": map[string]any{"anyOf": []any{map[string]any{"type": "string"}, map[string]any{"type": "integer"}}}, "type": "array", "title": "Location"},
				"msg": map[string]any{"type": "string", "title": "Message"}, "type": map[string]any{"type": "string", "title": "Error Type"},
				"ctx": map[string]any{"type": "object", "title": "Context"}, "input": map[string]any{"title": "Input"},
			}, "type": "object", "required": []string{"loc", "msg", "type"}, "title": "ValidationError"},
		}},
	}
}

func openAPIGet(operationID, summary, description string, parameters []map[string]any, hasValidation bool) map[string]any {
	return map[string]any{"get": openAPIOperation(operationID, summary, description, parameters, hasValidation)}
}

func openAPIPost(operationID, summary, description string, parameters []map[string]any, hasValidation bool) map[string]any {
	return map[string]any{"post": openAPIOperation(operationID, summary, description, parameters, hasValidation)}
}

func openAPIOperation(operationID, summary, description string, parameters []map[string]any, hasValidation bool) map[string]any {
	responses := map[string]any{
		"200": map[string]any{"description": "Successful Response", "content": map[string]any{"application/json": map[string]any{"schema": map[string]any{}}}},
	}
	if hasValidation {
		responses["422"] = map[string]any{"description": "Validation Error", "content": map[string]any{"application/json": map[string]any{"schema": map[string]any{"$ref": "#/components/schemas/HTTPValidationError"}}}}
	}
	operation := map[string]any{"operationId": operationID, "summary": summary, "responses": responses}
	if description != "" {
		operation["description"] = strings.TrimSpace(description)
	}
	if len(parameters) > 0 {
		operation["parameters"] = parameters
	}
	return operation
}
