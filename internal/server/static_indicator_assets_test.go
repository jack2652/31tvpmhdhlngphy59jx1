package server

import (
	"bytes"
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"testing/fstest"

	"github.com/gin-gonic/gin"
	"option-scope-go/internal/config"
)

func TestEmbeddedPageGzipMatchesPlainPage(t *testing.T) {
	for _, lowMemory := range []bool{false, true} {
		service := Service{cfg: config.Config{DefaultSymbols: []string{"SPY"}, AccessKey: "test", LowMemory: lowMemory}}
		router := gin.New()
		router.Use(responseCompression(lowMemory))
		router.GET("/", service.index())
		router.GET("/futures", service.futuresIndex())
		for path, symbol := range map[string]string{"/": "SPY", "/futures": "ES=F"} {
			request := httptest.NewRequest(http.MethodGet, path, nil)
			request.Header.Set("Accept-Encoding", "gzip;q=0")
			plain := httptest.NewRecorder()
			router.ServeHTTP(plain, request)
			if plain.Code != http.StatusOK || plain.Header().Get("Content-Encoding") != "" ||
				!bytes.Contains(plain.Body.Bytes(), []byte(symbol)) || bytes.Contains(plain.Body.Bytes(), []byte("__ASSET_VERSION__")) ||
				!bytes.Contains(plain.Body.Bytes(), []byte(`name="option-scope-access-required" content="true"`)) {
				t.Fatalf("页面 %s 未正确填入配置或忽略了 gzip;q=0", path)
			}
			request.Header.Set("Accept-Encoding", "gzip")
			compressed := httptest.NewRecorder()
			router.ServeHTTP(compressed, request)
			reader, err := gzip.NewReader(compressed.Body)
			if err != nil {
				t.Fatal(err)
			}
			decoded, err := io.ReadAll(reader)
			reader.Close()
			if err != nil || !bytes.Equal(decoded, plain.Body.Bytes()) || !hasVaryToken(compressed.Header(), "Accept-Encoding") {
				t.Fatalf("页面 %s 的 gzip 响应不一致，err=%v", path, err)
			}
		}
	}
}

func TestIndexAssetVersionChangesForEachIndicatorAlgorithm(t *testing.T) {
	for _, changed := range []string{"chan-turtle.js", "chan-granville.js", "chan-wyckoff.js",
		"chan-vwap.js", "chan-bollinger.js", "chan-rsi-divergence.js", "chan-relative-strength.js"} {
		t.Run(changed, func(t *testing.T) {
			assets := fstest.MapFS{
				"common/css/styles.css":       &fstest.MapFile{Data: []byte("style-v1")},
				"common/js/chan-turtle.js":    &fstest.MapFile{Data: []byte("turtle-v1")},
				"common/js/chan-granville.js": &fstest.MapFile{Data: []byte("granville-v1")},
				"common/js/chan-wyckoff.js":   &fstest.MapFile{Data: []byte("wyckoff-v1")},
			}
			file := "common/js/" + changed
			assets[file] = &fstest.MapFile{Data: []byte("algorithm-v1")}
			before := indexAssetVersion(assets)
			assets[file].Data = []byte("algorithm-v2")
			if after := indexAssetVersion(assets); after == before {
				t.Fatal("仅更新指标算法后版本未改变，浏览器仍可能使用旧的不可变缓存")
			}
		})
	}
}
