package auth

import "time"

// User is the public account shape returned to clients.
type User struct {
	ID          int64     `json:"id"`
	Email       string    `json:"email"`
	DisplayName string    `json:"display_name"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

type userWithPassword struct {
	User
	PasswordHash string
}

// AuthResponse is returned after registration and login.
type AuthResponse struct {
	User  User   `json:"user"`
	Token string `json:"token"`
}
