package store

import (
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/NicoMancinelli/notiphy/internal/model"
)

// CreateToken mints a new webhook token for the default account.
func (s *Store) CreateToken(name string) (*model.Token, error) {
	t := &model.Token{
		ID:        model.NewID("tok"),
		AccountID: DefaultAccountID,
		Token:     model.NewToken(),
		Name:      name,
		CreatedAt: time.Now().UTC(),
	}
	_, err := s.db.Exec(
		`INSERT INTO tokens (id, account_id, token, name, created_at, revoked)
		 VALUES (?, ?, ?, ?, ?, 0)`,
		t.ID, t.AccountID, t.Token, t.Name, t.CreatedAt.Unix(),
	)
	if err != nil {
		return nil, fmt.Errorf("create token: %w", err)
	}
	return t, nil
}

// TokenByValue resolves a raw webhook token. Revoked tokens are treated as
// missing so callers return 404 rather than leaking that the token once existed.
func (s *Store) TokenByValue(value string) (*model.Token, error) {
	var (
		t       model.Token
		created int64
		revoked int
	)
	err := s.db.QueryRow(
		`SELECT id, account_id, token, name, created_at, revoked
		 FROM tokens WHERE token = ? AND revoked = 0`, value,
	).Scan(&t.ID, &t.AccountID, &t.Token, &t.Name, &created, &revoked)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("lookup token: %w", err)
	}
	t.CreatedAt = fromUnix(created)
	t.Revoked = revoked == 1
	return &t, nil
}

// ListTokens returns all tokens for the default account, newest first.
func (s *Store) ListTokens() ([]*model.Token, error) {
	rows, err := s.db.Query(
		`SELECT id, account_id, token, name, created_at, revoked
		 FROM tokens WHERE account_id = ? ORDER BY created_at DESC`, DefaultAccountID,
	)
	if err != nil {
		return nil, fmt.Errorf("list tokens: %w", err)
	}
	defer rows.Close()

	var out []*model.Token
	for rows.Next() {
		var (
			t       model.Token
			created int64
			revoked int
		)
		if err := rows.Scan(&t.ID, &t.AccountID, &t.Token, &t.Name, &created, &revoked); err != nil {
			return nil, fmt.Errorf("scan token: %w", err)
		}
		t.CreatedAt = fromUnix(created)
		t.Revoked = revoked == 1
		out = append(out, &t)
	}
	return out, rows.Err()
}

// RevokeToken marks a token unusable. It is not deleted, so historical events
// keep referring to something meaningful.
func (s *Store) RevokeToken(id string) error {
	res, err := s.db.Exec(`UPDATE tokens SET revoked = 1 WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("revoke token: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}
