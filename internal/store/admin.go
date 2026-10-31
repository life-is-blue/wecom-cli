package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/life-is-blue/openetl-cli/internal/api"
	"github.com/life-is-blue/openetl-cli/internal/core"
)

func invalidArg(reason, format string, args ...any) *core.Error {
	return core.NewError(core.StatusInvalidArgument, reason, format, args...)
}

var accountRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

// ValidAccountID reports whether s is a legal account user name.
func ValidAccountID(s string) bool { return accountRe.MatchString(s) }

func pageSizeOf(n int) (int, error) {
	if n < 0 {
		return 0, invalidArg("INVALID_PAGE_SIZE", "page_size must not be negative")
	}
	if n == 0 {
		return defaultPageSize, nil
	}
	if n > maxPageSize {
		return maxPageSize, nil
	}
	return n, nil
}

// Admin lists use the same signed, query-bound tokens as every other list (INV-24). The position of
// a members page is the last account name (hex in the token's kind field), of a tokens page the last id.
func membersBinding(p Project) string     { return fmt.Sprintf("members\x00%d", p.ID) }
func tokensBinding(account string) string { return "tokens\x00" + account }

// ---------------------------------------------------------------- members

func renderMember(p Project, user, role, side string, ct time.Time) api.Member {
	return api.Member{
		Name:       ptr("projects/" + p.Name + "/members/" + user),
		Account:    ptr(accountName(user)),
		Role:       api.Role(strings.ToUpper(role)),
		Side:       api.MemberSide(strings.ToUpper(side)),
		CreateTime: ptr(ct),
	}
}

// ListMembers pages through the members of a project ordered by account name.
func (s *Store) ListMembers(ctx context.Context, p Project, pageSize int, token string) ([]api.Member, string, error) {
	size, err := pageSizeOf(pageSize)
	if err != nil {
		return nil, "", err
	}
	after := ""
	if token != "" {
		kind, _, err := openPageToken(token, membersBinding(p))
		if err != nil {
			return nil, "", err
		}
		b, herr := hex.DecodeString(kind)
		if herr != nil {
			return nil, "", badPageToken("invalid page_token")
		}
		after = string(b)
	}
	rows, err := s.DB.QueryContext(ctx, `SELECT user_name, role, side, created_at FROM member
		WHERE project_id = ? AND user_name > ? ORDER BY user_name LIMIT ?`, p.ID, after, size+1)
	if err != nil {
		return nil, "", internalErr(err)
	}
	defer rows.Close()
	out := []api.Member{}
	last, next := "", ""
	for rows.Next() {
		var u, r, sd string
		var ct time.Time
		if err := rows.Scan(&u, &r, &sd, &ct); err != nil {
			return nil, "", internalErr(err)
		}
		if len(out) == size {
			next = sealPageToken(hex.EncodeToString([]byte(last)), 0, membersBinding(p))
			break
		}
		out = append(out, renderMember(p, u, r, sd, ct))
		last = u
	}
	return out, next, internalErr(rows.Err())
}

func lastAdminErr() error {
	return core.NewError(core.StatusFailedPrecondition, "LAST_ADMIN", "a project must keep at least one admin")
}

// lockAdmins returns the admin account names of the project, locking those rows.
func lockAdmins(ctx context.Context, tx *sql.Tx, projectID int64) (map[string]bool, error) {
	rows, err := tx.QueryContext(ctx, `SELECT user_name FROM member WHERE project_id = ? AND role = 'admin' FOR UPDATE`, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	m := map[string]bool{}
	for rows.Next() {
		var u string
		if err := rows.Scan(&u); err != nil {
			return nil, err
		}
		m[u] = true
	}
	return m, rows.Err()
}

// accountScopeOK reports whether caller may take over account: every membership of account lies in a
// project caller administers, except for projects in `except` (typically the project being edited).
// An account without memberships is trivially in scope.
func accountScopeOK(ctx context.Context, q execer, caller, account string, except int64) (bool, error) {
	var n int
	err := q.QueryRowContext(ctx, `SELECT COUNT(*) FROM member t
		WHERE t.user_name = ? AND t.project_id <> ? AND NOT EXISTS (
			SELECT 1 FROM member a WHERE a.project_id = t.project_id AND a.user_name = ? AND a.role = 'admin')`,
		account, except, caller).Scan(&n)
	return n == 0, err
}

// SetMember adds a member or changes role/side. Unknown accounts are created as human accounts.
// Agent accounts cannot be admin; the last admin cannot be demoted. An EXISTING account that also
// belongs to projects the caller does not administer can only be edited when it is already a member
// of p: an admin of one project cannot link a foreign account (and so reach its account-wide tokens).
func (s *Store) SetMember(ctx context.Context, p Project, caller, account, role, side string) (*api.Member, error) {
	if !ValidAccountID(account) {
		return nil, invalidArg("INVALID_ACCOUNT", "account %q is not a valid account name", account)
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return nil, internalErr(err)
	}
	defer tx.Rollback()
	now := s.now()
	if _, err := tx.ExecContext(ctx, `INSERT IGNORE INTO account(user_name, kind, created_at) VALUES(?, 'human', ?)`, account, now); err != nil {
		return nil, internalErr(err)
	}
	var already int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM member WHERE project_id = ? AND user_name = ? FOR UPDATE`, p.ID, account).Scan(&already); err != nil {
		return nil, internalErr(err)
	}
	if already == 0 {
		ok, err := accountScopeOK(ctx, tx, caller, account, p.ID)
		if err != nil {
			return nil, internalErr(err)
		}
		if !ok {
			return nil, core.NewError(core.StatusPermissionDenied, "ACCOUNT_IN_OTHER_PROJECT", "account %q belongs to projects you do not administer and cannot be added here", account)
		}
	}
	var kind string
	if err := tx.QueryRowContext(ctx, `SELECT kind FROM account WHERE user_name = ?`, account).Scan(&kind); err != nil {
		return nil, internalErr(err)
	}
	if role == core.RoleAdmin && kind == "agent" {
		return nil, core.NewError(core.StatusFailedPrecondition, "AGENT_CANNOT_BE_ADMIN", "agent accounts cannot be admin")
	}
	admins, err := lockAdmins(ctx, tx, p.ID)
	if err != nil {
		return nil, internalErr(err)
	}
	if admins[account] && role != core.RoleAdmin && len(admins) == 1 {
		return nil, lastAdminErr()
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO member(project_id, user_name, role, side, created_at) VALUES(?,?,?,?,?)
		ON DUPLICATE KEY UPDATE role = VALUES(role), side = VALUES(side)`, p.ID, account, role, side, now); err != nil {
		return nil, internalErr(err)
	}
	var ct time.Time
	if err := tx.QueryRowContext(ctx, `SELECT created_at FROM member WHERE project_id = ? AND user_name = ?`, p.ID, account).Scan(&ct); err != nil {
		return nil, internalErr(err)
	}
	if err := tx.Commit(); err != nil {
		return nil, internalErr(err)
	}
	m := renderMember(p, account, role, side, ct)
	return &m, nil
}

// RemoveMember deletes a membership; the last admin cannot be removed.
func (s *Store) RemoveMember(ctx context.Context, p Project, account string) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return internalErr(err)
	}
	defer tx.Rollback()
	admins, err := lockAdmins(ctx, tx, p.ID)
	if err != nil {
		return internalErr(err)
	}
	if admins[account] && len(admins) == 1 {
		return lastAdminErr()
	}
	r, err := tx.ExecContext(ctx, `DELETE FROM member WHERE project_id = ? AND user_name = ?`, p.ID, account)
	if err != nil {
		return internalErr(err)
	}
	if n, _ := r.RowsAffected(); n == 0 {
		return core.NewError(core.StatusNotFound, "NOT_FOUND", "member not found")
	}
	return internalErr(tx.Commit())
}

// ---------------------------------------------------------------- tokens

// CanManageTokens reports whether caller may create, list or revoke the tokens of target. Tokens are
// account-wide, so the caller must administer EVERY project the target belongs to (the target must
// belong to at least one). A caller managing its own account needs the admin role somewhere.
func (s *Store) CanManageTokens(ctx context.Context, caller, target string) (bool, error) {
	var n int
	if caller == target {
		err := s.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM member WHERE user_name = ? AND role = 'admin'`, caller).Scan(&n)
		return n > 0, internalErr(err)
	}
	if err := s.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM member WHERE user_name = ?`, target).Scan(&n); err != nil {
		return false, internalErr(err)
	}
	if n == 0 {
		return false, nil
	}
	ok, err := accountScopeOK(ctx, s.DB, caller, target, 0)
	return ok, internalErr(err)
}

func tokenName(account string, id int64) string {
	return "accounts/" + account + "/tokens/" + strconv.FormatInt(id, 10)
}

func renderToken(account string, id int64, expires time.Time, revoked sql.NullTime, created time.Time) api.Token {
	t := api.Token{Name: ptr(tokenName(account, id)), ExpireTime: ptr(expires.UTC()), CreateTime: ptr(created.UTC())}
	if revoked.Valid {
		t.RevokeTime = ptr(revoked.Time.UTC())
	}
	return t
}

// ListTokens lists the tokens of an account (hashes and secrets are never returned).
func (s *Store) ListTokens(ctx context.Context, account string, pageSize int, token string) ([]api.Token, string, error) {
	size, err := pageSizeOf(pageSize)
	if err != nil {
		return nil, "", err
	}
	var after int64
	if token != "" {
		_, after, err = openPageToken(token, tokensBinding(account))
		if err != nil {
			return nil, "", err
		}
	}
	rows, err := s.DB.QueryContext(ctx, `SELECT id, expires_at, revoked_at, created_at FROM api_token
		WHERE user_name = ? AND id > ? ORDER BY id LIMIT ?`, account, after, size+1)
	if err != nil {
		return nil, "", internalErr(err)
	}
	defer rows.Close()
	out := []api.Token{}
	var last int64
	next := ""
	for rows.Next() {
		var id int64
		var ex, cr time.Time
		var rv sql.NullTime
		if err := rows.Scan(&id, &ex, &rv, &cr); err != nil {
			return nil, "", internalErr(err)
		}
		if len(out) == size {
			next = sealPageToken("t", last, tokensBinding(account))
			break
		}
		out = append(out, renderToken(account, id, ex, rv, cr))
		last = id
	}
	return out, next, internalErr(rows.Err())
}

// ProjectCount returns the number of projects of the instance.
func (s *Store) ProjectCount(ctx context.Context) (int, error) {
	var n int
	err := s.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM project`).Scan(&n)
	return n, internalErr(err)
}

// AccountExists reports whether the account exists.
func (s *Store) AccountExists(ctx context.Context, account string) (bool, error) {
	var n int
	err := s.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM account WHERE user_name = ?`, account).Scan(&n)
	return n > 0, internalErr(err)
}

// IssueToken creates a token for an existing account and returns it with the plaintext secret
// (shown once; only its hash is stored).
func (s *Store) IssueToken(ctx context.Context, account string, expires time.Time) (*api.Token, error) {
	secret, err := newTokenSecret()
	if err != nil {
		return nil, internalErr(err)
	}
	now := s.now()
	r, err := s.DB.ExecContext(ctx, `INSERT INTO api_token(user_name, token_hash, expires_at, created_at) VALUES(?,?,?,?)`,
		account, HashToken(secret), expires.UTC(), now)
	if err != nil {
		return nil, internalErr(err)
	}
	id, _ := r.LastInsertId()
	t := renderToken(account, id, expires, sql.NullTime{}, now)
	t.Secret = &secret
	return &t, nil
}

// RevokeToken revokes a token of the account; revoking twice is a no-op returning the first time.
func (s *Store) RevokeToken(ctx context.Context, account string, id int64) (*api.Token, error) {
	if _, err := s.DB.ExecContext(ctx, `UPDATE api_token SET revoked_at = ? WHERE id = ? AND user_name = ? AND revoked_at IS NULL`,
		s.now(), id, account); err != nil {
		return nil, internalErr(err)
	}
	var ex, cr time.Time
	var rv sql.NullTime
	err := s.DB.QueryRowContext(ctx, `SELECT expires_at, revoked_at, created_at FROM api_token WHERE id = ? AND user_name = ?`, id, account).Scan(&ex, &rv, &cr)
	if isNoRows(err) {
		return nil, core.NewError(core.StatusNotFound, "NOT_FOUND", "token not found")
	}
	if err != nil {
		return nil, internalErr(err)
	}
	t := renderToken(account, id, ex, rv, cr)
	return &t, nil
}

// ---------------------------------------------------------------- config

var (
	stageKeyRe   = regexp.MustCompile(`^[a-z][a-z0-9_]{0,31}$`)
	reasonKeyRe  = regexp.MustCompile(`^[a-z][a-z0-9_]{0,47}$`)
	settingKeyRe = regexp.MustCompile(`^[a-z0-9][a-z0-9_.-]{0,127}$`)
)

const (
	maxRules        = 500
	maxSettingValue = 4096
)

// ConfigStage, ConfigReason and ConfigRule use the wire format of the OpenAPI Config schema.
type ConfigStage struct {
	Stage  string  `json:"stage"`
	Seq    int     `json:"seq"`
	NameZh *string `json:"name_zh,omitempty"`
	NameEn *string `json:"name_en,omitempty"`
}

type ConfigReason struct {
	Reason       string  `json:"reason"`
	Kind         string  `json:"kind"`
	OwnerSide    *string `json:"owner_side,omitempty"`
	NameZh       *string `json:"name_zh,omitempty"`
	NameEn       *string `json:"name_en,omitempty"`
	NoteRequired *bool   `json:"note_required,omitempty"`
	Active       *bool   `json:"active,omitempty"`
}

type ConfigRule struct {
	KPattern   string  `json:"k_pattern"`
	ToValue    *string `json:"to_value,omitempty"`
	Side       string  `json:"side"`
	ReasonKind *string `json:"reason_kind,omitempty"`
}

// Config is the wire-compatible view of the OpenAPI Config schema (stages, reasons, rules and
// settings are global in this version: every project sees the same configuration).
type Config struct {
	Name     string            `json:"name,omitempty"`
	Etag     string            `json:"etag,omitempty"`
	Stages   []ConfigStage     `json:"stages,omitempty"`
	Reasons  []ConfigReason    `json:"reasons,omitempty"`
	Rules    []ConfigRule      `json:"rules,omitempty"`
	Settings map[string]string `json:"settings"`
}

func opt(ns sql.NullString) *string {
	if !ns.Valid {
		return nil
	}
	v := ns.String
	return &v
}

func upOpt(ns sql.NullString) *string {
	if !ns.Valid {
		return nil
	}
	v := strings.ToUpper(ns.String)
	return &v
}

// GetConfig returns the configuration with an etag derived from its content.
func (s *Store) GetConfig(ctx context.Context, p Project) (*Config, error) {
	c, err := readConfig(ctx, s.DB, p)
	return c, internalErr(err)
}

func readConfig(ctx context.Context, q execer, p Project) (*Config, error) {
	c := &Config{Name: "projects/" + p.Name + "/config", Settings: map[string]string{}}
	rows, err := q.QueryContext(ctx, `SELECT stage_key, seq, name_zh, name_en FROM stage_def ORDER BY seq, stage_key`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var st ConfigStage
		var zh, en string
		if err := rows.Scan(&st.Stage, &st.Seq, &zh, &en); err != nil {
			rows.Close()
			return nil, err
		}
		st.NameZh, st.NameEn = &zh, &en
		c.Stages = append(c.Stages, st)
	}
	rows.Close()
	rows, err = q.QueryContext(ctx, `SELECT reason_key, kind, owner_side, name_zh, name_en, note_required, active FROM reason ORDER BY reason_key`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var r ConfigReason
		var kind, owner, zh, en string
		var nr, act bool
		if err := rows.Scan(&r.Reason, &kind, &owner, &zh, &en, &nr, &act); err != nil {
			rows.Close()
			return nil, err
		}
		ow := strings.ToUpper(owner)
		r.Kind, r.OwnerSide, r.NameZh, r.NameEn, r.NoteRequired, r.Active = strings.ToUpper(kind), &ow, &zh, &en, &nr, &act
		c.Reasons = append(c.Reasons, r)
	}
	rows.Close()
	rows, err = q.QueryContext(ctx, `SELECT k_pattern, to_value, side, reason_kind FROM rule ORDER BY id`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var r ConfigRule
		var tv, rk sql.NullString
		var side string
		if err := rows.Scan(&r.KPattern, &tv, &side, &rk); err != nil {
			rows.Close()
			return nil, err
		}
		r.ToValue, r.Side, r.ReasonKind = opt(tv), strings.ToUpper(side), upOpt(rk)
		c.Rules = append(c.Rules, r)
	}
	rows.Close()
	rows, err = q.QueryContext(ctx, `SELECT k, v FROM setting ORDER BY k`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return nil, err
		}
		c.Settings[k] = v
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	raw, _ := json.Marshal(struct {
		S []ConfigStage
		R []ConfigReason
		U []ConfigRule
		T map[string]string
	}{c.Stages, c.Reasons, c.Rules, c.Settings})
	sum := sha256.Sum256(raw)
	c.Etag = "cfg-" + hex.EncodeToString(sum[:8])
	return c, nil
}

// ValidateRule checks one rule of a config update.
func ValidateRule(r ConfigRule) error {
	if r.KPattern == "" || len(r.KPattern) > 96 {
		return invalidArg("INVALID_RULE", "rule k_pattern must be 1-96 characters")
	}
	for i := 0; i < len(r.KPattern); i++ {
		c := r.KPattern[i]
		if c < 0x20 || c > 0x7e {
			return invalidArg("INVALID_RULE", "rule k_pattern %q must be printable ASCII", r.KPattern)
		}
		if c == '\\' {
			i++ // the escaped character is taken literally
			if i >= len(r.KPattern) {
				return invalidArg("INVALID_RULE", "rule k_pattern %q ends with a dangling backslash", r.KPattern)
			}
			if e := r.KPattern[i]; e < 0x20 || e > 0x7e {
				return invalidArg("INVALID_RULE", "rule k_pattern %q must be printable ASCII", r.KPattern)
			}
		}
	}
	if r.ToValue != nil && (len(*r.ToValue) > 64 || strings.ContainsAny(*r.ToValue, "\x00")) {
		return invalidArg("INVALID_RULE", "rule to_value must be at most 64 characters")
	}
	if !ruleSides[strings.ToLower(r.Side)] {
		return invalidArg("INVALID_RULE", "rule side must be DELIVERY, CUSTOMER or ANY")
	}
	if r.ReasonKind != nil && *r.ReasonKind != "" && !reasonKinds[strings.ToLower(*r.ReasonKind)] {
		return invalidArg("INVALID_RULE", "rule reason_kind must be GATE_OVERRIDE, DISCARD, NOT_APPLICABLE, REJECT or null")
	}
	return nil
}

var (
	reasonKinds = map[string]bool{"gate_override": true, "discard": true, "not_applicable": true, "reject": true}
	ownerSides  = map[string]bool{"delivery": true, "customer": true, "unknown": true}
	ruleSides   = map[string]bool{"delivery": true, "customer": true, "any": true}
)

func lowerPtr(p *string) sql.NullString {
	if p == nil || *p == "" {
		return sql.NullString{}
	}
	return sql.NullString{String: strings.ToLower(*p), Valid: true}
}

// UpdateConfig applies the fields named in mask (stages, reasons, rules, settings):
//   - stages and reasons are upserted by key and never deleted (set active=false to retire a reason);
//   - rules are replaced as a whole (an empty list is rejected: it would disable all rules);
//   - settings are merged; an empty value deletes the key.
//
// A non-empty in.Etag must match the current configuration.
func (s *Store) UpdateConfig(ctx context.Context, p Project, actor string, mask []string, in Config) (*Config, error) {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return nil, internalErr(err)
	}
	defer tx.Rollback()
	cur, err := readConfig(ctx, tx, p)
	if err != nil {
		return nil, internalErr(err)
	}
	if in.Etag != "" && in.Etag != cur.Etag {
		return nil, core.NewError(core.StatusAborted, "ETAG_MISMATCH", "the configuration changed since the supplied etag was read").With("current_etag", cur.Etag)
	}
	for _, f := range mask {
		switch f {
		case "stages":
			err = updateStages(ctx, tx, cur, in.Stages)
		case "reasons":
			err = updateReasons(ctx, tx, cur, in.Reasons)
		case "rules":
			err = replaceRules(ctx, tx, in.Rules)
		case "settings":
			err = updateSettings(ctx, tx, actor, s.now(), in.Settings)
		}
		if err != nil {
			return nil, internalErr(err)
		}
	}
	out, err := readConfig(ctx, tx, p)
	if err != nil {
		return nil, internalErr(err)
	}
	if err := tx.Commit(); err != nil {
		return nil, internalErr(err)
	}
	return out, nil
}

func updateStages(ctx context.Context, tx *sql.Tx, cur *Config, in []ConfigStage) error {
	type st struct {
		seq    int
		zh, en string
	}
	res := map[string]st{}
	for _, c := range cur.Stages {
		res[c.Stage] = st{c.Seq, *c.NameZh, *c.NameEn}
	}
	seen := map[string]bool{}
	for _, c := range in {
		if !stageKeyRe.MatchString(c.Stage) {
			return invalidArg("INVALID_STAGE", "stage key %q must match %s", c.Stage, stageKeyRe)
		}
		if seen[c.Stage] {
			return invalidArg("INVALID_STAGE", "stage %q is listed twice", c.Stage)
		}
		seen[c.Stage] = true
		if c.Seq < 1 || c.Seq > 255 {
			return invalidArg("INVALID_STAGE", "stage %q: seq must be 1-255", c.Stage)
		}
		v, ok := res[c.Stage]
		if !ok {
			v = st{zh: c.Stage, en: c.Stage}
		}
		v.seq = c.Seq
		if c.NameZh != nil && *c.NameZh != "" {
			v.zh = *c.NameZh
		}
		if c.NameEn != nil && *c.NameEn != "" {
			v.en = *c.NameEn
		}
		if len(v.zh) > 64 || len(v.en) > 64 {
			return invalidArg("INVALID_STAGE", "stage %q: names must be at most 64 bytes", c.Stage)
		}
		res[c.Stage] = v
	}
	used := map[int]string{}
	for k, v := range res {
		if o, dup := used[v.seq]; dup {
			return invalidArg("INVALID_STAGE", "stages %q and %q share seq %d", o, k, v.seq)
		}
		used[v.seq] = k
	}
	for _, c := range in {
		v := res[c.Stage]
		if _, err := tx.ExecContext(ctx, `INSERT INTO stage_def(stage_key, seq, name_zh, name_en) VALUES(?,?,?,?)
			ON DUPLICATE KEY UPDATE seq = VALUES(seq), name_zh = VALUES(name_zh), name_en = VALUES(name_en)`, c.Stage, v.seq, v.zh, v.en); err != nil {
			return err
		}
	}
	return nil
}

func updateReasons(ctx context.Context, tx *sql.Tx, cur *Config, in []ConfigReason) error {
	have := map[string]ConfigReason{}
	for _, r := range cur.Reasons {
		have[r.Reason] = r
	}
	seen := map[string]bool{}
	for _, r := range in {
		if !reasonKeyRe.MatchString(r.Reason) {
			return invalidArg("INVALID_REASON", "reason key %q must match %s", r.Reason, reasonKeyRe)
		}
		if seen[r.Reason] {
			return invalidArg("INVALID_REASON", "reason %q is listed twice", r.Reason)
		}
		seen[r.Reason] = true
		kind := strings.ToLower(r.Kind)
		if !reasonKinds[kind] {
			return invalidArg("INVALID_REASON", "reason %q: kind must be GATE_OVERRIDE, DISCARD, NOT_APPLICABLE or REJECT", r.Reason)
		}
		old, exists := have[r.Reason]
		owner := "unknown"
		if exists {
			owner = strings.ToLower(*old.OwnerSide)
		}
		if r.OwnerSide != nil && *r.OwnerSide != "" && !strings.HasSuffix(*r.OwnerSide, "UNSPECIFIED") {
			owner = strings.ToLower(*r.OwnerSide)
		}
		if !ownerSides[owner] {
			return invalidArg("INVALID_REASON", "reason %q: owner_side must be DELIVERY, CUSTOMER or UNKNOWN", r.Reason)
		}
		zh, en, nr, act := r.Reason, r.Reason, false, true
		if exists {
			zh, en, nr, act = *old.NameZh, *old.NameEn, *old.NoteRequired, *old.Active
		}
		if r.NameZh != nil && *r.NameZh != "" {
			zh = *r.NameZh
		}
		if r.NameEn != nil && *r.NameEn != "" {
			en = *r.NameEn
		}
		if r.NoteRequired != nil {
			nr = *r.NoteRequired
		}
		if r.Active != nil {
			act = *r.Active
		}
		if len(zh) > 64 || len(en) > 64 {
			return invalidArg("INVALID_REASON", "reason %q: names must be at most 64 bytes", r.Reason)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO reason(reason_key, kind, owner_side, name_zh, name_en, note_required, active) VALUES(?,?,?,?,?,?,?)
			ON DUPLICATE KEY UPDATE kind = VALUES(kind), owner_side = VALUES(owner_side), name_zh = VALUES(name_zh), name_en = VALUES(name_en),
			note_required = VALUES(note_required), active = VALUES(active)`, r.Reason, kind, owner, zh, en, nr, act); err != nil {
			return err
		}
	}
	return nil
}

type ruleKey struct {
	pat, to, side, kind string
	toNull, kindNull    bool
}

func keyOfRule(r ConfigRule) ruleKey {
	k := ruleKey{pat: r.KPattern, side: strings.ToLower(r.Side), toNull: r.ToValue == nil, kindNull: r.ReasonKind == nil || *r.ReasonKind == ""}
	if r.ToValue != nil {
		k.to = *r.ToValue
	}
	if !k.kindNull {
		k.kind = strings.ToLower(*r.ReasonKind)
	}
	return k
}

// replaceRules makes the rule table equal to in, keeping unchanged rows (and their ids).
func replaceRules(ctx context.Context, tx *sql.Tx, in []ConfigRule) error {
	if len(in) == 0 {
		return invalidArg("RULES_EMPTY", "rules must not be empty: that would disable every rule")
	}
	if len(in) > maxRules {
		return invalidArg("INVALID_RULE", "at most %d rules", maxRules)
	}
	want := map[ruleKey]int{}
	for _, r := range in {
		if err := ValidateRule(r); err != nil {
			return err
		}
		want[keyOfRule(r)]++
	}
	rows, err := tx.QueryContext(ctx, `SELECT id, k_pattern, to_value, side, reason_kind FROM rule ORDER BY id FOR UPDATE`)
	if err != nil {
		return err
	}
	var drop []int64
	for rows.Next() {
		var id int64
		var tv, rk sql.NullString
		var pat, side string
		if err := rows.Scan(&id, &pat, &tv, &side, &rk); err != nil {
			rows.Close()
			return err
		}
		k := ruleKey{pat: pat, side: side, to: tv.String, toNull: !tv.Valid, kind: rk.String, kindNull: !rk.Valid}
		if want[k] > 0 {
			want[k]--
		} else {
			drop = append(drop, id)
		}
	}
	rows.Close()
	if len(drop) > 0 {
		if _, err := tx.ExecContext(ctx, `DELETE FROM rule WHERE id IN (`+placeholders(len(drop))+`)`, idArgs(drop)...); err != nil {
			return err
		}
	}
	for _, r := range in {
		k := keyOfRule(r)
		if want[k] == 0 {
			continue
		}
		want[k]--
		var tv sql.NullString
		if r.ToValue != nil {
			tv = sql.NullString{String: *r.ToValue, Valid: true}
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO rule(k_pattern, to_value, side, reason_kind) VALUES(?,?,?,?)`,
			r.KPattern, tv, k.side, lowerPtr(r.ReasonKind)); err != nil {
			return err
		}
	}
	return nil
}

func updateSettings(ctx context.Context, tx *sql.Tx, actor string, now time.Time, in map[string]string) error {
	for k, v := range in {
		if !settingKeyRe.MatchString(k) {
			return invalidArg("INVALID_SETTING", "setting key %q must match %s", k, settingKeyRe)
		}
		if len(v) > maxSettingValue {
			return invalidArg("INVALID_SETTING", "setting %q: value must be at most %d bytes", k, maxSettingValue)
		}
	}
	for k, v := range in {
		var err error
		if v == "" {
			_, err = tx.ExecContext(ctx, `DELETE FROM setting WHERE k = ?`, k)
		} else {
			_, err = tx.ExecContext(ctx, `INSERT INTO setting(k, v, updated_by, updated_at) VALUES(?,?,?,?)
				ON DUPLICATE KEY UPDATE v = VALUES(v), updated_by = VALUES(updated_by), updated_at = VALUES(updated_at)`, k, v, actor, now)
		}
		if err != nil {
			return err
		}
	}
	return nil
}
