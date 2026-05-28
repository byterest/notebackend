package auth

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	notes3 "note-backend/internal/s3"
	"note-backend/internal/upload"
)

// S3Uploader defines the minimal interface for S3-compatible avatar uploads.
type S3Uploader interface {
	Upload(ctx context.Context, key string, reader io.Reader, contentType string) (string, error)
	Delete(ctx context.Context, key string) error
	PublicURL(endpoint string, key string) string
}

// Handler exposes auth HTTP endpoints.
type Handler struct {
	store        *Store
	uploadDir    string
	s3Client     S3Uploader
	s3Endpoint   string
}

// NewHandler creates an auth handler.
func NewHandler(store *Store, uploadDir string) *Handler {
	return &Handler{store: store, uploadDir: uploadDir}
}

// NewHandlerWithS3 creates an auth handler with S3 support.
func NewHandlerWithS3(store *Store, uploadDir string, s3Client S3Uploader, s3Endpoint string) *Handler {
	return &Handler{store: store, uploadDir: uploadDir, s3Client: s3Client, s3Endpoint: s3Endpoint}
}

type credentialsRequest struct {
	Email       string `json:"email"`
	Password    string `json:"password"`
	DisplayName string `json:"display_name"`
}

// Register creates a new user and returns a session token.
func (h *Handler) Register(c *gin.Context) {
	var request credentialsRequest
	if err := c.ShouldBindJSON(&request); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}

	user, err := h.store.CreateUser(c.Request.Context(), request.Email, request.DisplayName, request.Password)
	if err != nil {
		writeAuthError(c, err)
		return
	}

	token, err := h.store.CreateSession(c.Request.Context(), user.ID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusCreated, gin.H{"data": AuthResponse{User: user, Token: token}})
}

// Login verifies credentials and returns a session token.
func (h *Handler) Login(c *gin.Context) {
	var request credentialsRequest
	if err := c.ShouldBindJSON(&request); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}

	user, err := h.store.Authenticate(c.Request.Context(), request.Email, request.Password)
	if err != nil {
		writeAuthError(c, err)
		return
	}

	token, err := h.store.CreateSession(c.Request.Context(), user.ID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	_ = h.store.DeleteExpiredSessions(c.Request.Context())
	c.JSON(http.StatusOK, gin.H{"data": AuthResponse{User: user, Token: token}})
}

// Me returns the authenticated user.
func (h *Handler) Me(c *gin.Context) {
	user, ok := CurrentUser(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "authentication required"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"data": gin.H{"user": user}})
}

// Logout revokes the current session.
func (h *Handler) Logout(c *gin.Context) {
	if err := h.store.DeleteSession(c.Request.Context(), CurrentToken(c)); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

type updateProfileRequest struct {
	DisplayName string `json:"display_name"`
}

// UpdateProfile updates the authenticated user's display name.
func (h *Handler) UpdateProfile(c *gin.Context) {
	user, ok := CurrentUser(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "authentication required"})
		return
	}

	var request updateProfileRequest
	if err := c.ShouldBindJSON(&request); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}

	updated, err := h.store.UpdateUser(c.Request.Context(), user.ID, request.DisplayName)
	if err != nil {
		writeAuthError(c, err)
		return
	}

	c.JSON(http.StatusOK, gin.H{"data": updated})
}

type updatePasswordRequest struct {
	OldPassword string `json:"old_password"`
	NewPassword string `json:"new_password"`
}

// UpdatePassword changes the authenticated user's password.
func (h *Handler) UpdatePassword(c *gin.Context) {
	user, ok := CurrentUser(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "authentication required"})
		return
	}

	var request updatePasswordRequest
	if err := c.ShouldBindJSON(&request); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}

	if err := h.store.UpdatePassword(c.Request.Context(), user.ID, request.OldPassword, request.NewPassword); err != nil {
		writeAuthError(c, err)
		return
	}

	c.JSON(http.StatusOK, gin.H{"ok": true})
}

const maxAvatarSize = 2 << 20 // 2MB

var allowedAvatarTypes = map[string]bool{
	"image/jpeg": true,
	"image/png":  true,
	"image/webp": true,
}

// UploadAvatar handles user avatar uploads.
func (h *Handler) UploadAvatar(c *gin.Context) {
	user, ok := CurrentUser(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "authentication required"})
		return
	}

	fileHeader, err := c.FormFile("avatar")
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "avatar is required"})
		return
	}

	if fileHeader.Size > maxAvatarSize {
		c.JSON(http.StatusBadRequest, gin.H{"error": "avatar exceeds 2MB limit"})
		return
	}

	if err := upload.ValidateImage(fileHeader); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	file, err := fileHeader.Open()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	defer file.Close()

	data, err := io.ReadAll(file)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	contentType := http.DetectContentType(data)
	if !allowedAvatarTypes[contentType] {
		c.JSON(http.StatusBadRequest, gin.H{"error": "only jpeg, png, webp are allowed"})
		return
	}

	// Delete old avatar if exists
	if user.AvatarURL != nil && *user.AvatarURL != "" {
		_ = h.deleteAvatarFile(c.Request.Context(), *user.AvatarURL)
	}

	// Save new avatar
	filename := fmt.Sprintf("avatar_%d_%d%s", user.ID, time.Now().UnixNano(), filepath.Ext(fileHeader.Filename))
	var url string

	if h.s3Client != nil {
		// S3 upload
		key := notes3.GenerateKey("avatars", fileHeader.Filename)
		_, err := h.s3Client.Upload(c.Request.Context(), key, bytes.NewReader(data), contentType)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		url = h.s3Client.PublicURL(h.s3Endpoint, key)
	} else {
		// Local storage
		path := filepath.Join(h.uploadDir, filename)
		if err := os.WriteFile(path, data, 0o644); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}

		host := c.Request.Host
		if forwardedHost := c.GetHeader("X-Forwarded-Host"); forwardedHost != "" {
			host = forwardedHost
		}
		proto := "http"
		if c.Request.TLS != nil {
			proto = "https"
		}
		if forwardedProto := c.GetHeader("X-Forwarded-Proto"); forwardedProto != "" {
			proto = forwardedProto
		}
		baseURL := fmt.Sprintf("%s://%s", proto, host)
		url = baseURL + "/uploads/" + filename
	}

	updated, err := h.store.UpdateAvatarURL(c.Request.Context(), user.ID, &url)
	if err != nil {
		if h.s3Client == nil {
			_ = os.Remove(filepath.Join(h.uploadDir, filename))
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{"data": updated})
}

// DeleteAvatar removes the user's avatar.
func (h *Handler) DeleteAvatar(c *gin.Context) {
	user, ok := CurrentUser(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "authentication required"})
		return
	}

	if user.AvatarURL != nil && *user.AvatarURL != "" {
		_ = h.deleteAvatarFile(c.Request.Context(), *user.AvatarURL)
	}

	updated, err := h.store.UpdateAvatarURL(c.Request.Context(), user.ID, nil)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{"data": updated})
}

func (h *Handler) deleteAvatarFile(ctx context.Context, rawURL string) error {
	if h.s3Client != nil {
		endpoint := strings.TrimRight(h.s3Endpoint, "/")
		if strings.HasPrefix(rawURL, endpoint) {
			key := strings.TrimPrefix(rawURL, endpoint+"/")
			parts := strings.SplitN(key, "/", 2)
			if len(parts) == 2 {
				key = parts[1]
			}
			return h.s3Client.Delete(ctx, key)
		}
		return nil
	}

	// Local: extract filename from URL
	parts := strings.Split(rawURL, "/uploads/")
	if len(parts) != 2 {
		return nil
	}
	path := filepath.Join(h.uploadDir, parts[1])
	return os.Remove(path)
}

func writeAuthError(c *gin.Context, err error) {
	switch {
	case errors.Is(err, ErrEmailExists):
		c.JSON(http.StatusConflict, gin.H{"error": "email already registered"})
	case errors.Is(err, ErrInvalidCredential):
		c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid email or password"})
	case errors.Is(err, ErrInvalidEmail), errors.Is(err, ErrInvalidPassword), errors.Is(err, ErrInvalidDisplayName):
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
	default:
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
	}
}
