package store

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/life-is-blue/openetl-cli/internal/core"
)

// TokenPrefix marks openetl API tokens.
const TokenPrefix = "oetl_"

// Account is an authenticated principal (before project membership is resolved).
type Account struct {
	UserName    string
	DisplayName string
	Kind        string // human|agent
}

// Membership of an account in a project.
type Membership struct {
	ProjectID   int64
	ProjectName string
	UserName    string
	Role        string
	Side        string
	CreateTime  time.Time
}

// newTokenSecret returns a fresh random plaintext token.
func newTokenSecret() (string, error) {
	buf := make([]byte, 24)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return TokenPrefix + hex.EncodeToString(buf), nil
}

// HashToken returns the lowercase hex sha256 of a raw token.
func HashToken(raw string) string {
	h := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(h[:])
}

// AuthenticateToken resolves a bearer token to an account; ok=false when unknown, expired or revoked.
func (s *Store) AuthenticateToken(ctx context.Context, raw string) (*Account, bool, error) {
	var a Account
	var dn sql.NullString
	err := s.DB.QueryRowContext(ctx, `SELECT a.user_name, a.display_name, a.kind
		FROM api_token t JOIN account a ON a.user_name = t.user_name
		WHERE t.token_hash = ? AND t.revoked_at IS NULL AND t.expires_at > ?`, HashToken(raw), s.now()).
		Scan(&a.UserName, &dn, &a.Kind)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	a.DisplayName = dn.String
	return &a, true, nil
}

// Memberships lists all project memberships of an account.
func (s *Store) Memberships(ctx context.Context, userName string) ([]Membership, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT m.project_id, p.name, m.user_name, m.role, m.side, m.created_at
		FROM member m JOIN project p ON p.id = m.project_id WHERE m.user_name = ? ORDER BY p.name`, userName)
	if err != nil {
		// column name fallback handled below
		return nil, err
	}
	defer rows.Close()
	var out []Membership
	for rows.Next() {
		var m Membership
		if err := rows.Scan(&m.ProjectID, &m.ProjectName, &m.UserName, &m.Role, &m.Side, &m.CreateTime); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// MemberOf returns the membership of an account in a named project (ok=false when none).
func (s *Store) MemberOf(ctx context.Context, project, userName string) (*Membership, bool, error) {
	var m Membership
	err := s.DB.QueryRowContext(ctx, `SELECT m.project_id, p.name, m.user_name, m.role, m.side, m.created_at
		FROM member m JOIN project p ON p.id = m.project_id WHERE p.name = ? AND m.user_name = ?`, project, userName).
		Scan(&m.ProjectID, &m.ProjectName, &m.UserName, &m.Role, &m.Side, &m.CreateTime)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return &m, true, nil
}

// CreateToken (admin tool) creates project/account/member when missing and returns a new plaintext
// token. Only the hash is stored.
func (s *Store) CreateToken(ctx context.Context, project, account, side, role string, kind string, ttl time.Duration) (string, error) {
	if kind == "" {
		kind = "human"
	}
	if kind == "agent" && role == "admin" {
		return "", core.NewError(core.StatusFailedPrecondition, "AGENT_CANNOT_BE_ADMIN", "agent accounts cannot be admin")
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	now := s.now()
	if _, err := tx.ExecContext(ctx, `INSERT IGNORE INTO project(name, created_at) VALUES(?, ?)`, project, now); err != nil {
		return "", err
	}
	if _, err := tx.ExecContext(ctx, `INSERT IGNORE INTO account(user_name, kind, created_at) VALUES(?, ?, ?)`, account, kind, now); err != nil {
		return "", err
	}
	var pid int64
	if err := tx.QueryRowContext(ctx, `SELECT id FROM project WHERE name=?`, project).Scan(&pid); err != nil {
		return "", err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO member(project_id, user_name, role, side, created_at) VALUES(?,?,?,?,?)
		ON DUPLICATE KEY UPDATE role=VALUES(role), side=VALUES(side)`, pid, account, role, side, now); err != nil {
		return "", err
	}
	secret, err := newTokenSecret()
	if err != nil {
		return "", err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO api_token(user_name, token_hash, expires_at, created_at) VALUES(?,?,?,?)`,
		account, HashToken(secret), now.Add(ttl), now); err != nil {
		return "", fmt.Errorf("insert token: %w", err)
	}
	return secret, tx.Commit()
}
