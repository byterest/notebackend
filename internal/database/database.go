package database

import (
	"database/sql"
	"fmt"

	_ "github.com/lib/pq"
)

// Open creates the PostgreSQL connection used by the app.
func Open(databaseURL string) (*sql.DB, error) {
	db, err := sql.Open("postgres", databaseURL)
	if err != nil {
		return nil, err
	}

	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("ping database: %w", err)
	}

	return db, nil
}

// Migrate creates or upgrades the database schema.
func Migrate(db *sql.DB) error {
	if _, err := db.Exec(schema); err != nil {
		return err
	}

	if _, err := db.Exec(migrations); err != nil {
		return err
	}

	_, err := db.Exec(indexes)
	return err
}

const schema = `
CREATE TABLE IF NOT EXISTS users (
	id SERIAL PRIMARY KEY,
	email TEXT NOT NULL UNIQUE,
	display_name TEXT NOT NULL,
	password_hash TEXT NOT NULL,
	created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
	updated_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE IF NOT EXISTS sessions (
	token TEXT PRIMARY KEY,
	user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	expires_at TIMESTAMP NOT NULL,
	created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE IF NOT EXISTS canvases (
	id SERIAL PRIMARY KEY,
	user_id INTEGER REFERENCES users(id) ON DELETE CASCADE,
	name TEXT NOT NULL DEFAULT 'Untitled Canvas',
	data TEXT NOT NULL DEFAULT '{"nodes":[],"edges":[],"viewport":{"x":0,"y":0,"zoom":1}}',
	created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
	updated_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE IF NOT EXISTS canvas_locks (
	canvas_id INTEGER NOT NULL REFERENCES canvases(id) ON DELETE CASCADE,
	user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	client_id TEXT NOT NULL,
	lock_token TEXT NOT NULL,
	expires_at TIMESTAMP NOT NULL,
	updated_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
	PRIMARY KEY (canvas_id, user_id)
);
`

const migrations = `
DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM information_schema.columns
        WHERE table_name = 'users' AND column_name = 'avatar_url'
    ) THEN
        ALTER TABLE users ADD COLUMN avatar_url TEXT;
    END IF;
END $$;
`

const indexes = `
CREATE INDEX IF NOT EXISTS idx_sessions_user_id ON sessions(user_id);
CREATE INDEX IF NOT EXISTS idx_sessions_expires_at ON sessions(expires_at);
CREATE INDEX IF NOT EXISTS idx_canvases_user_updated ON canvases(user_id, updated_at DESC, id DESC);
CREATE INDEX IF NOT EXISTS idx_canvas_locks_expires_at ON canvas_locks(expires_at);
`
