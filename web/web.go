// Package web embeds the panel's templates, static assets and translation
// files into the binary.
package web

import "embed"

// Files holds templates/, static/ and locales/.
//
//go:embed templates static locales
var Files embed.FS
