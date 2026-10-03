package pgfs

import (
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"sync"

	"uuid"
)

// BinaryType is the MIME type of arbitrary binary data.
const BinaryType = "application/octet-stream"

// Tx is the database transaction an [FS] operates in.
// It is implemented by [*sql.Tx].
type Tx interface {
	Exec(query string, args ...any) (sql.Result, error)
	Query(query string, args ...any) (*sql.Rows, error)
	QueryRow(query string, args ...any) *sql.Row
}

var _ Tx = &sql.Tx{}

// ValidPath reports whether name is a valid name for an [FS]:
// either ".", which names the root directory, or a UUID in
// a form accepted by [uuid.Parse].
func ValidPath(name string) bool {
	if name == "." {
		return true
	}
	_, err := uuid.Parse(name)
	return err == nil
}

// An FS is a file system that stores files as large objects
// in a PostgreSQL database. It must be created with [New].
//
// An FS is safe for concurrent use by multiple goroutines.
// Since a transaction runs one query at a time, so does an FS:
// concurrent operations wait for each other.
type FS struct {
	mu sync.Mutex // serializes queries
	tx Tx
}

// New returns an [FS] that operates in tx.
//
// The FS can be used only until tx is committed or rolled back.
func New(tx Tx) *FS {
	return &FS{tx: tx}
}

// queryRow runs a query that returns a single row,
// and copies its columns into dest.
func (fsys *FS) queryRow(query string, args []any, dest ...any) error {
	fsys.mu.Lock()
	defer fsys.mu.Unlock()
	return fsys.tx.QueryRow(query, args...).Scan(dest...)
}

// exec runs a query that returns no rows.
func (fsys *FS) exec(query string, args ...any) error {
	fsys.mu.Lock()
	defer fsys.mu.Unlock()
	_, err := fsys.tx.Exec(query, args...)
	return err
}

// parse returns the UUID named by name, or an
// error matching [fs.ErrNotExist] if name is not one.
func parse(op, name string) (uuid.UUID, error) {
	id, err := uuid.Parse(name)
	if err != nil {
		return id, &fs.PathError{Op: op, Path: name, Err: fs.ErrNotExist}
	}
	return id, nil
}

// pathError wraps err in an [fs.PathError],
// unless it is nil or [io.EOF].
func pathError(op, name string, err error) error {
	if err == nil || err == io.EOF {
		return err
	}
	return &fs.PathError{Op: op, Path: name, Err: err}
}

// Open opens the named file for reading. It implements [fs.FS].
//
// The returned file also implements [io.Seeker] and [io.ReaderAt].
// If name is ".", Open returns the root directory,
// which implements [fs.ReadDirFile] and [http.File].
func (fsys *FS) Open(name string) (fs.File, error) {
	if name == "." {
		info, err := fsys.rootInfo()
		if err != nil {
			return nil, pathError("open", name, err)
		}
		return &dir{fsys: fsys, info: info}, nil
	}

	id, err := parse("open", name)
	if err != nil {
		return nil, err
	}
	info, fd, err := fsys.open(id, invRead)
	if err != nil {
		return nil, pathError("open", name, err)
	}
	return &file{fsys: fsys, fd: fd, info: info}, nil
}

// Stat returns an [fs.FileInfo] describing the named file.
// It implements [fs.StatFS].
//
// The returned value also implements [FileInfo]. If name is ".",
// it describes the root directory: its size is the total size of
// all files, and its modification time is the creation time of
// the most recent one.
func (fsys *FS) Stat(name string) (fs.FileInfo, error) {
	if name == "." {
		info, err := fsys.rootInfo()
		if err != nil {
			return nil, pathError("stat", name, err)
		}
		return info, nil
	}

	id, err := parse("stat", name)
	if err != nil {
		return nil, err
	}

	const q = `
		SELECT oid, created_at, sys, content_size, content_type, content_sha256
		FROM pgfs.metadata
		WHERE id = $1
	`
	e := &entry{id: id}
	err = fsys.queryRow(q, []any{id},
		&e.oid, &e.createdAt, &e.sys, &e.contentSize, &e.contentType, &e.contentSHA256,
	)
	if err == sql.ErrNoRows {
		err = fs.ErrNotExist
	}
	if err != nil {
		return nil, pathError("stat", name, err)
	}
	return e, nil
}

func (fsys *FS) rootInfo() (*entry, error) {
	const q = `
		SELECT COALESCE(MAX(created_at), NOW()), COALESCE(SUM(content_size), 0)
		FROM pgfs.metadata
	`
	e := &entry{mode: fs.ModeDir}
	err := fsys.queryRow(q, nil, &e.createdAt, &e.contentSize)
	if err != nil {
		return nil, err
	}
	return e, nil
}

// ReadFile reads the named file and returns its contents.
// It implements [fs.ReadFileFS].
func (fsys *FS) ReadFile(name string) ([]byte, error) {
	f, err := fsys.Open(name)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if info.IsDir() {
		return nil, &fs.PathError{Op: "read", Path: name, Err: fs.ErrInvalid}
	}

	data := make([]byte, info.Size())
	if _, err := io.ReadFull(f, data); err != nil {
		return nil, err
	}
	return data, nil
}

// ReadDir reads the root directory and returns all its entries,
// sorted by name. It implements [fs.ReadDirFS].
//
// Since the file system has no subdirectories, ReadDir returns
// an error matching [fs.ErrNotExist] if name is not ".".
func (fsys *FS) ReadDir(name string) ([]fs.DirEntry, error) {
	if name != "." {
		return nil, &fs.PathError{Op: "readdir", Path: name, Err: fs.ErrNotExist}
	}
	entries, err := fsys.list(nil, 0)
	if err != nil {
		return nil, pathError("readdir", name, err)
	}
	list := make([]fs.DirEntry, len(entries))
	for i, e := range entries {
		list[i] = e
	}
	return list, nil
}

// list returns up to n entries sorted by name, starting after
// the entry named after. If after is nil, the list starts with
// the first entry. If n <= 0, list returns all the entries.
func (fsys *FS) list(after *uuid.UUID, n int) ([]*entry, error) {
	const q = `
		SELECT id, oid, created_at, sys, content_size, content_type, content_sha256
		FROM pgfs.metadata
		WHERE $1::uuid IS NULL OR id > $1::uuid
		ORDER BY id
		LIMIT CASE WHEN $2 <= 0 THEN NULL ELSE $2 END
	`
	var start any
	if after != nil {
		start = *after
	}
	fsys.mu.Lock()
	defer fsys.mu.Unlock()
	rows, err := fsys.tx.Query(q, start, n)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var entries []*entry
	for rows.Next() {
		e := &entry{}
		err := rows.Scan(
			&e.id, &e.oid, &e.createdAt, &e.sys, &e.contentSize, &e.contentType, &e.contentSHA256,
		)
		if err != nil {
			return nil, err
		}
		entries = append(entries, e)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return entries, nil
}

// Create creates a new file named id and returns a writer
// for its content. If a file named id already exists, Create
// returns an error matching [fs.ErrExist].
//
// The file is recorded when the writer is closed, so the caller
// must call Close and check its error. Until then, the file does
// not appear in the file system.
//
// contentType is the MIME type of the content, such as
// "application/pdf". If it is empty, the type is detected from
// the first 512 bytes written using [http.DetectContentType].
//
// sys holds custom attributes stored with the file, and may be nil.
// They are returned by [fs.FileInfo.Sys].
func (fsys *FS) Create(id uuid.UUID, contentType string, sys Sys) (io.WriteCloser, error) {
	oid, fd, err := fsys.create(id)
	if err != nil {
		return nil, pathError("create", id.String(), err)
	}
	w := &writer{
		fsys:        fsys,
		fd:          fd,
		oid:         oid,
		id:          id,
		sys:         sys,
		contentType: contentType,
		hash:        sha256.New(),
	}
	return w, nil
}

// Remove removes the named file and its content.
// If the file does not exist, Remove returns an error
// matching [fs.ErrNotExist].
func (fsys *FS) Remove(name string) error {
	id, err := parse("remove", name)
	if err != nil {
		return err
	}
	return pathError("remove", name, fsys.remove(id))
}

var (
	_ fs.StatFS     = &FS{}
	_ fs.ReadDirFS  = &FS{}
	_ fs.ReadFileFS = &FS{}
)

// ServeFile replies to the request with the contents of f.
//
// If f was opened by an [FS], ServeFile sets the following headers
// from the file's [FileInfo]:
//
//	Content-Type: image/png
//	ETag: "0de648a9c8c19264e6cd6a441a867d0989a03929cacec442ad1f0cd192bc9072"
//	Repr-Digest: sha-256=:DeZIqcjBkmTmzWpEGoZ9CYmgOSnKzsRCrR8M0ZK8kHI=:
//
// ETag and Repr-Digest are derived from the SHA-256 digest of the
// content, which is computed when the file is created.
//
// If f implements [io.Seeker], as files opened by an FS do, it is
// served with [http.ServeContent], which handles Range and conditional
// requests, sets Last-Modified, and detects the content type if it is
// not set. Otherwise, the content of f is copied to the response,
// with [BinaryType] as its content type.
//
// If f is a directory, or if its information cannot be read,
// ServeFile replies with a 500 Internal Server Error.
func ServeFile(w http.ResponseWriter, r *http.Request, f fs.File) {
	info, err := f.Stat()
	if err != nil || info.IsDir() {
		code := http.StatusInternalServerError
		http.Error(w, http.StatusText(code), code)
		return
	}

	h := w.Header()
	if fi, ok := info.(FileInfo); ok {
		sum := fi.ContentSHA256()
		h.Set("Content-Type", fi.ContentType())
		h.Set("ETag", fmt.Sprintf("%q", hex.EncodeToString(sum)))
		h.Set("Repr-Digest", fmt.Sprintf("sha-256=:%s:", base64.StdEncoding.EncodeToString(sum)))
	}

	if rs, ok := f.(io.ReadSeeker); ok {
		http.ServeContent(w, r, info.Name(), info.ModTime(), rs)
		return
	}

	if h.Get("Content-Type") == "" {
		h.Set("Content-Type", BinaryType)
	}
	if t := info.ModTime(); !t.IsZero() {
		h.Set("Last-Modified", t.UTC().Format(http.TimeFormat))
	}
	io.Copy(w, f)
}
