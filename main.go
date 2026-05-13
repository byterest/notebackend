package main

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	_ "modernc.org/sqlite"
)

const (
	defaultCanvasName = "Untitled Canvas"
	defaultCanvasData = `{"nodes":[],"edges":[],"viewport":{"x":0,"y":0,"zoom":1}}`
	maxUploadSize     = 20 << 20
)

type canvas struct {
	ID        int64     `json:"id"`
	Name      string    `json:"name"`
	Data      string    `json:"data"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

type app struct {
	db        *sql.DB
	uploadDir string
}

func main() {
	port := envOrDefault("PORT", "8081")
	frontendOrigin := envOrDefault("FRONTEND_ORIGIN", "http://localhost:5174")
	databasePath := envOrDefault("DATABASE_PATH", "./note.db")
	uploadDir := envOrDefault("UPLOAD_DIR", "./uploads")

	if err := os.MkdirAll(uploadDir, 0o755); err != nil {
		log.Fatalf("create upload dir: %v", err)
	}

	db, err := sql.Open("sqlite", databasePath)
	if err != nil {
		log.Fatalf("open database: %v", err)
	}
	defer db.Close()

	if err := initDB(db); err != nil {
		log.Fatalf("init database: %v", err)
	}

	application := &app{db: db, uploadDir: uploadDir}
	router := gin.Default()
	router.MaxMultipartMemory = maxUploadSize
	router.Use(cors(frontendOrigin))

	router.GET("/api/health", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"status": "ok"})
	})
	router.StaticFS("/uploads", http.Dir(uploadDir))

	api := router.Group("/api")
	{
		api.GET("/canvases", application.listCanvases)
		api.POST("/canvases", application.createCanvas)
		api.GET("/canvases/:id", application.getCanvas)
		api.PUT("/canvases/:id", application.updateCanvas)
		api.DELETE("/canvases/:id", application.deleteCanvas)
		api.POST("/uploads/images", application.uploadImage)
	}

	log.Printf("backend listening on http://localhost:%s", port)
	if err := router.Run(":" + port); err != nil {
		log.Fatalf("run server: %v", err)
	}

}

func envOrDefault(key, fallback string) string {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return fallback
	}
	return value
}

func cors(origin string) gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Header("Access-Control-Allow-Origin", origin)
		c.Header("Access-Control-Allow-Methods", "GET,POST,PUT,DELETE,OPTIONS")
		c.Header("Access-Control-Allow-Headers", "Content-Type")
		if c.Request.Method == http.MethodOptions {
			c.AbortWithStatus(http.StatusNoContent)
			return
		}
		c.Next()
	}
}

func initDB(db *sql.DB) error {
	schema := `
	CREATE TABLE IF NOT EXISTS canvases (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		name TEXT NOT NULL DEFAULT 'Untitled Canvas',
		data TEXT NOT NULL DEFAULT '{"nodes":[],"edges":[],"viewport":{"x":0,"y":0,"zoom":1}}',
		created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
		updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
	);
	`
	_, err := db.Exec(schema)
	return err
}

func (a *app) listCanvases(c *gin.Context) {
	rows, err := a.db.Query(`SELECT id, name, data, created_at, updated_at FROM canvases ORDER BY updated_at DESC, id DESC`)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	defer rows.Close()

	items := make([]canvas, 0)
	for rows.Next() {
		item, scanErr := scanCanvas(rows.Scan)
		if scanErr != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": scanErr.Error()})
			return
		}
		items = append(items, item)
	}

	c.JSON(http.StatusOK, gin.H{"data": items})
}

func (a *app) createCanvas(c *gin.Context) {
	var request struct {
		Name string `json:"name"`
		Data string `json:"data"`
	}
	if err := c.ShouldBindJSON(&request); err != nil && !errors.Is(err, ioEOF()) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}

	name := strings.TrimSpace(request.Name)
	if name == "" {
		name = defaultCanvasName
	}
	data := normalizeCanvasData(request.Data)

	result, err := a.db.Exec(`INSERT INTO canvases (name, data) VALUES (?, ?)`, name, data)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	id, _ := result.LastInsertId()
	canvasItem, err := a.fetchCanvas(id)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusCreated, gin.H{"data": canvasItem})
}

func (a *app) getCanvas(c *gin.Context) {
	id, err := parseID(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid canvas id"})
		return
	}

	canvasItem, err := a.fetchCanvas(id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			c.JSON(http.StatusNotFound, gin.H{"error": "canvas not found"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{"data": canvasItem})
}

func (a *app) updateCanvas(c *gin.Context) {
	id, err := parseID(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid canvas id"})
		return
	}

	current, err := a.fetchCanvas(id)
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
		trimmed := strings.TrimSpace(*request.Name)
		if trimmed == "" {
			trimmed = defaultCanvasName
		}
		name = trimmed
	}

	data := current.Data
	if request.Data != nil {
		data = normalizeCanvasData(*request.Data)
	}

	_, err = a.db.Exec(`UPDATE canvases SET name = ?, data = ?, updated_at = CURRENT_TIMESTAMP WHERE id = ?`, name, data, id)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	updated, err := a.fetchCanvas(id)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{"data": updated})
}

func (a *app) deleteCanvas(c *gin.Context) {
	id, err := parseID(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid canvas id"})
		return
	}

	result, err := a.db.Exec(`DELETE FROM canvases WHERE id = ?`, id)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	affected, _ := result.RowsAffected()
	if affected == 0 {
		c.JSON(http.StatusNotFound, gin.H{"error": "canvas not found"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"ok": true})
}

func (a *app) uploadImage(c *gin.Context) {
	fileHeader, err := c.FormFile("image")
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "image is required"})
		return
	}

	if fileHeader.Size > maxUploadSize {
		c.JSON(http.StatusBadRequest, gin.H{"error": "image exceeds 20MB limit"})
		return
	}

	if err := validateImage(fileHeader); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	filename := fmt.Sprintf("%d%s", time.Now().UnixNano(), filepath.Ext(fileHeader.Filename))
	path := filepath.Join(a.uploadDir, filename)
	if err := c.SaveUploadedFile(fileHeader, path); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	baseURL := fmt.Sprintf("%s://%s", scheme(c.Request), c.Request.Host)
	if forwarded := c.GetHeader("X-Forwarded-Host"); forwarded != "" {
		proto := c.GetHeader("X-Forwarded-Proto")
		if proto == "" {
			proto = scheme(c.Request)
		}
		baseURL = fmt.Sprintf("%s://%s", proto, forwarded)
	}

	c.JSON(http.StatusCreated, gin.H{
		"data": gin.H{
			"url": baseURL + "/uploads/" + filename,
			"filename": filename,
		},
	})
}

func (a *app) fetchCanvas(id int64) (canvas, error) {
	row := a.db.QueryRow(`SELECT id, name, data, created_at, updated_at FROM canvases WHERE id = ?`, id)
	return scanCanvas(row.Scan)
}

func scanCanvas(scan func(dest ...any) error) (canvas, error) {
	var item canvas
	if err := scan(&item.ID, &item.Name, &item.Data, &item.CreatedAt, &item.UpdatedAt); err != nil {
		return canvas{}, err
	}
	return item, nil
}

func parseID(raw string) (int64, error) {
	return strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
}

func normalizeCanvasData(raw string) string {
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

func validateImage(fileHeader *multipart.FileHeader) error {
	file, err := fileHeader.Open()
	if err != nil {
		return err
	}
	defer file.Close()

	buffer := make([]byte, 512)
	count, err := file.Read(buffer)
	if err != nil && !errors.Is(err, os.ErrClosed) && !errors.Is(err, sql.ErrNoRows) {
		// Ignore EOF-like short read cases.
	}

	contentType := http.DetectContentType(buffer[:count])
	if !strings.HasPrefix(contentType, "image/") {
		return errors.New("only image uploads are supported")
	}
	return nil
}

func scheme(request *http.Request) string {
	if request.TLS != nil {
		return "https"
	}
	return "http"
}

func ioEOF() error {
	return errors.New("EOF")
}