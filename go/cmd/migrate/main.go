// Command peer-inbox-migrate — applies goose migrations against a SQLite
// or PostgreSQL database. Wraps github.com/pressly/goose/v3 in the
// minimal CLI shape the rest of the toolchain expects:
//
//	peer-inbox-migrate -driver sqlite -dsn /path/to/sessions.db -dir ./migrations/sqlite up
//	peer-inbox-migrate -driver pgx    -dsn postgres://...        -dir ./migrations/postgres up
//	peer-inbox-migrate -driver sqlite -dsn /path/to/db -dir ... status
//
// Python's open_db() auto-invokes this binary on stale/missing
// goose_db_version state. CI invokes it against postgres:16.
package main

import (
	"context"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/pressly/goose/v3"
	_ "github.com/jackc/pgx/v5/stdlib"
	_ "modernc.org/sqlite"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "peer-inbox-migrate: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		driver  = flag.String("driver", "sqlite", "database driver: sqlite | pgx")
		dsn     = flag.String("dsn", "", "data source name (SQLite file path or Postgres URL)")
		dir     = flag.String("dir", "", "migrations directory (e.g. migrations/sqlite)")
		verbose = flag.Bool("v", false, "verbose goose logging")
	)
	flag.Usage = func() {
		fmt.Fprintf(flag.CommandLine.Output(),
			"usage: peer-inbox-migrate -driver <sqlite|pgx> -dsn <...> -dir <...> <up|down|status|version>\n")
		flag.PrintDefaults()
	}
	flag.Parse()

	cmd := flag.Arg(0)
	if cmd == "" {
		cmd = "up"
	}

	if *dsn == "" {
		return errors.New("missing required -dsn")
	}
	if *dir == "" {
		return errors.New("missing required -dir")
	}
	absDir, err := filepath.Abs(*dir)
	if err != nil {
		return fmt.Errorf("resolve migrations dir: %w", err)
	}
	if _, err := os.Stat(absDir); err != nil {
		return fmt.Errorf("migrations dir %s: %w", absDir, err)
	}

	// goose's driver string uses different names than database/sql's — map
	// our -driver flag to the pair (sql driver name, goose dialect).
	var (
		sqlDriver    string
		gooseDialect string
	)
	switch *driver {
	case "sqlite", "sqlite3":
		sqlDriver = "sqlite"
		gooseDialect = "sqlite3"
	case "pgx", "postgres":
		sqlDriver = "pgx"
		gooseDialect = "postgres"
	default:
		return fmt.Errorf("unsupported driver %q (want sqlite | pgx)", *driver)
	}

	db, err := sql.Open(sqlDriver, *dsn)
	if err != nil {
		return fmt.Errorf("sql.Open(%s): %w", sqlDriver, err)
	}
	defer db.Close()
	if err := db.PingContext(context.Background()); err != nil {
		return fmt.Errorf("ping: %w", err)
	}

	if err := goose.SetDialect(gooseDialect); err != nil {
		return fmt.Errorf("goose dialect: %w", err)
	}
	if *verbose {
		goose.SetVerbose(true)
	}

	switch cmd {
	case "up":
		return goose.Up(db, absDir)
	case "down":
		return goose.Down(db, absDir)
	case "status":
		return goose.Status(db, absDir)
	case "version":
		return goose.Version(db, absDir)
	default:
		return fmt.Errorf("unknown subcommand %q (want up|down|status|version)", cmd)
	}
}
