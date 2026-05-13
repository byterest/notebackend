package canvas

import (
	"context"
	"database/sql"
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
