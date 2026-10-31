package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/life-is-blue/openetl-cli/internal/api"
)

// localDryRun reports the request a write would send, without sending it. It
// serves the admin endpoints that have no validate_only parameter.
func (a *App) localDryRun(method, path string, q url.Values, body any) error {
	req := map[string]any{"method": method, "path": path}
	if len(q) > 0 {
		req["query"] = q.Encode()
	}
	if body != nil {
		req["body"] = body
	}
	return a.emitValue(map[string]any{"dry_run": true, "validated": false,
		"note": "the endpoint has no validate_only; nothing was sent", "request": req})
}

// lastSegment turns "accounts/alice", "projects/p/members/alice" or "alice" into "alice".
func lastSegment(in string) string {
	in = strings.Trim(strings.TrimSpace(in), "/")
	if i := strings.LastIndex(in, "/"); i >= 0 {
		in = in[i+1:]
	}
	return in
}

func (a *App) adminDryRunOnly(w *writeFlags, what string) error {
	if w.etag != "" || w.reason != "" || w.note != "" {
		return usagef("--etag, --reason and --note are not supported by %s (the API has no field for them)", what)
	}
	return nil
}

// ---------------------------------------------------------------- member

func (a *App) memberCmd() *cobra.Command {
	return group("member", "List, add and remove project members (admin)", a.memberList(), a.memberAdd(), a.memberRemove())
}

func (a *App) memberPath(account string) (string, error) {
	p, err := a.projectID()
	if err != nil {
		return "", err
	}
	path := "/v1/projects/" + url.PathEscape(p) + "/members"
	if account != "" {
		id := strings.ToLower(lastSegment(account))
		if !idRe.MatchString(id) {
			return "", usagef("invalid account %q", account)
		}
		path += "/" + id
	}
	return path, nil
}

func (a *App) memberList() *cobra.Command {
	var l listFlags
	cmd := &cobra.Command{
		Use: "list [--page-count N]", Short: "List project members", Args: exactArgs(0),
		RunE: func(*cobra.Command, []string) error {
			path, err := a.memberPath("")
			if err != nil {
				return err
			}
			return a.listCall(path, &l, "")
		},
	}
	l.bind(cmd, false)
	return cmd
}

func (a *App) memberAdd() *cobra.Command {
	var w writeFlags
	var role, side string
	cmd := &cobra.Command{
		Use: "add ACCOUNT --role viewer|editor|admin --side delivery|customer", Short: "Add a member or change role/side (idempotent PUT)",
		Args: exactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			if err := a.adminDryRunOnly(&w, "member add"); err != nil {
				return err
			}
			r := api.Role(strings.ToUpper(role))
			if role == "" || (r != api.VIEWER && r != api.EDITOR && r != api.ADMIN) {
				return usagef("--role must be viewer, editor or admin")
			}
			s := api.MemberSide(strings.ToUpper(side))
			if side == "" || (s != api.MemberSideDELIVERY && s != api.MemberSideCUSTOMER) {
				return usagef("--side must be delivery or customer")
			}
			path, err := a.memberPath(args[0])
			if err != nil {
				return err
			}
			body := api.Member{Role: r, Side: s}
			if w.dryRun {
				return a.localDryRun("PUT", path, nil, body)
			}
			out, err := a.call("PUT", path, nil, body)
			if err != nil {
				return err
			}
			return a.emit(out)
		},
	}
	cmd.Flags().StringVar(&role, "role", "", "viewer, editor or admin (agents cannot be admin)")
	cmd.Flags().StringVar(&side, "side", "", "delivery or customer")
	w.bind(cmd)
	return cmd
}

func (a *App) memberRemove() *cobra.Command {
	var w writeFlags
	cmd := &cobra.Command{
		Use: "remove ACCOUNT", Short: "Remove a member", Args: exactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			if err := a.adminDryRunOnly(&w, "member remove"); err != nil {
				return err
			}
			path, err := a.memberPath(args[0])
			if err != nil {
				return err
			}
			if w.dryRun {
				return a.localDryRun("DELETE", path, nil, nil)
			}
			if _, err := a.call("DELETE", path, nil, nil); err != nil {
				return err
			}
			return a.emitValue(map[string]any{"removed": true, "member": strings.TrimPrefix(path, "/v1/")})
		},
	}
	w.bind(cmd)
	return cmd
}

// ---------------------------------------------------------------- token

func (a *App) tokenCmd() *cobra.Command {
	return group("token", "Create, list and revoke API tokens (admin)", a.tokenCreate(), a.tokenList(), a.tokenRevoke())
}

// accountOrMe resolves --account, defaulting to the caller (GET /v1/me).
func (a *App) accountOrMe(flag string) (string, error) {
	if flag != "" {
		id := strings.ToLower(lastSegment(flag))
		if !idRe.MatchString(id) {
			return "", usagef("invalid account %q", flag)
		}
		return id, nil
	}
	raw, err := a.call("GET", "/v1/me", nil, nil)
	if err != nil {
		return "", err
	}
	var me api.Me
	if json.Unmarshal(raw, &me) != nil || me.Account == nil || *me.Account == "" {
		return "", runtimef("cannot determine the current account; pass --account")
	}
	return lastSegment(*me.Account), nil
}

func (a *App) tokenCreate() *cobra.Command {
	var w writeFlags
	var account, expire string
	var ttl time.Duration
	cmd := &cobra.Command{
		Use: "create [--account ACCOUNT] [--ttl 720h | --expire-time RFC3339]", Short: "Create a token; the secret is printed once and never stored by the CLI",
		Args: exactArgs(0),
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := a.adminDryRunOnly(&w, "token create"); err != nil {
				return err
			}
			body := api.Token{}
			switch {
			case expire != "" && ttl > 0:
				return usagef("--ttl and --expire-time are mutually exclusive")
			case expire != "":
				t, err := time.Parse(time.RFC3339, expire)
				if err != nil {
					return usagef("--expire-time must be RFC 3339")
				}
				body.ExpireTime = &t
			case ttl > 0:
				t := time.Now().UTC().Add(ttl).Truncate(time.Second)
				body.ExpireTime = &t
			}
			acc, err := a.accountOrMe(account)
			if err != nil {
				return err
			}
			path := "/v1/accounts/" + acc + "/tokens"
			if w.dryRun {
				return a.localDryRun("POST", path, nil, body)
			}
			out, err := a.call("POST", path, nil, body)
			if err != nil {
				return err
			}
			fmt.Fprintln(a.Stderr, "note: the token secret is shown only once; copy it now. The CLI does not store it.")
			if a.output != "" { // a secret file is private
				if err := os.WriteFile(a.output, append(compact(out), '\n'), 0o600); err != nil {
					return runtimef("write %s: %v", a.output, err)
				}
				return a.emitValue(map[string]any{"output": a.output, "bytes": len(out)})
			}
			return a.emit(out)
		},
	}
	cmd.Flags().StringVar(&account, "account", "", "account to mint the token for (default: the caller)")
	cmd.Flags().DurationVar(&ttl, "ttl", 0, "token lifetime, e.g. 720h (default: server decides)")
	cmd.Flags().StringVar(&expire, "expire-time", "", "absolute expiry, RFC 3339")
	w.bind(cmd)
	return cmd
}

func (a *App) tokenList() *cobra.Command {
	var l listFlags
	var account string
	cmd := &cobra.Command{
		Use: "list [--account ACCOUNT] [--page-count N]", Short: "List tokens of an account (no secrets)", Args: exactArgs(0),
		RunE: func(*cobra.Command, []string) error {
			acc, err := a.accountOrMe(account)
			if err != nil {
				return err
			}
			return a.listCall("/v1/accounts/"+acc+"/tokens", &l, "")
		},
	}
	l.bind(cmd, false)
	cmd.Flags().StringVar(&account, "account", "", "account (default: the caller)")
	return cmd
}

func (a *App) tokenRevoke() *cobra.Command {
	var w writeFlags
	var account string
	cmd := &cobra.Command{
		Use: "revoke TOKEN_ID [--account ACCOUNT]", Short: "Revoke a token (id or accounts/alice/tokens/9)", Args: exactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			if err := a.adminDryRunOnly(&w, "token revoke"); err != nil {
				return err
			}
			id := lastSegment(args[0])
			if !idRe.MatchString(strings.ToLower(id)) {
				return usagef("invalid token id %q", args[0])
			}
			acc := account
			if acc == "" && strings.Contains(args[0], "accounts/") {
				segs := strings.Split(strings.Trim(args[0], "/"), "/")
				for i := 0; i+1 < len(segs); i++ {
					if segs[i] == "accounts" {
						acc = segs[i+1]
					}
				}
			}
			acc, err := a.accountOrMe(acc)
			if err != nil {
				return err
			}
			path := "/v1/accounts/" + acc + "/tokens/" + id + ":revoke"
			if w.dryRun {
				return a.localDryRun("POST", path, nil, nil)
			}
			out, err := a.call("POST", path, nil, map[string]any{})
			if err != nil {
				return err
			}
			return a.emit(out)
		},
	}
	cmd.Flags().StringVar(&account, "account", "", "account owning the token (default: from the name, else the caller)")
	w.bind(cmd)
	return cmd
}

// ---------------------------------------------------------------- config

var configSections = []string{"stages", "reasons", "rules", "settings"}

func (a *App) configCmd() *cobra.Command {
	return group("config", "Read and change the project configuration (admin to change)", a.configGet(), a.configSet())
}

func (a *App) configPath() (string, error) {
	p, err := a.projectID()
	if err != nil {
		return "", err
	}
	return "/v1/projects/" + url.PathEscape(p) + "/config", nil
}

func (a *App) configGet() *cobra.Command {
	var section string
	cmd := &cobra.Command{
		Use: "get [--section stages|reasons|rules|settings]", Short: "Get the project configuration", Args: exactArgs(0),
		RunE: func(*cobra.Command, []string) error {
			path, err := a.configPath()
			if err != nil {
				return err
			}
			out, err := a.call("GET", path, nil, nil)
			if err != nil {
				return err
			}
			if section == "" {
				return a.emit(out)
			}
			if !contains(configSections, section) {
				return usagef("--section must be one of %s", strings.Join(configSections, ", "))
			}
			var m map[string]json.RawMessage
			if err := json.Unmarshal(out, &m); err != nil {
				return runtimef("decode config: %v", err)
			}
			v := m[section]
			if len(v) == 0 {
				v = json.RawMessage("null")
			}
			return a.emit(v)
		},
	}
	cmd.Flags().StringVar(&section, "section", "", "print only one section")
	return cmd
}

func contains(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}

// readJSONArg reads JSON from a file, or stdin for "-". The value may be the
// section itself or an object wrapping it under key.
func (a *App) readJSONArg(flag, src, key string) (json.RawMessage, error) {
	var b []byte
	var err error
	if src == "-" {
		b, err = io.ReadAll(a.Stdin)
	} else {
		b, err = os.ReadFile(src)
	}
	if err != nil {
		return nil, usagef("--%s: %v", flag, err)
	}
	var raw json.RawMessage
	if err := json.Unmarshal(b, &raw); err != nil {
		return nil, usagef("--%s: invalid JSON: %v", flag, err)
	}
	var wrap map[string]json.RawMessage
	if json.Unmarshal(raw, &wrap) == nil {
		if v, ok := wrap[key]; ok && key != "settings" {
			return v, nil
		}
		if key == "settings" {
			if v, ok := wrap["settings"]; ok && len(wrap) == 1 {
				return v, nil
			}
		}
	}
	return raw, nil
}

func (a *App) configSet() *cobra.Command {
	var w writeFlags
	var files = map[string]*string{}
	var settingKVs []string
	cmd := &cobra.Command{
		Use: "set [--stages FILE|-] [--reasons FILE|-] [--rules FILE|-] [--settings FILE|-] [--setting k=v]...", Short: "Replace whole config sections (JSON in) or merge single settings",
		Args: exactArgs(0),
		RunE: func(cmd *cobra.Command, _ []string) error {
			if w.reason != "" || w.note != "" {
				return usagef("--reason and --note are not supported by config set (the API has no field for them)")
			}
			path, err := a.configPath()
			if err != nil {
				return err
			}
			body := map[string]any{}
			var mask []string
			stdinUsed := 0
			for _, sec := range configSections {
				src := *files[sec]
				if src == "" {
					continue
				}
				if src == "-" {
					if stdinUsed++; stdinUsed > 1 {
						return usagef("only one section can be read from stdin")
					}
				}
				v, err := a.readJSONArg(sec, src, sec)
				if err != nil {
					return err
				}
				body[sec] = v
				mask = append(mask, sec)
			}
			kv, err := parseKV(settingKVs, "setting")
			if err != nil {
				return err
			}
			etag := w.etag
			if len(kv) > 0 {
				if _, whole := body["settings"]; whole {
					return usagef("--settings and --setting are mutually exclusive")
				}
				// settings is replaced as a whole by update_mask: merge into the current map.
				cur, err := a.call("GET", path, nil, nil)
				if err != nil {
					return err
				}
				var c api.Config
				if err := json.Unmarshal(cur, &c); err != nil {
					return runtimef("decode config: %v", err)
				}
				merged := map[string]string{}
				if c.Settings != nil {
					for k, v := range *c.Settings {
						merged[k] = v
					}
				}
				for k, v := range kv {
					merged[k] = v
				}
				body["settings"] = merged
				mask = append(mask, "settings")
				if etag == "" && c.Etag != nil {
					etag = *c.Etag
				}
			}
			if len(mask) == 0 {
				return usagef("nothing to set: pass --stages, --reasons, --rules, --settings or --setting")
			}
			if etag != "" {
				body["etag"] = etag
			}
			q := url.Values{"update_mask": {strings.Join(mask, ",")}}
			if w.dryRun {
				return a.localDryRun("PATCH", path, q, body)
			}
			out, err := a.call("PATCH", path, q, body)
			if err != nil {
				return err
			}
			return a.emit(out)
		},
	}
	for _, sec := range configSections {
		s := new(string)
		files[sec] = s
		cmd.Flags().StringVar(s, sec, "", sec+" JSON (file or - for stdin); replaces the whole section")
	}
	cmd.Flags().StringArrayVar(&settingKVs, "setting", nil, "settings key=value, merged into the current settings (repeatable)")
	w.bind(cmd)
	return cmd
}
