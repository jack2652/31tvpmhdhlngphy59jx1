package server

import (
	"testing"
	"testing/fstest"
)

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
