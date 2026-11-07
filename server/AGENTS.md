# Server conventions

- `cmd/main.go` is the composition root. It supplies environment access, entropy,
  the clock, logger, database, repositories, handlers, and the HTTP listener.
- Explicit `panic` is permitted only inside a function named `Must...`.
- `Must...` functions may be called or referenced only directly inside `func main`.
  Do not call them from helpers, callbacks, tests, other Must functions, or package
  initializers. Each Must constructor wraps an ordinary error-returning constructor.
- Request handling, token generation, database operations, migrations, and code
  generation return errors. Never convert operational failures into panics.
- Keep dependencies private after construction. Consume narrow interfaces at
  resource boundaries; use `slog.Logger`/`slog.Handler` for logging, not a custom
  logging abstraction. Do not open connections or invent fallback dependencies
  inside repositories or handlers.
- Infrastructure lives in focused internal packages (`config`, `logger`,
  `database`, `httpserver`). Business persistence uses `store` and `store/dbstore`.
  HTTP routes are grouped by responsibility within `handlers`.
- `go test ./internal/architecture` enforces these boundaries. Run server tests
  with `HK_TEST_DATABASE_URL` against a disposable Postgres service for integration
  coverage, and run root `make format`, `make lint`, and `make check-generated`.
