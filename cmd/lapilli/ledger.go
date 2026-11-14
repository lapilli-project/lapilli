package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"lapilli.dev/internal/ledger"
)

const ledgerUsage = `lapilli ledger copy copies a ledger into another, empty one: a SQLite
ledger into Postgres, to keep it there from then on, or one back. Stop
lapilli serve first: a Postgres ledger serve holds is refused, and what a
SQLite ledger is written after the copy reads it is left out.

Usage:
  lapilli ledger copy --ledger DIR --to-url-file FILE
  lapilli ledger copy --ledger-url-file FILE --to DIR

  --ledger DIR        the SQLite ledger to copy, as lapilli serve --ledger
                      keeps it (DIR/ledger.db)
  --ledger-url-file FILE
                      or the Postgres ledger to copy: the file holds its
                      connection string, as for lapilli serve
  --to DIR            copy into a SQLite ledger there, made if it isn't
  --to-url-file FILE  or into the Postgres database the file's connection
                      string names, in its schema (search_path)

The ledger copied into must be empty: new, or only opened. Every table is
copied in one transaction, so a copy that fails leaves it empty; ids are
kept. Then both are read back, table by table, and a copy that differs
fails. The ledger copied from is brought to this lapilli's schema first, as
lapilli serve would, and its days and weeks stay counted in its time zone:
serve counts them again if --tz says another.
`

func runLedger(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" || args[0] == "help" {
		fmt.Fprint(stdout, ledgerUsage)
		if len(args) == 0 {
			return 2
		}
		return 0
	}
	if args[0] != "copy" {
		fmt.Fprintf(stderr, "lapilli ledger: unknown command %q\nRun \"lapilli ledger -h\" for the options.\n", args[0])
		return 2
	}
	var from, fromURL, to, toURL string
	rest := args[1:]
	for i := 0; i < len(rest); i++ {
		name, val, hasVal := strings.Cut(rest[i], "=")
		if name == "-h" || name == "--help" {
			fmt.Fprint(stdout, ledgerUsage)
			return 0
		}
		if !hasVal {
			if i+1 >= len(rest) {
				fmt.Fprintf(stderr, "lapilli ledger copy: %s needs a value\n", name)
				return 2
			}
			i++
			val = rest[i]
		}
		switch name {
		case "--ledger":
			from = val
		case "--ledger-url-file":
			fromURL = val
		case "--to":
			to = val
		case "--to-url-file":
			toURL = val
		default:
			fmt.Fprintf(stderr, "lapilli ledger copy: unknown flag %s\nRun \"lapilli ledger -h\" for the options.\n", name)
			return 2
		}
	}
	switch {
	case (from == "") == (fromURL == ""):
		fmt.Fprintln(stderr, "lapilli ledger copy: give the ledger to copy, --ledger or --ledger-url-file")
		return 2
	case (to == "") == (toURL == ""):
		fmt.Fprintln(stderr, "lapilli ledger copy: give where to copy it, --to or --to-url-file")
		return 2
	case from != "" && to != "" && filepath.Clean(from) == filepath.Clean(to), fromURL != "" && fromURL == toURL:
		fmt.Fprintln(stderr, "lapilli ledger copy: a ledger into itself")
		return 2
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	src, err := openForCopy(ctx, from, fromURL, true)
	if err != nil {
		fmt.Fprintf(stderr, "lapilli ledger copy: the ledger to copy: %v\n", err)
		return 1
	}
	defer src.Close()
	dst, err := openForCopy(ctx, to, toURL, false)
	if err != nil {
		fmt.Fprintf(stderr, "lapilli ledger copy: the ledger to copy into: %v\n", err)
		return 1
	}
	defer dst.Close()
	for _, l := range []*ledger.Ledger{src, dst} {
		for _, w := range l.Warnings() {
			fmt.Fprintf(stderr, "lapilli ledger copy: warning: %s\n", w)
		}
	}
	rep, err := ledger.Copy(ctx, src, dst, func(table string, rows int64) {
		fmt.Fprintf(stderr, "copied %s: %d rows\n", table, rows)
	})
	if err != nil {
		fmt.Fprintf(stderr, "lapilli ledger copy: %v\n", err)
		return 1
	}
	var rows int64
	for _, t := range rep.Tables {
		rows += t.Rows
	}
	fmt.Fprintf(stdout, "copied %d tables, %d rows, and read them back alike\n", len(rep.Tables), rows)
	return 0
}

// openForCopy opens a ledger as it is, in the zone it was counted in. The
// ledger to copy must be there already.
func openForCopy(ctx context.Context, dir, urlFile string, there bool) (*ledger.Ledger, error) {
	if urlFile != "" {
		b, err := os.ReadFile(urlFile)
		if err != nil {
			return nil, err
		}
		// Alone: refused while serve's replicas hold the ledger, and
		// refusing them while it copies.
		return ledger.OpenPostgresAlone(ctx, string(b), nil)
	}
	path := filepath.Join(dir, "ledger.db")
	if _, err := os.Stat(path); there && err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("no ledger in %s", dir)
		}
		return nil, err
	}
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, err
	}
	return ledger.Open(ctx, path, nil)
}
