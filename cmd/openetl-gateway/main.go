// Command openetl-gateway serves the openetl REST API.
//
//	openetl-gateway serve
//	openetl-gateway migrate
//	openetl-gateway admin create-token --project P --account A --side delivery|customer --role viewer|editor|admin
//
// Environment: OPENETL_DB_DSN (go-sql-driver DSN, required), OPENETL_ADDR (default :8080).
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/life-is-blue/openetl-cli/internal/gateway"
	"github.com/life-is-blue/openetl-cli/internal/store"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func usage() error {
	return errors.New("usage: openetl-gateway serve | migrate | admin create-token --project P --account A --side delivery|customer --role viewer|editor|admin [--kind human|agent] [--ttl 8760h]")
}

func openStore() (*store.Store, error) {
	dsn := os.Getenv("OPENETL_DB_DSN")
	if dsn == "" {
		return nil, errors.New("OPENETL_DB_DSN is not set")
	}
	return store.Open(dsn)
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
		applied, err := st.Migrate(context.Background())
		for _, a := range applied {
			fmt.Println("applied", a)
		}
		if err == nil && len(applied) == 0 {
			fmt.Println("up to date")
		}
		return err
	case "admin":
		if len(args) < 2 || args[1] != "create-token" {
			return usage()
		}
		return createToken(args[2:])
	}
	return usage()
}

func serve() error {
	st, err := openStore()
	if err != nil {
		return err
	}
	defer st.Close()
	if applied, err := st.Migrate(context.Background()); err != nil {
		return fmt.Errorf("migrate: %w", err)
	} else {
		for _, a := range applied {
			log.Println("applied migration", a)
		}
	}
	addr := os.Getenv("OPENETL_ADDR")
	if addr == "" {
		addr = ":8080"
	}
	srv := &http.Server{
		Addr:              addr,
		Handler:           gateway.New(st, log.Default()),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       60 * time.Second,
		WriteTimeout:      60 * time.Second,
	}
	errCh := make(chan error, 1)
	go func() { errCh <- srv.ListenAndServe() }()
	log.Println("openetl-gateway listening on", addr)
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
	if _, err := st.Migrate(context.Background()); err != nil {
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
