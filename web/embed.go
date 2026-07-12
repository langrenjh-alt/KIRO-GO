// Package web exposes the administration UI as build-time embedded assets.
package web

import "embed"

// Assets contains every file served below /admin. Keep source files out of the
// patterns so the embedded file server cannot expose this Go file.
//
//go:embed index.html *.js *.css *.ico *.png locales/*.json vendor/fontawesome/css/*.css vendor/fontawesome/webfonts/*.woff2 vendor/tailwindcss-browser/*.js
var Assets embed.FS
