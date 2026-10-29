// Package migrations embeds the SQL schema migrations into the binary.
package migrations

import "embed"

//go:embed *.sql
var Files embed.FS
