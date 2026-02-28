//go:build embed
// +build embed

package main

import (
	"embed"
	"io/fs"
)

//go:embed all:web/dist
var embeddedFiles embed.FS

// getEmbeddedFS returns the embedded static files
func getEmbeddedFS() fs.FS {
	return embeddedFiles
}
