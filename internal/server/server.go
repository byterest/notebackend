package server

import (
	"database/sql"
	"net/http"

	"note-backend/internal/auth"
	"note-backend/internal/canvas"
	"note-backend/internal/config"
	"note-backend/internal/upload"

	"github.com/gin-gonic/gin"
)

// NewRouter wires middleware and routes.
func NewRouter(cfg config.Config, db *sql.DB) *gin.Engine {
	authStore := auth.NewStore(db, cfg.SessionTTL)
	authHandler := auth.NewHandler(authStore)
	canvasHandler := canvas.NewHandler(canvas.NewStore(db))
	uploadHandler := upload.NewHandler(cfg.UploadDir, cfg.MaxUploadSize)

	router := gin.Default()
	router.MaxMultipartMemory = cfg.MaxUploadSize
	router.Use(cors(cfg.FrontendOrigin))

	router.GET("/api/health", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"status": "ok"})
	})
	router.StaticFS("/uploads", http.Dir(cfg.UploadDir))

	api := router.Group("/api")
	{
		api.POST("/auth/register", authHandler.Register)
		api.POST("/auth/login", authHandler.Login)

		protected := api.Group("")
		protected.Use(auth.Middleware(authStore))
		{
			protected.GET("/auth/me", authHandler.Me)
			protected.POST("/auth/logout", authHandler.Logout)

			protected.GET("/canvases", canvasHandler.List)
			protected.POST("/canvases", canvasHandler.Create)
			protected.GET("/canvases/:id", canvasHandler.Get)
			protected.GET("/canvases/:id/stream", canvasHandler.Stream)
			protected.PUT("/canvases/:id", canvasHandler.Update)
			protected.DELETE("/canvases/:id", canvasHandler.Delete)
			protected.POST("/uploads/images", uploadHandler.Image)
		}
	}

	return router
}

func cors(origin string) gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Header("Access-Control-Allow-Origin", origin)
		c.Header("Access-Control-Allow-Methods", "GET,POST,PUT,DELETE,OPTIONS")
		c.Header("Access-Control-Allow-Headers", "Authorization, Content-Type")
		if c.Request.Method == http.MethodOptions {
			c.AbortWithStatus(http.StatusNoContent)
			return
		}
		c.Next()
	}
}
