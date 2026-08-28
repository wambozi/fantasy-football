// Package web embeds the single-page UI so the server binary is self-contained.
// No build step: plain HTML/CSS/JS, because on draft night every toolchain is a
// thing that can fail.
package web

import "embed"

//go:embed index.html app.js broadsheet.css draft-copilot.css
var FS embed.FS
