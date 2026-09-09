package server

import (
	"database/sql"
	"log"
	"net/http"

	"note-backend/internal/auth"
	"note-backend/internal/canvas"
	"note-backend/internal/config"
	"note-backend/internal/s3"
	"note-backend/internal/upload"

	"github.com/gin-gonic/gin"
)

// NewRouter wires middleware and routes.
func NewRouter(cfg config.Config, db *sql.DB) *gin.Engine {
	authStore := auth.NewStore(db, cfg.SessionTTL)

	var s3Client *s3.Client
	var s3Endpoint string
	var s3Bucket string
	var authHandler *auth.Handler
	if cfg.S3 != nil {
		client, err := s3.NewClient(cfg.S3)
		if err != nil {
			log.Fatalf("create s3 client: %v", err)
		}
		s3Client = client
		s3Endpoint = cfg.S3.Endpoint
		s3Bucket = cfg.S3.Bucket
		authHandler = auth.NewHandlerWithS3(authStore, cfg.UploadDir, s3Client, s3Endpoint)
	} else {
		authHandler = auth.NewHandler(authStore, cfg.UploadDir)
	}

	canvasStore := canvas.NewStore(db)
	var assetClient canvas.AssetClient
	if s3Client != nil {
		assetClient = s3Client
	}
	canvasHandler := canvas.NewHandler(canvasStore, cfg.UploadDir, assetClient, s3Endpoint, s3Bucket)

	var uploadHandler *upload.Handler
	if s3Client != nil {
		uploadHandler = upload.NewHandlerWithS3(cfg.UploadDir, cfg.MaxUploadSize, s3Client, s3Endpoint)
	} else {
		uploadHandler = upload.NewHandler(cfg.UploadDir, cfg.MaxUploadSize)
	}

	router := gin.Default()
	router.MaxMultipartMemory = 200 << 20
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
			protected.PUT("/auth/profile", authHandler.UpdateProfile)
			protected.PUT("/auth/password", authHandler.UpdatePassword)
			protected.POST("/auth/logout", authHandler.Logout)
			protected.POST("/auth/avatar", authHandler.UploadAvatar)
			protected.DELETE("/auth/avatar", authHandler.DeleteAvatar)

			protected.GET("/canvases", canvasHandler.List)
			protected.POST("/canvases", canvasHandler.Create)
			protected.POST("/canvases/export", canvasHandler.ExportSnapshot)
			protected.POST("/canvases/import", canvasHandler.Import)
			protected.GET("/canvases/:id", canvasHandler.Get)
			protected.GET("/canvases/:id/export", canvasHandler.Export)
			protected.GET("/canvases/:id/stream", canvasHandler.Stream)
			protected.POST("/canvases/:id/lock", canvasHandler.AcquireLock)
			protected.DELETE("/canvases/:id/lock", canvasHandler.ReleaseLock)
			protected.PUT("/canvases/:id", canvasHandler.Update)
			protected.DELETE("/canvases/:id", canvasHandler.Delete)
			protected.POST("/uploads/images", uploadHandler.Image)
			protected.POST("/uploads/pdfs", uploadHandler.PDF)
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
