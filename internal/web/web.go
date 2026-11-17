// Package web holds MineNest's GUI: a page the daemon serves on this computer.
package web

import (
	"embed"
	"io/fs"
)

//go:embed static
var files embed.FS

// Files is the GUI's folder, ready to serve.
func Files() fs.FS {
	f, _ := fs.Sub(files, "static")
	return f
}
