# caddy-ui-version

Caddy HTTP handler that selects a UI document-root name from a SQLite database
keyed by request host, and exposes it as `{http.vars.ui_version}`. It also
exposes the domain's locale, if it has one, as `{http.vars.ui_locale}`.

## Caddyfile

```caddyfile
{
	order ui_version before root
}

:80 {
	ui_version {
		sqlite file:/ui-versions.sqlite?immutable=1&mode=ro&nolock=1
		default original
	}

	root * /{http.vars.ui_version}
	file_server
}
```

- `sqlite` (required): path or URI of the database.
- `default` (required): `ui_version` for a domain without a row.
- `key` (optional): the lookup key, evaluated per request with placeholders.
  Defaults to the request Host. Use it when the domain is not the Host, e.g.
  taken from the path:

  ```caddyfile
  @domain_only path_regexp d ^/([^/]+)/?$
  handle @domain_only {
  	ui_version {
  		sqlite file:/ui-versions.sqlite?immutable=1&mode=ro&nolock=1
  		default original
  		key {re.d.1}
  	}
  	respond "{re.d.1}: {http.vars.ui_version} {http.vars.ui_locale}"
  }
  ```

  The handler must run after the matcher that sets the placeholder, so put it
  inside the `handle` (`{re.d.1}` is shorthand for `{http.regexp.d.1}`).

The key (Host or `key`) is trimmed, lowercased and stripped of a port, a
trailing dot and a leading `www.` (unless only a TLD would remain). An empty
key skips the lookup and uses the defaults.

## Variables

Both are always set, as strings:

- `{http.vars.ui_version}`: the row's `ui_version`, or `default` when there is
  no row or it is empty.
- `{http.vars.ui_locale}`: the row's `locale`, lowercased, or `""` when there
  is no row, no `locale` column, the value is NULL, or it is not of the form
  `xx-yy` (e.g. `nl-nl`). Treat `""` as "no preference" (e.g. fall back to
  Accept-Language).

## SQLite schema

```sql
CREATE TABLE domains (
  name TEXT PRIMARY KEY NOT NULL,
  ui_version TEXT NOT NULL,
  locale TEXT
);
```

`name` is the bare hostname (lowercase, no port, no `www.`). Missing hosts use
`default` (typically `original`).

`locale` is optional: databases without the column (as read by v0.0.3 and
earlier) still work and give an empty `ui_locale`. Columns are read by name on
each query, so replacing the file with one that adds or drops the column needs
no restart.

The sample DB maps `dagplannen.nl` → `modern` (no locale) and `kas.nl` →
`original` with locale `nl-nl`.

## Update strategy

Same model as CoreDNS `nameshift_context`:

- Open a stable pool against an immutable read-only URI
- Connections recycle every 5 minutes (`SetConnMaxLifetime`)
- Publishers replace `/ui-versions.sqlite` atomically (`mv`)
- In-memory host cache is cleared every 15 minutes, and reset when it reaches
  100,000 entries; keys longer than a DNS name (253 bytes) are neither looked
  up nor cached
- Query errors (e.g. a file caught mid-replace) are not cached, so the next
  request retries

## Build

```sh
xcaddy build --with github.com/randock/caddy-ui-version@v0.0.4
```

## Sample database

```sh
go run ./cmd/mksampledb -o ui-versions.sqlite
```
