// Package web embeds the pyrite front end so the binary ships as a
// single self-contained file with no runtime asset dependencies.
package web

import "embed"

//go:embed index.html app.js styles.css vendor
var FS embed.FS
