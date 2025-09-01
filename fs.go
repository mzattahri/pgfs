// Package pgfs implements an [fs.FS]-compatible file system that reads and writes
// files to Postgres as [Large Objects].
//
// # Large Objects
//
// On Postgres, [Large Objects] offer the ability to store files of any size up to
// 4GB. While [BYTEA] columns are often easier to use and come with many benefits,
// [Large Objects] allow content to be streamed and processed in chunks, just like
// a regular local file. As such, they're a perfect fit for the interfaces of the
// [io] and [fs] packages.
//
// # Structure
//
// [FS] is organized as a flat file system where files use UUID strings as names.
//
// Files are meant to be written once, and used as immutable read-only blobs
// afterwards. They're tracked in a dedicated metadata table called "pgfs_metadata",
// which can be created by calling [MigrateUp]. See [Up] for more information
// on the schema used.
//
// While Postgres does not currently support referential integrity for [Large Objects],
// the "pgfs_metadata" table can be referenced by foreign keys to obtain
// the same guarantees. To that effect, it is recommended to use a [deletion constraint]
// ("ON DELETE") constraint in order to prevent a row referencing a file from being deleted
// before it's been formally removed with [FS.Remove].
//
//	CREATE TABLE user_files (
//		[...]
//		file_id UUID NOT NULL,
//		FOREIGN KEY (file_id) REFERENCES pgfs_metadata (id) ON DELETE RESTRICT,
//		[...]
//	);
//
// # Metadata
//
// Attributes that do not require referential integrity can be stored
// with each file using the [Sys] map passed when [FS.Create] is called.
//
// It can later be accessed via the [FileInfo] interface, either using [FS.Stat] or
// by opening the file.
//
//	info, err := fsys.Stat("d7f225c4-db00-4b9f-8ed3-82682ca4171c")
//	if err != nil {
//	   log.Fatal(err)
//	}
//	sys := info.Sys().(pgfs.Sys)
//	log.Println(sys["someAttribute"])
//
// Because [Sys] is stored as a [JSONB] column, metadata can also be queried
// directly from the "pgfs_metadata" table using the standard
// [JSON operators] of Postgres.
//
//	SELECT sys ->> 'someAttribute' as 'someAttribute'
//	FROM pgfs_metadata
//	WHERE id = 'd7f225c4-db00-4b9f-8ed3-82682ca4171c'::uuid
//
// [Large Objects]: https://www.postgresql.org/docs/current/largeobjects.html
// [BYTEA]: https://www.postgresql.org/docs/current/datatype-binary.html
// [JSONB]: https://www.postgresql.org/docs/current/datatype-json.html
// [JSON operators]: https://www.postgresql.org/docs/current/functions-json.html#FUNCTIONS-JSON-OP-TABLE
// [deletion constraint]: https://www.postgresql.org/docs/current/ddl-constraints.html#DDL-CONSTRAINTS-FK
package pgfs

import (
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/http"

	"github.com/google/uuid"
)

// root is the UUID assigned to the virtual root
// directory of the file system.
const root = "00000000-0000-0000-0000-000000000000"

var rootUUID = uuid.MustParse(root)

// GenerateUUID returns a new random UUID string.
func GenerateUUID() string {
	return uuid.New().String()
}

// BinaryType is the generic MIME type for
// binary content.
const BinaryType = "application/octet-stream"

// Tx represents a database transaction type, such as [sql.Tx].
type Tx interface {
	Query(query string, args ...any) (*sql.Rows, error)
	QueryRow(query string, args ...any) *sql.Row
	Exec(query string, args ...any) (sql.Result, error)
	Rollback() error
	Commit() error
}

var _ Tx = &sql.Tx{}

// ValidPath is analog to [fs.ValidPath], and checks
// if name is a valid UUID.
func ValidPath(name string) bool {
	if name == "" { // root directory
		return true
	}
	_, err := uuid.Parse(name)
	return err == nil
}

// FS implements a file system using the Large Objects API
// of Postgres.
//
// FS implements [fs.StatFS] and [fs.ReadDirFS].
type FS struct {
	conn Tx
}

// New returns a new instance of [FS] bound to
// a database transaction.
func New(conn Tx) *FS {
	return &FS{conn: conn}
}

// ReadFile returns the content of the file with the
// given name.
func (fsys *FS) ReadFile(name string) ([]byte, error) {
	f, err := fsys.Open(name)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return io.ReadAll(f)
}

// ReadDir implements [fs.ReadDirFS].
// [fs.ErrNotExist] is returned if name is not an empty string.
func (fsys *FS) ReadDir(name string) ([]fs.DirEntry, error) {
	if name != "" {
		return nil, fs.ErrNotExist
	}

	const q = `
	  SELECT 
			id, oid, created_at,
			sys, content_size, content_type,
			content_sha256
	  FROM pgfs_metadata
	  ORDER BY created_at ASC
	`
	rows, err := fsys.conn.Query(q)
	if err != nil {
		return nil, err
	}

	entries := make([]fs.DirEntry, 0)
	defer rows.Close()
	for rows.Next() {
		e := &entry{}
		err := rows.Scan(
			&e.id,
			&e.oid,
			&e.createdAt,
			&e.sys,
			&e.contentSize,
			&e.contentType,
			&e.contentSHA256,
		)
		if err != nil {
			return nil, err
		}
		entries = append(entries, e)
	}
	return entries, nil
}

func (fsys *FS) rootInfo() (fs.FileInfo, error) {
	const q = `
		SELECT 
			COALESCE(MAX(created_at), NOW()) as created_at, 
			COALESCE(SUM(content_size), 0) as content_size 
		FROM pgfs_metadata
	`
	fi := &entry{
		id:   rootUUID,
		mode: fs.ModeDir,
	}
	err := fsys.conn.QueryRow(q).Scan(&fi.createdAt, &fi.contentSize)
	if err != nil && err != sql.ErrNoRows {
		return nil, err
	}
	return fi, nil
}

// Stat returns info on the file with the given name.
//
// If name is an empty string, the returned info is on the
// root directory.
//
// The returned value implements [FileInfo].
func (fsys *FS) Stat(name string) (fs.FileInfo, error) {
	if name == "" {
		return fsys.rootInfo()
	}

	id, err := uuid.Parse(name)
	if err != nil {
		return nil, fs.ErrNotExist
	}

	const q = `
	  SELECT 
			oid, created_at, sys,
			content_size, content_type, content_sha256
		FROM pgfs_metadata
		WHERE id = $1
	`
	row := fsys.conn.QueryRow(q, id)
	e := &entry{
		id:   id,
		mode: 0,
	}
	err = row.Scan(
		&e.oid,
		&e.createdAt,
		&e.sys,
		&e.contentSize,
		&e.contentType,
		&e.contentSHA256,
	)
	if err == sql.ErrNoRows {
		err = fs.ErrNotExist
	}
	return e, err
}

// Open returns the file with the given name.
//
// If name is an empty string, the root directory
// is returned.
func (fsys *FS) Open(name string) (fs.File, error) {
	if name == "" {
		di, err := fsys.Stat("")
		if err != nil {
			return nil, err
		}
		return &dir{fsys: fsys, info: di.(*entry)}, nil
	}

	id, err := uuid.Parse(name)
	if err != nil {
		return nil, fs.ErrNotExist
	}

	info, fd, err := open(fsys.conn, id, invRead)
	if err != nil {
		return nil, err
	}

	f := &file{
		fd:   fd,
		fsys: fsys,
		info: info,
	}
	return f, nil
}

// Create returns a writer to a new file with the given
// name and content type. The caller must close the writer
// for the operation to complete.
//
// The name must be a valid and unique UUID.
//
// The content type should be a valid MIME type, such as
// "application/pdf" or "image/png". If an empty string is passed,
// [http.DetectContentType] will be used to make a guess
// from the first 512 bytes of data written.
//
// Custom metadata attributes can be passed and stored with the file
// using sys. They can later be accessed using [fs.FileInfo.Sys]
// by either opening the file or calling [FS.Stat].
func (fsys *FS) Create(name, contentType string, sys map[string]string) (io.WriteCloser, error) {
	id, err := uuid.Parse(name)
	if err != nil {
		pErr := &fs.PathError{
			Op:   "create",
			Path: name,
			Err:  err,
		}
		return nil, pErr
	}

	oid, fd, err := create(fsys.conn, id)
	if err != nil {
		return nil, err
	}

	w := &writer{
		fd:          fd,
		oid:         oid,
		fsys:        fsys,
		hasher:      sha256.New(),
		id:          id,
		sys:         sys,
		contentType: contentType,
	}
	return w, nil
}

// Remove deletes the file with the given name.
func (fsys *FS) Remove(name string) error {
	id, err := uuid.Parse(name)
	if err != nil {
		return fs.ErrNotExist
	}

	return remove(fsys.conn, id)
}

var (
	_ fs.StatFS    = &FS{}
	_ fs.ReadDirFS = &FS{}
)

// detectContentType detects the content type of a seekable source by reading
// the first 512 bytes. The seeking position is restored before the function exits.
func detectContentType(src io.ReadSeeker) (string, error) {
	// Save initial position
	pos, err := src.Seek(0, io.SeekCurrent)
	if err != nil {
		return "", fmt.Errorf("error seeking initial position: %w", err)
	}

	// Seek top
	if _, err := src.Seek(0, io.SeekStart); err != nil {
		return "", fmt.Errorf("error seeking top: %w", err)
	}

	tag := make([]byte, 512)
	n, err := io.ReadFull(src, tag)
	if err != nil && !errors.Is(err, io.EOF) {
		return "", fmt.Errorf("error reading from src: %w", err)
	}

	// Return to initial position
	if _, err := src.Seek(pos, io.SeekStart); err != nil {
		return "", fmt.Errorf("error returning to initial position: %w", err)
	}

	return http.DetectContentType(tag[:n]), nil
}

// ServeFile serves the content of a file over HTTP.
//
// If f is a file created by this package, [http.ServeContent]
// is used after adding the appropriate headers sourced
// from its [FileInfo].
//
//	[...]
//	Content-Type: application/png                                             // FileInfo.ContentType()
//	ETag: "0de648a9c8c19264e6cd6a441a867d0989a03929cacec442ad1f0cd192bc9072"  // FileInfo.ContentSHA256()
//	Last-Modified: Thu, 22 Jun 2023 14:34:35 GMT                              // FileInfo.ModTime()
//	Repr-Digest: sha-256=:DeZIqcjBkmTmzWpEGoZ9CYmgOSnKzsRCrR8M0ZK8kHI=:       // FileInfo.ContentSHA256()
//	[...]
func ServeFile(w http.ResponseWriter, r *http.Request, f fs.File) {
	if handler, ok := f.(http.Handler); ok {
		handler.ServeHTTP(w, r)
		return
	}

	info, err := f.Stat()
	if err != nil {
		slog.ErrorContext(r.Context(), "error reading file stat", "err", err)
		http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
		return
	}
	if info.IsDir() {
		slog.ErrorContext(r.Context(), "error serving directory")
		http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
		return
	}

	if rsc, ok := f.(io.ReadSeekCloser); ok {
		cty, err := detectContentType(rsc)
		if err != nil {
			slog.ErrorContext(r.Context(), "error detecting content type", "err", err)
			http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", cty)
		http.ServeContent(w, r, info.Name(), info.ModTime(), rsc)
		return
	}

	w.Header().Set("Last-Modified", info.ModTime().Format(http.TimeFormat))
	w.Header().Set("Content-Type", BinaryType)
	if _, err := io.Copy(w, f); err != nil {
		slog.ErrorContext(r.Context(), "error copying file to response", "id", info.Name(), "err", err)
	}
}
