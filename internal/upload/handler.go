package upload

import (
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

// Handler exposes file upload endpoints.
type Handler struct {
	uploadDir     string
	maxUploadSize int64
}

// NewHandler creates an upload handler.
func NewHandler(uploadDir string, maxUploadSize int64) *Handler {
	return &Handler{uploadDir: uploadDir, maxUploadSize: maxUploadSize}
}

// Image stores an uploaded image and returns its URL.
func (h *Handler) Image(c *gin.Context) {
	fileHeader, err := c.FormFile("image")
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "image is required"})
		return
	}

	if fileHeader.Size > h.maxUploadSize {
		c.JSON(http.StatusBadRequest, gin.H{"error": "image exceeds 20MB limit"})
		return
	}

	if err := validateImage(fileHeader); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	filename := fmt.Sprintf("%d%s", time.Now().UnixNano(), filepath.Ext(fileHeader.Filename))
	path := filepath.Join(h.uploadDir, filename)
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
			"url":      baseURL + "/uploads/" + filename,
			"filename": filename,
		},
	})
}

func validateImage(fileHeader *multipart.FileHeader) error {
	file, err := fileHeader.Open()
	if err != nil {
		return err
	}
	defer file.Close()

	buffer := make([]byte, 512)
	count, err := file.Read(buffer)
	if err != nil && !errors.Is(err, io.EOF) {
		return err
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
