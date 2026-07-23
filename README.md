# caddy-ui-version

Caddy HTTP handler that selects a UI document-root name from a SQLite database
keyed by request host, and exposes it as `{http.vars.ui_version}`.

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

## SQLite schema

```sql
CREATE TABLE domains (
  name TEXT PRIMARY KEY NOT NULL,
  ui_version TEXT NOT NULL
);
```

`name` is the bare hostname (lowercase, no port). Missing hosts use `default`
(typically `original`).

The sample DB maps `dagplannen.nl` → `modern`.

## Update strategy

Same model as CoreDNS `nameshift_context`:

- Open a stable pool against an immutable read-only URI
- Connections recycle every 5 minutes (`SetConnMaxLifetime`)
- Publishers replace `/ui-versions.sqlite` atomically (`mv`)
- In-memory host cache is cleared every 15 minutes

## Build

```sh
xcaddy build --with github.com/randock/caddy-ui-version@v0.0.2
```

## Sample database

```sh
go run ./cmd/mksampledb -o ui-versions.sqlite
```
