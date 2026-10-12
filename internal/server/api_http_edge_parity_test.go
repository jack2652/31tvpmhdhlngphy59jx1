package server

import (
	"encoding/json"
	"net/http"
	"testing"
)

func TestChainExpirationAcceptsUnicodeDecimalDigitsLikeFastAPI(t *testing.T) {
	_, ts := testService(t, "")
	const expiration = "٢٠٣٠-٠١-١٨"

	response, err := http.Get(ts.URL + "/api/chain/QQQ?expiration=" + expiration)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()

	var body map[string]any
	if err := json.NewDecoder(response.Body).Decode(&body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%#v; FastAPI's Unicode-aware \\d pattern accepts %q", response.StatusCode, body, expiration)
	}
	if body["expiration"] != expiration {
		t.Errorf("expiration=%#v, want %q", body["expiration"], expiration)
	}
}
