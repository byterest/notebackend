package canvas

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

func newLockTestStore(t *testing.T) (*Store, int64, int64) {
	t.Helper()

	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	t.Cleanup(func() {
		db.Close()
	})

	db.SetMaxOpenConns(1)
	if _, err := db.Exec(`PRAGMA foreign_keys = ON`); err != nil {
		t.Fatalf("enable foreign keys: %v", err)
	}

	if _, err := db.Exec(sqliteSchema); err != nil {
		t.Fatalf("migrate database: %v", err)
	}

	result, err := db.Exec(`
		INSERT INTO users (email, display_name, password_hash)
		VALUES (?, ?, ?)
	`, "lock-test@example.com", "Lock Test", "hash")
	if err != nil {
		t.Fatalf("insert user: %v", err)
	}
	userID, err := result.LastInsertId()
	if err != nil {
		t.Fatalf("read user id: %v", err)
	}

	store := NewStore(db)
	item, err := store.Create(context.Background(), userID, "Test Canvas", defaultCanvasData, nil)
	if err != nil {
		t.Fatalf("create canvas: %v", err)
	}

	return store, userID, item.ID
}

const sqliteSchema = `
CREATE TABLE IF NOT EXISTS users (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	email TEXT NOT NULL UNIQUE,
	display_name TEXT NOT NULL,
	password_hash TEXT NOT NULL,
	created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
	updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE IF NOT EXISTS sessions (
	token TEXT PRIMARY KEY,
	user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	expires_at DATETIME NOT NULL,
	created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE IF NOT EXISTS folders (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	name TEXT NOT NULL DEFAULT 'Untitled Folder',
	created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
	updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE IF NOT EXISTS canvases (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	user_id INTEGER REFERENCES users(id) ON DELETE CASCADE,
	folder_id INTEGER REFERENCES folders(id) ON DELETE SET NULL,
	name TEXT NOT NULL DEFAULT 'Untitled Canvas',
	data TEXT NOT NULL DEFAULT '{"nodes":[],"edges":[],"viewport":{"x":0,"y":0,"zoom":1}}',
	created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
	updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE IF NOT EXISTS canvas_locks (
	canvas_id INTEGER NOT NULL REFERENCES canvases(id) ON DELETE CASCADE,
	user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	client_id TEXT NOT NULL,
	lock_token TEXT NOT NULL,
	expires_at DATETIME NOT NULL,
	updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
	PRIMARY KEY (canvas_id, user_id)
);
`

func TestAcquireLockBlocksSecondActiveClient(t *testing.T) {
	store, userID, canvasID := newLockTestStore(t)

	first, acquired, err := store.AcquireLock(context.Background(), userID, canvasID, "client-a", "token-a", time.Minute)
	if err != nil {
		t.Fatalf("acquire first lock: %v", err)
	}
	if !acquired {
		t.Fatal("first client should acquire the lock")
	}
	if first.LockToken != "token-a" {
		t.Fatalf("first lock token = %q, want token-a", first.LockToken)
	}

	second, acquired, err := store.AcquireLock(context.Background(), userID, canvasID, "client-b", "token-b", time.Minute)
	if err != nil {
		t.Fatalf("acquire second lock: %v", err)
	}
	if acquired {
		t.Fatal("second client should not acquire an active lock")
	}
	if second.ClientID != "client-a" {
		t.Fatalf("reported lock holder = %q, want client-a", second.ClientID)
	}
}

func TestAcquireLockRenewsSameClientToken(t *testing.T) {
	store, userID, canvasID := newLockTestStore(t)

	if _, acquired, err := store.AcquireLock(context.Background(), userID, canvasID, "client-a", "token-a", time.Minute); err != nil || !acquired {
		t.Fatalf("acquire first lock: acquired=%v err=%v", acquired, err)
	}

	renewed, acquired, err := store.AcquireLock(context.Background(), userID, canvasID, "client-a", "token-b", time.Minute)
	if err != nil {
		t.Fatalf("renew lock: %v", err)
	}
	if !acquired {
		t.Fatal("same client should renew the lock")
	}
	if renewed.LockToken != "token-a" {
		t.Fatalf("renewed token = %q, want original token-a", renewed.LockToken)
	}
}

func TestAcquireLockAllowsTakeoverAfterExpiry(t *testing.T) {
	store, userID, canvasID := newLockTestStore(t)

	if _, acquired, err := store.AcquireLock(context.Background(), userID, canvasID, "client-a", "token-a", -time.Second); err != nil || !acquired {
		t.Fatalf("acquire expired lock: acquired=%v err=%v", acquired, err)
	}

	taken, acquired, err := store.AcquireLock(context.Background(), userID, canvasID, "client-b", "token-b", time.Minute)
	if err != nil {
		t.Fatalf("take over expired lock: %v", err)
	}
	if !acquired {
		t.Fatal("second client should acquire an expired lock")
	}
	if taken.ClientID != "client-b" || taken.LockToken != "token-b" {
		t.Fatalf("taken lock = (%q, %q), want (client-b, token-b)", taken.ClientID, taken.LockToken)
	}
}

func TestValidateLockRejectsMissingOrExpiredToken(t *testing.T) {
	store, userID, canvasID := newLockTestStore(t)

	if err := store.ValidateLock(context.Background(), userID, canvasID, "client-a", ""); !errors.Is(err, ErrInvalidLock) {
		t.Fatalf("missing token error = %v, want ErrInvalidLock", err)
	}

	if _, acquired, err := store.AcquireLock(context.Background(), userID, canvasID, "client-a", "token-a", -time.Second); err != nil || !acquired {
		t.Fatalf("acquire expired lock: acquired=%v err=%v", acquired, err)
	}

	if err := store.ValidateLock(context.Background(), userID, canvasID, "client-a", "token-a"); !errors.Is(err, ErrInvalidLock) {
		t.Fatalf("expired token error = %v, want ErrInvalidLock", err)
	}
}
