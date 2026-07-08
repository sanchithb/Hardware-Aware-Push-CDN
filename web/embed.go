// Package web embeds the controller's web console (a dependency-free
// single-page application) into the hpcdn binary via go:embed, so the
// console ships with the controller and needs no separate frontend server
// or build step.
package web

import (
	"embed"
	"io/fs"
)

//go:embed console
var consoleFS embed.FS

// Console returns the console filesystem rooted at its index.
func Console() fs.FS {
	sub, err := fs.Sub(consoleFS, "console")
	if err != nil {
		panic(err) // embed layout is fixed at build time
	}
	return sub
}
