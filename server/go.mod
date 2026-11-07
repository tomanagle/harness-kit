module github.com/tomanagle/harness-kit/server

go 1.27.3

require (
	github.com/a-h/templ v0.3.1100
	github.com/jackc/pgx/v5 v5.12.0
	golang.org/x/crypto v0.58.0
)

require (
	github.com/jackc/pgpassfile v1.0.0 // indirect
	github.com/jackc/pgservicefile v0.0.0-20240606120523-5a60cdf6a761 // indirect
	github.com/jackc/puddle/v2 v2.2.3 // indirect
	github.com/tomanagle/harness-kit/packages/client-go v0.0.0
	golang.org/x/sync v0.24.0 // indirect
	golang.org/x/sys v0.49.0 // indirect
	golang.org/x/text v0.43.0 // indirect
)

replace github.com/tomanagle/harness-kit/packages/client-go => ../packages/client-go
