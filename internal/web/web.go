// Package web embeds the console's templates and static assets.
//
// embed.FS plus html/template plus stdlib net/http, with htmx vendored as a
// static file. No framework and no npm: the whole console ships inside the one
// binary, which is what makes "copy it to a Pi and run it" true.
package web

import "embed"

// Templates holds the HTML.
//
//go:embed templates/*.html
var Templates embed.FS

// Static holds CSS, JavaScript and icons.
//
//go:embed static
var Static embed.FS
