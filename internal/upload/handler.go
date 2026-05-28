package upload

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	notes3 "note-backend/internal/s3"
)

// S3Uploader defines the minimal interface for S3-like uploads.
type S3Uploader interface {
	Upload(ctx context.Context, key string, reader io.Reader, contentType string) (string, error)
	PublicURL(endpoint string, key string) string
}

// Handler exposes file upload endpoints.
type Handler struct {
	uploadDir     string
	maxUploadSize int64
	s3Client      S3Uploader
	s3Endpoint    string
}

// NewHandler creates an upload handler.
func NewHandler(uploadDir string, maxUploadSize int64) *Handler {
	return &Handler{uploadDir: uploadDir, maxUploadSize: maxUploadSize}
}

// NewHandlerWithS3 creates an upload handler with S3 support.
func NewHandlerWithS3(uploadDir string, maxUploadSize int64, s3Client S3Uploader, s3Endpoint string) *Handler {
	return &Handler{
		uploadDir:  uploadDir,
		maxUploadSize: maxUploadSize,
		s3Client:   s3Client,
		s3Endpoint: s3Endpoint,
	}
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

	if err := ValidateImage(fileHeader); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	file, err := fileHeader.Open()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	defer file.Close()

	var filename, url string

	if h.s3Client != nil {
		// S3 upload
		key := notes3.GenerateKey("uploads", fileHeader.Filename)
		data, err := io.ReadAll(file)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		contentType := http.DetectContentType(data)
		_, err = h.s3Client.Upload(c.Request.Context(), key, bytes.NewReader(data), contentType)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		filename = filepath.Base(key)
		url = h.s3Client.PublicURL(h.s3Endpoint, key)
	} else {
		// Local storage fallback
		filename = fmt.Sprintf("%d%s", time.Now().UnixNano(), filepath.Ext(fileHeader.Filename))
		path := filepath.Join(h.uploadDir, filename)
		if err := c.SaveUploadedFile(fileHeader, path); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}

		host := c.Request.Host
		if forwardedHost := c.GetHeader("X-Forwarded-Host"); forwardedHost != "" {
			host = forwardedHost
		}
		proto := scheme(c.Request)
		if forwardedProto := c.GetHeader("X-Forwarded-Proto"); forwardedProto != "" {
			proto = forwardedProto
		}
		baseURL := fmt.Sprintf("%s://%s", proto, host)
		url = baseURL + "/uploads/" + filename
	}

	c.JSON(http.StatusCreated, gin.H{
		"data": gin.H{
			"url":      url,
			"filename": filename,
		},
	})
}

// ValidateImage checks if the uploaded file is a valid image.
func ValidateImage(fileHeader *multipart.FileHeader) error {
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
