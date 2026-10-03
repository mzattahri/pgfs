package pgfs

import (
	"database/sql/driver"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"time"

	"uuid"
)

// Sys holds the custom attributes of a file, as passed to [FS.Create].
// It is the type of the value returned by the Sys method of the
// [fs.FileInfo] of a file.
//
// Sys is stored as a JSON object.
type Sys map[string]string

// Scan implements [sql.Scanner]. It decodes a JSON object
// read from the database into sys. A NULL value leaves sys unchanged.
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

// Value implements [driver.Valuer]. It encodes sys as a JSON object,
// or as NULL if sys is nil.
func (sys Sys) Value() (driver.Value, error) {
	if sys == nil {
		return nil, nil
	}
	return json.Marshal(sys)
}

// A FileInfo describes a file stored in an [FS]. The [fs.FileInfo]
// values returned by [FS.Stat], [FS.ReadDir] and the Stat method of
// files opened by an FS implement it.
type FileInfo interface {
	fs.FileInfo

	// ContentSHA256 returns the SHA-256 digest of the file's content.
	ContentSHA256() []byte

	// ContentType returns the MIME type of the file's content.
	ContentType() string

	// OID returns the OID of the large object holding the
	// file's content. It is zero for the root directory.
	OID() OID
}

// dir is the root directory, as returned by [FS.Open].
type dir struct {
	fsys   *FS
	info   *entry
	after  *uuid.UUID // name of the last entry read
	closed bool
}

func (d *dir) Stat() (fs.FileInfo, error) {
	if d.closed {
		return nil, d.error("stat", fs.ErrClosed)
	}
	return d.info, nil
}

func (d *dir) Read([]byte) (int, error) {
	return 0, d.error("read", fs.ErrInvalid)
}

func (d *dir) Seek(int64, int) (int64, error) {
	return 0, d.error("seek", fs.ErrInvalid)
}

// Readdir implements [http.File]. It behaves like ReadDir,
// but returns [fs.FileInfo] values.
func (d *dir) Readdir(n int) ([]fs.FileInfo, error) {
	entries, err := d.readdir(n)
	infos := make([]fs.FileInfo, len(entries))
	for i, e := range entries {
		infos[i] = e
	}
	return infos, err
}

// ReadDir implements [fs.ReadDirFile]. Entries are sorted by name.
func (d *dir) ReadDir(n int) ([]fs.DirEntry, error) {
	entries, err := d.readdir(n)
	list := make([]fs.DirEntry, len(entries))
	for i, e := range entries {
		list[i] = e
	}
	return list, err
}

func (d *dir) readdir(n int) ([]*entry, error) {
	if d.closed {
		return nil, d.error("readdir", fs.ErrClosed)
	}
	entries, err := d.fsys.list(d.after, n)
	if err != nil {
		return nil, d.error("readdir", err)
	}
	if len(entries) > 0 {
		last := entries[len(entries)-1].id
		d.after = &last
	} else if n > 0 {
		return nil, io.EOF
	}
	return entries, nil
}

func (d *dir) Close() error {
	if d.closed {
		return d.error("close", fs.ErrClosed)
	}
	d.closed = true
	return nil
}

func (d *dir) error(op string, err error) error {
	return pathError(op, ".", err)
}

var (
	_ fs.ReadDirFile = &dir{}
	_ http.File      = &dir{}
)

// file is a file opened by [FS.Open].
type file struct {
	fsys   *FS
	fd     int32
	info   *entry
	closed bool
}

func (f *file) Stat() (fs.FileInfo, error) {
	if f.closed {
		return nil, f.error("stat", fs.ErrClosed)
	}
	return f.info, nil
}

func (f *file) Read(p []byte) (int, error) {
	if f.closed {
		return 0, f.error("read", fs.ErrClosed)
	}
	n, err := f.fsys.loRead(f.fd, p)
	return n, f.error("read", err)
}

// ReadAt implements [io.ReaderAt]. It does not use or change
// the offset used by Read and Seek, and may be called
// concurrently.
func (f *file) ReadAt(p []byte, off int64) (int, error) {
	if f.closed {
		return 0, f.error("read", fs.ErrClosed)
	}
	if off < 0 {
		return 0, f.error("readat", errors.New("negative offset"))
	}

	var n int
	for n < len(p) {
		m, err := f.fsys.loGet(f.info.oid, off+int64(n), p[n:])
		n += m
		if err != nil {
			return n, f.error("read", err)
		}
	}
	return n, nil
}

func (f *file) Seek(offset int64, whence int) (int64, error) {
	if f.closed {
		return 0, f.error("seek", fs.ErrClosed)
	}
	n, err := f.fsys.loSeek(f.fd, offset, whence)
	return n, f.error("seek", err)
}

func (f *file) Close() error {
	if f.closed {
		return f.error("close", fs.ErrClosed)
	}
	f.closed = true
	return f.error("close", f.fsys.loClose(f.fd))
}

func (f *file) error(op string, err error) error {
	return pathError(op, f.info.Name(), err)
}

var (
	_ io.ReadSeekCloser = &file{}
	_ io.ReaderAt       = &file{}
)

// entry describes a file or the root directory.
// It implements [FileInfo] and [fs.DirEntry].
type entry struct {
	id            uuid.UUID
	oid           OID
	mode          fs.FileMode
	createdAt     time.Time
	contentType   string
	contentSize   int64
	contentSHA256 []byte
	sys           Sys
}

func (e *entry) Name() string {
	if e.IsDir() {
		return "."
	}
	return e.id.String()
}

func (e *entry) Size() int64                { return e.contentSize }
func (e *entry) Mode() fs.FileMode          { return e.mode }
func (e *entry) ModTime() time.Time         { return e.createdAt }
func (e *entry) IsDir() bool                { return e.mode.IsDir() }
func (e *entry) Sys() any                   { return e.sys }
func (e *entry) Type() fs.FileMode          { return e.mode.Type() }
func (e *entry) Info() (fs.FileInfo, error) { return e, nil }
func (e *entry) ContentSHA256() []byte      { return e.contentSHA256 }
func (e *entry) ContentType() string        { return e.contentType }
func (e *entry) OID() OID                   { return e.oid }

var (
	_ FileInfo    = &entry{}
	_ fs.DirEntry = &entry{}
)
