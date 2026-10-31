package gateway

import (
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/life-is-blue/openetl-cli/internal/api"
	"github.com/life-is-blue/openetl-cli/internal/core"
	"github.com/life-is-blue/openetl-cli/internal/store"
)

const (
	defaultTokenTTL = 365 * 24 * time.Hour
	maxTokenTTL     = 10 * 365 * 24 * time.Hour
)

// routeAdmin serves members and config of a project; the caller is already known to be an admin.
func (s *Server) routeAdmin(q *request) error {
	m, n := q.r.Method, len(q.segs)
	if q.verb != "" {
		return notFound("route")
	}
	switch q.segs[2] {
	case "members":
		switch n {
		case 3:
			if m == http.MethodGet {
				return s.listMembers(q)
			}
		case 4:
			switch m {
			case http.MethodPut:
				return s.setMember(q, store.AccountID(q.segs[3]))
			case http.MethodDelete:
				return s.removeMember(q, store.AccountID(q.segs[3]))
			}
		default:
			return notFound("route")
		}
		return methodNotAllowed(q)
	case "config":
		if n != 3 {
			return notFound("route")
		}
		switch m {
		case http.MethodGet:
			c, err := s.Store.GetConfig(q.r.Context(), q.pr.project())
			if err != nil {
				return err
			}
			return s.respond(q, c)
		case http.MethodPatch:
			return s.updateConfig(q)
		}
		return methodNotAllowed(q)
	}
	return notFound("route")
}

func (s *Server) listMembers(q *request) error {
	ps := 0
	if v := q.query("page_size"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			return invalid("INVALID_PAGE_SIZE", "page_size must be a non-negative integer")
		}
		ps = n
	}
	ms, next, err := s.Store.ListMembers(q.r.Context(), q.pr.project(), ps, q.query("page_token"))
	if err != nil {
		return err
	}
	resp := api.ListMembersResponse{Members: &ms}
	if next != "" {
		resp.NextPageToken = &next
	}
	return s.respond(q, resp)
}

func (s *Server) setMember(q *request, account string) error {
	var body api.Member
	if err := q.decodeBody(&body, false); err != nil {
		return err
	}
	var role, side string
	switch body.Role {
	case api.ADMIN:
		role = core.RoleAdmin
	case api.EDITOR:
		role = core.RoleEditor
	case api.VIEWER:
		role = core.RoleViewer
	default:
		return invalid("INVALID_ROLE", "role must be VIEWER, EDITOR or ADMIN")
	}
	switch body.Side {
	case api.MemberSideDELIVERY:
		side = core.SideDelivery
	case api.MemberSideCUSTOMER:
		side = core.SideCustomer
	default:
		return invalid("INVALID_SIDE", "side must be DELIVERY or CUSTOMER")
	}
	m, err := s.Store.SetMember(q.r.Context(), q.pr.project(), q.pr.Account.UserName, account, role, side)
	if err != nil {
		return err
	}
	s.Log.Printf("admin: %s set member %s of project %s to %s/%s", q.pr.Account.UserName, account, q.project, role, side)
	return s.respond(q, m)
}

func (s *Server) removeMember(q *request, account string) error {
	if err := s.Store.RemoveMember(q.r.Context(), q.pr.project(), account); err != nil {
		return err
	}
	s.Log.Printf("admin: %s removed member %s from project %s", q.pr.Account.UserName, account, q.project)
	q.w.WriteHeader(http.StatusNoContent)
	return nil
}

// EnvAllowGlobalConfig lets config updates through on instances with several projects. Stages,
// reasons, rules and settings are global (not per project), so any admin edit affects every project.
const EnvAllowGlobalConfig = "OPENETL_ALLOW_GLOBAL_CONFIG"

func (s *Server) updateConfig(q *request) error {
	if q.pr.Member.Role != core.RoleAdmin {
		return core.NewError(core.StatusPermissionDenied, "ROLE_INSUFFICIENT", "this call requires the admin role")
	}
	if os.Getenv(EnvAllowGlobalConfig) != "1" {
		n, err := s.Store.ProjectCount(q.r.Context())
		if err != nil {
			return err
		}
		if n > 1 {
			return core.NewError(core.StatusPermissionDenied, "GLOBAL_CONFIG_SHARED",
				"the configuration is shared by all %d projects of this instance; updating it is disabled (set %s=1 on the gateway to allow it)", n, EnvAllowGlobalConfig)
		}
	}
	mask, err := parseMask(q.query("update_mask"), []string{"stages", "reasons", "rules", "settings"})
	if err != nil {
		return err
	}
	var top []string
	seen := map[string]bool{}
	for _, f := range mask {
		t, _, _ := strings.Cut(f, ".")
		if !seen[t] {
			seen[t] = true
			top = append(top, t)
		}
	}
	var body store.Config
	if err := q.decodeBody(&body, false); err != nil {
		return err
	}
	c, err := s.Store.UpdateConfig(q.r.Context(), q.pr.project(), q.pr.Account.UserName, top, body)
	if err != nil {
		return err
	}
	s.Log.Printf("admin: %s updated config fields %s of project %s", q.pr.Account.UserName, strings.Join(top, ","), q.project)
	return s.respond(q, c)
}

// ---------------------------------------------------------------- accounts / tokens

// routeAccounts serves /v1/accounts/{a}/tokens[...]. The caller must be an admin of a project that
// the target account belongs to (so project admins manage tokens of their own members).
func (s *Server) routeAccounts(q *request) error {
	m, n := q.r.Method, len(q.segs)
	if n < 3 || q.segs[2] != "tokens" {
		return notFound("route")
	}
	account := store.AccountID(q.segs[1])
	if !store.ValidAccountID(account) {
		return notFound("account")
	}
	ok, err := s.Store.CanManageTokens(q.r.Context(), q.pr.Account.UserName, account)
	if err != nil {
		return err
	}
	if !ok {
		return core.NewError(core.StatusPermissionDenied, "ROLE_INSUFFICIENT", "token management requires the admin role in every project the account belongs to")
	}
	switch n {
	case 3:
		if q.verb != "" {
			return notFound("route")
		}
		switch m {
		case http.MethodGet:
			return s.listTokens(q, account)
		case http.MethodPost:
			return s.createToken(q, account)
		}
		return methodNotAllowed(q)
	case 4:
		id, err := strconv.ParseInt(q.segs[3], 10, 64)
		if err != nil || id <= 0 || q.verb != "revoke" {
			return notFound("token")
		}
		if m != http.MethodPost {
			return methodNotAllowed(q)
		}
		t, err := s.Store.RevokeToken(q.r.Context(), account, id)
		if err != nil {
			return err
		}
		s.Log.Printf("admin: %s revoked token %d of %s", q.pr.Account.UserName, id, account)
		return s.respond(q, t)
	}
	return notFound("route")
}

func (s *Server) listTokens(q *request, account string) error {
	ps := 0
	if v := q.query("page_size"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			return invalid("INVALID_PAGE_SIZE", "page_size must be a non-negative integer")
		}
		ps = n
	}
	ts, next, err := s.Store.ListTokens(q.r.Context(), account, ps, q.query("page_token"))
	if err != nil {
		return err
	}
	resp := api.ListTokensResponse{Tokens: &ts}
	if next != "" {
		resp.NextPageToken = &next
	}
	return s.respond(q, resp)
}

func (s *Server) createToken(q *request, account string) error {
	var body api.Token
	if err := q.decodeBody(&body, true); err != nil {
		return err
	}
	now := time.Now().UTC()
	exp := now.Add(defaultTokenTTL)
	if body.ExpireTime != nil {
		exp = body.ExpireTime.UTC()
		if !exp.After(now) || exp.After(now.Add(maxTokenTTL)) {
			return invalid("INVALID_EXPIRE_TIME", "expire_time must be in the future and within 10 years")
		}
	}
	if ok, err := s.Store.AccountExists(q.r.Context(), account); err != nil {
		return err
	} else if !ok {
		return notFound("account")
	}
	t, err := s.Store.IssueToken(q.r.Context(), account, exp)
	if err != nil {
		return err
	}
	s.Log.Printf("admin: %s created a token for %s", q.pr.Account.UserName, account) // never log the secret
	q.w.Header().Set("Cache-Control", "no-store")
	return s.respond(q, t)
}
