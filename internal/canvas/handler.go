package canvas

import (
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"

	"note-backend/internal/auth"

	"github.com/gin-gonic/gin"
)

// Handler exposes canvas HTTP endpoints.
type Handler struct {
	store *Store
}

// NewHandler creates a canvas handler.
func NewHandler(store *Store) *Handler {
	return &Handler{store: store}
}

// List returns canvases for the current user.
func (h *Handler) List(c *gin.Context) {
	user, ok := auth.CurrentUser(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "authentication required"})
		return
	}

	items, err := h.store.List(c.Request.Context(), user.ID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{"data": items})
}

// Create creates a new canvas for the current user.
func (h *Handler) Create(c *gin.Context) {
	user, ok := auth.CurrentUser(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "authentication required"})
		return
	}

	var request struct {
		Name string `json:"name"`
		Data string `json:"data"`
	}
	if err := c.ShouldBindJSON(&request); err != nil && !errors.Is(err, io.EOF) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}

	item, err := h.store.Create(c.Request.Context(), user.ID, normalizeName(request.Name), normalizeData(request.Data))
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusCreated, gin.H{"data": item})
}

// Get returns a canvas by ID for the current user.
func (h *Handler) Get(c *gin.Context) {
	user, ok := auth.CurrentUser(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "authentication required"})
		return
	}

	id, err := parseID(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid canvas id"})
		return
	}

	item, err := h.store.Get(c.Request.Context(), user.ID, id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			c.JSON(http.StatusNotFound, gin.H{"error": "canvas not found"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{"data": item})
}

// Update changes canvas name or data.
func (h *Handler) Update(c *gin.Context) {
	user, ok := auth.CurrentUser(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "authentication required"})
		return
	}

	id, err := parseID(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid canvas id"})
		return
	}

	current, err := h.store.Get(c.Request.Context(), user.ID, id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			c.JSON(http.StatusNotFound, gin.H{"error": "canvas not found"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	var request struct {
		Name *string `json:"name"`
		Data *string `json:"data"`
	}
	if err := c.ShouldBindJSON(&request); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}

	name := current.Name
	if request.Name != nil {
		name = normalizeName(*request.Name)
	}

	data := current.Data
	if request.Data != nil {
		data = normalizeData(*request.Data)
	}

	updated, err := h.store.Update(c.Request.Context(), user.ID, id, name, data)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			c.JSON(http.StatusNotFound, gin.H{"error": "canvas not found"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{"data": updated})
}

// Delete removes a canvas.
func (h *Handler) Delete(c *gin.Context) {
	user, ok := auth.CurrentUser(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "authentication required"})
		return
	}

	id, err := parseID(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid canvas id"})
		return
	}

	deleted, err := h.store.Delete(c.Request.Context(), user.ID, id)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	if !deleted {
		c.JSON(http.StatusNotFound, gin.H{"error": "canvas not found"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"ok": true})
}

func parseID(raw string) (int64, error) {
	return strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
}

func normalizeName(raw string) string {
	name := strings.TrimSpace(raw)
	if name == "" {
		return defaultCanvasName
	}
	return name
}

func normalizeData(raw string) string {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return defaultCanvasData
	}

	var decoded map[string]any
	if err := json.Unmarshal([]byte(trimmed), &decoded); err != nil {
		return defaultCanvasData
	}

	if _, ok := decoded["nodes"]; !ok {
		decoded["nodes"] = []any{}
	}
	if _, ok := decoded["edges"]; !ok {
		decoded["edges"] = []any{}
	}
	if _, ok := decoded["viewport"]; !ok {
		decoded["viewport"] = map[string]any{"x": 0, "y": 0, "zoom": 1}
	}

	normalized, err := json.Marshal(decoded)
	if err != nil {
		return defaultCanvasData
	}
	return string(normalized)
}
