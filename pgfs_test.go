// nolint
package pgfs_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"embed"
	"encoding/base64"
	"encoding/hex"
	"io"
	"io/fs"
	"log"
	"maps"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib" // Postgres driver
	"mz.attahri.com/code/pgfs/v2"
)

var TestDB *sql.DB

//go:embed testing
var TestFS embed.FS

//go:embed testing/gopher.png
var TestBytes []byte

// TestBytesSHA256 is the SHA-256 of the test bytes.
var TestBytesSHA256 []byte

func init() {
	digest := sha256.Sum256(TestBytes)
	TestBytesSHA256 = digest[:sha256.Size]
}

func connect(url string) (*sql.DB, error) {
	var db *sql.DB

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	log.Printf("Connecting to database: %s", url)
	db, err := sql.Open("pgx", url)
	if err != nil {
		return nil, err
	}

	go func() {
		var (
			interval = 2 * time.Second
			retries  int
		)
		for ctx.Err() == nil {
			if err := db.Ping(); err == nil {
				cancel()
				break
			}
			retries++
			log.Printf("(#%d) database not accessible. Retrying in %s...", retries, interval.String())
			time.Sleep(interval)
		}
	}()

	<-ctx.Done()
	if err := ctx.Err(); err != context.Canceled {
		log.Fatalf("unable to connect to database: %v", err)
	}
	return db, nil
}

func migrate(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if err := pgfs.MigrateUp(tx); err != nil {
		return err
	}

	return tx.Commit()
}

func reset(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if err := pgfs.MigrateDown(tx); err != nil {
		return err
	}

	return tx.Commit()
}

func withFS(t *testing.T, fn func(fsys *pgfs.FS)) {
	t.Helper()

	tx, err := TestDB.Begin()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := tx.Rollback(); err != nil && err != sql.ErrTxDone {
			t.Log(err)
		}
	})

	fn(pgfs.New(tx))
}

func createFile(t *testing.T, fsys *pgfs.FS, name, contentType string, sys pgfs.Sys) {
	t.Helper()

	w, err := fsys.Create(name, contentType, sys)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(TestBytes); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestValidPath(t *testing.T) {
	testCases := map[string]bool{
		pgfs.GenerateUUID():          true,
		"":                           true,
		"hello":                      false,
		"12345":                      false,
		pgfs.GenerateUUID() + "1234": false,
	}

	for name, wanted := range testCases {
		if got := pgfs.ValidPath(name); wanted != got {
			t.Error("Name:", name, "Wanted:", wanted, "Got:", got)
		}
	}
}

func TestFSStat(t *testing.T) {
	withFS(t, func(fsys *pgfs.FS) {
		var (
			name        = pgfs.GenerateUUID()
			contentType = "image/png"
			sys         = pgfs.Sys{
				"a": "1",
				"b": "2",
				"c": "3",
			}
		)
		createFile(t, fsys, name, contentType, sys)

		info, err := fsys.Stat(name)
		if err != nil {
			t.Fatal("error getting info on created file", err)
		}

		if info.Name() != name {
			t.Error("names don't match. Wanted:", name, "Got:", info.Name())
		}
		if info.Size() != int64(len(TestBytes)) {
			t.Error("sizes don't match. Wanted:", len(TestBytes), "Got:", info.Size())
		}
		if info.ModTime().IsZero() {
			t.Error("time is zero")
		}
		if info.IsDir() {
			t.Error("file is not a dir")
		}
		if !info.Mode().IsRegular() {
			t.Error("file should be regular")
		}

		fi, ok := info.(pgfs.FileInfo)
		if !ok {
			t.Fatal("info.Sys is not of type *Sys")
		}

		m, ok := fi.Sys().(pgfs.Sys)
		if !ok {
			t.Error("not of type Sys")
		}
		if !maps.Equal(m, sys) {
			t.Error("sys doesn't match")
		}

		if fi.ContentType() != contentType {
			t.Error("content types don't match. Wanted", contentType, "Got", fi.ContentType())
		}
		if fi.OID() == 0 {
			t.Error("OID should not be nil")
		}
		if !bytes.Equal(fi.ContentSHA256(), TestBytesSHA256) {
			t.Error("SHA256 digests don't match")
		}
	})
}

func TestFileRead(t *testing.T) {
	withFS(t, func(fsys *pgfs.FS) {
		name := pgfs.GenerateUUID()
		createFile(t, fsys, name, pgfs.BinaryType, nil)

		f, err := fsys.Open(name)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { f.Close() })

		b, err := io.ReadAll(f)
		if err != nil {
			t.Fatal(err)
		}

		if !bytes.Equal(b, TestBytes) {
			t.Log(string(b), string(TestBytes))
			t.Fatal("bytes don't match")
		}
	})
}

func TestFileSeek(t *testing.T) {
	withFS(t, func(fsys *pgfs.FS) {
		name := pgfs.GenerateUUID()
		createFile(t, fsys, name, pgfs.BinaryType, nil)

		f, err := fsys.Open(name)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { f.Close() })

		info, err := f.Stat()
		if err != nil {
			t.Fatal(err)
		}

		seeker, ok := f.(io.Seeker)
		if !ok {
			t.Fatal("file is not an io.Seeker")
		}

		pos, err := seeker.Seek(0, io.SeekCurrent)
		if err != nil {
			t.Fatal(err)
		}
		if pos != 0 {
			t.Fatal("wrong position. Wanted 0. Got", pos)
		}

		pos, err = seeker.Seek(0, io.SeekEnd)
		if err != nil {
			t.Fatal(err)
		}
		if pos != info.Size() {
			t.Fatal("wrong position. Wanted:", info.Size(), "Got:", pos)
		}

		val := int64(math.Ceil(float64(info.Size()) / 2))
		pos, err = seeker.Seek(-val, io.SeekCurrent)
		if err != nil {
			t.Fatal(err)
		}
		if wanted := info.Size() - val; pos != wanted {
			t.Fatal("wrong position. Wanted:", wanted, "Got:", pos)
		}

		p, err := io.ReadAll(f)
		if err != nil {
			t.Fatal(err)
		}
		if wanted := info.Size() - val; int64(len(p)) != wanted {
			t.Fatal("wrong amount of data read. Wanted:", wanted, "Got:", len(p))
		}
	})
}

func TestReadFile(t *testing.T) {
	withFS(t, func(fsys *pgfs.FS) {
		name := pgfs.GenerateUUID()
		createFile(t, fsys, name, pgfs.BinaryType, nil)

		b, err := fsys.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}

		if !bytes.Equal(b, TestBytes) {
			t.Fatal("bytes don't match")
		}
	})
}

func TestFSOpenBadName(t *testing.T) {
	withFS(t, func(fsys *pgfs.FS) {
		_, err := fsys.Open("bad name")
		if err != fs.ErrNotExist {
			t.Fatal("expected fs.ErrNotExist", err)
		}
	})
}

func TestFSRemoveNotExist(t *testing.T) {
	withFS(t, func(fsys *pgfs.FS) {
		err := fsys.Remove(pgfs.GenerateUUID())
		if err != fs.ErrNotExist {
			t.Fatal("expected fs.ErrNotExist", err)
		}
	})
}

func TestFSReaddir(t *testing.T) {
	withFS(t, func(fsys *pgfs.FS) {
		wanted := make([]string, 0)
		if result, err := fsys.ReadDir(""); err != nil {
			t.Fatal(err)
		} else {
			for _, item := range result {
				wanted = append(wanted, item.Name())
			}
		}

		const more = 100
		for i := 0; i < more; i++ {
			name := pgfs.GenerateUUID()
			wanted = append(wanted, name)
			createFile(t, fsys, name, pgfs.BinaryType, nil)
		}

		got, err := fsys.ReadDir("")
		if err != nil {
			t.Fatal(err)
		}

		if len(got) != len(wanted) {
			t.Fatal("number of files don't match", "Wanted", len(wanted), "Got", len(got))
		}

		for i, item := range got {
			if item.Name() != wanted[i] {
				t.Fatal("item", i, "don't match", "Wanted", wanted[i], "Got", item.Name())
			}
		}
	})
}

func TestFSRemove(t *testing.T) {
	withFS(t, func(fsys *pgfs.FS) {
		name := pgfs.GenerateUUID()
		createFile(t, fsys, name, pgfs.BinaryType, nil)

		if err := fsys.Remove(name); err != nil {
			t.Fatal(err)
		}
	})
}

func TestFSRemoveBadName(t *testing.T) {
	withFS(t, func(fsys *pgfs.FS) {
		err := fsys.Remove("bad name")
		if err != fs.ErrNotExist {
			t.Fatal("expected fs.ErrNotExit. Got", err)
		}
	})
}

func TestFSStatNotExist(t *testing.T) {
	withFS(t, func(fsys *pgfs.FS) {
		_, err := fsys.Stat(pgfs.GenerateUUID())
		if err != fs.ErrNotExist {
			t.Fatal("expected fs.ErrNotExist")
		}
	})
}

func TestFSCreate(t *testing.T) {
	withFS(t, func(fsys *pgfs.FS) {
		name := pgfs.GenerateUUID()
		contentType := "application/pdf"
		w, err := fsys.Create(name, contentType, nil)
		if err != nil {
			t.Fatal(err)
		}

		n, err := w.Write(TestBytes)
		if err != nil {
			t.Fatal(err)
		}
		if wanted := len(TestBytes); n != wanted {
			t.Fatalf("short write. Wanted: %d. Got: %d", wanted, n)
		}

		if err := w.Close(); err != nil {
			t.Fatalf("error closing writer: %v", err)
		}

		info, err := fsys.Stat(name)
		if err != nil {
			t.Fatal("error getting info on created file", err)
		}

		if info.Size() != int64(len(TestBytes)) {
			t.Fatal("sizes don't match")
		}
	})
}
func TestFSCreateBadName(t *testing.T) {
	withFS(t, func(fsys *pgfs.FS) {
		_, err := fsys.Create("bad name", "", nil)
		if _, ok := err.(*fs.PathError); !ok {
			t.Fatal("expected path error")
		}
	})
}

func TestFSCreateFileExists(t *testing.T) {
	withFS(t, func(fsys *pgfs.FS) {
		name := pgfs.GenerateUUID()
		createFile(t, fsys, name, pgfs.BinaryType, nil)

		_, err := fsys.Create(name, pgfs.BinaryType, nil)
		if err != fs.ErrExist {
			t.Fatal("expected fs.ErrExist. Got", err)
		}
	})
}

// loopingReader is an [io.Reader]
// that loops over the same source
// of data without ever returning
// io.EOF.
//
// Useful because reading from crypto/rand
// would be too resource intensive for tests.
type loopingReader struct {
	src []byte
	cur int
}

// Read implements [io.Reader].
func (r *loopingReader) Read(p []byte) (n int, err error) {
	for n < len(p) {
		max := len(p) - n
		if (r.cur + max) > len(r.src) {
			max = len(r.src) - r.cur
		}
		n += copy(p[n:n+max], r.src[r.cur:r.cur+max])
		r.cur = (r.cur + n) % len(r.src)
	}
	return
}

// Test consists of two steps:
//
// (1) Writing a large 100Mb file into the database
// while computing its sha256 hash;
// (2) Reading it back from the database while
// computing another hash that can be compared
// with the first one.
func TestFSCreateLargeFile(t *testing.T) {
	withFS(t, func(fsys *pgfs.FS) {
		var (
			name = pgfs.GenerateUUID()
			h    = sha256.New()
		)

		w, err := fsys.Create(name, pgfs.BinaryType, nil)
		if err != nil {
			t.Fatal(err)
		}

		mw := io.MultiWriter(h, w)
		written, err := io.Copy(mw, io.LimitReader(&loopingReader{src: TestBytes}, 100*1024<<10)) // 100MB
		if err != nil {
			t.Fatal(err)
		}
		if err := w.Close(); err != nil {
			t.Fatal(err)
		}
		wDigest := h.Sum(nil)
		h.Reset()

		f, err := fsys.Open(name)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { f.Close() })

		read, err := io.Copy(h, f)
		if err != nil {
			t.Fatal(err)
		}
		if err := f.Close(); err != nil {
			t.Fatal(err)
		}
		rDigest := h.Sum(nil)

		if written != read {
			t.Fatal("Bytes written", written, "Bytes read:", read)
		}

		if !bytes.Equal(wDigest, rDigest) {
			t.Fatal("checksums don't match")
		}
	})
}

func TestFSCreateWriteClosedFile(t *testing.T) {
	withFS(t, func(fsys *pgfs.FS) {
		w, err := fsys.Create(pgfs.GenerateUUID(), pgfs.BinaryType, nil)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write(TestBytes); err != nil {
			t.Fatal(err)
		}
		if err := w.Close(); err != nil {
			t.Fatal(err)
		}

		if _, err := w.Write(TestBytes); err != fs.ErrClosed {
			t.Fatal("expected fs.ErrClosed. Got:", err)
		}
		if err := w.Close(); err != nil {
			t.Fatal("expected nil. Got:", err)
		}
	})
}

func TestFSCreateEmptyContentType(t *testing.T) {
	withFS(t, func(fsys *pgfs.FS) {
		name := pgfs.GenerateUUID()
		w, err := fsys.Create(name, "", nil)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write(TestBytes); err != nil {
			t.Fatal(err)
		}
		if err := w.Close(); err != nil {
			t.Fatal(err)
		}

		const wanted = "image/png"

		info, err := fsys.Stat(name)
		if err != nil {
			t.Fatal(err)
		}

		got := info.(pgfs.FileInfo).ContentType()
		if wanted != got {
			t.Fatal("Wanted:", wanted, "Got:", got)
		}
	})
}

func TestHTTPHandler(t *testing.T) {
	withFS(t, func(fsys *pgfs.FS) {
		name := pgfs.GenerateUUID()
		createFile(t, fsys, name, "application/png", nil)

		f, err := fsys.Open(name)
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()

		fi, err := f.Stat()
		if err != nil {
			t.Fatal(err)
		}
		info := fi.(pgfs.FileInfo)

		assertFn := func(t *testing.T, resp *http.Response) {
			tests := map[string]string{
				"Content-Type":  info.ContentType(),
				"Last-Modified": info.ModTime().Format(http.TimeFormat),
				"Repr-Digest":   "sha-256=:" + base64.StdEncoding.EncodeToString(info.ContentSHA256()) + ":",
				"ETag":          "\"" + hex.EncodeToString(info.ContentSHA256()) + "\"",
			}
			for name, wanted := range tests {
				got := resp.Header.Get(name)
				if wanted != got {
					t.Error("header", name, "Wanted", wanted, "Got", got)
				}
			}
		}

		t.Run("Serve File handler", func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "https://example.com", nil)
			w := httptest.NewRecorder()
			pgfs.ServeFile(w, r, f)
			resp := w.Result()
			assertFn(t, resp)
		})
	})
}

func TestServeFile(t *testing.T) {
	// scenario for *file is covered in TestHTTPHandler.

	f, err := TestFS.Open("testing/gopher.png")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	r := httptest.NewRequest(http.MethodGet, "https://example.com", nil)
	w := httptest.NewRecorder()
	pgfs.ServeFile(w, r, f)
	resp := w.Result()

	if resp.StatusCode != http.StatusOK {
		t.Fatal(resp.StatusCode)
	}

	const wanted = "image/png"
	if got := resp.Header.Get("Content-Type"); got != wanted {
		t.Fatal("Content-Type Wanted:", wanted, "Got:", got)
	}
}

func TestOpenRoot(t *testing.T) {
	withFS(t, func(fsys *pgfs.FS) {
		for i := 0; i < 100; i++ {
			createFile(t, fsys, pgfs.GenerateUUID(), pgfs.BinaryType, nil)
		}

		d, err := fsys.Open("")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { d.Close() })

		info, err := d.Stat()
		if err != nil {
			t.Fatal(err)
		}

		if !info.IsDir() {
			t.Error("info is not for a dir")
		}

		if info.Mode() != fs.ModeDir {
			t.Error("mode is not fs.ModeDir")
		}

		if info.ModTime().IsZero() {
			t.Error("invalid mod time")
		}

		if wanted := 100 * len(TestBytes); info.Size() < int64(wanted) {
			t.Error("size is lower than expected", "Got", info.Size(), "Wanted >=", wanted)
		}
	})
}

// Test strategy:
//
// Get the list of all the files available
// with ReadDir, then deleted them all.
// If calling ReadDir again yields no files,
// it means that all the files available
// were returned.
func TestRootReadDir(t *testing.T) {
	withFS(t, func(fsys *pgfs.FS) {
		root, err := fsys.Open("")
		if err != nil {
			t.Fatal(err)
		}

		r, ok := root.(fs.ReadDirFile)
		if !ok {
			t.Fatal("root does not implement fs.ReadDirFile")
		}

		for i := 0; i < 20; i++ {
			createFile(t, fsys, pgfs.GenerateUUID(), pgfs.BinaryType, nil)
		}

		all := make([]fs.DirEntry, 0)
		for {
			entries, err := r.ReadDir(10)
			all = append(all, entries...)
			if err == io.EOF {
				break
			}
			if err != nil {
				t.Fatal(err)
			}
		}

		for _, e := range all {
			if err := fsys.Remove(e.Name()); err != nil {
				t.Fatal(err)
			}
		}

		entries, err := r.ReadDir(10)
		if err != io.EOF {
			t.Fatal("expected io.EOF. Got:", err)
		}
		if len(entries) != 0 {
			t.Fatal("expected 0 files in the fsys. Got:", len(entries))
		}
	})
}

func TestRootReadDirAll(t *testing.T) {
	withFS(t, func(fsys *pgfs.FS) {
		root, err := fsys.Open("")
		if err != nil {
			t.Fatal(err)
		}

		r, ok := root.(fs.ReadDirFile)
		if !ok {
			t.Fatal("root does not implement fs.ReadDirFile")
		}

		wanted := make([]string, 0, 13)
		for i := 0; i < cap(wanted); i++ {
			id := pgfs.GenerateUUID()
			createFile(t, fsys, id, pgfs.BinaryType, nil)
			wanted = append(wanted, id)
		}

		entries, err := r.ReadDir(-1)
		if err != nil {
			t.Fatal(err)
		}
		if len(entries) != len(wanted) {
			t.Fatal("Expected", len(wanted), " items. Got:", len(entries))
		}

		for i, e := range entries {
			if e.Name() != wanted[i] {
				t.Fatal("item mismatch. Index:", i, "Wanted:", wanted[i], "Got:", e.Name())
			}
		}
	})
}

func TestWalkFunc(t *testing.T) {
	withFS(t, func(fsys *pgfs.FS) {
		for i := 0; i < 100; i++ {
			createFile(t, fsys, pgfs.GenerateUUID(), pgfs.BinaryType, nil)
		}

		seen := 0
		fs.WalkDir(fsys, "", func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				t.Fatal(err)
			}
			seen++
			return nil
		})

		if seen < 100 {
			t.Fatal("saw fewer files than expected")
		}
	})
}

func TestMain(m *testing.M) {
	connURL := os.Getenv("POSTGRES_URL")
	if connURL == "" {
		log.Fatal("POSTGRES_URL env variable is missing or empty")
	}

	var err error
	TestDB, err = connect(connURL)
	if err != nil {
		log.Fatal(err)
	}
	defer TestDB.Close()

	if err := migrate(TestDB); err != nil {
		log.Fatal(err)
	}
	code := m.Run()
	if err := reset(TestDB); err != nil {
		log.Fatal(err)
	}

	os.Exit(code)
}
