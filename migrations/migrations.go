// Package migrations embeds the versioned PostgreSQL DDL applied by the
// migration runner.
package migrations

import "embed"

// FS contains all *.sql migrations in lexical order.
//
//go:embed *.sql
var FS embed.FS
