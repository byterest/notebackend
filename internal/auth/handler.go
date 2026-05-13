package auth

import (
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"
)

// Handler exposes auth HTTP endpoints.
type Handler struct {
	store *Store
}

// NewHandler creates an auth handler.
func NewHandler(store *Store) *Handler {
	return &Handler{store: store}
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
