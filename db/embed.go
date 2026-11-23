// Package dbmigrations embeds the SQL migrations into the binary so a
// deployment is always migrated with exactly the schema it was built with.
package dbmigrations

import "embed"

// FS holds db/migrations/*.sql.
//
//go:embed migrations/*.sql
var FS embed.FS
