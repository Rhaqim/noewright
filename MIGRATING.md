# Migrating from loom to Noewright

loom is now **Noewright**. The code is the same project under a new name and a new
Go module path. Your database needs no changes.

The last release under the old path is `github.com/rhaqim/loom v0.0.4`. The first
release under the new path is `github.com/rhaqim/noewright v0.1.0`.

## What you need to change

**1. Module path and imports.**

| Before | After |
|---|---|
| `github.com/rhaqim/loom` | `github.com/rhaqim/noewright` |
| `github.com/rhaqim/loom/generator/anthropic` | `github.com/rhaqim/noewright/generator/anthropic` |
| `github.com/rhaqim/loom/<subpackage>` | `github.com/rhaqim/noewright/<subpackage>` |

```bash
# from your module root
grep -rl 'github.com/rhaqim/loom' --include='*.go' --include='go.mod' . \
  | xargs sed -i.bak 's#github.com/rhaqim/loom#github.com/rhaqim/noewright#g'
find . -name '*.bak' -delete
go get github.com/rhaqim/noewright@v0.1.0
go mod tidy
```

On Linux, `sed -i` works without the `.bak` suffix.

**2. Package name.** The root package is now `noewright`, so `loom.New(...)` becomes
`noewright.New(...)`. To change as little code as possible for now, alias the import:

```go
import loom "github.com/rhaqim/noewright"
```

**3. CLI.** `loom-cli` is now `noewright` (built from `cmd/noewright`). Its commands
and flags are unchanged.

**4. DSN environment variable.** The CLI and the test suite read `NOEWRIGHT_DSN`.
`LOOM_DSN` is still read when `NOEWRIGHT_DSN` is unset, but that fallback will be
removed in a later release, so switch when convenient.

**5. Error strings.** Error messages now start with `noewright:` instead of `loom:`.
The sentinel errors (`ErrNotFound`, `ErrSessionConflict`, …) are the same values, so
`errors.Is` / `errors.As` checks are unaffected. Only code that matches on error
*text* needs updating. Log lines from the `gc` worker now start with `noewright/gc:`.

## What did not change

- **Database tables keep the `loom_` prefix.** `Config.SchemaPrefix` still defaults
  to `"loom_"`, so an upgraded application finds its existing tables and data.
  Renaming the default would have pointed existing apps at new, empty tables.
  New projects that want a different prefix can set `Config.SchemaPrefix` (and
  the CLI's `--prefix`).
- **The migration ledger** (`schema_migrations`) is unchanged. No migration runs
  because of the rename.
- **Cache keys** still start with `loom` by default (`Config.CacheKeyPrefix`), so a
  shared cache stays warm across the upgrade.
- **Local development database.** `docker-compose.yml` still uses the `loom`
  user, password and database and the `loom_pgdata` volume, so an existing local
  database survives. The default DSN
  `postgres://loom:loom@localhost:5432/loom?sslmode=disable` is unchanged.

## The old module path

- Every `github.com/rhaqim/loom` release (`v0.0.1`–`v0.0.4`) stays downloadable
  from the Go module proxy, so existing builds keep working.
- `v0.0.4` is marked deprecated, and `go list -m -u` and pkg.go.dev point to the new path.
- The old path gets no further releases. Because the GitHub repository was
  renamed, `go get -u github.com/rhaqim/loom` may fail with
  `module declares its path as: github.com/rhaqim/noewright`. That error means
  "switch to the new path" (step 1 above), not that something is broken.
