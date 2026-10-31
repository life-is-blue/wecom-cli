// Package cli implements the openetl command line client.
package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/life-is-blue/openetl-cli/internal/cli/credentials"
)

// Environment variables.
const (
	EnvToken   = "OPENETL_TOKEN"
	EnvBaseURL = "OPENETL_BASE_URL"
	EnvProject = "OPENETL_PROJECT"
)

// App is one CLI invocation. Everything external is injectable for tests.
type App struct {
	Stdin   io.Reader
	Stdout  io.Writer
	Stderr  io.Writer
	Getenv  func(string) string
	HTTP    *http.Client
	Version string
	Keyring credentials.Keyring // nil: OS keyring

	// IsTerminal reports whether interactive confirmation is possible (nil: stdin is a character device).
	IsTerminal func() bool
	// PollInterval is the wait between import job polls (zero: 2s).
	PollInterval time.Duration

	// global flags
	flagProject string
	flagBase    string
	output      string
	pageCount   int

	resolved *resolved
}

// cliError is every failure the CLI reports; it is rendered as JSON on stderr.
type cliError struct {
	Exit    int
	Code    int
	Status  string
	Message string
	Details json.RawMessage
	Silent  bool // exit code only, already reported on stdout
}

func (e *cliError) Error() string { return e.Message }

func usagef(format string, a ...any) *cliError {
	return &cliError{Exit: 2, Status: "USAGE", Message: fmt.Sprintf(format, a...)}
}

func runtimef(format string, a ...any) *cliError {
	return &cliError{Exit: 1, Status: "CLIENT_ERROR", Message: fmt.Sprintf(format, a...)}
}

func (e *cliError) body() []byte {
	inner := map[string]any{"status": e.Status, "message": e.Message}
	if e.Code != 0 {
		inner["code"] = e.Code
	}
	if len(e.Details) > 0 {
		inner["details"] = e.Details
	}
	b, _ := json.Marshal(map[string]any{"error": inner})
	return b
}

// Run executes the CLI and returns the process exit code.
func (a *App) Run(args []string) int {
	if a.Getenv == nil {
		a.Getenv = os.Getenv
	}
	if a.HTTP == nil {
		a.HTTP = &http.Client{}
	}
	if a.Stdin == nil {
		a.Stdin = strings.NewReader("")
	}
	root := a.newRoot()
	root.SetArgs(args)
	err := root.Execute()
	if err == nil {
		return 0
	}
	var ce *cliError
	if !errors.As(err, &ce) {
		ce = runtimef("%v", err)
	}
	if !ce.Silent {
		fmt.Fprintf(a.Stderr, "%s\n", ce.body())
	}
	return ce.Exit
}

func (a *App) newRoot() *cobra.Command {
	root := &cobra.Command{
		Use:           "openetl",
		Short:         "openetl: migration progress management CLI",
		Version:       a.Version,
		SilenceErrors: true,
		SilenceUsage:  true,
		RunE:          groupRun,
		Args:          groupArgs,
	}
	root.SetVersionTemplate("openetl {{.Version}}\n")
	root.SetOut(a.Stdout)
	root.SetErr(a.Stderr)
	root.SetFlagErrorFunc(func(_ *cobra.Command, err error) error { return usagef("%v", err) })
	root.CompletionOptions.DisableDefaultCmd = true
	pf := root.PersistentFlags()
	pf.StringVar(&a.flagProject, "project", "", "project id (env "+EnvProject+")")
	pf.StringVar(&a.flagBase, "base-url", "", "gateway base URL (env "+EnvBaseURL+")")
	pf.StringVarP(&a.output, "output", "o", "", "write output to FILE instead of stdout")
	pf.IntVar(&a.pageCount, "page-count", 0, "list/query: fetch up to N pages and print NDJSON (one page per line)")

	root.AddCommand(
		a.authCmd(), a.meCmd(), a.workflowCmd(), a.taskCmd(), a.stageCmd(),
		a.scopeCmd(), a.commentCmd(), a.queryCmd(), a.viewsCmd(),
		a.eventsCmd(), a.attachmentCmd(), a.memberCmd(), a.tokenCmd(), a.configCmd(), a.importCmd(),
	)
	return root
}

func groupRun(cmd *cobra.Command, _ []string) error { return cmd.Help() }

func groupArgs(cmd *cobra.Command, args []string) error {
	if len(args) > 0 {
		return usagef("unknown command %q for %q", args[0], cmd.CommandPath())
	}
	return nil
}

func group(use, short string, subs ...*cobra.Command) *cobra.Command {
	c := &cobra.Command{Use: use, Short: short, RunE: groupRun, Args: groupArgs}
	c.AddCommand(subs...)
	return c
}

func exactArgs(n int) cobra.PositionalArgs {
	return func(cmd *cobra.Command, args []string) error {
		if len(args) != n {
			return usagef("%s: expected %d argument(s), got %d (usage: %s)", cmd.CommandPath(), n, len(args), cmd.Use)
		}
		return nil
	}
}

// ---------------------------------------------------------------- output

func compact(raw []byte) []byte {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 {
		return []byte("{}")
	}
	var b bytes.Buffer
	if err := json.Compact(&b, raw); err != nil {
		return raw
	}
	return b.Bytes()
}

// emit prints one JSON document (or writes it with --output).
func (a *App) emit(raw []byte) error { return a.emitLines([][]byte{compact(raw)}) }

func (a *App) emitLines(lines [][]byte) error {
	var buf bytes.Buffer
	for _, l := range lines {
		buf.Write(l)
		buf.WriteByte('\n')
	}
	if a.output == "" {
		_, err := a.Stdout.Write(buf.Bytes())
		return err
	}
	if err := os.WriteFile(a.output, buf.Bytes(), 0o644); err != nil {
		return runtimef("write %s: %v", a.output, err)
	}
	res, _ := json.Marshal(map[string]any{"output": a.output, "bytes": buf.Len()})
	fmt.Fprintf(a.Stdout, "%s\n", res)
	return nil
}

func (a *App) emitValue(v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return runtimef("encode output: %v", err)
	}
	return a.emit(b)
}

// paginate drives list-style calls. Without --page-count it makes one call and
// prints the response; with it, up to N pages are printed as NDJSON.
func (a *App) paginate(fetch func(token string) ([]byte, error), token string) error {
	if a.pageCount <= 0 {
		b, err := fetch(token)
		if err != nil {
			return err
		}
		return a.emit(b)
	}
	var lines [][]byte
	for i := 0; i < a.pageCount; i++ {
		b, err := fetch(token)
		if err != nil {
			return err
		}
		lines = append(lines, compact(b))
		var p struct {
			Next string `json:"next_page_token"`
		}
		_ = json.Unmarshal(b, &p)
		if p.Next == "" {
			break
		}
		token = p.Next
	}
	return a.emitLines(lines)
}
