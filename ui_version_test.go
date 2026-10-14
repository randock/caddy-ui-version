package uiversion

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/caddyconfig"
	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
	_ "modernc.org/sqlite"
)

func TestNormalizeHost(t *testing.T) {
	cases := map[string]string{
		"DagPlannen.nl":     "dagplannen.nl",
		"dagplannen.nl:443": "dagplannen.nl",
		"DAGPLANNEN.NL.":    "dagplannen.nl",
		"example.com:8080":  "example.com",
		"www.kas.nl":        "kas.nl",
		"WWW.Kas.co.uk.":    "kas.co.uk",
		"www.kas.nl.:8080":  "kas.nl",
		" www.kas.nl ":      "kas.nl",
		"www.nl":            "www.nl",
		"wwwkas.nl":         "wwwkas.nl",
		"":                  "",
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
		SQLite:  "file:" + dbPath + "?immutable=1&mode=ro&nolock=1",
		Default: "original",
	}
	ctx, cancel := caddy.NewContext(caddy.Context{Context: context.Background()})
	defer cancel()
	if err := m.Provision(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.Cleanup() })

	if got := m.lookup("dagplannen.nl").version; got != "modern" {
		t.Fatalf("dagplannen.nl -> %q want modern", got)
	}
	if got := m.lookup("unknown.example").version; got != "original" {
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

// TestServeHTTPConcurrentCacheMisses runs many uncached lookups at once, as
// happens after every cache clear. Run with -race; before the pointer-receiver
// fix this crashed with "concurrent map read and map write".
func TestServeHTTPConcurrentCacheMisses(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "ui-versions.sqlite")
	mustWriteSampleDB(t, dbPath)

	m := &UIVersion{
		SQLite:  "file:" + dbPath + "?immutable=1&mode=ro&nolock=1",
		Default: "original",
	}
	ctx, cancel := caddy.NewContext(caddy.Context{Context: context.Background()})
	defer cancel()
	if err := m.Provision(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.Cleanup() })

	var handler caddyhttp.MiddlewareHandler = m
	next := caddyhttp.HandlerFunc(func(http.ResponseWriter, *http.Request) error { return nil })

	var wg sync.WaitGroup
	for i := 0; i < 64; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 20; j++ {
				req := httptest.NewRequest(http.MethodGet, fmt.Sprintf("http://d%d-%d.example/", i, j), nil)
				req = req.WithContext(context.WithValue(req.Context(), caddyhttp.VarsCtxKey, make(map[string]any)))
				if err := handler.ServeHTTP(httptest.NewRecorder(), req, next); err != nil {
					t.Error(err)
					return
				}
			}
		}(i)
	}
	wg.Wait()
}

const newSchemaDB = `
CREATE TABLE "domains" (
  "name" TEXT PRIMARY KEY NOT NULL,
  "ui_version" TEXT NOT NULL,
  "locale" TEXT
);
INSERT INTO domains (name, ui_version, locale) VALUES
  ('dagplannen.nl', 'modern', NULL),
  ('kas.nl', 'original', 'nl-nl'),
  ('kas.co.uk', 'minimal', ' EN-GB '),
  ('bad.nl', 'modern', 'nl_NL'),
  ('script.nl', 'modern', 'nl-nl"><script>'),
  ('blank.nl', '', 'de-de');
`

func mustExecDB(t *testing.T, path, query string) {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(query); err != nil {
		t.Fatal(err)
	}
}

func newTestHandler(t *testing.T, dsn, key string) *UIVersion {
	t.Helper()
	m := &UIVersion{SQLite: dsn, Default: "original", Key: key}
	ctx, cancel := caddy.NewContext(caddy.Context{Context: context.Background()})
	t.Cleanup(cancel)
	if err := m.Provision(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.Cleanup() })
	return m
}

func immutableDSN(path string) string {
	return "file:" + path + "?immutable=1&mode=ro&nolock=1"
}

// serve runs one request through m and returns the vars it set; a nil repl
// leaves the replacer out of the context.
func serve(t *testing.T, m *UIVersion, host string, repl *caddy.Replacer) (version, locale any) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "http://"+host+"/", nil)
	ctx := context.WithValue(req.Context(), caddyhttp.VarsCtxKey, make(map[string]any))
	if repl != nil {
		ctx = context.WithValue(ctx, caddy.ReplacerCtxKey, repl)
	}
	next := caddyhttp.HandlerFunc(func(w http.ResponseWriter, r *http.Request) error {
		version = caddyhttp.GetVar(r.Context(), "ui_version")
		locale = caddyhttp.GetVar(r.Context(), "ui_locale")
		return nil
	})
	if err := m.ServeHTTP(httptest.NewRecorder(), req.WithContext(ctx), next); err != nil {
		t.Fatal(err)
	}
	return version, locale
}

func cacheLen(m *UIVersion) int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.cache)
}

func TestServeHTTPOldSchemaSetsEmptyLocale(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "ui-versions.sqlite")
	mustWriteSampleDB(t, dbPath)
	m := newTestHandler(t, immutableDSN(dbPath), "")

	for host, want := range map[string]string{
		"dagplannen.nl":     "modern",
		"www.dagplannen.nl": "modern",
		"unknown.example":   "original",
	} {
		version, locale := serve(t, m, host, nil)
		if version != want || locale != "" {
			t.Errorf("%s: ui_version=%#v ui_locale=%#v want %q, \"\"", host, version, locale, want)
		}
	}
}

func TestServeHTTPNewSchemaLocale(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "ui-versions.sqlite")
	mustExecDB(t, dbPath, newSchemaDB)
	m := newTestHandler(t, immutableDSN(dbPath), "")

	cases := []struct{ host, version, locale string }{
		{"kas.nl", "original", "nl-nl"},
		{"www.kas.nl:80", "original", "nl-nl"},
		{"dagplannen.nl", "modern", ""},   // NULL: use Accept-Language
		{"kas.co.uk", "minimal", "en-gb"}, // trimmed and lowercased
		{"bad.nl", "modern", ""},          // not xx-yy
		{"script.nl", "modern", ""},
		{"blank.nl", "original", "de-de"}, // empty ui_version: default
		{"unknown.example", "original", ""},
	}
	for _, c := range cases {
		version, locale := serve(t, m, c.host, nil)
		if version != c.version || locale != c.locale {
			t.Errorf("%s: ui_version=%#v ui_locale=%#v want %q, %q", c.host, version, locale, c.version, c.locale)
		}
	}
}

// TestServeHTTPKeyPlaceholder covers buy.nameshift.com/<domain>, where the
// domain comes from the path regexp instead of the Host.
func TestServeHTTPKeyPlaceholder(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "ui-versions.sqlite")
	mustExecDB(t, dbPath, newSchemaDB)
	m := newTestHandler(t, immutableDSN(dbPath), "{http.regexp.d.1}")

	repl := caddy.NewReplacer()
	repl.Set("http.regexp.d.1", "WWW.Kas.NL")
	if version, locale := serve(t, m, "buy.nameshift.com", repl); version != "original" || locale != "nl-nl" {
		t.Fatalf("key www.kas.nl: ui_version=%#v ui_locale=%#v want original, nl-nl", version, locale)
	}

	// An empty key (placeholder unset, or no replacer at all) means defaults
	// without a lookup, and never the Host.
	for _, repl := range []*caddy.Replacer{caddy.NewReplacer(), nil} {
		if version, locale := serve(t, m, "kas.nl", repl); version != "original" || locale != "" {
			t.Fatalf("empty key: ui_version=%#v ui_locale=%#v want original, \"\"", version, locale)
		}
	}
	if n := cacheLen(m); n != 1 {
		t.Fatalf("cache has %d entries want 1", n)
	}
}

func TestUnmarshalCaddyfileKey(t *testing.T) {
	d := caddyfile.NewTestDispenser(`ui_version {
		sqlite file:/ui-versions.sqlite?immutable=1&mode=ro&nolock=1
		default original
		key {re.d.1}
	}`)
	m := new(UIVersion)
	if err := m.UnmarshalCaddyfile(d); err != nil {
		t.Fatal(err)
	}
	if m.Key != "{re.d.1}" || m.Default != "original" {
		t.Fatalf("key=%q default=%q", m.Key, m.Default)
	}
}

// TestCaddyfileAdaptsKeyShorthand checks that the Caddyfile adapter expands
// {re.d.1} in the key to the placeholder path_regexp sets.
func TestCaddyfileAdaptsKeyShorthand(t *testing.T) {
	adapter := caddyconfig.GetAdapter("caddyfile")
	out, _, err := adapter.Adapt([]byte(`
{
	order ui_version before root
}

http://buy.example {
	@domain_only path_regexp d ^/([^/]+)/?$
	handle @domain_only {
		ui_version {
			sqlite file:/ui-versions.sqlite?immutable=1&mode=ro&nolock=1
			default original
			key {re.d.1}
		}
		respond "{http.vars.ui_locale}"
	}
}`), nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), `"key":"{http.regexp.d.1}"`) {
		t.Fatalf("adapted config has no expanded key:\n%s", out)
	}
}

// TestQueryErrorNotCached: a failed query (here the table is missing, as when
// the file is caught mid-replace) must not pin the default; the next request
// retries.
func TestQueryErrorNotCached(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "ui-versions.sqlite")
	mustExecDB(t, dbPath, `CREATE TABLE other (x TEXT);`)
	// Not immutable, so the pooled connection sees the table appear.
	m := newTestHandler(t, "file:"+dbPath+"?mode=ro", "")

	if version, locale := serve(t, m, "kas.nl", nil); version != "original" || locale != "" {
		t.Fatalf("no table: ui_version=%#v ui_locale=%#v want original, \"\"", version, locale)
	}
	if n := cacheLen(m); n != 0 {
		t.Fatalf("error was cached (%d entries)", n)
	}

	mustExecDB(t, dbPath, newSchemaDB)
	if version, locale := serve(t, m, "kas.nl", nil); version != "original" || locale != "nl-nl" {
		t.Fatalf("after table appeared: ui_version=%#v ui_locale=%#v want original, nl-nl", version, locale)
	}
}

// TestAtomicReplaceSwitchesSchema replaces an old-schema file with a
// new-schema one under an open pool, as the 15-minute refresh does.
func TestAtomicReplaceSwitchesSchema(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "ui-versions.sqlite")
	mustExecDB(t, dbPath, `
CREATE TABLE domains (name TEXT PRIMARY KEY NOT NULL, ui_version TEXT NOT NULL);
INSERT INTO domains (name, ui_version) VALUES ('kas.nl', 'modern');
`)
	m := newTestHandler(t, immutableDSN(dbPath), "")
	if version, locale := serve(t, m, "kas.nl", nil); version != "modern" || locale != "" {
		t.Fatalf("old file: ui_version=%#v ui_locale=%#v want modern, \"\"", version, locale)
	}

	tmpPath := filepath.Join(dir, "ui-versions.sqlite.new")
	mustExecDB(t, tmpPath, newSchemaDB)
	if err := os.Rename(tmpPath, dbPath); err != nil {
		t.Fatal(err)
	}
	// Stand-ins for the 15m cache clear and the 5m connection recycling.
	m.mu.Lock()
	m.cache = make(map[string]domainUI)
	m.mu.Unlock()
	m.db.SetMaxIdleConns(0)

	if version, locale := serve(t, m, "kas.nl", nil); version != "original" || locale != "nl-nl" {
		t.Fatalf("new file: ui_version=%#v ui_locale=%#v want original, nl-nl", version, locale)
	}
}

func TestCacheResetsWhenFull(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "ui-versions.sqlite")
	mustExecDB(t, dbPath, newSchemaDB)
	m := newTestHandler(t, immutableDSN(dbPath), "")
	m.cacheMax = 3

	for _, host := range []string{"a.example", "b.example", "kas.nl"} {
		m.lookup(host)
	}
	if n := cacheLen(m); n != 3 {
		t.Fatalf("cache has %d entries want 3", n)
	}
	m.lookup("kas.nl") // hit: no reset
	if n := cacheLen(m); n != 3 {
		t.Fatalf("cache hit changed size to %d", n)
	}
	if got := m.lookup("d.example"); got.version != "original" {
		t.Fatalf("d.example -> %+v", got)
	}
	m.mu.RLock()
	_, ok := m.cache["d.example"]
	n := len(m.cache)
	m.mu.RUnlock()
	if n != 1 || !ok {
		t.Fatalf("after overflow cache has %d entries (d.example cached: %v) want 1", n, ok)
	}
	if got := m.lookup("kas.nl"); got != (domainUI{version: "original", locale: "nl-nl"}) {
		t.Fatalf("kas.nl after reset -> %+v", got)
	}
}

// TestLongKeyNotLookedUp: with key set the lookup key comes from the request
// path, so a key longer than any DNS name must not reach the query or the cache.
func TestLongKeyNotLookedUp(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "ui-versions.sqlite")
	mustExecDB(t, dbPath, newSchemaDB)
	m := newTestHandler(t, immutableDSN(dbPath), "{http.regexp.d.1}")

	repl := caddy.NewReplacer()
	repl.Set("http.regexp.d.1", strings.Repeat("a", 296)+".kas.nl")
	if version, locale := serve(t, m, "buy.nameshift.com", repl); version != "original" || locale != "" {
		t.Fatalf("long key: ui_version=%#v ui_locale=%#v want original, \"\"", version, locale)
	}
	if n := cacheLen(m); n != 0 {
		t.Fatalf("long key was cached (%d entries)", n)
	}
}
