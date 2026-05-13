package auth

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"time"

	"golang.org/x/crypto/bcrypt"
)

var (
	ErrEmailExists        = errors.New("email already registered")
	ErrInvalidCredential  = errors.New("invalid email or password")
	ErrInvalidSession     = errors.New("invalid or expired session")
	ErrInvalidEmail       = errors.New("valid email is required")
	ErrInvalidPassword    = errors.New("password must be at least 8 characters")
	ErrInvalidDisplayName = errors.New("display name is too long")
)

// Store handles user and session persistence.
type Store struct {
	db         *sql.DB
	sessionTTL time.Duration
}

// NewStore creates an auth store.
func NewStore(db *sql.DB, sessionTTL time.Duration) *Store {
	return &Store{db: db, sessionTTL: sessionTTL}
}

// CreateUser validates and stores a new account.
func (s *Store) CreateUser(ctx context.Context, email, displayName, password string) (User, error) {
	normalizedEmail, err := normalizeEmail(email)
	if err != nil {
		return User{}, err
	}

	if len(password) < 8 {
		return User{}, ErrInvalidPassword
	}

	displayName = strings.TrimSpace(displayName)
	if displayName == "" {
		displayName = strings.Split(normalizedEmail, "@")[0]
	}
	if len([]rune(displayName)) > 80 {
		return User{}, ErrInvalidDisplayName
	}

	passwordHash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return User{}, fmt.Errorf("hash password: %w", err)
	}

	result, err := s.db.ExecContext(ctx,
		`INSERT INTO users (email, display_name, password_hash) VALUES (?, ?, ?)`,
		normalizedEmail,
		displayName,
		string(passwordHash),
	)
	if err != nil {
		if isUniqueConstraintError(err) {
			return User{}, ErrEmailExists
		}
		return User{}, err
	}

	id, _ := result.LastInsertId()
	return s.GetUserByID(ctx, id)
}

// Authenticate verifies account credentials.
func (s *Store) Authenticate(ctx context.Context, email, password string) (User, error) {
	normalizedEmail, err := normalizeEmail(email)
	if err != nil {
		return User{}, ErrInvalidCredential
	}

	account, err := s.getUserByEmail(ctx, normalizedEmail)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return User{}, ErrInvalidCredential
		}
		return User{}, err
	}

	if err := bcrypt.CompareHashAndPassword([]byte(account.PasswordHash), []byte(password)); err != nil {
		return User{}, ErrInvalidCredential
	}

	return account.User, nil
}

// CreateSession creates a bearer token for a user.
func (s *Store) CreateSession(ctx context.Context, userID int64) (string, error) {
	token, err := randomToken()
	if err != nil {
		return "", err
	}

	expiresAt := time.Now().UTC().Add(s.sessionTTL)
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO sessions (token, user_id, expires_at) VALUES (?, ?, ?)`,
		token,
		userID,
		expiresAt,
	); err != nil {
		return "", err
	}

	return token, nil
}

// GetUserBySession returns the active user for a token.
func (s *Store) GetUserBySession(ctx context.Context, token string) (User, error) {
	token = strings.TrimSpace(token)
	if token == "" {
		return User{}, ErrInvalidSession
	}

	var user User
	err := s.db.QueryRowContext(ctx, `
		SELECT users.id, users.email, users.display_name, users.created_at, users.updated_at
		FROM sessions
		JOIN users ON users.id = sessions.user_id
		WHERE sessions.token = ? AND sessions.expires_at > CURRENT_TIMESTAMP
	`, token).Scan(&user.ID, &user.Email, &user.DisplayName, &user.CreatedAt, &user.UpdatedAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return User{}, ErrInvalidSession
		}
		return User{}, err
	}

	return user, nil
}

// DeleteSession revokes a token.
func (s *Store) DeleteSession(ctx context.Context, token string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE token = ?`, strings.TrimSpace(token))
	return err
}

// DeleteExpiredSessions removes expired sessions opportunistically.
func (s *Store) DeleteExpiredSessions(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE expires_at <= CURRENT_TIMESTAMP`)
	return err
}

// GetUserByID returns a user by ID.
func (s *Store) GetUserByID(ctx context.Context, id int64) (User, error) {
	var user User
	err := s.db.QueryRowContext(ctx,
		`SELECT id, email, display_name, created_at, updated_at FROM users WHERE id = ?`,
		id,
	).Scan(&user.ID, &user.Email, &user.DisplayName, &user.CreatedAt, &user.UpdatedAt)
	if err != nil {
		return User{}, err
	}
	return user, nil
}

func (s *Store) getUserByEmail(ctx context.Context, email string) (userWithPassword, error) {
	var account userWithPassword
	err := s.db.QueryRowContext(ctx,
		`SELECT id, email, display_name, password_hash, created_at, updated_at FROM users WHERE email = ?`,
		email,
	).Scan(
		&account.ID,
		&account.Email,
		&account.DisplayName,
		&account.PasswordHash,
		&account.CreatedAt,
		&account.UpdatedAt,
	)
	if err != nil {
		return userWithPassword{}, err
	}
	return account, nil
}

func normalizeEmail(email string) (string, error) {
	trimmed := strings.ToLower(strings.TrimSpace(email))
	if trimmed == "" || !strings.Contains(trimmed, "@") || strings.ContainsAny(trimmed, " \t\r\n") {
		return "", ErrInvalidEmail
	}
	return trimmed, nil
}

func randomToken() (string, error) {
	buffer := make([]byte, 32)
	if _, err := rand.Read(buffer); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buffer), nil
}

func isUniqueConstraintError(err error) bool {
	message := strings.ToLower(err.Error())
	return strings.Contains(message, "unique") || strings.Contains(message, "constraint failed")
}
