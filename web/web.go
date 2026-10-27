// Package web embeds the Mini App front end into the binary.
package web

import (
	"embed"
	"io/fs"
)

//go:embed static
var files embed.FS

// FS returns the static files with "static/" stripped, ready for http.FileServerFS.
func FS() fs.FS {
	sub, err := fs.Sub(files, "static")
	if err != nil {
		panic(err) // the path is a compile-time constant
	}
	return sub
}
