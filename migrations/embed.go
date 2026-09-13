// Package migrations embeds the SQL schema so the binary carries its own
// migrations. Tests apply the identical files, which means a passing test run
// exercises the same schema that ships — there is no second, drifting copy of
// the DDL maintained for tests.
package migrations

import "embed"

//go:embed *.sql
var FS embed.FS
