package uiversion

import (
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
	_ "modernc.org/sqlite"
)

func TestNormalizeHost(t *testing.T) {
	cases := map[string]string{
		"DagPlannen.nl":     "dagplannen.nl",
		"dagplannen.nl:443": "dagplannen.nl",
		"DAGPLANNEN.NL.":    "dagplannen.nl",
		"example.com:8080":  "example.com",
	}
	for in, want := range cases {
		if got := normalizeHost(in); got != want {
			t.Fatalf("normalizeHost(%q)=%q want %q", in, got, want)
		}
	}
}

func TestLookupModernAndDefault(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "ui-versions.sqlite")
	mustWriteSampleDB(t, dbPath)

	m := &UIVersion{
		SQLite:  dbPath,
		Default: "original",
	}
	ctx, cancel := caddy.NewContext(caddy.Context{Context: context.Background()})
	defer cancel()
	if err := m.Provision(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.Cleanup() })

	if got := m.lookup("dagplannen.nl"); got != "modern" {
		t.Fatalf("dagplannen.nl -> %q want modern", got)
	}
	if got := m.lookup("unknown.example"); got != "original" {
		t.Fatalf("unknown -> %q want original", got)
	}

	var saw string
	next := caddyhttp.HandlerFunc(func(w http.ResponseWriter, r *http.Request) error {
		saw, _ = caddyhttp.GetVar(r.Context(), "ui_version").(string)
		return nil
	})

	req := httptest.NewRequest(http.MethodGet, "http://dagplannen.nl/", nil)
	req = req.WithContext(context.WithValue(req.Context(), caddyhttp.VarsCtxKey, make(map[string]any)))
	if err := m.ServeHTTP(httptest.NewRecorder(), req, next); err != nil {
		t.Fatal(err)
	}
	if saw != "modern" {
		t.Fatalf("var ui_version=%q want modern", saw)
	}
}

func mustWriteSampleDB(t *testing.T, path string) {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	_, err = db.Exec(`
CREATE TABLE domains (
  name TEXT PRIMARY KEY NOT NULL,
  ui_version TEXT NOT NULL
);
INSERT INTO domains (name, ui_version) VALUES ('dagplannen.nl', 'modern');
`)
	if err != nil {
		t.Fatal(err)
	}
}
