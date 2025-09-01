package pgfs

import (
	"database/sql"
	"errors"
	"fmt"
	"io"
	"io/fs"

	"github.com/google/uuid"
)

// file modes on Postgres.
const (
	invRead  = 0x00020000
	invWrite = 0x00040000
)

// OID is the internal ID of a large object
// on Postgres.
type OID uint32

// open returns info and a file descriptor for an existing
// large object.
func open(conn Tx, id uuid.UUID, mode int) (info *entry, fd int32, err error) {
	const q = `
		SELECT 
			oid, created_at,
			content_size, content_type, content_sha256,
			lo_open(oid, $2) as fd
		FROM pgfs_metadata
		WHERE id = $1
	`
	info = &entry{id: id}
	err = conn.QueryRow(q, id, mode).Scan(
		&info.oid,
		&info.createdAt,
		&info.contentSize,
		&info.contentType,
		&info.contentSHA256,
		&fd,
	)
	switch {
	case err == sql.ErrNoRows:
		err = fs.ErrNotExist
	case err != nil:
		break
	case fd == -1:
		err = errors.New("error opening large object")
	}
	return
}

// create creates and opens a new large object for writing
// if no other object with the same name exists in the metadata
// table.
func create(conn Tx, id uuid.UUID) (oid OID, fd int32, err error) {
	const q = `
		WITH 
			meta AS (
				SELECT id
				FROM pgfs_metadata
				WHERE id = $1
			),
			lob AS (
				SELECT lo_create(0) AS oid
				WHERE NOT EXISTS (SELECT id FROM meta)
			)
		SELECT 
			(SELECT oid FROM lob) as oid,
			lo_open((SELECT oid FROM lob), $2) as fd
		WHERE EXISTS (SELECT oid FROM lob)
	`
	err = conn.QueryRow(q, id, invRead|invWrite).Scan(&oid, &fd)
	switch {
	case err == sql.ErrNoRows:
		err = fs.ErrExist
	case err != nil:
		break
	case fd == -1:
		err = fmt.Errorf("error creating large object")
	}
	return
}

// write is analog to [io.Writer], and writes b
// in the file fd.
func write(conn Tx, fd int32, b []byte) (n int, err error) {
	const q = `SELECT lowrite($1, $2)`

	err = conn.QueryRow(q, fd, b).Scan(&n)
	switch {
	case err != nil:
		break
	case n < 0:
		err = errors.New("error writing to large object")
	case n < len(b):
		err = io.ErrShortWrite
	}
	return
}

// seek is analog to [io.Seeker], and changes the read/write
// position in fd.
func seek(conn Tx, fd int32, offset int64, whence int) (n int64, err error) {
	const q = `SELECT lo_lseek64($1, $2, $3)`

	err = conn.QueryRow(q, fd, offset, whence).Scan(&n)
	switch {
	case err != nil:
		break
	case n == -1:
		err = errors.New("error seeking position in large object")
	}
	return
}

// read is analog to [io.Reader], and fills p with len(p)
// bytes from the file fd.
func read(conn Tx, fd int32, p []byte) (n int, err error) {
	const q = `SELECT loread($1, $2)`

	buf := make([]byte, 0, len(p))
	err = conn.QueryRow(q, fd, len(p)).Scan(&buf)
	if err != nil {
		return
	}
	if len(p) != len(buf) {
		err = io.EOF
	}
	n = copy(p, buf)
	return
}

// close closes the file.
func close(conn Tx, fd int32) (err error) {
	const q = `SELECT lo_close($1)`

	var result int
	err = conn.QueryRow(q, fd).Scan(&result)
	switch {
	case err != nil:
		break
	case result == -1:
		return errors.New("error closing large object")
	}
	return
}

// unlink the lo file.
func unlink(conn Tx, fd OID) (err error) {
	const q = `SELECT lo_unlink($1)`

	var result int
	err = conn.QueryRow(q, fd).Scan(&result)
	switch {
	case err != nil:
		break
	case result == -1:
		return errors.New("error unlinking large object")
	}
	return
}

// remove deletes the large object with the given
// name, along with its metadata row.
func remove(conn Tx, id uuid.UUID) (err error) {
	const q = `
		WITH meta AS (
			DELETE FROM pgfs_metadata
			WHERE id = $1
			RETURNING oid
		)
		SELECT lo_unlink((SELECT oid FROM meta))
		WHERE EXISTS(SELECT oid FROM meta)
	`

	var result int
	err = conn.QueryRow(q, id).Scan(&result)
	switch {
	case err == sql.ErrNoRows:
		err = fs.ErrNotExist
	case err != nil:
		break
	case result == -1:
		err = errors.New("error deleting large object")
	}
	return
}
