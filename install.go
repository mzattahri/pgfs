package pgfs

import _ "embed"

// Schema is the name of the schema created by [Install].
const Schema = "pgfs"

// Table is the fully qualified name of the metadata table
// created by [Install].
const Table = "pgfs.metadata"

// InstallSQL is the SQL executed by [Install]. It is exported for
// programs that manage their schema with a migration tool.
//
// It creates [Schema] and [Table] if they do not exist.
//
//go:embed sql/install.sql
var InstallSQL string

// UninstallSQL is the SQL executed by [Uninstall]. It is exported for
// programs that manage their schema with a migration tool.
//
// It drops [Table] and [Schema]. It does not drop dependent objects:
// it fails if a foreign key references [Table] or if [Schema]
// contains other objects.
//
//go:embed sql/uninstall.sql
var UninstallSQL string

// Install creates the schema and table used by [FS] by executing
// [InstallSQL] in tx. It must be called before the first use of
// a database, and it is safe to call again: if the table already
// exists, Install does nothing.
//
// The changes take effect when tx is committed.
func Install(tx Tx) error {
	_, err := tx.Exec(InstallSQL)
	return err
}

// Uninstall drops the schema and table used by [FS] by executing
// [UninstallSQL] in tx.
//
// Uninstall drops only the metadata. The large objects holding the
// content of files are not removed; call [FS.Remove] on each file
// first to delete them.
//
// The changes take effect when tx is committed.
func Uninstall(tx Tx) error {
	_, err := tx.Exec(UninstallSQL)
	return err
}
