// Package pgfs implements a file system that stores files in PostgreSQL
// as [large objects].
//
// Unlike a [BYTEA] column, a large object can be read and written in
// chunks, and can hold up to 4 TB of data. This makes it a good fit for
// the streaming interfaces of the [io] and [fs] packages: [FS] implements
// [fs.FS], [fs.StatFS], [fs.ReadDirFS] and [fs.ReadFileFS], and files
// can be served over HTTP with [ServeFile].
//
// # Setup
//
// Files are recorded in a metadata table, [Table], which lives in its own
// schema, [Schema]. [Install] creates both, and must be called once before
// a database is used:
//
//	tx, err := db.Begin()
//	if err != nil {
//		log.Fatal(err)
//	}
//	defer tx.Rollback()
//	if err := pgfs.Install(tx); err != nil {
//		log.Fatal(err)
//	}
//	if err := tx.Commit(); err != nil {
//		log.Fatal(err)
//	}
//
// Programs that manage their schema with a migration tool can use
// [InstallSQL] and [UninstallSQL] instead.
//
// # Transactions
//
// PostgreSQL only allows large objects to be accessed inside a
// transaction, so an [FS] is bound to one with [New]. Files opened or
// created with an FS can be used only until the transaction ends, and
// files created with it become visible to other transactions only once
// it is committed. If the transaction is rolled back, the files created
// with it are discarded.
//
// When an operation fails, PostgreSQL aborts the transaction, and it
// must be rolled back.
//
// # Contexts
//
// The methods of [fs.FS] and [fs.File] do not take a context. To bound
// the operations of an [FS] with one, begin its transaction with
// [sql.DB.BeginTx]: once the context is canceled, the transaction is
// rolled back, and further operations fail. When serving files over
// HTTP, the context of the request is usually the right one.
//
// # Names
//
// The file system is flat: it consists of a root directory containing
// files, and has no subdirectories. Each file is named by a UUID, chosen
// by the caller when the file is created with [FS.Create]. Methods that
// take a name accept any form accepted by [uuid.Parse], and return
// entries named with the canonical form returned by [uuid.UUID.String].
// The root directory is named ".", as required by [fs.FS].
//
// Files are immutable: once created, their content cannot be changed.
// To replace a file, remove it with [FS.Remove] and create a new one.
//
// # Metadata
//
// Each file is stored with its size, MIME type, SHA-256 digest and
// creation time, which are available through [FileInfo]. A file can also
// carry custom attributes, passed to [FS.Create] as a [Sys] and returned
// by [fs.FileInfo.Sys]:
//
//	info, err := fsys.Stat("d7f225c4-db00-4b9f-8ed3-82682ca4171c")
//	if err != nil {
//		log.Fatal(err)
//	}
//	sys := info.Sys().(pgfs.Sys)
//	fmt.Println(sys["author"])
//
// Attributes are stored in a [JSONB] column, so they can also be queried
// with the [JSON operators] of PostgreSQL:
//
//	SELECT id FROM pgfs.metadata WHERE sys ->> 'author' = 'Renee French';
//
// # Referential integrity
//
// PostgreSQL does not enforce referential integrity for large objects,
// but other tables can reference files through [Table]. A foreign key
// with ON DELETE RESTRICT prevents a file from being removed while it is
// still referenced:
//
//	CREATE TABLE user_files (
//		user_id BIGINT NOT NULL,
//		file_id UUID NOT NULL REFERENCES pgfs.metadata (id) ON DELETE RESTRICT
//	);
//
// [large objects]: https://www.postgresql.org/docs/current/largeobjects.html
// [BYTEA]: https://www.postgresql.org/docs/current/datatype-binary.html
// [JSONB]: https://www.postgresql.org/docs/current/datatype-json.html
// [JSON operators]: https://www.postgresql.org/docs/current/functions-json.html
package pgfs
