package canvas

import "time"

const (
	defaultCanvasName = "Untitled Canvas"
	defaultCanvasData = `{"nodes":[],"edges":[],"viewport":{"x":0,"y":0,"zoom":1}}`
)

// Canvas is a persisted note canvas.
type Canvas struct {
	ID        int64     `json:"id"`
	UserID    int64     `json:"user_id"`
	Name      string    `json:"name"`
	Data      string    `json:"data"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// CanvasLock is an editing lease for one user's canvas.
type CanvasLock struct {
	CanvasID  int64     `json:"canvas_id"`
	UserID    int64     `json:"user_id"`
	ClientID  string    `json:"client_id"`
	LockToken string    `json:"lock_token,omitempty"`
	ExpiresAt time.Time `json:"expires_at"`
	UpdatedAt time.Time `json:"updated_at"`
}
