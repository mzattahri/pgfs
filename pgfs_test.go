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
	"errors"
	"io"
	"io/fs"
	"log"
	"maps"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"
	"uuid"

	_ "github.com/jackc/pgx/v5/stdlib" // Postgres driver
	"mz.attahri.com/code/pgfs/v3"
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

	if err := pgfs.Install(tx); err != nil {
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

	if err := pgfs.Uninstall(tx); err != nil {
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

func createFile(t *testing.T, fsys *pgfs.FS, name uuid.UUID, contentType string, sys pgfs.Sys) {
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
		uuid.New().String():          true,
		".":                          true,
		"":                           false,
		"hello":                      false,
		"12345":                      false,
		uuid.New().String() + "1234": false,
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
			name        = uuid.New()
			contentType = "image/png"
			sys         = pgfs.Sys{
				"a": "1",
				"b": "2",
				"c": "3",
			}
		)
		createFile(t, fsys, name, contentType, sys)

		info, err := fsys.Stat(name.String())
		if err != nil {
			t.Fatal("error getting info on created file", err)
		}

		if info.Name() != name.String() {
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
		name := uuid.New()
		createFile(t, fsys, name, pgfs.BinaryType, nil)

		f, err := fsys.Open(name.String())
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
		name := uuid.New()
		createFile(t, fsys, name, pgfs.BinaryType, nil)

		f, err := fsys.Open(name.String())
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
		name := uuid.New()
		createFile(t, fsys, name, pgfs.BinaryType, nil)

		b, err := fsys.ReadFile(name.String())
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
		if !errors.Is(err, fs.ErrNotExist) {
			t.Fatal("expected fs.ErrNotExist", err)
		}
	})
}

func TestFSRemoveNotExist(t *testing.T) {
	withFS(t, func(fsys *pgfs.FS) {
		err := fsys.Remove(uuid.New().String())
		if !errors.Is(err, fs.ErrNotExist) {
			t.Fatal("expected fs.ErrNotExist", err)
		}
	})
}

func TestFSReaddir(t *testing.T) {
	withFS(t, func(fsys *pgfs.FS) {
		wanted := make([]string, 0)
		if result, err := fsys.ReadDir("."); err != nil {
			t.Fatal(err)
		} else {
			for _, item := range result {
				wanted = append(wanted, item.Name())
			}
		}

		const more = 100
		for i := 0; i < more; i++ {
			name := uuid.New()
			wanted = append(wanted, name.String())
			createFile(t, fsys, name, pgfs.BinaryType, nil)
		}

		slices.Sort(wanted)
		got, err := fsys.ReadDir(".")
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
		name := uuid.New()
		createFile(t, fsys, name, pgfs.BinaryType, nil)

		if err := fsys.Remove(name.String()); err != nil {
			t.Fatal(err)
		}
	})
}

func TestFSRemoveBadName(t *testing.T) {
	withFS(t, func(fsys *pgfs.FS) {
		err := fsys.Remove("bad name")
		if !errors.Is(err, fs.ErrNotExist) {
			t.Fatal("expected fs.ErrNotExit. Got", err)
		}
	})
}

func TestFSStatNotExist(t *testing.T) {
	withFS(t, func(fsys *pgfs.FS) {
		_, err := fsys.Stat(uuid.New().String())
		if !errors.Is(err, fs.ErrNotExist) {
			t.Fatal("expected fs.ErrNotExist")
		}
	})
}

func TestFSCreate(t *testing.T) {
	withFS(t, func(fsys *pgfs.FS) {
		name := uuid.New()
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

		info, err := fsys.Stat(name.String())
		if err != nil {
			t.Fatal("error getting info on created file", err)
		}

		if info.Size() != int64(len(TestBytes)) {
			t.Fatal("sizes don't match")
		}
	})
}

func TestFSCreateFileExists(t *testing.T) {
	withFS(t, func(fsys *pgfs.FS) {
		name := uuid.New()
		createFile(t, fsys, name, pgfs.BinaryType, nil)

		_, err := fsys.Create(name, pgfs.BinaryType, nil)
		if !errors.Is(err, fs.ErrExist) {
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
			name = uuid.New()
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

		f, err := fsys.Open(name.String())
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
		w, err := fsys.Create(uuid.New(), pgfs.BinaryType, nil)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write(TestBytes); err != nil {
			t.Fatal(err)
		}
		if err := w.Close(); err != nil {
			t.Fatal(err)
		}

		if _, err := w.Write(TestBytes); !errors.Is(err, fs.ErrClosed) {
			t.Fatal("expected fs.ErrClosed. Got:", err)
		}
		if err := w.Close(); !errors.Is(err, fs.ErrClosed) {
			t.Fatal("expected fs.ErrClosed. Got:", err)
		}
	})
}

func TestFSCreateEmptyContentType(t *testing.T) {
	withFS(t, func(fsys *pgfs.FS) {
		name := uuid.New()
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

		info, err := fsys.Stat(name.String())
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
		name := uuid.New()
		createFile(t, fsys, name, "application/png", nil)

		f, err := fsys.Open(name.String())
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
				"Last-Modified": info.ModTime().UTC().Format(http.TimeFormat),
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
			createFile(t, fsys, uuid.New(), pgfs.BinaryType, nil)
		}

		d, err := fsys.Open(".")
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
		root, err := fsys.Open(".")
		if err != nil {
			t.Fatal(err)
		}

		r, ok := root.(fs.ReadDirFile)
		if !ok {
			t.Fatal("root does not implement fs.ReadDirFile")
		}

		for i := 0; i < 20; i++ {
			createFile(t, fsys, uuid.New(), pgfs.BinaryType, nil)
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
		root, err := fsys.Open(".")
		if err != nil {
			t.Fatal(err)
		}

		r, ok := root.(fs.ReadDirFile)
		if !ok {
			t.Fatal("root does not implement fs.ReadDirFile")
		}

		wanted := make([]string, 0, 13)
		for i := 0; i < cap(wanted); i++ {
			id := uuid.New()
			createFile(t, fsys, id, pgfs.BinaryType, nil)
			wanted = append(wanted, id.String())
		}

		slices.Sort(wanted)
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
			createFile(t, fsys, uuid.New(), pgfs.BinaryType, nil)
		}

		seen := 0
		fs.WalkDir(fsys, ".", func(path string, d fs.DirEntry, err error) error {
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

func TestFSOpenNotExist(t *testing.T) {
	withFS(t, func(fsys *pgfs.FS) {
		if _, err := fsys.Open(uuid.New().String()); !errors.Is(err, fs.ErrNotExist) {
			t.Fatal("expected fs.ErrNotExist", err)
		}
		if _, err := fsys.ReadFile(uuid.New().String()); !errors.Is(err, fs.ErrNotExist) {
			t.Fatal("expected fs.ErrNotExist", err)
		}
	})
}

func TestFileClosed(t *testing.T) {
	withFS(t, func(fsys *pgfs.FS) {
		name := uuid.New()
		createFile(t, fsys, name, pgfs.BinaryType, nil)

		f, err := fsys.Open(name.String())
		if err != nil {
			t.Fatal(err)
		}
		if err := f.Close(); err != nil {
			t.Fatal(err)
		}

		if _, err := f.Stat(); !errors.Is(err, fs.ErrClosed) {
			t.Error("Stat: expected fs.ErrClosed", err)
		}
		if _, err := f.Read(make([]byte, 1)); !errors.Is(err, fs.ErrClosed) {
			t.Error("Read: expected fs.ErrClosed", err)
		}
		if _, err := f.(io.Seeker).Seek(0, io.SeekStart); !errors.Is(err, fs.ErrClosed) {
			t.Error("Seek: expected fs.ErrClosed", err)
		}
		if err := f.Close(); !errors.Is(err, fs.ErrClosed) {
			t.Error("Close: expected fs.ErrClosed", err)
		}
	})
}

func TestDirReadSeekClose(t *testing.T) {
	withFS(t, func(fsys *pgfs.FS) {
		d, err := fsys.Open(".")
		if err != nil {
			t.Fatal(err)
		}

		if _, err := d.Read(make([]byte, 1)); !errors.Is(err, fs.ErrInvalid) {
			t.Error("Read: expected fs.ErrInvalid", err)
		}
		if _, err := d.(io.Seeker).Seek(0, io.SeekStart); !errors.Is(err, fs.ErrInvalid) {
			t.Error("Seek: expected fs.ErrInvalid", err)
		}
		if err := d.Close(); err != nil {
			t.Fatal(err)
		}
		if err := d.Close(); !errors.Is(err, fs.ErrClosed) {
			t.Error("Close: expected fs.ErrClosed", err)
		}
	})
}

func TestDirEntryInfo(t *testing.T) {
	withFS(t, func(fsys *pgfs.FS) {
		createFile(t, fsys, uuid.New(), pgfs.BinaryType, nil)

		entries, err := fsys.ReadDir(".")
		if err != nil {
			t.Fatal(err)
		}
		if len(entries) == 0 {
			t.Fatal("expected at least one entry")
		}

		for _, e := range entries {
			info, err := e.Info()
			if err != nil {
				t.Fatal(err)
			}
			if info.Name() != e.Name() {
				t.Error("name mismatch", "Wanted", e.Name(), "Got", info.Name())
			}
			if e.Type() != info.Mode().Type() {
				t.Error("type mismatch", "Wanted", info.Mode().Type(), "Got", e.Type())
			}
		}
	})
}

func TestUninstall(t *testing.T) {
	tx, err := TestDB.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()

	if err := pgfs.Uninstall(tx); err != nil {
		t.Fatal(err)
	}

	var exists bool
	if err := tx.QueryRow("SELECT to_regclass($1) IS NOT NULL", pgfs.Table).Scan(&exists); err != nil {
		t.Fatal(err)
	}
	if exists {
		t.Fatal("table still exists after Uninstall")
	}

	if err := tx.QueryRow("SELECT EXISTS (SELECT 1 FROM pg_namespace WHERE nspname = $1)", pgfs.Schema).Scan(&exists); err != nil {
		t.Fatal(err)
	}
	if exists {
		t.Fatal("schema still exists after Uninstall")
	}
}

func TestInstallTwice(t *testing.T) {
	tx, err := TestDB.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()

	// The test database is already installed by TestMain.
	if err := pgfs.Install(tx); err != nil {
		t.Fatal(err)
	}
}

// stubFile is an [fs.File] that does not implement [io.Seeker].
type stubFile struct {
	io.Reader
	info    fs.FileInfo
	statErr error
}

func (f *stubFile) Stat() (fs.FileInfo, error) { return f.info, f.statErr }
func (f *stubFile) Close() error               { return nil }

// badSeekFile is an [fs.File] whose Seek always fails.
type badSeekFile struct{ stubFile }

func (f *badSeekFile) Seek(int64, int) (int64, error) { return 0, errors.New("seek failed") }

func TestServeFileErrors(t *testing.T) {
	gopher, err := TestFS.Open("testing/gopher.png")
	if err != nil {
		t.Fatal(err)
	}
	defer gopher.Close()

	info, err := gopher.Stat()
	if err != nil {
		t.Fatal(err)
	}

	serve := func(f fs.File) *http.Response {
		r := httptest.NewRequest(http.MethodGet, "https://example.com", nil)
		w := httptest.NewRecorder()
		pgfs.ServeFile(w, r, f)
		return w.Result()
	}

	t.Run("Stat error", func(t *testing.T) {
		resp := serve(&stubFile{statErr: errors.New("stat failed")})
		if resp.StatusCode != http.StatusInternalServerError {
			t.Fatal("Wanted", http.StatusInternalServerError, "Got", resp.StatusCode)
		}
	})

	t.Run("Directory", func(t *testing.T) {
		withFS(t, func(fsys *pgfs.FS) {
			d, err := fsys.Open(".")
			if err != nil {
				t.Fatal(err)
			}
			defer d.Close()

			resp := serve(d)
			if resp.StatusCode != http.StatusInternalServerError {
				t.Fatal("Wanted", http.StatusInternalServerError, "Got", resp.StatusCode)
			}
		})
	})

	t.Run("Seek error", func(t *testing.T) {
		resp := serve(&badSeekFile{stubFile{Reader: bytes.NewReader(TestBytes), info: info}})
		if resp.StatusCode != http.StatusInternalServerError {
			t.Fatal("Wanted", http.StatusInternalServerError, "Got", resp.StatusCode)
		}
	})

	t.Run("Not seekable", func(t *testing.T) {
		resp := serve(&stubFile{Reader: bytes.NewReader(TestBytes), info: info})
		if resp.StatusCode != http.StatusOK {
			t.Fatal("Wanted", http.StatusOK, "Got", resp.StatusCode)
		}
		if got := resp.Header.Get("Content-Type"); got != pgfs.BinaryType {
			t.Error("Content-Type Wanted:", pgfs.BinaryType, "Got:", got)
		}
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(body, TestBytes) {
			t.Error("body mismatch")
		}
	})
}

func TestFSConformance(t *testing.T) {
	withFS(t, func(fsys *pgfs.FS) {
		var names []string
		for range 3 {
			name := uuid.New()
			createFile(t, fsys, name, pgfs.BinaryType, nil)
			names = append(names, name.String())
		}

		if err := fstest.TestFS(fsys, names...); err != nil {
			t.Fatal(err)
		}
	})
}

func TestFSTxDone(t *testing.T) {
	tx, err := TestDB.Begin()
	if err != nil {
		t.Fatal(err)
	}
	fsys := pgfs.New(tx)
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}

	if _, err := fsys.Stat(uuid.New().String()); !errors.Is(err, sql.ErrTxDone) {
		t.Fatal("expected sql.ErrTxDone. Got:", err)
	}
}

func TestFSPathError(t *testing.T) {
	withFS(t, func(fsys *pgfs.FS) {
		const name = "bad name"
		_, err := fsys.Open(name)

		var pathErr *fs.PathError
		if !errors.As(err, &pathErr) {
			t.Fatal("expected *fs.PathError. Got:", err)
		}
		if pathErr.Op != "open" || pathErr.Path != name {
			t.Error("Wanted open", name, "Got", pathErr.Op, pathErr.Path)
		}
	})
}

func TestFileReadAt(t *testing.T) {
	withFS(t, func(fsys *pgfs.FS) {
		name := uuid.New()
		createFile(t, fsys, name, pgfs.BinaryType, nil)

		f, err := fsys.Open(name.String())
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		r := f.(io.ReaderAt)
		size := int64(len(TestBytes))

		t.Run("Ranges", func(t *testing.T) {
			tests := []struct {
				off     int64
				n       int
				wantErr error
			}{
				{0, 100, nil},
				{1000, 4096, nil},
				{size - 10, 10, nil},
				{size - 10, 20, io.EOF},
				{size, 10, io.EOF},
				{size + 100, 10, io.EOF},
			}
			for _, tt := range tests {
				p := make([]byte, tt.n)
				n, err := r.ReadAt(p, tt.off)
				if !errors.Is(err, tt.wantErr) {
					t.Errorf("ReadAt(%d, %d): error Wanted %v Got %v", tt.n, tt.off, tt.wantErr, err)
				}
				wanted := TestBytes[min(tt.off, size):min(tt.off+int64(tt.n), size)]
				if !bytes.Equal(p[:n], wanted) {
					t.Errorf("ReadAt(%d, %d): content mismatch, read %d bytes", tt.n, tt.off, n)
				}
			}
		})

		t.Run("Negative offset", func(t *testing.T) {
			if _, err := r.ReadAt(make([]byte, 1), -1); err == nil {
				t.Fatal("expected error")
			}
		})

		t.Run("Offset unchanged", func(t *testing.T) {
			if _, err := f.(io.Seeker).Seek(0, io.SeekStart); err != nil {
				t.Fatal(err)
			}
			if _, err := r.ReadAt(make([]byte, 100), 500); err != nil {
				t.Fatal(err)
			}
			p := make([]byte, 10)
			if _, err := io.ReadFull(f, p); err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(p, TestBytes[:10]) {
				t.Fatal("ReadAt changed the offset of Read")
			}
		})

		t.Run("Concurrent", func(t *testing.T) {
			var wg sync.WaitGroup
			for i := range 8 {
				wg.Go(func() {
					off := int64(i * 1000)
					p := make([]byte, 1000)
					n, err := r.ReadAt(p, off)
					if err != nil && err != io.EOF {
						t.Error(err)
						return
					}
					if !bytes.Equal(p[:n], TestBytes[off:off+int64(n)]) {
						t.Error("content mismatch at offset", off)
					}
				})
			}
			wg.Wait()
		})

		t.Run("SectionReader", func(t *testing.T) {
			b, err := io.ReadAll(io.NewSectionReader(r, 0, size))
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(b, TestBytes) {
				t.Fatal("content mismatch")
			}
		})
	})
}

// TestTxDone checks that every operation fails
// once the transaction has ended.
func TestTxDone(t *testing.T) {
	tx, err := TestDB.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	fsys := pgfs.New(tx)

	name := uuid.New()
	createFile(t, fsys, name, pgfs.BinaryType, nil)
	f, err := fsys.Open(name.String())
	if err != nil {
		t.Fatal(err)
	}
	d, err := fsys.Open(".")
	if err != nil {
		t.Fatal(err)
	}
	w, err := fsys.Create(uuid.New(), pgfs.BinaryType, nil)
	if err != nil {
		t.Fatal(err)
	}

	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}

	// Close comes last, since the other operations
	// of files and writers fail once they're closed.
	ops := []struct {
		name string
		fn   func() error
	}{
		{"FS.Open", func() error { _, err := fsys.Open(name.String()); return err }},
		{"FS.Open root", func() error { _, err := fsys.Open("."); return err }},
		{"FS.Stat", func() error { _, err := fsys.Stat(name.String()); return err }},
		{"FS.Stat root", func() error { _, err := fsys.Stat("."); return err }},
		{"FS.ReadDir", func() error { _, err := fsys.ReadDir("."); return err }},
		{"FS.ReadFile", func() error { _, err := fsys.ReadFile(name.String()); return err }},
		{"FS.Create", func() error { _, err := fsys.Create(uuid.New(), "", nil); return err }},
		{"FS.Remove", func() error { return fsys.Remove(name.String()) }},
		{"File.Read", func() error { _, err := f.Read(make([]byte, 1)); return err }},
		{"File.ReadAt", func() error { _, err := f.(io.ReaderAt).ReadAt(make([]byte, 1), 0); return err }},
		{"File.Seek", func() error { _, err := f.(io.Seeker).Seek(0, io.SeekStart); return err }},
		{"File.Close", func() error { return f.Close() }},
		{"Dir.ReadDir", func() error { _, err := d.(fs.ReadDirFile).ReadDir(-1); return err }},
		{"Writer.Write", func() error { _, err := w.Write(TestBytes); return err }},
		{"Writer.Close", func() error { return w.Close() }},
	}
	for _, op := range ops {
		err := op.fn()
		if !errors.Is(err, sql.ErrTxDone) {
			t.Errorf("%s: expected sql.ErrTxDone. Got: %v", op.name, err)
		}
		var pathErr *fs.PathError
		if !errors.As(err, &pathErr) {
			t.Errorf("%s: expected *fs.PathError. Got: %T", op.name, err)
		}
	}
}

// TestWriterCloseInsertFails checks that Close reports a failed
// insert, by closing two writers created for the same name.
func TestWriterCloseInsertFails(t *testing.T) {
	withFS(t, func(fsys *pgfs.FS) {
		name := uuid.New()
		w1, err := fsys.Create(name, pgfs.BinaryType, nil)
		if err != nil {
			t.Fatal(err)
		}
		w2, err := fsys.Create(name, pgfs.BinaryType, nil)
		if err != nil {
			t.Fatal(err)
		}

		if err := w1.Close(); err != nil {
			t.Fatal(err)
		}
		err = w2.Close()
		var pathErr *fs.PathError
		if !errors.As(err, &pathErr) || pathErr.Op != "close" {
			t.Fatal("expected close *fs.PathError. Got:", err)
		}
	})
}

// faultTx is a [pgfs.Tx] that replaces the queries
// containing match with replace.
type faultTx struct {
	*sql.Tx
	match, replace string
}

func (tx faultTx) Query(q string, args ...any) (*sql.Rows, error) {
	if strings.Contains(q, tx.match) {
		q = tx.replace
	}
	return tx.Tx.Query(q, args...)
}

// TestReadDirFailsMidway checks that listing the root directory
// fails, rather than returning a truncated list, when an error
// occurs after some rows were read.
func TestReadDirFailsMidway(t *testing.T) {
	// Returns two rows, then fails with a division by zero.
	const failing = `
		SELECT id, oid, created_at, sys, content_size, content_type, content_sha256
		FROM (SELECT *, row_number() OVER (ORDER BY id) AS rn FROM pgfs.metadata) m
		WHERE ($1::uuid IS NULL OR true) AND $2::int IS NOT NULL AND 1 / (3 - rn) > 0
		ORDER BY id
	`

	// Each test runs in its own transaction,
	// since the failure aborts it.
	withFaultyFS := func(t *testing.T, fn func(fsys *pgfs.FS)) {
		tx, err := TestDB.Begin()
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback()

		for range 3 {
			createFile(t, pgfs.New(tx), uuid.New(), pgfs.BinaryType, nil)
		}
		fn(pgfs.New(faultTx{Tx: tx, match: "FROM pgfs.metadata", replace: failing}))
	}

	t.Run("FS.ReadDir", func(t *testing.T) {
		withFaultyFS(t, func(fsys *pgfs.FS) {
			if entries, err := fsys.ReadDir("."); err == nil {
				t.Fatal("expected error. Got", len(entries), "entries")
			}
		})
	})

	t.Run("Dir.ReadDir", func(t *testing.T) {
		withFaultyFS(t, func(fsys *pgfs.FS) {
			d, err := fsys.Open(".")
			if err != nil {
				t.Fatal(err)
			}
			if entries, err := d.(fs.ReadDirFile).ReadDir(-1); err == nil {
				t.Fatal("expected error. Got", len(entries), "entries")
			}
		})
	})
}

func TestRootReaddir(t *testing.T) {
	withFS(t, func(fsys *pgfs.FS) {
		var wanted []string
		for range 5 {
			name := uuid.New()
			createFile(t, fsys, name, pgfs.BinaryType, nil)
			wanted = append(wanted, name.String())
		}
		slices.Sort(wanted)

		t.Run("Pages", func(t *testing.T) {
			d, err := fsys.Open(".")
			if err != nil {
				t.Fatal(err)
			}
			defer d.Close()

			var got []string
			for {
				infos, err := d.(http.File).Readdir(2)
				if err == io.EOF {
					if len(infos) != 0 {
						t.Fatal("expected no entries with io.EOF. Got:", len(infos))
					}
					break
				}
				if err != nil {
					t.Fatal(err)
				}
				if len(infos) > 2 {
					t.Fatal("expected at most 2 entries. Got:", len(infos))
				}
				for _, info := range infos {
					got = append(got, info.Name())
				}
			}
			if !slices.Equal(got, wanted) {
				t.Fatal("Wanted", wanted, "Got", got)
			}
		})

		t.Run("All", func(t *testing.T) {
			d, err := fsys.Open(".")
			if err != nil {
				t.Fatal(err)
			}
			defer d.Close()

			infos, err := d.(http.File).Readdir(-1)
			if err != nil {
				t.Fatal(err)
			}
			var got []string
			for _, info := range infos {
				got = append(got, info.Name())
			}
			if !slices.Equal(got, wanted) {
				t.Fatal("Wanted", wanted, "Got", got)
			}

			// Reading all entries again returns none, without error.
			infos, err = d.(http.File).Readdir(-1)
			if err != nil || len(infos) != 0 {
				t.Fatal("expected no entries and no error. Got:", len(infos), err)
			}
		})
	})
}
