package server

import (
	"reflect"
	"testing"
)

func TestFastAPIOpenAPIBusinessRouteContracts(t *testing.T) {
	datePattern := `^\d{4}-\d{2}-\d{2}$`
	pathSymbol := map[string]any{
		"name": "stock_symbol", "in": "path", "required": true,
		"schema": map[string]any{"type": "string", "title": "Stock Symbol"},
	}
	boolSchema := func(title string, defaultValue bool) map[string]any {
		return map[string]any{"type": "boolean", "default": defaultValue, "title": title}
	}
	intSchema := func(title string, defaultValue, minimum, maximum int) map[string]any {
		return map[string]any{"type": "integer", "default": defaultValue, "minimum": minimum, "maximum": maximum, "title": title}
	}
	optionalDateSchema := func(title string) map[string]any {
		return map[string]any{"anyOf": []any{
			map[string]any{"type": "string", "pattern": datePattern},
			map[string]any{"type": "null"},
		}, "title": title}
	}
	requiredDateSchema := func(title string) map[string]any {
		return map[string]any{"type": "string", "pattern": datePattern, "title": title}
	}
	query := func(name string, required bool, schema map[string]any) map[string]any {
		return map[string]any{"name": name, "in": "query", "required": required, "schema": schema}
	}
	optionalIntSchema := map[string]any{"anyOf": []any{
		map[string]any{"type": "integer", "minimum": 0},
		map[string]any{"type": "null"},
	}, "title": "Last Event Id"}
	spotSchema := map[string]any{"anyOf": []any{
		map[string]any{"type": "number", "exclusiveMinimum": 0},
		map[string]any{"type": "null"},
	}, "title": "Spot"}

	levelsDescription := "压力位/支撑位：技术面与近 45 天多期限期权持仓综合。\n\n" +
		"``raw=true`` 是轻量客户端计算模式。它只读取已经落库的日线和分析快照，\n" +
		"不触发历史、Beta、财报回源，也不执行 ``build_levels``；浏览器可以用这些\n" +
		"原始数据计算趋势、极值和基础价位。原始数据不足时，前端显示期权分布回退，\n" +
		"不在倒计时刷新链路里启动默认综合模式。\n\n" +
		"`expiration` 可省略；省略时仅按已缓存的日线与分析快照计算，不筛选选中期限的期权链。\n" +
		"`spot` 是前端传入的当前展示基准价；候选池优先使用快照中的前一交易日收盘价作为\n" +
		"日内稳定锚点，避免实时价变化或实时价与盘后价切换时反复生成不同的价位簇。"
	gammaDescription := "返回近期期限的链，供 Zero Gamma/Gamma Flip 曲线使用。\n\n" +
		"轮询传 status_only 时只回任务状态，避免每秒读取并序列化全部合约。\n" +
		"页面展示传 include_rows=false：仍计算零 Gamma 和模型 IV，但不把合约行送到浏览器。"

	type routeContract struct {
		path, method, operationID, summary, description, responseTitle string
		parameters                                                     []map[string]any
		validation                                                     bool
	}
	contracts := []routeContract{
		{
			path: "/api/events/{stock_symbol}", method: "get", operationID: "events_api_events__stock_symbol__get",
			summary: "Events", description: "SSE update feed. Initial data stays on REST; frames only invalidate changed symbol snapshots.",
			parameters: []map[string]any{
				pathSymbol,
				query("expiration", false, optionalDateSchema("Expiration")),
				query("last_event_id", false, optionalIntSchema),
			}, validation: true,
		},
		{
			path: "/api/quote/{stock_symbol}", method: "get", operationID: "quote_api_quote__stock_symbol__get", summary: "Quote",
			responseTitle: "Response Quote Api Quote  Stock Symbol  Get",
			parameters:    []map[string]any{pathSymbol, query("refresh", false, boolSchema("Refresh", false))}, validation: true,
		},
		{
			path: "/api/expirations/{stock_symbol}", method: "get", operationID: "expirations_api_expirations__stock_symbol__get", summary: "Expirations",
			responseTitle: "Response Expirations Api Expirations  Stock Symbol  Get",
			parameters:    []map[string]any{pathSymbol, query("refresh", false, boolSchema("Refresh", false))}, validation: true,
		},
		{
			path: "/api/chain/{stock_symbol}", method: "get", operationID: "chain_api_chain__stock_symbol__get", summary: "Chain",
			responseTitle: "Response Chain Api Chain  Stock Symbol  Get",
			parameters:    []map[string]any{pathSymbol, query("expiration", true, requiredDateSchema("Expiration"))}, validation: true,
		},
		{
			path: "/api/flow/{stock_symbol}", method: "get", operationID: "option_flow_api_flow__stock_symbol__get", summary: "Option Flow",
			description:   "比较同一期限最近两次快照，返回估算的 Call/Put 买卖流向。",
			responseTitle: "Response Option Flow Api Flow  Stock Symbol  Get",
			parameters:    []map[string]any{pathSymbol, query("expiration", true, requiredDateSchema("Expiration"))}, validation: true,
		},
		{
			path: "/api/gamma/{stock_symbol}", method: "get", operationID: "gamma_profile_api_gamma__stock_symbol__get", summary: "Gamma Profile",
			description: gammaDescription, responseTitle: "Response Gamma Profile Api Gamma  Stock Symbol  Get",
			parameters: []map[string]any{
				pathSymbol,
				query("refresh", false, boolSchema("Refresh", false)),
				query("horizon_days", false, intSchema("Horizon Days", 45, 1, 365)),
				query("status_only", false, boolSchema("Status Only", false)),
				query("include_rows", false, boolSchema("Include Rows", true)),
			}, validation: true,
		},
		{
			path: "/api/refresh/{stock_symbol}", method: "post", operationID: "refresh_api_refresh__stock_symbol__post", summary: "Refresh",
			description:   "刷新一次快照；调用方可指定缓存新鲜期，SSE 自动调度由订阅服务独立控制。",
			responseTitle: "Response Refresh Api Refresh  Stock Symbol  Post",
			parameters: []map[string]any{
				pathSymbol,
				query("expiration", false, optionalDateSchema("Expiration")),
				query("max_age", false, intSchema("Max Age", 60, 0, 3600)),
			}, validation: true,
		},
		{
			path: "/api/levels/{stock_symbol}", method: "get", operationID: "levels_api_levels__stock_symbol__get", summary: "Levels",
			description: levelsDescription, responseTitle: "Response Levels Api Levels  Stock Symbol  Get",
			parameters: []map[string]any{
				pathSymbol,
				query("expiration", false, optionalDateSchema("Expiration")),
				query("spot", false, spotSchema),
				query("raw", false, boolSchema("Raw", false)),
				query("raw_expirations", false, intSchema("Raw Expirations", 4, 1, 8)),
			}, validation: true,
		},
		{
			path: "/api/status/{stock_symbol}", method: "get", operationID: "status_api_status__stock_symbol__get", summary: "Status",
			responseTitle: "Response Status Api Status  Stock Symbol  Get", parameters: []map[string]any{pathSymbol}, validation: true,
		},
		{
			path: "/api/cleanup", method: "post", operationID: "cleanup_api_cleanup_post", summary: "Cleanup",
			responseTitle: "Response Cleanup Api Cleanup Post",
		},
		{
			path: "/api/chan/{stock_symbol}", method: "get", operationID: "chan_api_chan__stock_symbol__get", summary: "Chan Structure",
			description:   "日线与周线缠论笔结构：严格旧笔、笔中枢、当前位置、力度衰减线索和条件式走势推演。只读取历史缓存，缺失或过期时后台更新；spot 可传页面现价。未收盘日线、未完成周线和形成中的笔不用于中枢确认。笔中枢不代表递归级别；第三类位置只标记候选。",
			responseTitle: "Response Chan Api Chan  Stock Symbol  Get",
			parameters:    []map[string]any{pathSymbol, query("spot", false, spotSchema)}, validation: true,
		},
	}

	document := fastAPIOpenAPIDocument()
	paths := document["paths"].(map[string]any)
	if len(paths) != len(contracts) {
		t.Fatalf("business path count = %d, want %d", len(paths), len(contracts))
	}
	for _, contract := range contracts {
		t.Run(contract.path, func(t *testing.T) {
			pathItem, ok := paths[contract.path].(map[string]any)
			if !ok {
				t.Fatalf("path %q is missing", contract.path)
			}
			if len(pathItem) != 1 {
				t.Fatalf("methods for %q = %v, want only %s", contract.path, openAPIContractMapKeys(pathItem), contract.method)
			}
			operation, ok := pathItem[contract.method].(map[string]any)
			if !ok {
				t.Fatalf("method %q is missing for %q", contract.method, contract.path)
			}
			for field, want := range map[string]any{
				"operationId": contract.operationID,
				"summary":     contract.summary,
			} {
				if got := operation[field]; got != want {
					t.Errorf("%s = %#v, want %#v", field, got, want)
				}
			}
			if contract.description == "" {
				if _, exists := operation["description"]; exists {
					t.Errorf("unexpected description: %#v", operation["description"])
				}
			} else if got := operation["description"]; got != contract.description {
				t.Errorf("description = %#v, want %#v", got, contract.description)
			}

			gotParameters, _ := operation["parameters"].([]map[string]any)
			if !reflect.DeepEqual(gotParameters, contract.parameters) {
				t.Errorf("parameters = %#v, want %#v", gotParameters, contract.parameters)
			}
			responses := operation["responses"].(map[string]any)
			wantResponseCodes := 1
			if contract.validation {
				wantResponseCodes++
			}
			if len(responses) != wantResponseCodes {
				t.Errorf("response codes = %v, want %d response(s)", openAPIContractMapKeys(responses), wantResponseCodes)
			}
			response := responses["200"].(map[string]any)
			if response["description"] != "Successful Response" {
				t.Errorf("200 description = %#v", response["description"])
			}
			content := response["content"].(map[string]any)
			if len(content) != 1 {
				t.Errorf("200 content types = %v, want only application/json", openAPIContractMapKeys(content))
			}
			mediaType := content["application/json"].(map[string]any)
			wantSchema := map[string]any{}
			if contract.responseTitle != "" {
				wantSchema = map[string]any{"type": "object", "additionalProperties": true, "title": contract.responseTitle}
			}
			if got := mediaType["schema"]; !reflect.DeepEqual(got, wantSchema) {
				t.Errorf("200 schema = %#v, want %#v", got, wantSchema)
			}
			if contract.validation {
				validation := responses["422"].(map[string]any)
				if validation["description"] != "Validation Error" {
					t.Errorf("422 description = %#v", validation["description"])
				}
				validationSchema := validation["content"].(map[string]any)["application/json"].(map[string]any)["schema"]
				wantValidation := map[string]any{"$ref": "#/components/schemas/HTTPValidationError"}
				if !reflect.DeepEqual(validationSchema, wantValidation) {
					t.Errorf("422 schema = %#v, want %#v", validationSchema, wantValidation)
				}
			} else if _, exists := responses["422"]; exists {
				t.Error("unexpected 422 response")
			}
		})
	}

	schemas := document["components"].(map[string]any)["schemas"].(map[string]any)
	wantValidationError := map[string]any{
		"properties": map[string]any{
			"ctx":   map[string]any{"type": "object", "title": "Context"},
			"input": map[string]any{"title": "Input"},
			"loc": map[string]any{"items": map[string]any{"anyOf": []any{
				map[string]any{"type": "string"}, map[string]any{"type": "integer"},
			}}, "type": "array", "title": "Location"},
			"msg":  map[string]any{"type": "string", "title": "Message"},
			"type": map[string]any{"type": "string", "title": "Error Type"},
		},
		"type": "object", "required": []string{"loc", "msg", "type"}, "title": "ValidationError",
	}
	if !reflect.DeepEqual(schemas["ValidationError"], wantValidationError) {
		t.Errorf("ValidationError schema = %#v, want %#v", schemas["ValidationError"], wantValidationError)
	}
}

func openAPIContractMapKeys(values map[string]any) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	return keys
}
