// nolint
package pgfs_test

import (
	"database/sql"
	"io"
	"log"
	"os"

	"mz.attahri.com/code/pgfs/v2"
)

var db *sql.DB

func ExampleFS_Create() {
	tx, err := db.Begin()
	if err != nil {
		log.Fatal(err)
	}
	defer tx.Rollback()

	img, err := os.Open("testing/gopher.png")
	if err != nil {
		log.Fatal(err)
	}
	defer img.Close()

	sys := pgfs.Sys{
		"Description": "The Go Gopher",
		"Credit":      "Renee French",
		"Year":        "2009",
	}
	w, err := pgfs.New(tx).Create(pgfs.GenerateUUID(), "image/png", sys)
	if err != nil {
		log.Fatal(err)
	}

	if _, err := io.Copy(w, img); err != nil {
		log.Fatal(err)
	}
	if err := w.Close(); err != nil {
		log.Fatal(err)
	}

	if err := tx.Commit(); err != nil {
		log.Fatal(err)
	}
}
