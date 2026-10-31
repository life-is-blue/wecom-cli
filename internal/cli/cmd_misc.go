package cli

import (
	_ "embed"
	"encoding/json"
	"net/url"

	"github.com/spf13/cobra"

	"github.com/life-is-blue/openetl-cli/internal/api"
)

//go:embed views.json
var viewsJSON []byte

func (a *App) meCmd() *cobra.Command {
	return &cobra.Command{
		Use: "me", Short: "Show the current identity and project memberships", Args: exactArgs(0),
		RunE: func(*cobra.Command, []string) error {
			out, err := a.call("GET", "/v1/me", nil, nil)
			if err != nil {
				return err
			}
			return a.emit(out)
		},
	}
}

func (a *App) viewsCmd() *cobra.Command {
	return &cobra.Command{
		Use: "views", Short: "List the views and columns that `openetl query` can read", Args: exactArgs(0),
		RunE: func(*cobra.Command, []string) error {
			return a.emit([]byte(`{"views":` + string(viewsJSON) + `}`))
		},
	}
}

func (a *App) queryCmd() *cobra.Command {
	var pageSize int
	var pageToken string
	cmd := &cobra.Command{
		Use: `query "<SELECT ...>" [--page-size N] [--page-count N]`, Short: "Run a read-only SQL SELECT over the documented views (see `openetl views`)",
		Args: exactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			p, err := a.projectID()
			if err != nil {
				return err
			}
			req := api.QueryRequest{Sql: args[0]}
			if pageSize > 0 {
				req.PageSize = &pageSize
			}
			return a.paginate(func(tok string) ([]byte, error) {
				if tok != "" {
					req.PageToken = &tok
				}
				return a.call("POST", "/v1/projects/"+url.PathEscape(p)+":query", nil, req)
			}, pageToken)
		},
	}
	cmd.Flags().IntVar(&pageSize, "page-size", 0, "rows per page (max 1000)")
	cmd.Flags().StringVar(&pageToken, "page-token", "", "continue from a next_page_token")
	return cmd
}

// ---------------------------------------------------------------- auth

func (a *App) authCmd() *cobra.Command {
	return group("auth", "Log in, inspect and clear the saved API token", a.authLogin(), a.authStatus(), a.authLogout())
}

func (a *App) authLogin() *cobra.Command {
	var fromStdin bool
	cmd := &cobra.Command{
		Use: "login [--token-stdin]", Short: "Verify an API token via GET /v1/me and save it encrypted",
		Args: exactArgs(0),
		RunE: func(cmd *cobra.Command, _ []string) error {
			tok, err := a.readToken(fromStdin)
			if err != nil {
				return err
			}
			r, err := a.resolve()
			if err != nil {
				return err
			}
			if r.base == "" {
				return usagef("base URL required: pass --base-url or set %s", EnvBaseURL)
			}
			raw, err := a.callWith(r.base, tok, "GET", "/v1/me", nil, nil)
			if err != nil {
				return err
			}
			var me api.Me
			_ = json.Unmarshal(raw, &me)
			if err := a.store().Save(credentialsOf(r.base, r.project, tok)); err != nil {
				return runtimef("save credentials: %v", err)
			}
			return a.emitValue(map[string]any{
				"logged_in": true, "account": me.Account, "base_url": r.base, "stored_in": a.store().Path(),
			})
		},
	}
	cmd.Flags().BoolVar(&fromStdin, "token-stdin", false, "read the token from stdin (default when stdin is not a terminal)")
	return cmd
}

func (a *App) authStatus() *cobra.Command {
	return &cobra.Command{
		Use: "status", Short: "Show whether a valid token is available (exit 1 if not)", Args: exactArgs(0),
		RunE: func(*cobra.Command, []string) error {
			r, err := a.resolve()
			if err != nil {
				return err
			}
			st := map[string]any{"authenticated": false, "token_source": r.tokenSource, "base_url": r.base, "project": r.project}
			if r.tokenErr != nil {
				return r.tokenErr
			}
			if r.token == "" || r.base == "" {
				_ = a.emitValue(st)
				return &cliError{Exit: 1, Silent: true}
			}
			raw, err := a.call("GET", "/v1/me", nil, nil)
			if err != nil {
				var ce *cliError
				if asCLI(err, &ce) && ce.Code == 401 {
					st["error"] = ce.Message
					_ = a.emitValue(st)
					return &cliError{Exit: 1, Silent: true}
				}
				return err
			}
			var me api.Me
			_ = json.Unmarshal(raw, &me)
			st["authenticated"] = true
			st["account"] = me.Account
			if me.Kind != nil {
				st["kind"] = me.Kind
			}
			return a.emitValue(st)
		},
	}
}

func (a *App) authLogout() *cobra.Command {
	return &cobra.Command{
		Use: "logout", Short: "Delete the saved credentials", Args: exactArgs(0),
		RunE: func(*cobra.Command, []string) error {
			if err := a.store().Delete(); err != nil {
				return runtimef("%v", err)
			}
			return a.emitValue(map[string]any{"logged_out": true})
		},
	}
}
