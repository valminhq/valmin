// Package web carries the built SPA into the binary.
//
// One artefact to deploy and no Node process in production (`02 §2.1`, ADR-002): the Go
// binary serves these files itself. `make build` writes them here before compiling; a fresh
// checkout has only the directory, and the panel then serves an unbuilt-SPA page rather
// than failing to compile.
package web

import "embed"

//go:embed all:build
var Assets embed.FS
