package web

import (
	"embed"
	"io/fs"
)

//go:embed static
var embedded embed.FS

// Static returns the bundled browser assets rooted at static/.
func Static() fs.FS {
	assets, err := fs.Sub(embedded, "static")
	if err != nil {
		panic(err)
	}
	return assets
}
