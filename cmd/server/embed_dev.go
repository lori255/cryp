//go:build !embed
// +build !embed

package main

import "io/fs"

// getEmbeddedFS returns nil in dev mode (no embedded files)
func getEmbeddedFS() fs.FS {
	return nil
}
