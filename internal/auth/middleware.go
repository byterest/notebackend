package auth

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
)

const (
	contextUserKey  = "auth.user"
	contextTokenKey = "auth.token"
)

// Middleware requires a valid bearer token and stores the user in Gin context.
func Middleware(store *Store) gin.HandlerFunc {
	return func(c *gin.Context) {
		token := bearerToken(c.GetHeader("Authorization"))
		if token == "" {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "authentication required"})
			return
		}

		user, err := store.GetUserBySession(c.Request.Context(), token)
		if err != nil {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "session expired, please sign in again"})
			return
		}

		c.Set(contextUserKey, user)
		c.Set(contextTokenKey, token)
		c.Next()
	}
}

// CurrentUser returns the authenticated user from Gin context.
func CurrentUser(c *gin.Context) (User, bool) {
	value, ok := c.Get(contextUserKey)
	if !ok {
		return User{}, false
	}
	user, ok := value.(User)
	return user, ok
}

// CurrentToken returns the current bearer token from Gin context.
func CurrentToken(c *gin.Context) string {
	value, ok := c.Get(contextTokenKey)
	if !ok {
		return ""
	}
	token, _ := value.(string)
	return token
}

func bearerToken(header string) string {
	parts := strings.Fields(strings.TrimSpace(header))
	if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") {
		return ""
	}
	return strings.TrimSpace(parts[1])
}
