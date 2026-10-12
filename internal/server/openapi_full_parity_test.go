package server

import (
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"testing"
)

// Generated with app.main.app.openapi() using FastAPI 0.142.2. DATABASE_PATH
// pointed at a temporary file so importing the app could not touch production data.
// JSON is decoded before comparison so map order and Go's numeric types do not matter.
const fastAPIOpenAPIFrozenSchema = `
{
  "openapi": "3.1.0",
  "info": {"title": "Option Scope", "version": "0.1.0"},
  "paths": {
    "/api/events/{stock_symbol}": {
      "get": {
        "operationId": "events_api_events__stock_symbol__get",
        "summary": "Events",
        "description": "SSE update feed. Initial data stays on REST; frames only invalidate changed symbol snapshots.",
        "parameters": [
          {"name": "stock_symbol", "in": "path", "required": true, "schema": {"type": "string", "title": "Stock Symbol"}},
          {"name": "expiration", "in": "query", "required": false, "schema": {"anyOf": [{"type": "string", "pattern": "^\\d{4}-\\d{2}-\\d{2}$"}, {"type": "null"}], "title": "Expiration"}},
          {"name": "last_event_id", "in": "query", "required": false, "schema": {"anyOf": [{"type": "integer", "minimum": 0}, {"type": "null"}], "title": "Last Event Id"}}
        ],
        "responses": {
          "200": {"description": "Successful Response", "content": {"application/json": {"schema": {}}}},
          "422": {"description": "Validation Error", "content": {"application/json": {"schema": {"$ref": "#/components/schemas/HTTPValidationError"}}}}
        }
      }
    },
    "/api/quote/{stock_symbol}": {
      "get": {
        "operationId": "quote_api_quote__stock_symbol__get", "summary": "Quote",
        "parameters": [
          {"name": "stock_symbol", "in": "path", "required": true, "schema": {"type": "string", "title": "Stock Symbol"}},
          {"name": "refresh", "in": "query", "required": false, "schema": {"type": "boolean", "default": false, "title": "Refresh"}}
        ],
        "responses": {
          "200": {"description": "Successful Response", "content": {"application/json": {"schema": {"type": "object", "additionalProperties": true, "title": "Response Quote Api Quote  Stock Symbol  Get"}}}},
          "422": {"description": "Validation Error", "content": {"application/json": {"schema": {"$ref": "#/components/schemas/HTTPValidationError"}}}}
        }
      }
    },
    "/api/expirations/{stock_symbol}": {
      "get": {
        "operationId": "expirations_api_expirations__stock_symbol__get", "summary": "Expirations",
        "parameters": [
          {"name": "stock_symbol", "in": "path", "required": true, "schema": {"type": "string", "title": "Stock Symbol"}},
          {"name": "refresh", "in": "query", "required": false, "schema": {"type": "boolean", "default": false, "title": "Refresh"}}
        ],
        "responses": {
          "200": {"description": "Successful Response", "content": {"application/json": {"schema": {"type": "object", "additionalProperties": true, "title": "Response Expirations Api Expirations  Stock Symbol  Get"}}}},
          "422": {"description": "Validation Error", "content": {"application/json": {"schema": {"$ref": "#/components/schemas/HTTPValidationError"}}}}
        }
      }
    },
    "/api/chain/{stock_symbol}": {
      "get": {
        "operationId": "chain_api_chain__stock_symbol__get", "summary": "Chain",
        "parameters": [
          {"name": "stock_symbol", "in": "path", "required": true, "schema": {"type": "string", "title": "Stock Symbol"}},
          {"name": "expiration", "in": "query", "required": true, "schema": {"type": "string", "pattern": "^\\d{4}-\\d{2}-\\d{2}$", "title": "Expiration"}}
        ],
        "responses": {
          "200": {"description": "Successful Response", "content": {"application/json": {"schema": {"type": "object", "additionalProperties": true, "title": "Response Chain Api Chain  Stock Symbol  Get"}}}},
          "422": {"description": "Validation Error", "content": {"application/json": {"schema": {"$ref": "#/components/schemas/HTTPValidationError"}}}}
        }
      }
    },
    "/api/flow/{stock_symbol}": {
      "get": {
        "operationId": "option_flow_api_flow__stock_symbol__get", "summary": "Option Flow",
        "description": "比较同一期限最近两次快照，返回估算的 Call/Put 买卖流向。",
        "parameters": [
          {"name": "stock_symbol", "in": "path", "required": true, "schema": {"type": "string", "title": "Stock Symbol"}},
          {"name": "expiration", "in": "query", "required": true, "schema": {"type": "string", "pattern": "^\\d{4}-\\d{2}-\\d{2}$", "title": "Expiration"}}
        ],
        "responses": {
          "200": {"description": "Successful Response", "content": {"application/json": {"schema": {"type": "object", "additionalProperties": true, "title": "Response Option Flow Api Flow  Stock Symbol  Get"}}}},
          "422": {"description": "Validation Error", "content": {"application/json": {"schema": {"$ref": "#/components/schemas/HTTPValidationError"}}}}
        }
      }
    },
    "/api/gamma/{stock_symbol}": {
      "get": {
        "operationId": "gamma_profile_api_gamma__stock_symbol__get", "summary": "Gamma Profile",
        "description": "返回近期期限的链，供 Zero Gamma/Gamma Flip 曲线使用。\n\n轮询传 status_only 时只回任务状态，避免每秒读取并序列化全部合约。\n页面展示传 include_rows=false：仍计算零 Gamma 和模型 IV，但不把合约行送到浏览器。",
        "parameters": [
          {"name": "stock_symbol", "in": "path", "required": true, "schema": {"type": "string", "title": "Stock Symbol"}},
          {"name": "refresh", "in": "query", "required": false, "schema": {"type": "boolean", "default": false, "title": "Refresh"}},
          {"name": "horizon_days", "in": "query", "required": false, "schema": {"type": "integer", "default": 45, "minimum": 1, "maximum": 365, "title": "Horizon Days"}},
          {"name": "status_only", "in": "query", "required": false, "schema": {"type": "boolean", "default": false, "title": "Status Only"}},
          {"name": "include_rows", "in": "query", "required": false, "schema": {"type": "boolean", "default": true, "title": "Include Rows"}}
        ],
        "responses": {
          "200": {"description": "Successful Response", "content": {"application/json": {"schema": {"type": "object", "additionalProperties": true, "title": "Response Gamma Profile Api Gamma  Stock Symbol  Get"}}}},
          "422": {"description": "Validation Error", "content": {"application/json": {"schema": {"$ref": "#/components/schemas/HTTPValidationError"}}}}
        }
      }
    },
    "/api/refresh/{stock_symbol}": {
      "post": {
        "operationId": "refresh_api_refresh__stock_symbol__post", "summary": "Refresh",
        "description": "刷新一次快照；调用方可指定缓存新鲜期，SSE 自动调度由订阅服务独立控制。",
        "parameters": [
          {"name": "stock_symbol", "in": "path", "required": true, "schema": {"type": "string", "title": "Stock Symbol"}},
          {"name": "expiration", "in": "query", "required": false, "schema": {"anyOf": [{"type": "string", "pattern": "^\\d{4}-\\d{2}-\\d{2}$"}, {"type": "null"}], "title": "Expiration"}},
          {"name": "max_age", "in": "query", "required": false, "schema": {"type": "integer", "default": 60, "minimum": 0, "maximum": 3600, "title": "Max Age"}}
        ],
        "responses": {
          "200": {"description": "Successful Response", "content": {"application/json": {"schema": {"type": "object", "additionalProperties": true, "title": "Response Refresh Api Refresh  Stock Symbol  Post"}}}},
          "422": {"description": "Validation Error", "content": {"application/json": {"schema": {"$ref": "#/components/schemas/HTTPValidationError"}}}}
        }
      }
    },
    "/api/levels/{stock_symbol}": {
      "get": {
        "operationId": "levels_api_levels__stock_symbol__get", "summary": "Levels",
        "description": "压力位/支撑位：技术面与近 45 天多期限期权持仓综合。\n\n\u0060\u0060raw=true\u0060\u0060 是轻量客户端计算模式。它只读取已经落库的日线和分析快照，\n不触发历史、Beta、财报回源，也不执行 \u0060\u0060build_levels\u0060\u0060；浏览器可以用这些\n原始数据计算趋势、极值和基础价位。原始数据不足时，前端显示期权分布回退，\n不在倒计时刷新链路里启动默认综合模式。\n\n\u0060spot\u0060 是前端传入的当前展示基准价；候选池优先使用快照中的前一交易日收盘价作为\n日内稳定锚点，避免实时价变化或实时价与盘后价切换时反复生成不同的价位簇。",
        "parameters": [
          {"name": "stock_symbol", "in": "path", "required": true, "schema": {"type": "string", "title": "Stock Symbol"}},
          {"name": "expiration", "in": "query", "required": true, "schema": {"type": "string", "pattern": "^\\d{4}-\\d{2}-\\d{2}$", "title": "Expiration"}},
          {"name": "spot", "in": "query", "required": false, "schema": {"anyOf": [{"type": "number", "exclusiveMinimum": 0}, {"type": "null"}], "title": "Spot"}},
          {"name": "raw", "in": "query", "required": false, "schema": {"type": "boolean", "default": false, "title": "Raw"}},
          {"name": "raw_expirations", "in": "query", "required": false, "schema": {"type": "integer", "default": 4, "minimum": 1, "maximum": 8, "title": "Raw Expirations"}}
        ],
        "responses": {
          "200": {"description": "Successful Response", "content": {"application/json": {"schema": {"type": "object", "additionalProperties": true, "title": "Response Levels Api Levels  Stock Symbol  Get"}}}},
          "422": {"description": "Validation Error", "content": {"application/json": {"schema": {"$ref": "#/components/schemas/HTTPValidationError"}}}}
        }
      }
    },
    "/api/status/{stock_symbol}": {
      "get": {
        "operationId": "status_api_status__stock_symbol__get", "summary": "Status",
        "parameters": [{"name": "stock_symbol", "in": "path", "required": true, "schema": {"type": "string", "title": "Stock Symbol"}}],
        "responses": {
          "200": {"description": "Successful Response", "content": {"application/json": {"schema": {"type": "object", "additionalProperties": true, "title": "Response Status Api Status  Stock Symbol  Get"}}}},
          "422": {"description": "Validation Error", "content": {"application/json": {"schema": {"$ref": "#/components/schemas/HTTPValidationError"}}}}
        }
      }
    },
    "/api/cleanup": {
      "post": {
        "operationId": "cleanup_api_cleanup_post", "summary": "Cleanup",
        "responses": {"200": {"description": "Successful Response", "content": {"application/json": {"schema": {"type": "object", "additionalProperties": true, "title": "Response Cleanup Api Cleanup Post"}}}}}
      }
    }
  },
  "components": {
    "schemas": {
      "HTTPValidationError": {
        "properties": {"detail": {"items": {"$ref": "#/components/schemas/ValidationError"}, "type": "array", "title": "Detail"}},
        "type": "object", "title": "HTTPValidationError"
      },
      "ValidationError": {
        "properties": {
          "ctx": {"type": "object", "title": "Context"},
          "input": {"title": "Input"},
          "loc": {"items": {"anyOf": [{"type": "string"}, {"type": "integer"}]}, "type": "array", "title": "Location"},
          "msg": {"type": "string", "title": "Message"},
          "type": {"type": "string", "title": "Error Type"}
        },
        "type": "object", "required": ["loc", "msg", "type"], "title": "ValidationError"
      }
    }
  }
}
`

func TestGoOpenAPIMatchesFrozenFastAPISchema(t *testing.T) {
	var want any
	if err := json.Unmarshal([]byte(fastAPIOpenAPIFrozenSchema), &want); err != nil {
		t.Fatalf("decode frozen FastAPI OpenAPI schema: %v", err)
	}

	encodedGot, err := json.Marshal(fastAPIOpenAPIDocument())
	if err != nil {
		t.Fatalf("encode Go OpenAPI document: %v", err)
	}
	var got any
	if err := json.Unmarshal(encodedGot, &got); err != nil {
		t.Fatalf("decode Go OpenAPI document: %v", err)
	}

	if differences := openAPISchemaDifferences(got, want, "$", 20); len(differences) > 0 {
		t.Errorf("Go OpenAPI differs from the frozen FastAPI schema:\n%s", differences)
	}
}

func openAPISchemaDifferences(got, want any, path string, limit int) []string {
	if limit <= 0 {
		return nil
	}
	gotMap, gotIsMap := got.(map[string]any)
	wantMap, wantIsMap := want.(map[string]any)
	if gotIsMap && wantIsMap {
		keys := make(map[string]struct{}, len(gotMap)+len(wantMap))
		for key := range gotMap {
			keys[key] = struct{}{}
		}
		for key := range wantMap {
			keys[key] = struct{}{}
		}
		sortedKeys := make([]string, 0, len(keys))
		for key := range keys {
			sortedKeys = append(sortedKeys, key)
		}
		sort.Strings(sortedKeys)
		var differences []string
		for _, key := range sortedKeys {
			gotValue, gotExists := gotMap[key]
			wantValue, wantExists := wantMap[key]
			childPath := path + "." + key
			if !gotExists {
				differences = append(differences, fmt.Sprintf("%s: missing in Go; FastAPI has %#v", childPath, wantValue))
			} else if !wantExists {
				differences = append(differences, fmt.Sprintf("%s: Go has unexpected %#v", childPath, gotValue))
			} else {
				differences = append(differences, openAPISchemaDifferences(gotValue, wantValue, childPath, limit-len(differences))...)
			}
			if len(differences) >= limit {
				break
			}
		}
		return differences
	}
	gotSlice, gotIsSlice := got.([]any)
	wantSlice, wantIsSlice := want.([]any)
	if gotIsSlice && wantIsSlice {
		var differences []string
		if len(gotSlice) != len(wantSlice) {
			differences = append(differences, fmt.Sprintf("%s: Go has %d item(s); FastAPI has %d", path, len(gotSlice), len(wantSlice)))
		}
		for index := 0; index < len(gotSlice) && index < len(wantSlice) && len(differences) < limit; index++ {
			differences = append(differences, openAPISchemaDifferences(gotSlice[index], wantSlice[index], fmt.Sprintf("%s[%d]", path, index), limit-len(differences))...)
		}
		return differences
	}
	if !reflect.DeepEqual(got, want) {
		return []string{fmt.Sprintf("%s: Go has %#v; FastAPI has %#v", path, got, want)}
	}
	return nil
}
