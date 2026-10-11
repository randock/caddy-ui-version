package uiversion

import (
	"context"
	"database/sql"
	"fmt"
	"net"
	"net/http"
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
)

func init() {
	caddy.RegisterModule(new(UIVersion))
	httpcaddyfile.RegisterHandlerDirective("ui_version", parseCaddyfile)
}

// UIVersion looks up the Host header in a SQLite database and sets
// {http.vars.ui_version} so later directives (e.g. root) can select a site tree.
//
// Open the DB the same way as CoreDNS nameshift_context: a stable pool against
// an immutable read-only URI. Publishers replace the file atomically (mv); pool
// connections recycle every 5m so the new inode becomes visible. An in-memory
// host cache is cleared every 15m so stale mappings do not linger longer than
// that after an update.
type UIVersion struct {
	// SQLite is the path / URI to the domains database. Prefer the CoreDNS
	// style URI, e.g. file:/ui-versions.sqlite?immutable=1&mode=ro&nolock=1
	SQLite string `json:"sqlite,omitempty"`

	// Default is used when the host is missing from the database.
	Default string `json:"default,omitempty"`

	db     *sql.DB
	logger *zap.Logger

	mu    sync.RWMutex
	cache map[string]string

	cacheClearStop context.CancelFunc
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
	m.cache = make(map[string]string)

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
			m.cache = make(map[string]string)
			m.mu.Unlock()
			m.logger.Info("ui_version: cleared host cache")
		}
	}
}

func normalizeHost(host string) string {
	host = strings.TrimSpace(strings.ToLower(host))
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	return strings.TrimSuffix(host, ".")
}

func (m *UIVersion) lookup(host string) string {
	m.mu.RLock()
	if v, ok := m.cache[host]; ok {
		m.mu.RUnlock()
		return v
	}
	m.mu.RUnlock()

	if m.db == nil {
		return m.Default
	}

	var version string
	err := m.db.QueryRow(`SELECT ui_version FROM domains WHERE name = ? LIMIT 1`, host).Scan(&version)
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

// ServeHTTP implements caddyhttp.MiddlewareHandler. It must use a pointer
// receiver: a value receiver copies mu on every request, so concurrent
// requests stop excluding each other and the shared cache map crashes the
// process with "concurrent map read and map write".
func (m *UIVersion) ServeHTTP(w http.ResponseWriter, r *http.Request, next caddyhttp.Handler) error {
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
//	    sqlite file:/ui-versions.sqlite?immutable=1&mode=ro&nolock=1
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
