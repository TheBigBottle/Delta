// Package web holds the web interface (HTML, CSS, JavaScript, the help text).
// The files are embedded into the binary, so Delta needs no files besides its
// config, and works on machines without internet access.
package web

import "embed"

// Files contains index.html, style.css and the scripts at the top level,
// help.md (the help page, plain Markdown) with its images in help/, and the third-party libraries
// of the browser side in vendor/ (uPlot for the traffic chart).
//
//go:embed index.html style.css *.js help.md help vendor
var Files embed.FS
