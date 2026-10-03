package pgfs

import (
	"hash"
	"io/fs"
	"net/http"

	"uuid"
)

// sniffLen is the number of bytes used by [http.DetectContentType].
const sniffLen = 512

// writer writes the content of a new file to its large object,
// and records the file in the metadata table when closed.
type writer struct {
	fsys        *FS
	fd          int32
	oid         OID
	id          uuid.UUID
	sys         Sys
	contentType string
	size        int64
	hash        hash.Hash
	head        []byte // first bytes written, to detect the content type
	closed      bool
}

func (w *writer) Write(p []byte) (int, error) {
	if w.closed {
		return 0, w.error("write", fs.ErrClosed)
	}

	n, err := w.fsys.loWrite(w.fd, p)
	w.size += int64(n)
	w.hash.Write(p[:n])
	if w.contentType == "" && len(w.head) < sniffLen {
		w.head = append(w.head, p[:min(n, sniffLen-len(w.head))]...)
	}
	return n, w.error("write", err)
}

func (w *writer) Close() error {
	if w.closed {
		return w.error("close", fs.ErrClosed)
	}
	w.closed = true

	if err := w.fsys.loClose(w.fd); err != nil {
		return w.error("close", err)
	}

	if w.contentType == "" {
		w.contentType = http.DetectContentType(w.head)
	}

	const q = `
		INSERT INTO pgfs.metadata (oid, id, sys, content_size, content_type, content_sha256)
		VALUES ($1, $2, $3, $4, $5, $6)
	`
	err := w.fsys.exec(q, w.oid, w.id, w.sys, w.size, w.contentType, w.hash.Sum(nil))
	return w.error("close", err)
}

func (w *writer) error(op string, err error) error {
	return pathError(op, w.id.String(), err)
}
