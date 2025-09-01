package pgfs

import (
	"database/sql"
	"database/sql/driver"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"time"

	"github.com/google/uuid"
)

// Sys is the type returned by [fs.FileInfo.Sys],
// and holds the metadata passed with [FS.Create].
type Sys map[string]string

// Scan implements [sql.Scanner], so
// sys can be populated from the content
// of a JSONB column.
func (sys *Sys) Scan(data any) error {
	if data == nil {
		return nil
	}

	if sys == nil {
		return nil
	}

	b, ok := data.([]byte)
	if !ok {
		return fmt.Errorf("cannot cast data as []byte")
	}
	return json.Unmarshal(b, sys)
}

// Value implements [driver.Valuer] so sys
// can be stored as a JSONB column.
func (sys Sys) Value() (driver.Value, error) {
	if sys == nil {
		return nil, nil
	}
	return json.Marshal(sys)
}

// FileInfo extends [fs.FileInfo] to include metadata
// about the object in the database. It's the
// interface returned by [FS.Stat].
type FileInfo interface {
	fs.FileInfo

	// SHA-256 digest of the object's content.
	ContentSHA256() []byte

	// MIME type of the object's content.
	ContentType() string

	// OID of the object in the database.
	OID() OID
}

// dir is the [fs.File] of the root directory.
// It implements [http.File] and [fs.ReadDirFile].
type dir struct {
	fsys   *FS
	cur    int
	info   *entry
	closed bool
}

func (d *dir) Read(p []byte) (int, error)                   { return 0, fs.ErrInvalid }
func (d *dir) Seek(offset int64, whence int) (int64, error) { return 0, fs.ErrInvalid }

// Close implements [http.File].
func (d *dir) Close() error {
	if d.closed {
		return fs.ErrClosed
	}
	d.closed = true
	return nil
}

// Stat implements [http.File].
func (d *dir) Stat() (fs.FileInfo, error) {
	return d.info, nil
}

// Readdir implements [http.File].
func (d *dir) Readdir(n int) (entries []fs.FileInfo, err error) {
	const q = `
	  SELECT 
			id, oid, created_at, sys,
			content_size, content_type, content_sha256
	  FROM pgfs_metadata
	  ORDER BY created_at ASC
	  OFFSET $1
	  LIMIT CASE WHEN $2 <= 0 THEN NULL ELSE $2 END
	`
	var rows *sql.Rows
	rows, err = d.fsys.conn.Query(q, d.cur, n)
	if err != nil {
		return
	}

	defer rows.Close()
	for rows.Next() {
		e := &entry{
			mode: 0,
		}
		err = rows.Scan(
			&e.id,
			&e.oid,
			&e.createdAt,
			&e.sys,
			&e.contentSize,
			&e.contentType,
			&e.contentSHA256,
		)
		if err != nil {
			return
		}
		entries = append(entries, e)
		d.cur++
	}

	if n > 0 && len(entries) < n {
		err = io.EOF
	}
	return
}

// ReadDir implements [fs.ReadDirFile].
func (d *dir) ReadDir(n int) ([]fs.DirEntry, error) {
	entries, err := d.Readdir(n)
	all := make([]fs.DirEntry, len(entries))
	for i := range entries {
		all[i] = entries[i].(fs.DirEntry)
	}
	return all, err
}

var _ fs.File = &dir{}
var _ http.File = &dir{}
var _ fs.ReadDirFile = &dir{}

// entry implements [fs.FileInfo] and [fs.DirEntry]
type entry struct {
	oid           OID
	id            uuid.UUID
	createdAt     time.Time
	mode          fs.FileMode
	contentType   string
	contentSize   int64
	contentSHA256 []byte
	sys           Sys
}

func (e *entry) Info() (fs.FileInfo, error) { return e, nil }
func (e *entry) Type() fs.FileMode          { return e.Mode() }
func (e *entry) Name() string               { return e.id.String() }
func (e *entry) Size() int64                { return e.contentSize }
func (e *entry) ModTime() time.Time         { return e.createdAt }
func (e *entry) IsDir() bool                { return e.mode.IsDir() }
func (e *entry) Mode() fs.FileMode          { return e.mode }
func (e *entry) Sys() any                   { return e.sys }
func (e *entry) ContentSHA256() []byte      { return e.contentSHA256 }
func (e *entry) ContentType() string        { return e.contentType }
func (e *entry) OID() OID                   { return e.oid }

var _ FileInfo = &entry{}
var _ fs.DirEntry = &entry{}

// file implements [http.File] and [http.Handler].
type file struct {
	fsys   *FS
	fd     int32
	info   *entry
	closed bool
}

// ServeHTTP implements [http.Handler].
func (f *file) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", f.info.contentType)
	w.Header().Set("ETag", fmt.Sprintf(`"%s"`, hex.EncodeToString(f.info.contentSHA256)))
	w.Header().Set("Last-Modified", f.info.createdAt.Format(http.TimeFormat))
	w.Header().Set("Repr-Digest", fmt.Sprintf("sha-256=:%s:", base64.StdEncoding.EncodeToString(f.info.contentSHA256)))
	http.ServeContent(w, r, f.info.id.String(), f.info.createdAt, f)
}

func (f *file) Stat() (fs.FileInfo, error) {
	if f.closed {
		return nil, fs.ErrClosed
	}
	return f.info, nil
}

func (f *file) Read(p []byte) (int, error) {
	if f.closed {
		return 0, fs.ErrClosed
	}
	return read(f.fsys.conn, f.fd, p)
}

func (f *file) Seek(offset int64, whence int) (n int64, err error) {
	if f.closed {
		return 0, fs.ErrClosed
	}

	n, err = seek(f.fsys.conn, f.fd, offset, whence)
	if err != nil {
		return
	}
	return
}

func (f *file) Close() error {
	if f.closed {
		return nil
	}
	defer func() {
		f.closed = true
	}()
	return close(f.fsys.conn, f.fd)
}

var _ fs.File = &file{}
