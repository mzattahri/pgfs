package pgfs

import (
	"database/sql"
	"io"
	"io/fs"

	"uuid"
)

// Modes of large object descriptors, from libpq/libpq-fs.h.
const (
	invRead  = 0x00020000
	invWrite = 0x00040000
)

// maxRead is the maximum number of bytes read from
// a large object in a single query.
const maxRead = 1 << 20

// An OID is the object identifier PostgreSQL assigns to a large object.
type OID uint32

// The functions below run the server-side large object functions.
// These report failures as SQL errors, never as -1 like their
// counterparts in libpq.

// open opens the large object of the file named id,
// and returns the file's metadata and descriptor.
func (fsys *FS) open(id uuid.UUID, mode int) (*entry, int32, error) {
	const q = `
		SELECT oid, created_at, sys, content_size, content_type, content_sha256, lo_open(oid, $2)
		FROM pgfs.metadata
		WHERE id = $1
	`
	e := &entry{id: id}
	var fd int32
	err := fsys.queryRow(q, []any{id, mode},
		&e.oid, &e.createdAt, &e.sys, &e.contentSize, &e.contentType, &e.contentSHA256, &fd,
	)
	if err == sql.ErrNoRows {
		err = fs.ErrNotExist
	}
	return e, fd, err
}

// create creates and opens a large object for a new file
// named id, unless a file with that name already exists.
func (fsys *FS) create(id uuid.UUID) (OID, int32, error) {
	const q = `
		WITH lob AS (
			SELECT lo_create(0) AS oid
			WHERE NOT EXISTS (SELECT 1 FROM pgfs.metadata WHERE id = $1)
		)
		SELECT oid, lo_open(oid, $2) FROM lob
	`
	var (
		oid OID
		fd  int32
	)
	err := fsys.queryRow(q, []any{id, invRead | invWrite}, &oid, &fd)
	if err == sql.ErrNoRows {
		err = fs.ErrExist
	}
	return oid, fd, err
}

// remove deletes the file named id, and its large object.
func (fsys *FS) remove(id uuid.UUID) error {
	const q = `
		WITH meta AS (
			DELETE FROM pgfs.metadata WHERE id = $1 RETURNING oid
		)
		SELECT lo_unlink(oid) FROM meta
	`
	var result int
	err := fsys.queryRow(q, []any{id}, &result)
	if err == sql.ErrNoRows {
		err = fs.ErrNotExist
	}
	return err
}

// loRead reads up to len(p) bytes from the large object fd into p.
// It returns io.EOF if it reads fewer bytes than requested.
func (fsys *FS) loRead(fd int32, p []byte) (int, error) {
	const q = `SELECT loread($1, $2)`

	n := min(len(p), maxRead)
	var buf []byte
	if err := fsys.queryRow(q, []any{fd, n}, &buf); err != nil {
		return 0, err
	}
	m := copy(p, buf)
	if m < n {
		return m, io.EOF
	}
	return m, nil
}

// loGet reads up to len(p) bytes from the large object oid,
// starting at offset off, into p. Unlike loRead, it does not use
// a descriptor, and does not change its offset.
// It returns io.EOF if it reads fewer bytes than requested.
func (fsys *FS) loGet(oid OID, off int64, p []byte) (int, error) {
	const q = `SELECT lo_get($1, $2, $3)`

	n := min(len(p), maxRead)
	var buf []byte
	if err := fsys.queryRow(q, []any{oid, off, n}, &buf); err != nil {
		return 0, err
	}
	m := copy(p, buf)
	if m < n {
		return m, io.EOF
	}
	return m, nil
}

// loWrite writes p to the large object fd.
func (fsys *FS) loWrite(fd int32, p []byte) (int, error) {
	const q = `SELECT lowrite($1, $2)`

	var n int
	if err := fsys.queryRow(q, []any{fd, p}, &n); err != nil {
		return 0, err
	}
	if n < len(p) {
		return n, io.ErrShortWrite
	}
	return n, nil
}

// loSeek sets the offset of the large object fd, as [io.Seeker] does.
func (fsys *FS) loSeek(fd int32, offset int64, whence int) (int64, error) {
	const q = `SELECT lo_lseek64($1, $2, $3)`

	var n int64
	err := fsys.queryRow(q, []any{fd, offset, whence}, &n)
	return n, err
}

// loClose closes the large object descriptor fd.
func (fsys *FS) loClose(fd int32) error {
	const q = `SELECT lo_close($1)`

	var result int
	return fsys.queryRow(q, []any{fd}, &result)
}
