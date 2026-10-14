package uiversion

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"
	"github.com/caddyserver/caddy/v2/caddyconfig/httpcaddyfile"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
	"go.uber.org/zap"
	_ "modernc.org/sqlite"
)

const (
	defaultSQLiteOpenTimeout      = 60 * time.Second
	defaultSQLiteReconnectInterval = 5 * time.Minute
	defaultSQLiteMaxOpenConns     = 64
	defaultSQLiteMaxIdleConns     = 64
	defaultCacheClearInterval     = 15 * time.Minute
	defaultCacheMaxEntries        = 100_000

	// maxHostLen is the longest DNS name. Longer keys cannot be a domain, so
	// they are neither looked up nor cached.
	maxHostLen = 253
)

// localeRe is the only locale shape exposed; anything else in the DB is
// treated as "no locale" so a bad value can never end up in a redirect.
var localeRe = regexp.MustCompile(`^[a-z]{2}-[a-z]{2}$`)

func init() {
	caddy.RegisterModule(new(UIVersion))
	httpcaddyfile.RegisterHandlerDirective("ui_version", parseCaddyfile)
}

// UIVersion looks up the Host header (or a configured key) in a SQLite
// database and sets {http.vars.ui_version} so later directives (e.g. root) can
// select a site tree, and {http.vars.ui_locale} so they can pick a locale.
//
// Open the DB the same way as CoreDNS nameshift_context: a stable pool against
// an immutable read-only URI. Publishers replace the file atomically (mv); pool
// connections recycle every 5m so the new inode becomes visible. An in-memory
// host cache is cleared every 15m so stale mappings do not linger longer than
// that after an update.
//
// The locale column is optional and read per query (SELECT *), because after a
// mv the pool mixes connections on the old and the new file for up to 5m.
type UIVersion struct {
	// SQLite is the path / URI to the domains database. Prefer the CoreDNS
	// style URI, e.g. file:/ui-versions.sqlite?immutable=1&mode=ro&nolock=1
	SQLite string `json:"sqlite,omitempty"`

	// Default is used when the host is missing from the database.
	Default string `json:"default,omitempty"`

	// Key is the lookup key, evaluated per request with placeholders, e.g.
	// {http.regexp.d.1} for a domain taken from the path. Empty means the
	// request Host.
	Key string `json:"key,omitempty"`

	db     *sql.DB
	logger *zap.Logger

	mu       sync.RWMutex
	cache    map[string]domainUI
	cacheMax int

	cacheClearStop context.CancelFunc
}

// domainUI is what a key resolves to; locale is "" when the domain has none.
type domainUI struct {
	version string
	locale  string
}

// CaddyModule returns the Caddy module information.
func (*UIVersion) CaddyModule() caddy.ModuleInfo {
	return caddy.ModuleInfo{
		ID:  "http.handlers.ui_version",
		New: func() caddy.Module { return new(UIVersion) },
	}
}

// Provision sets up the module.
func (m *UIVersion) Provision(ctx caddy.Context) error {
	m.logger = ctx.Logger(m)
	if m.Default == "" {
		m.Default = "original"
	}
	if m.SQLite == "" {
		return fmt.Errorf("ui_version: sqlite path is required")
	}
	m.cache = make(map[string]domainUI)
	m.cacheMax = defaultCacheMaxEntries

	db, err := sql.Open("sqlite", m.SQLite)
	if err != nil {
		return fmt.Errorf("ui_version: open sqlite: %w", err)
	}
	db.SetConnMaxIdleTime(60 * time.Second)
	db.SetConnMaxLifetime(defaultSQLiteReconnectInterval)
	db.SetMaxOpenConns(defaultSQLiteMaxOpenConns)
	db.SetMaxIdleConns(defaultSQLiteMaxIdleConns)

	pingCtx, cancel := context.WithTimeout(context.Background(), defaultSQLiteOpenTimeout)
	defer cancel()
	if err := db.PingContext(pingCtx); err != nil {
		_ = db.Close()
		return fmt.Errorf("ui_version: ping sqlite: %w", err)
	}
	m.db = db

	clearCtx, clearStop := context.WithCancel(context.Background())
	m.cacheClearStop = clearStop
	go m.clearCacheLoop(clearCtx)

	m.logger.Info("ui_version ready",
		zap.String("sqlite", m.SQLite),
		zap.String("default", m.Default),
		zap.String("key", m.Key),
		zap.Duration("conn_max_lifetime", defaultSQLiteReconnectInterval),
		zap.Duration("cache_clear_interval", defaultCacheClearInterval))
	return nil
}

// Validate implements caddy.Validator.
func (m *UIVersion) Validate() error {
	if m.SQLite == "" {
		return fmt.Errorf("ui_version: sqlite path is required")
	}
	if m.Default == "" {
		return fmt.Errorf("ui_version: default is required")
	}
	return nil
}

// Cleanup closes the database and stops the cache clearer.
func (m *UIVersion) Cleanup() error {
	if m.cacheClearStop != nil {
		m.cacheClearStop()
		m.cacheClearStop = nil
	}
	if m.db != nil {
		err := m.db.Close()
		m.db = nil
		return err
	}
	return nil
}

func (m *UIVersion) clearCacheLoop(ctx context.Context) {
	ticker := time.NewTicker(defaultCacheClearInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.mu.Lock()
			m.cache = make(map[string]domainUI)
			m.mu.Unlock()
			m.logger.Info("ui_version: cleared host cache")
		}
	}
}

// normalizeHost turns a Host header or key into the form stored in the
// database: lowercase, no port, no trailing dot and no leading "www." (unless
// that would leave a bare TLD).
func normalizeHost(host string) string {
	host = strings.TrimSpace(strings.ToLower(host))
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	host = strings.TrimSuffix(host, ".")
	if rest, ok := strings.CutPrefix(host, "www."); ok && strings.Contains(rest, ".") {
		host = rest
	}
	return host
}

func normalizeLocale(locale string) string {
	locale = strings.TrimSpace(strings.ToLower(locale))
	if !localeRe.MatchString(locale) {
		return ""
	}
	return locale
}

func (m *UIVersion) lookup(host string) domainUI {
	if host == "" || len(host) > maxHostLen || m.db == nil {
		return domainUI{version: m.Default}
	}

	m.mu.RLock()
	if v, ok := m.cache[host]; ok {
		m.mu.RUnlock()
		return v
	}
	m.mu.RUnlock()

	ui, err := m.query(host)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		// Not cached: a transient error (e.g. the file being replaced) would
		// otherwise pin the default until the next cache clear.
		m.logger.Warn("ui_version: query failed",
			zap.String("host", host),
			zap.Error(err))
		return domainUI{version: m.Default}
	}
	if ui.version == "" {
		ui.version = m.Default
	}

	m.mu.Lock()
	// With key set the host comes from the request path, so anyone can mint
	// entries; start over rather than grow without bound. maxHostLen above
	// bounds the size of each entry.
	if len(m.cache) >= m.cacheMax {
		m.cache = make(map[string]domainUI)
	}
	m.cache[host] = ui
	m.mu.Unlock()
	return ui
}

// query reads the domain's row by column name, so it works whether or not the
// file on this connection has the locale column.
func (m *UIVersion) query(host string) (domainUI, error) {
	rows, err := m.db.Query(`SELECT * FROM domains WHERE name = ? LIMIT 1`, host)
	if err != nil {
		return domainUI{}, err
	}
	defer rows.Close()

	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return domainUI{}, err
		}
		return domainUI{}, sql.ErrNoRows
	}
	cols, err := rows.Columns()
	if err != nil {
		return domainUI{}, err
	}
	vals := make([]sql.NullString, len(cols))
	dest := make([]any, len(cols))
	for i := range vals {
		dest[i] = &vals[i]
	}
	if err := rows.Scan(dest...); err != nil {
		return domainUI{}, err
	}

	var ui domainUI
	for i, col := range cols {
		switch strings.ToLower(col) {
		case "ui_version":
			ui.version = strings.TrimSpace(vals[i].String)
		case "locale":
			ui.locale = normalizeLocale(vals[i].String)
		}
	}
	return ui, nil
}

// requestHost returns the normalized lookup key for r: the evaluated Key
// placeholder when set, otherwise the Host header.
func (m *UIVersion) requestHost(r *http.Request) string {
	if m.Key == "" {
		return normalizeHost(r.Host)
	}
	repl, ok := r.Context().Value(caddy.ReplacerCtxKey).(*caddy.Replacer)
	if !ok {
		// Caddy always sets one; without it request placeholders resolve to
		// "" (no lookup) while literal and global ones still work.
		repl = caddy.NewReplacer()
	}
	return normalizeHost(repl.ReplaceAll(m.Key, ""))
}

// ServeHTTP implements caddyhttp.MiddlewareHandler. It must use a pointer
// receiver: a value receiver copies mu on every request, so concurrent
// requests stop excluding each other and the shared cache map crashes the
// process with "concurrent map read and map write".
func (m *UIVersion) ServeHTTP(w http.ResponseWriter, r *http.Request, next caddyhttp.Handler) error {
	host := m.requestHost(r)
	ui := m.lookup(host)
	// Always set both, as strings: an unset var is null in expression
	// matchers, so `!= ""` would be true for it.
	caddyhttp.SetVar(r.Context(), "ui_version", ui.version)
	caddyhttp.SetVar(r.Context(), "ui_locale", ui.locale)
	m.logger.Debug("ui_version selected",
		zap.String("host", host),
		zap.String("ui_version", ui.version),
		zap.String("ui_locale", ui.locale))
	return next.ServeHTTP(w, r)
}

// UnmarshalCaddyfile implements caddyfile.Unmarshaler.
//
//	ui_version {
//	    sqlite file:/ui-versions.sqlite?immutable=1&mode=ro&nolock=1
//	    default original
//	    key {re.d.1}  # optional; default the request Host
//	}
func (m *UIVersion) UnmarshalCaddyfile(d *caddyfile.Dispenser) error {
	for d.Next() {
		for nesting := d.Nesting(); d.NextBlock(nesting); {
			switch d.Val() {
			case "sqlite":
				if !d.NextArg() {
					return d.ArgErr()
				}
				m.SQLite = d.Val()
			case "default":
				if !d.NextArg() {
					return d.ArgErr()
				}
				m.Default = d.Val()
			case "key":
				if !d.NextArg() {
					return d.ArgErr()
				}
				m.Key = d.Val()
			default:
				return d.Errf("unknown ui_version subdirective: %s", d.Val())
			}
		}
	}
	return nil
}

func parseCaddyfile(h httpcaddyfile.Helper) (caddyhttp.MiddlewareHandler, error) {
	m := new(UIVersion)
	err := m.UnmarshalCaddyfile(h.Dispenser)
	return m, err
}

// Interface guards
var (
	_ caddy.Provisioner           = (*UIVersion)(nil)
	_ caddy.Validator             = (*UIVersion)(nil)
	_ caddy.CleanerUpper          = (*UIVersion)(nil)
	_ caddyhttp.MiddlewareHandler = (*UIVersion)(nil)
	_ caddyfile.Unmarshaler       = (*UIVersion)(nil)
)
