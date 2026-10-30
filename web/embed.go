// Package web holds the web interface (HTML, CSS, JavaScript). The files are
// embedded into the binary, so Delta needs no files besides its config.
package web

import "embed"

// Files contains index.html, style.css and the scripts, at the top level.
//
//go:embed index.html style.css *.js
var Files embed.FS
