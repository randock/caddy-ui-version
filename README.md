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
		sqlite /ui-versions.sqlite
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

## Build

```sh
xcaddy build --with github.com/randock/caddy-ui-version@v0.0.1
```

## Sample database

```sh
go run ./cmd/mksampledb -o ui-versions.sqlite
```
