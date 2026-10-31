package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/life-is-blue/openetl-cli/internal/api"
	"github.com/life-is-blue/openetl-cli/internal/importsrc"
)

// importClasses is the review order of diff classes: what needs a human first.
var importClasses = []string{"CONFLICT", "SUSPECT", "CHANGED", "NEW", "MISSING", "UNCHANGED"}

func (a *App) importCmd() *cobra.Command {
	return group("import", "Stage legacy rows, review the diff, then apply or roll back (batch import)",
		a.importStage(), a.importShow(), a.importDiff(), a.importApply(), a.importRollback())
}

func (a *App) importJobPath(in string) (string, error) {
	in = strings.TrimSpace(in)
	id := in
	if strings.Contains(in, "/") {
		i := strings.LastIndex(in, "importJobs/")
		if i < 0 {
			return "", usagef("invalid import job %q", in)
		}
		id = strings.Trim(in[i+len("importJobs/"):], "/")
	}
	if id == "" || !idRe.MatchString(strings.ToLower(id)) {
		return "", usagef("invalid import job %q", in)
	}
	p, err := a.projectID()
	if err != nil {
		return "", err
	}
	return "/v1/projects/" + url.PathEscape(p) + "/importJobs/" + strings.ToLower(id), nil
}

// ---------------------------------------------------------------- stage

func (a *App) importStage() *cobra.Command {
	var from, mapPath, system, format string
	var maxRows int
	var w writeFlags
	cmd := &cobra.Command{
		Use: "stage --from SRC --map mapping.yaml [--source-system NAME]", Short: "Read a source (NDJSON/CSV file, '-' for stdin, mysql://user@host:port/db), map it and stage a job",
		Long: "Stages rows only; no workflow changes until `import apply`.\n" +
			"A mysql:// source is scanned read-only; its password comes from the DSN or " + importsrc.EnvSourcePassword +
			" and is never printed or sent to the gateway.\n--dry-run reads and maps the source locally and reports what would be staged; nothing is sent.",
		Args: exactArgs(0),
		RunE: func(_ *cobra.Command, _ []string) error {
			if from == "" || mapPath == "" {
				return usagef("--from and --map are required")
			}
			if err := w.noBody("import stage"); err != nil {
				return err
			}
			m, err := importsrc.LoadMapping(mapPath)
			if err != nil {
				return usagef("--map %s: %v", mapPath, err)
			}
			src, err := importsrc.Open(from, m, importsrc.Options{Getenv: a.Getenv, Stdin: a.Stdin, Format: format})
			if err != nil {
				return usagef("--from: %v", err)
			}
			if system == "" {
				system = m.SourceSystem
			}
			if system == "" {
				system = src.Label()
			}
			rows, err := importsrc.Build(context.Background(), src, m, system, maxRows)
			if err != nil {
				return runtimef("%v", err)
			}
			if w.dryRun {
				n := len(rows)
				if n > 3 {
					n = 3
				}
				return a.emitValue(map[string]any{"dry_run": true, "source_system": system, "source": src.Label(),
					"rows": len(rows), "sample": rows[:n]})
			}
			path, err := a.collectionPath("importJobs")
			if err != nil {
				return err
			}
			q, err := w.query(false)
			if err != nil {
				return err
			}
			q.Del("validate_only")
			out, err := a.call("POST", path, q, api.ImportJob{Source: &system, Rows: &rows})
			if err != nil {
				return err
			}
			return a.emit(out)
		},
	}
	f := cmd.Flags()
	f.StringVar(&from, "from", "", "source: file path (.csv or NDJSON), - for NDJSON on stdin, or mysql://user@host:port/db")
	f.StringVar(&mapPath, "map", "", "mapping.yaml (see docs/cli-import.md)")
	f.StringVar(&system, "source-system", "", "source system label (default: mapping source_system, else the source)")
	f.StringVar(&format, "format", "", "force csv or ndjson for file/stdin sources")
	f.IntVar(&maxRows, "max-rows", importsrc.DefaultMaxRows, "refuse to stage more rows than this in one job")
	w.bind(cmd)
	return cmd
}

// ---------------------------------------------------------------- show / diff

func (a *App) getImportJob(path string) (api.ImportJob, []byte, error) {
	raw, err := a.call("GET", path, nil, nil)
	if err != nil {
		return api.ImportJob{}, nil, err
	}
	var j api.ImportJob
	if err := json.Unmarshal(raw, &j); err != nil {
		return j, raw, runtimef("decode import job: %v", err)
	}
	return j, raw, nil
}

func (a *App) importShow() *cobra.Command {
	return &cobra.Command{
		Use: "show JOB", Short: "Show an import job: state and row counts per class", Args: exactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			path, err := a.importJobPath(args[0])
			if err != nil {
				return err
			}
			j, raw, err := a.getImportJob(path)
			if err != nil {
				return err
			}
			var m map[string]json.RawMessage
			_ = json.Unmarshal(raw, &m)
			out := map[string]any{}
			for k, v := range m {
				out[k] = v
			}
			if j.Summary != nil {
				out["total"] = sumCounts(*j.Summary)
				out["needs_review"] = (*j.Summary)["CONFLICT"]+(*j.Summary)["SUSPECT"] > 0
			}
			return a.emitValue(out)
		},
	}
}

func sumCounts(m map[string]int) int {
	n := 0
	for _, v := range m {
		n += v
	}
	return n
}

type diffClass struct {
	Count     int               `json:"count"`
	Rows      []json.RawMessage `json:"rows"`
	Truncated bool              `json:"truncated"`
}

func (a *App) importDiff() *cobra.Command {
	var limit int
	var actions []string
	var raw bool
	var l listFlags
	cmd := &cobra.Command{
		Use: "diff JOB [--limit N] [--action CLASS]... [--raw]", Short: "Compare staged rows with current data: counts per class and the first N rows per class",
		Long: "Classes: CONFLICT and SUSPECT need a human decision; CHANGED and NEW are merged on apply; MISSING and UNCHANGED are informational.\n" +
			"By default rows of every class except UNCHANGED are listed (first --limit each). --raw prints the server's diff pages instead (--filter, --page-size, --page-count).",
		Args: exactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			path, err := a.importJobPath(args[0])
			if err != nil {
				return err
			}
			if raw {
				return a.listCall(path+":diff", &l, "")
			}
			if limit <= 0 {
				return usagef("--limit must be positive")
			}
			classes := importClasses[:len(importClasses)-1]
			if len(actions) > 0 {
				classes = nil
				for _, x := range actions {
					c := strings.ToUpper(x)
					if !contains(importClasses, c) {
						return usagef("--action must be one of %s", strings.Join(importClasses, ", "))
					}
					classes = append(classes, c)
				}
			}
			job, _, err := a.getImportJob(path)
			if err != nil {
				return err
			}
			counts := map[string]int{}
			if job.Summary != nil {
				counts = *job.Summary
			}
			out := map[string]any{"counts": counts, "total": sumCounts(counts), "limit": limit,
				"needs_review": counts["CONFLICT"]+counts["SUSPECT"] > 0}
			if job.Name != nil {
				out["job"] = *job.Name
			}
			if job.State != nil {
				out["state"] = *job.State
			}
			res := map[string]*diffClass{}
			for _, c := range classes {
				if job.Summary != nil && counts[c] == 0 {
					res[c] = &diffClass{Rows: []json.RawMessage{}}
					continue
				}
				q := url.Values{"filter": {"action = " + c}, "page_size": {fmt.Sprint(limit)}}
				body, err := a.call("GET", path+":diff", q, nil)
				if err != nil {
					return err
				}
				var page struct {
					Rows []json.RawMessage `json:"rows"`
					Next string            `json:"next_page_token"`
				}
				if err := json.Unmarshal(body, &page); err != nil {
					return runtimef("decode diff: %v", err)
				}
				dc := &diffClass{Count: counts[c], Rows: []json.RawMessage{}}
				for _, r := range page.Rows {
					var probe struct {
						Action string `json:"action"`
					}
					if json.Unmarshal(r, &probe) == nil && probe.Action != "" && probe.Action != c {
						continue // server ignored the filter: keep only this class
					}
					if len(dc.Rows) < limit {
						dc.Rows = append(dc.Rows, r)
					} else {
						dc.Truncated = true
					}
				}
				if dc.Count == 0 {
					dc.Count = len(dc.Rows)
				}
				if page.Next != "" || dc.Count > len(dc.Rows) {
					dc.Truncated = true
				}
				res[c] = dc
			}
			ordered := make([]string, 0, len(res))
			for c := range res {
				ordered = append(ordered, c)
			}
			sort.Slice(ordered, func(i, j int) bool { return classIdx(ordered[i]) < classIdx(ordered[j]) })
			out["classes"] = res // map keys are emitted sorted; "order" gives the review order
			out["order"] = ordered
			return a.emitValue(out)
		},
	}
	cmd.Flags().IntVar(&limit, "limit", 10, "rows listed per class")
	cmd.Flags().StringArrayVar(&actions, "action", nil, "only this class (repeatable): "+strings.Join(importClasses, ", "))
	cmd.Flags().BoolVar(&raw, "raw", false, "print the server's diff pages (use with --filter/--page-size/--page-count)")
	l.bind(cmd, false)
	return cmd
}

func classIdx(c string) int {
	for i, x := range importClasses {
		if x == c {
			return i
		}
	}
	return len(importClasses)
}

// ---------------------------------------------------------------- apply / rollback

func (a *App) importApply() *cobra.Command {
	return a.importAction("apply", "Merge a staged job into the workflows (admin; asks for confirmation)",
		[]string{"APPLIED", "FAILED"})
}

func (a *App) importRollback() *cobra.Command {
	return a.importAction("rollback", "Roll back a whole applied job (admin; asks for confirmation)",
		[]string{"ROLLED_BACK", "FAILED"})
}

func (a *App) isTTY() bool {
	if a.IsTerminal != nil {
		return a.IsTerminal()
	}
	f, ok := a.Stdin.(*os.File)
	if !ok {
		return false
	}
	st, err := f.Stat()
	return err == nil && st.Mode()&os.ModeCharDevice != 0
}

func (a *App) importAction(verb, short string, done []string) *cobra.Command {
	var w writeFlags
	var yes, noWait bool
	var wait time.Duration
	cmd := &cobra.Command{
		Use: verb + " JOB [--yes] [--dry-run]", Short: short, Args: exactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			path, err := a.importJobPath(args[0])
			if err != nil {
				return err
			}
			o, err := w.options()
			if err != nil {
				return err
			}
			if !w.dryRun {
				if !yes && !a.isTTY() {
					return usagef("import %s changes data: pass --yes to confirm (non-interactive), or --dry-run to preview", verb)
				}
				job, _, err := a.getImportJob(path)
				if err != nil {
					return err
				}
				if !yes {
					if !a.confirm(verb, args[0], job) {
						return &cliError{Exit: 1, Status: "CANCELLED", Message: "not confirmed; nothing was changed"}
					}
				} else if job.Summary != nil {
					if n := (*job.Summary)["CONFLICT"] + (*job.Summary)["SUSPECT"]; n > 0 {
						fmt.Fprintf(a.Stderr, "warning: job has %d CONFLICT/SUSPECT row(s); make sure a human reviewed them\n", n)
					}
				}
			}
			raw, err := a.call("POST", path+":"+verb, nil, o)
			if err != nil {
				return err
			}
			var op api.Operation
			if err := json.Unmarshal(raw, &op); err != nil {
				return runtimef("decode operation: %v", err)
			}
			if w.dryRun || op.Done || noWait {
				return a.finishOp(op, raw)
			}
			deadline := time.Now().Add(wait)
			iv := a.PollInterval
			if iv <= 0 {
				iv = 2 * time.Second
			}
			for time.Now().Before(deadline) {
				time.Sleep(iv)
				job, jraw, err := a.getImportJob(path)
				if err != nil {
					return err
				}
				if job.State != nil && contains(done, string(*job.State)) {
					var jm json.RawMessage = jraw
					b, _ := json.Marshal(map[string]any{"name": op.Name, "done": true, "response": jm})
					if *job.State == "FAILED" {
						_ = a.emit(b)
						return &cliError{Exit: 1, Silent: true}
					}
					return a.emit(b)
				}
			}
			_ = a.emit(raw)
			return runtimef("import %s still running after %s; check with `openetl import show %s`", verb, wait, args[0])
		},
	}
	cmd.Flags().BoolVar(&yes, "yes", false, "confirm without prompting (required when stdin is not a terminal)")
	cmd.Flags().BoolVar(&noWait, "no-wait", false, "return the Operation immediately instead of waiting for completion")
	cmd.Flags().DurationVar(&wait, "wait", 10*time.Minute, "how long to wait for completion")
	w.bind(cmd)
	return cmd
}

func (a *App) finishOp(op api.Operation, raw []byte) error {
	if op.Done && op.Error != nil {
		return &cliError{Exit: 1, Status: string(op.Error.Error.Status), Message: op.Error.Error.Message, Code: op.Error.Error.Code}
	}
	return a.emit(raw)
}

func (a *App) confirm(verb, job string, j api.ImportJob) bool {
	fmt.Fprintf(a.Stderr, "About to %s import job %s", verb, job)
	if j.Source != nil {
		fmt.Fprintf(a.Stderr, " (source %s)", *j.Source)
	}
	if j.Summary != nil {
		var parts []string
		for _, c := range importClasses {
			if n := (*j.Summary)[c]; n > 0 {
				parts = append(parts, fmt.Sprintf("%s=%d", c, n))
			}
		}
		fmt.Fprintf(a.Stderr, ": %s", strings.Join(parts, " "))
	}
	fmt.Fprint(a.Stderr, "\nProceed? [y/N] ")
	line, _ := bufio.NewReader(a.Stdin).ReadString('\n')
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "y", "yes":
		return true
	}
	return false
}
