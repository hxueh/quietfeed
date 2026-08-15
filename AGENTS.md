# Repository Guidelines

## Project Structure & Module Organization

QuietFeed is a single Go module (`github.com/hxueh/quietfeed`) using package `main`. Production source files live in the repository root and are grouped by responsibility: `api.go` implements Google Reader endpoints, `fetch.go` refreshes feeds, `db.go` owns the SQLite schema, and `auth.go` handles login sessions. Tests sit beside their targets as `*_test.go` files. Deployment examples are `quietfeed.service`, `quietfeed.env.example`, and `Caddyfile`; GitHub automation lives under `.github/`. Generated binaries belong in `bin/` and must not be committed.

## Build, Test, and Development Commands

- `make build` builds the stripped binary at `bin/quietfeed`.
- `make test` runs the complete Go test suite.
- `make coverage` writes `coverage.out` and prints function-level coverage.
- `make fmt` formats all root-level Go files with `gofmt`.
- `go vet ./...` performs static analysis.

For local development, create a writable socket directory and provide a password:

```sh
mkdir -p run
QUIETFEED_PASSWORD=test-password QUIETFEED_SOCKET="$PWD/run/quietfeed.sock" go run .
```

## Coding Style & Naming Conventions

Use standard Go formatting and tabs as produced by `gofmt`. Keep functions small and grouped in the file matching their responsibility. Use descriptive camelCase names for unexported identifiers; export only reusable public API, which is currently uncommon in this command module. Handle errors explicitly and use request contexts for database and network operations. Do not silently replace invalid configuration with defaults.

## Testing Guidelines

Use Go's `testing` package, `httptest` for HTTP behavior, and temporary SQLite databases from `t.TempDir()`. Name tests `TestBehaviorOrCondition`, for example `TestRefreshRejectsOversizedFeed`. Cover success, validation, and controlled failure paths. Overall statement coverage must remain at or above 90%, as enforced by CI. Run `go test -count=1 -cover ./...` and `go vet ./...` before submitting changes.

## Commit & Pull Request Guidelines

The repository has no established commit history yet. Use concise, imperative subjects such as `Fix duplicate feed IDs` or `Add OPML import tests`. Keep each commit focused. Pull requests should explain the behavior change, motivation, test evidence, and any configuration or database impact. Link relevant issues. Screenshots are unnecessary unless a future user interface is introduced.

## Security & Configuration

Never commit passwords, tokens, databases, OPML exports, or production configuration. Preserve the 10 MiB feed limit and private-network destination blocking unless a documented security review justifies changing them. Report vulnerabilities according to `SECURITY.md`.
