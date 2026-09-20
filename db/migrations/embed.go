// Package migrations embeds the SQL migration set in the application binary.
package migrations

import "embed"

// Files contains every up and down migration shipped with this build.
//
//go:embed *.sql
var Files embed.FS
