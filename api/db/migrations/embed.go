// Package migrations embeds the SQL migration files so they ship inside the
// binary and auto-apply on boot. The .sql files are also read directly by the
// tern CLI (make migrate) and by sqlc (schema source).
package migrations

import "embed"

//go:embed *.sql
var FS embed.FS
