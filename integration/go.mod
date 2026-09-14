// This module is deliberately separate from the library's.
//
// The library depends on nothing, and its go.mod require block being empty is a
// rule rather than a preference. Testing it against real databases needs drivers,
// so those drivers live here, in a module the library never imports.
//
//	go test ./...                 # at the repository root: no database needed
//	cd integration && go test ./...   # requires the containers below
module github.com/darkinno-tech/distledger/integration

go 1.25.0

replace github.com/darkinno-tech/distledger => ../

require (
	github.com/go-sql-driver/mysql v1.10.1
	github.com/darkinno-tech/distledger v0.0.0-00010101000000-000000000000
	github.com/jackc/pgx/v5 v5.11.0
	modernc.org/sqlite v1.58.0
)

require (
	filippo.io/edwards25519 v1.2.0 // indirect
	github.com/dustin/go-humanize v1.0.1 // indirect
	github.com/google/uuid v1.6.0 // indirect
	github.com/jackc/pgpassfile v1.0.0 // indirect
	github.com/jackc/pgservicefile v0.0.0-20240606120523-5a60cdf6a761 // indirect
	github.com/jackc/puddle/v2 v2.2.2 // indirect
	github.com/mattn/go-isatty v0.0.24 // indirect
	github.com/ncruces/go-strftime v1.0.0 // indirect
	github.com/remyoudompheng/bigfft v0.0.0-20230129092748-24d4a6f8daec // indirect
	golang.org/x/sync v0.22.0 // indirect
	golang.org/x/sys v0.47.0 // indirect
	golang.org/x/text v0.29.0 // indirect
	modernc.org/libc v1.75.6 // indirect
	modernc.org/mathutil v1.7.1 // indirect
	modernc.org/memory v1.12.1 // indirect
)
