// Package migrations embeds cs-user's goose SQL migrations into the binary
// so that `go install` produces a single self-contained artifact — operators
// do not need to ship migrations separately.
//
// Add new migrations as `*.sql` files in this directory; the //go:embed
// directive picks them up automatically. Files MUST start with a sortable
// timestamp prefix (YYYYMMDDhhmmss_) so goose orders them correctly.
package migrations

import "embed"

// FS holds the embedded migration files. Callers pass it to migration.Runner
// via fs.Sub to obtain a goose-compatible fs.FS.
//
//go:embed *.sql
var FS embed.FS
