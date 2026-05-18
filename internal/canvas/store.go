package canvas

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

var (
	ErrCanvasLocked = errors.New("canvas is locked")
	ErrInvalidLock  = errors.New("invalid canvas lock")
)

// Store handles canvas persistence.
type Store struct {
	db *sql.DB
}

// NewStore creates a canvas store.
func NewStore(db *sql.DB) *Store {
	return &Store{db: db}
}

// List returns canvases owned by a user.
func (s *Store) List(ctx context.Context, userID int64) ([]Canvas, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, user_id, name, data, created_at, updated_at
		FROM canvases
		WHERE user_id = ?
		ORDER BY updated_at DESC, id DESC
	`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	items := make([]Canvas, 0)
	for rows.Next() {
		item, scanErr := scanCanvas(rows.Scan)
		if scanErr != nil {
			return nil, scanErr
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

// Create inserts a new canvas for a user.
func (s *Store) Create(ctx context.Context, userID int64, name, data string) (Canvas, error) {
	result, err := s.db.ExecContext(ctx,
		`INSERT INTO canvases (user_id, name, data) VALUES (?, ?, ?)`,
		userID,
		name,
		data,
	)
	if err != nil {
		return Canvas{}, err
	}

	id, _ := result.LastInsertId()
	return s.Get(ctx, userID, id)
}

// Get fetches a canvas owned by a user.
func (s *Store) Get(ctx context.Context, userID, id int64) (Canvas, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT id, user_id, name, data, created_at, updated_at
		FROM canvases
		WHERE id = ? AND user_id = ?
	`, id, userID)
	return scanCanvas(row.Scan)
}

// Update replaces mutable canvas fields.
func (s *Store) Update(ctx context.Context, userID, id int64, name, data string) (Canvas, error) {
	_, err := s.db.ExecContext(ctx,
		`UPDATE canvases SET name = ?, data = ?, updated_at = CURRENT_TIMESTAMP WHERE id = ? AND user_id = ?`,
		name,
		data,
		id,
		userID,
	)
	if err != nil {
		return Canvas{}, err
	}
	return s.Get(ctx, userID, id)
}

// AcquireLock creates or renews the editing lease for a canvas.
func (s *Store) AcquireLock(ctx context.Context, userID, canvasID int64, clientID, lockToken string, ttl time.Duration) (CanvasLock, bool, error) {
	now := time.Now().UTC()
	expiresAt := now.Add(ttl)

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return CanvasLock{}, false, err
	}
	defer tx.Rollback()

	var current CanvasLock
	row := tx.QueryRowContext(ctx, `
		SELECT canvas_id, user_id, client_id, lock_token, expires_at, updated_at
		FROM canvas_locks
		WHERE canvas_id = ? AND user_id = ?
	`, canvasID, userID)
	err = row.Scan(&current.CanvasID, &current.UserID, &current.ClientID, &current.LockToken, &current.ExpiresAt, &current.UpdatedAt)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return CanvasLock{}, false, err
	}

	if err == nil && current.ClientID != clientID && current.ExpiresAt.After(now) {
		return current, false, tx.Commit()
	}

	if err == nil && current.ClientID == clientID && current.LockToken != "" {
		lockToken = current.LockToken
	}

	lock := CanvasLock{
		CanvasID:  canvasID,
		UserID:    userID,
		ClientID:  clientID,
		LockToken: lockToken,
		ExpiresAt: expiresAt,
		UpdatedAt: now,
	}

	if errors.Is(err, sql.ErrNoRows) {
		_, err = tx.ExecContext(ctx, `
			INSERT INTO canvas_locks (canvas_id, user_id, client_id, lock_token, expires_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?)
		`, lock.CanvasID, lock.UserID, lock.ClientID, lock.LockToken, lock.ExpiresAt, lock.UpdatedAt)
	} else {
		_, err = tx.ExecContext(ctx, `
			UPDATE canvas_locks
			SET client_id = ?, lock_token = ?, expires_at = ?, updated_at = ?
			WHERE canvas_id = ? AND user_id = ?
		`, lock.ClientID, lock.LockToken, lock.ExpiresAt, lock.UpdatedAt, canvasID, userID)
	}
	if err != nil {
		return CanvasLock{}, false, err
	}

	if err := tx.Commit(); err != nil {
		return CanvasLock{}, false, err
	}
	return lock, true, nil
}

// ValidateLock confirms that the client still owns the editing lease.
func (s *Store) ValidateLock(ctx context.Context, userID, canvasID int64, clientID, lockToken string) error {
	if clientID == "" || lockToken == "" {
		return ErrInvalidLock
	}

	var expiresAt time.Time
	err := s.db.QueryRowContext(ctx, `
		SELECT expires_at
		FROM canvas_locks
		WHERE canvas_id = ? AND user_id = ? AND client_id = ? AND lock_token = ?
	`, canvasID, userID, clientID, lockToken).Scan(&expiresAt)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrInvalidLock
	}
	if err != nil {
		return err
	}
	if !expiresAt.After(time.Now().UTC()) {
		return ErrInvalidLock
	}
	return nil
}

// ReleaseLock removes the editing lease if it is still held by the client.
func (s *Store) ReleaseLock(ctx context.Context, userID, canvasID int64, clientID, lockToken string) error {
	if clientID == "" || lockToken == "" {
		return nil
	}

	_, err := s.db.ExecContext(ctx, `
		DELETE FROM canvas_locks
		WHERE canvas_id = ? AND user_id = ? AND client_id = ? AND lock_token = ?
	`, canvasID, userID, clientID, lockToken)
	return err
}

// Delete removes a user-owned canvas.
func (s *Store) Delete(ctx context.Context, userID, id int64) (bool, error) {
	result, err := s.db.ExecContext(ctx, `DELETE FROM canvases WHERE id = ? AND user_id = ?`, id, userID)
	if err != nil {
		return false, err
	}
	affected, _ := result.RowsAffected()
	return affected > 0, nil
}

func scanCanvas(scan func(dest ...any) error) (Canvas, error) {
	var item Canvas
	if err := scan(&item.ID, &item.UserID, &item.Name, &item.Data, &item.CreatedAt, &item.UpdatedAt); err != nil {
		return Canvas{}, err
	}
	return item, nil
}
