// Package web holds the explorer's web UI. Its source is in src/; "npm run
// build" compiles it into dist/, which is embedded here.
package web

import (
	"embed"
	"io/fs"
)

//go:embed all:dist
var dist embed.FS

// UI returns the built web UI. It has no index.html until the UI is built.
func UI() fs.FS {
	sub, err := fs.Sub(dist, "dist")
	if err != nil {
		panic(err)
	}
	return sub
}
