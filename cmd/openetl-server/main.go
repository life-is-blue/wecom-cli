// Command openetl-server serves the openetl REST API.
//
//	openetl-server serve
//	openetl-server migrate
//	openetl-server admin create-token --project P --account A --side delivery|customer --role viewer|editor|admin
//	openetl-server admin rebuild-latest   (rebuild the entity_latest cache from delta, one transaction)
//	openetl-server admin verify-latest    (compare the cache with a recomputation from delta; exit 1 on mismatch)
//
// Environment: OPENETL_DB_DSN (go-sql-driver DSN, required), OPENETL_ADDR (default :8080),
// OPENETL_QUERY_DSN (optional SELECT-only DSN for the query endpoint), OPENETL_PUBLIC_URL,
// OPENETL_TRUST_PROXY=1, OPENETL_ALLOW_GLOBAL_CONFIG=1, OPENETL_QUERY_MAX_PER_ACCOUNT, OPENETL_QUERY_MAX_TOTAL,
// OPENETL_PROJECT_STORAGE_QUOTA_BYTES (see docs/security.md), OPENETL_ACCESS_LOG=off, OPENETL_METRICS_TOKEN (docs/operations.md).
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/life-is-blue/openetl-cli/db"
	"github.com/life-is-blue/openetl-cli/internal/server"
	"github.com/life-is-blue/openetl-cli/internal/store"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func usage() error {
	return errors.New("usage: openetl-server serve | migrate | admin rebuild-latest | admin verify-latest | admin create-token --project P --account A --side delivery|customer --role viewer|editor|admin [--kind human|agent] [--ttl 8760h]")
}

func openStore() (*store.Store, error) {
	dsn := os.Getenv("OPENETL_DB_DSN")
	if dsn == "" {
		return nil, errors.New("OPENETL_DB_DSN is not set")
	}
	return store.Open(dsn)
}

// migrate refuses to touch a database that has migrations this binary does not know (a rollback of
// the binary after an upgrade), then applies the pending ones. See docs/operations.md.
func migrate(ctx context.Context, st *store.Store) ([]string, error) {
	known := map[string]bool{}
	entries, err := fs.ReadDir(db.Migrations, "migrations")
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		known[e.Name()] = true
	}
	rows, err := st.DB.QueryContext(ctx, `SELECT version FROM schema_migrations`)
	if err == nil { // a missing table just means a fresh database
		defer rows.Close()
		var unknown []string
		for rows.Next() {
			var v string
			if err := rows.Scan(&v); err != nil {
				return nil, err
			}
			if !known[v] {
				unknown = append(unknown, v)
			}
		}
		if err := rows.Err(); err != nil {
			return nil, err
		}
		if len(unknown) > 0 {
			return nil, fmt.Errorf("database has migrations this binary does not know (%s): it was upgraded by a newer openetl-server; run that version or restore a backup", strings.Join(unknown, ", "))
		}
	}
	return st.Migrate(ctx)
}

func run(args []string) error {
	if len(args) == 0 {
		return usage()
	}
	switch args[0] {
	case "serve":
		return serve()
	case "migrate":
		st, err := openStore()
		if err != nil {
			return err
		}
		defer st.Close()
		applied, err := migrate(context.Background(), st)
		for _, a := range applied {
			fmt.Println("applied", a)
		}
		if err == nil && len(applied) == 0 {
			fmt.Println("up to date")
		}
		return err
	case "admin":
		if len(args) < 2 {
			return usage()
		}
		switch args[1] {
		case "create-token":
			return createToken(args[2:])
		case "rebuild-latest":
			return rebuildLatest()
		case "verify-latest":
			return verifyLatest()
		}
		return usage()
	}
	return usage()
}

func serve() error {
	st, err := openStore()
	if err != nil {
		return err
	}
	defer st.Close()
	if applied, err := migrate(context.Background(), st); err != nil {
		return fmt.Errorf("migrate: %w", err)
	} else {
		for _, a := range applied {
			log.Println("applied migration", a)
		}
	}
	if qdsn := os.Getenv("OPENETL_QUERY_DSN"); qdsn != "" {
		if err := st.OpenQuery(qdsn); err != nil {
			return fmt.Errorf("OPENETL_QUERY_DSN: %w", err)
		}
		log.Println("query endpoint uses OPENETL_QUERY_DSN")
	} else {
		log.Println("WARNING: OPENETL_QUERY_DSN is not set; user SQL runs on a separate small pool but with the server's main database credentials. " +
			"Create a SELECT-only user (scripts/create-query-user.sql) and point OPENETL_QUERY_DSN at it.")
	}
	addr := os.Getenv("OPENETL_ADDR")
	if addr == "" {
		addr = ":8080"
	}
	gw := server.New(st, log.Default())
	if os.Getenv("OPENETL_ACCESS_LOG") != "off" {
		gw.AccessLog = os.Stderr // one JSON line per request: request id, route, status, duration, account
	}
	stopClean := make(chan struct{})
	defer close(stopClean)
	go func() { // drop unregistered uploads older than an hour: at startup and hourly
		clean := func() {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
			defer cancel()
			if n, err := gw.CleanupStaging(ctx); err != nil {
				log.Println("staging cleanup:", err)
			} else if n > 0 {
				log.Println("staging cleanup removed", n, "stale upload(s)")
			}
		}
		clean()
		t := time.NewTicker(time.Hour)
		defer t.Stop()
		for {
			select {
			case <-t.C:
				clean()
			case <-stopClean:
				return
			}
		}
	}()
	srv := &http.Server{
		Addr:              addr,
		Handler:           gw,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       60 * time.Second,
		WriteTimeout:      60 * time.Second,
	}
	errCh := make(chan error, 1)
	go func() { errCh <- srv.ListenAndServe() }()
	log.Println("openetl-server listening on", addr)
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	select {
	case err := <-errCh:
		return err
	case <-sig:
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return srv.Shutdown(ctx)
	}
}

func createToken(args []string) error {
	fs := flag.NewFlagSet("create-token", flag.ContinueOnError)
	project := fs.String("project", "", "project name (lowercase slug), created if missing")
	account := fs.String("account", "", "account user name, created if missing")
	side := fs.String("side", "", "delivery|customer")
	role := fs.String("role", "", "viewer|editor|admin")
	kind := fs.String("kind", "human", "human|agent (used when the account is created)")
	ttl := fs.Duration("ttl", 365*24*time.Hour, "token lifetime")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *project == "" || *account == "" {
		return errors.New("--project and --account are required")
	}
	if *side != "delivery" && *side != "customer" {
		return errors.New("--side must be delivery or customer")
	}
	if *role != "viewer" && *role != "editor" && *role != "admin" {
		return errors.New("--role must be viewer, editor or admin")
	}
	if *kind != "human" && *kind != "agent" {
		return errors.New("--kind must be human or agent")
	}
	st, err := openStore()
	if err != nil {
		return err
	}
	defer st.Close()
	if _, err := migrate(context.Background(), st); err != nil {
		return fmt.Errorf("migrate: %w", err)
	}
	secret, err := st.CreateToken(context.Background(), *project, *account, *side, *role, *kind, *ttl)
	if err != nil {
		return err
	}
	fmt.Fprintln(os.Stderr, "token created; it is shown once and only its hash is stored")
	fmt.Println(secret)
	return nil
}

func rebuildLatest() error {
	st, err := openStore()
	if err != nil {
		return err
	}
	defer st.Close()
	if _, err := migrate(context.Background(), st); err != nil {
		return fmt.Errorf("migrate: %w", err)
	}
	n, err := st.RebuildLatest(context.Background())
	if err != nil {
		return err
	}
	fmt.Printf("entity_latest rebuilt from delta: %d rows\n", n)
	return nil
}

func verifyLatest() error {
	st, err := openStore()
	if err != nil {
		return err
	}
	defer st.Close()
	total, first, err := st.VerifyLatest(context.Background(), 50)
	if err != nil {
		return err
	}
	if total == 0 {
		fmt.Println("entity_latest matches delta")
		return nil
	}
	for _, m := range first {
		fmt.Println(m)
	}
	return fmt.Errorf("entity_latest differs from delta in %d row(s); run `admin rebuild-latest`", total)
}
