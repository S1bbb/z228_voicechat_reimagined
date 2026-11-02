// Package web — фронтенд, встроенный в бинарник.
package web

import "embed"

//go:embed static
var Static embed.FS
