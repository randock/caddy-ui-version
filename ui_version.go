package uiversion

import (
	"database/sql"
	"fmt"
	"net"
	"net/http"
	"os"
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

func init() {
	caddy.RegisterModule(UIVersion{})
	httpcaddyfile.RegisterHandlerDirective("ui_version", parseCaddyfile)
}

// UIVersion looks up the Host header in a SQLite database and sets
// {http.vars.ui_version} so later directives (e.g. root) can select a site tree.
type UIVersion struct {
	// SQLite is the path to the domains database.
	SQLite string `json:"sqlite,omitempty"`

	// Default is used when the host is missing from the database.
	Default string `json:"default,omitempty"`

	db     *sql.DB
	stmt   *sql.Stmt
	logger *zap.Logger

	mu       sync.RWMutex
	cache    map[string]string
	dbPath   string
	dbMtime  time.Time
	dbOpened string
}

// CaddyModule returns the Caddy module information.
func (UIVersion) CaddyModule() caddy.ModuleInfo {
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
	m.cache = make(map[string]string)
	m.dbPath = m.SQLite
	if err := m.openDB(); err != nil {
		return err
	}
	m.logger.Info("ui_version ready",
		zap.String("sqlite", m.SQLite),
		zap.String("default", m.Default))
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

// Cleanup closes the database.
func (m *UIVersion) Cleanup() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.closeDBLocked()
}

func (m *UIVersion) openDB() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.openDBLocked()
}

func (m *UIVersion) openDBLocked() error {
	if err := m.closeDBLocked(); err != nil {
		return err
	}

	info, err := os.Stat(m.dbPath)
	if err != nil {
		return fmt.Errorf("ui_version: stat sqlite: %w", err)
	}

	// Read-only, immutable-friendly open for a file replaced atomically by publishers.
	dsn := fmt.Sprintf("file:%s?mode=ro&_pragma=query_only(1)", m.dbPath)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return fmt.Errorf("ui_version: open sqlite: %w", err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)

	if err := db.Ping(); err != nil {
		_ = db.Close()
		return fmt.Errorf("ui_version: ping sqlite: %w", err)
	}

	stmt, err := db.Prepare(`SELECT ui_version FROM domains WHERE name = ? LIMIT 1`)
	if err != nil {
		_ = db.Close()
		return fmt.Errorf("ui_version: prepare: %w", err)
	}

	m.db = db
	m.stmt = stmt
	m.dbMtime = info.ModTime()
	m.dbOpened = m.dbPath
	m.cache = make(map[string]string)
	return nil
}

func (m *UIVersion) closeDBLocked() error {
	var err error
	if m.stmt != nil {
		err = m.stmt.Close()
		m.stmt = nil
	}
	if m.db != nil {
		if cerr := m.db.Close(); cerr != nil && err == nil {
			err = cerr
		}
		m.db = nil
	}
	return err
}

func (m *UIVersion) reloadIfChanged() {
	info, err := os.Stat(m.dbPath)
	if err != nil {
		m.logger.Warn("ui_version: could not stat sqlite", zap.Error(err))
		return
	}
	m.mu.RLock()
	same := info.ModTime().Equal(m.dbMtime)
	m.mu.RUnlock()
	if same {
		return
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	if info.ModTime().Equal(m.dbMtime) {
		return
	}
	if err := m.openDBLocked(); err != nil {
		m.logger.Error("ui_version: reload failed", zap.Error(err))
		return
	}
	m.logger.Info("ui_version: reloaded sqlite", zap.Time("mtime", m.dbMtime))
}

func normalizeHost(host string) string {
	host = strings.TrimSpace(strings.ToLower(host))
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	return strings.TrimSuffix(host, ".")
}

func (m *UIVersion) lookup(host string) string {
	m.reloadIfChanged()

	m.mu.RLock()
	if v, ok := m.cache[host]; ok {
		m.mu.RUnlock()
		return v
	}
	stmt := m.stmt
	m.mu.RUnlock()

	if stmt == nil {
		return m.Default
	}

	var version string
	err := stmt.QueryRow(host).Scan(&version)
	if err == sql.ErrNoRows {
		version = m.Default
	} else if err != nil {
		m.logger.Warn("ui_version: query failed",
			zap.String("host", host),
			zap.Error(err))
		version = m.Default
	} else {
		version = strings.TrimSpace(version)
		if version == "" {
			version = m.Default
		}
	}

	m.mu.Lock()
	m.cache[host] = version
	m.mu.Unlock()
	return version
}

// ServeHTTP implements caddyhttp.MiddlewareHandler.
func (m UIVersion) ServeHTTP(w http.ResponseWriter, r *http.Request, next caddyhttp.Handler) error {
	host := normalizeHost(r.Host)
	version := m.lookup(host)
	caddyhttp.SetVar(r.Context(), "ui_version", version)
	m.logger.Debug("ui_version selected",
		zap.String("host", host),
		zap.String("ui_version", version))
	return next.ServeHTTP(w, r)
}

// UnmarshalCaddyfile implements caddyfile.Unmarshaler.
//
//	ui_version {
//	    sqlite /ui-versions.sqlite
//	    default original
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
			default:
				return d.Errf("unknown ui_version subdirective: %s", d.Val())
			}
		}
	}
	return nil
}

func parseCaddyfile(h httpcaddyfile.Helper) (caddyhttp.MiddlewareHandler, error) {
	var m UIVersion
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
