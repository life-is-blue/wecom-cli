// Package db embeds the versioned MySQL migrations.
package db

import "embed"

// Migrations holds migrations/*.sql, applied in lexical order by the server.
//
//go:embed migrations/*.sql
var Migrations embed.FS
