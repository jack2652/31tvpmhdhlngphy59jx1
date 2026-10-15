package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"testing"
)

func TestRawLevelsPropagatesSQLiteCacheReadErrors(t *testing.T) {
	for _, table := range []string{
		"quote_snapshots",
		"price_extremes",
		"beta_snapshots",
	} {
		t.Run(table, func(t *testing.T) {
			service, ts := testService(t, "")
			admin, err := sql.Open("sqlite", service.cfg.DatabasePath+"?_busy_timeout=30000")
			if err != nil {
				t.Fatal(err)
			}
			defer admin.Close()
			if _, err := admin.ExecContext(context.Background(), "DROP TABLE "+table); err != nil {
				t.Fatalf("drop fixture table: %v", err)
			}

			response, err := http.Get(ts.URL + "/api/levels/QQQ?expiration=2099-01-16&raw=true")
			if err != nil {
				t.Fatal(err)
			}
			defer response.Body.Close()
			var body map[string]any
			if err := json.NewDecoder(response.Body).Decode(&body); err != nil {
				t.Fatalf("decode error response: %v", err)
			}
			if response.StatusCode != http.StatusInternalServerError {
				t.Fatalf("status=%d body=%#v, want 500 for failed %s cache read", response.StatusCode, body, table)
			}
			if detail, ok := body["detail"].(string); !ok || detail == "" {
				t.Fatalf("error response detail=%#v, want database read error", body["detail"])
			}
		})
	}
}
