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
