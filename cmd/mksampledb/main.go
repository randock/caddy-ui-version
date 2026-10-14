package main

import (
	"database/sql"
	"flag"
	"fmt"
	"os"

	_ "modernc.org/sqlite"
)

func main() {
	out := flag.String("o", "ui-versions.sqlite", "output sqlite path")
	flag.Parse()

	_ = os.Remove(*out)
	db, err := sql.Open("sqlite", *out)
	if err != nil {
		fatal(err)
	}
	defer db.Close()

	_, err = db.Exec(`
CREATE TABLE domains (
  name TEXT PRIMARY KEY NOT NULL,
  ui_version TEXT NOT NULL,
  locale TEXT
);
INSERT INTO domains (name, ui_version, locale) VALUES
  ('dagplannen.nl', 'modern', NULL),
  ('kas.nl', 'original', 'nl-nl');
`)
	if err != nil {
		fatal(err)
	}
	fmt.Println("wrote", *out)
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, err)
	os.Exit(1)
}
