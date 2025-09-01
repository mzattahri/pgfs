package pgfs

// Table is the name of the metadata table
// created when [MigrateUp] is called.
const Table = "pgfs_metadata"

// Up is the SQL query executed by [MigrateUp].
const Up = `
	CREATE EXTENSION IF NOT EXISTS lo;
	CREATE TABLE IF NOT EXISTS pgfs_metadata (
		id UUID NOT NULL PRIMARY KEY,
		oid OID NOT NULL UNIQUE,
		created_at TIMESTAMP NOT NULL DEFAULT NOW(),
		sys JSONB,
		content_type TEXT NOT NULL DEFAULT 'application/octet-stream',
		content_size BIGINT NOT NULL,
		content_sha256 BYTEA NOT NULL
	);
`

// Down is the SQL query executed by [MigrateDown].
const Down = "DROP TABLE pgfs_metadata;"

// MigrateUp executes the SQL query in [Up].
//
// Calling MigrateUp multiple times has no effect.
func MigrateUp(conn Tx) error {
	_, err := conn.Exec(Up)
	return err
}

// MigrateDown executes the SQL query in [Down].
func MigrateDown(conn Tx) error {
	_, err := conn.Exec(Down)
	return err
}
