package canvas

import (
	"database/sql"
	"errors"
	"io"
	"net/http"
	"strings"

	"note-backend/internal/auth"

	"github.com/gin-gonic/gin"
)

// ListFolders returns folders for the current user.
func (h *Handler) ListFolders(c *gin.Context) {
	user, ok := auth.CurrentUser(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "authentication required"})
		return
	}

	items, err := h.store.ListFolders(c.Request.Context(), user.ID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{"data": items})
}

// CreateFolder creates a one-level folder for the current user.
func (h *Handler) CreateFolder(c *gin.Context) {
	user, ok := auth.CurrentUser(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "authentication required"})
		return
	}

	var request struct {
		Name string `json:"name"`
	}
	if err := c.ShouldBindJSON(&request); err != nil && !errors.Is(err, io.EOF) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}

	item, err := h.store.CreateFolder(c.Request.Context(), user.ID, normalizeFolderName(request.Name))
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusCreated, gin.H{"data": item})
}

// UpdateFolder renames a folder.
func (h *Handler) UpdateFolder(c *gin.Context) {
	user, ok := auth.CurrentUser(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "authentication required"})
		return
	}

	id, err := parseID(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid folder id"})
		return
	}

	var request struct {
		Name string `json:"name"`
	}
	if err := c.ShouldBindJSON(&request); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}

	item, err := h.store.UpdateFolder(c.Request.Context(), user.ID, id, normalizeFolderName(request.Name))
	if err != nil {
		if errors.Is(err, ErrFolderNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"error": "folder not found"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{"data": item})
}

// DeleteFolder removes a folder and unfiles its canvases.
func (h *Handler) DeleteFolder(c *gin.Context) {
	user, ok := auth.CurrentUser(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "authentication required"})
		return
	}

	id, err := parseID(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid folder id"})
		return
	}

	deleted, err := h.store.DeleteFolder(c.Request.Context(), user.ID, id)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	if !deleted {
		c.JSON(http.StatusNotFound, gin.H{"error": "folder not found"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"ok": true})
}

// SetCanvasFolder moves a canvas into a folder or back to the root.
func (h *Handler) SetCanvasFolder(c *gin.Context) {
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

	var request struct {
		FolderID *int64 `json:"folder_id"`
	}
	if err := c.ShouldBindJSON(&request); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}

	item, err := h.store.SetCanvasFolder(c.Request.Context(), user.ID, id, request.FolderID)
	if err != nil {
		if errors.Is(err, ErrFolderNotFound) {
			c.JSON(http.StatusBadRequest, gin.H{"error": "folder not found"})
			return
		}
		if errors.Is(err, sql.ErrNoRows) {
			c.JSON(http.StatusNotFound, gin.H{"error": "canvas not found"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	item.Preview = buildPreview(item.Data)
	item.Data = ""
	c.JSON(http.StatusOK, gin.H{"data": item})
}

func normalizeFolderName(raw string) string {
	name := strings.TrimSpace(raw)
	if name == "" {
		return defaultFolderName
	}
	return name
}
