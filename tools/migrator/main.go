// Command migrator is the production migration entry point (I04 review P1).
//
// It enforces the issue's advisory-lock guarantee: ONE migration runner per
// database. It takes session-scoped pg_advisory_lock(781927001) on a
// dedicated connection, invokes the pinned goose CLI, and releases the lock
// on exit. A second concurrent runner exits with a clear error instead of
// racing.
//
// Usage: migrator -dsn <postgres-url> [goose args…=up]
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"

	"github.com/jackc/pgx/v5"
)

const lockKey = 781927001

func main() {
	dsn := flag.String("dsn", "", "postgres DSN (required)")
	dir := flag.String("dir", "migrations", "migrations directory")
	flag.Parse()
	if *dsn == "" {
		fmt.Fprintln(os.Stderr, "migrator: -dsn is required")
		os.Exit(2)
	}
	args := flag.Args()
	if len(args) == 0 {
		args = []string{"up"}
	}

	ctx := context.Background()
	conn, err := pgx.Connect(ctx, *dsn)
	if err != nil {
		fmt.Fprintf(os.Stderr, "migrator: connect: %v\n", err)
		os.Exit(2)
	}
	var ok bool
	if err := conn.QueryRow(ctx, "SELECT pg_try_advisory_lock($1)", lockKey).Scan(&ok); err != nil {
		fmt.Fprintf(os.Stderr, "migrator: lock query: %v\n", err)
		os.Exit(2)
	}
	if !ok {
		fmt.Fprintln(os.Stderr, "migrator: another migration runner holds the advisory lock")
		os.Exit(3)
	}
	defer func() { _, _ = conn.Exec(ctx, "SELECT pg_advisory_unlock($1)", lockKey) }()

	goose := os.Getenv("GOOSE_BIN")
	if goose == "" {
		var err error
		if goose, err = exec.LookPath("goose"); err != nil {
			fmt.Fprintln(os.Stderr, "migrator: goose CLI not found (GOOSE_BIN or PATH)")
			os.Exit(2)
		}
	}
	cmd := exec.Command(goose, "-dir", *dir, "postgres", *dsn, args[0])
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			os.Exit(ee.ExitCode())
		}
		fmt.Fprintf(os.Stderr, "migrator: goose: %v\n", err)
		os.Exit(2)
	}
	_ = conn.Close(ctx)
}
