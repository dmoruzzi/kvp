// Package web embeds the static admin UI served at "/" (spec §5). The UI is
// self-contained: no CDN, no inline scripts, strict CSP — values are rendered
// with textContent so stored payloads can never inject HTML.
package web

import "embed"

//go:embed index.html style.css app.js
var FS embed.FS
