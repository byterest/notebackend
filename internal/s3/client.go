package s3

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	"note-backend/internal/config"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/feature/s3/manager"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// Client wraps an S3-compatible upload client.
type Client struct {
	client *s3.Client
	bucket string
}

func normalizeEndpoint(endpoint string, useSSL bool) string {
	if !strings.Contains(endpoint, "://") {
		if useSSL {
			endpoint = "https://" + endpoint
		} else {
			endpoint = "http://" + endpoint
		}
	}
	return strings.TrimRight(endpoint, "/")
}

// NewClient creates an S3-compatible client from config.
func NewClient(cfg *config.S3Config) (*Client, error) {
	endpoint := cfg.InternalEndpoint
	if endpoint == "" {
		endpoint = cfg.Endpoint
	}
	endpoint = normalizeEndpoint(endpoint, cfg.UseSSL)

	awsCfg := aws.Config{
		Credentials:                credentials.NewStaticCredentialsProvider(cfg.AccessKeyID, cfg.SecretAccessKey, ""),
		Region:                     cfg.Region,
		RequestChecksumCalculation: aws.RequestChecksumCalculationWhenRequired,
		ResponseChecksumValidation: aws.ResponseChecksumValidationWhenRequired,
		EndpointResolverWithOptions: aws.EndpointResolverWithOptionsFunc(func(service, region string, options ...interface{}) (aws.Endpoint, error) {
			if service == s3.ServiceID {
				return aws.Endpoint{
					URL:               endpoint,
					SigningRegion:     cfg.Region,
					HostnameImmutable: true,
					Source:            aws.EndpointSourceCustom,
				}, nil
			}
			return aws.Endpoint{}, fmt.Errorf("unknown endpoint requested")
		}),
	}

	client := s3.NewFromConfig(awsCfg, func(o *s3.Options) {
		o.UsePathStyle = true
		o.RequestChecksumCalculation = aws.RequestChecksumCalculationWhenRequired
		o.ResponseChecksumValidation = aws.ResponseChecksumValidationWhenRequired
		o.ContinueHeaderThresholdBytes = -1
		o.HTTPClient = &http.Client{
			Transport: &http.Transport{
				ForceAttemptHTTP2: false,
			},
		}
	})

	return &Client{
		client: client,
		bucket: cfg.Bucket,
	}, nil
}

// Upload sends a file to S3 and returns the object key.
func (c *Client) Upload(ctx context.Context, key string, reader io.Reader, contentType string) (string, error) {
	up := manager.NewUploader(c.client)
	_, err := up.Upload(ctx, &s3.PutObjectInput{
		Bucket:      aws.String(c.bucket),
		Key:         aws.String(key),
		Body:        reader,
		ContentType: aws.String(contentType),
	})
	if err != nil {
		return "", fmt.Errorf("s3 upload failed: %w", err)
	}
	return key, nil
}

// Download fetches an object from S3.
func (c *Client) Download(ctx context.Context, key string) (io.ReadCloser, string, error) {
	out, err := c.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(c.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return nil, "", fmt.Errorf("s3 download failed: %w", err)
	}

	contentType := ""
	if out.ContentType != nil {
		contentType = *out.ContentType
	}
	return out.Body, contentType, nil
}

// Delete removes an object from S3.
func (c *Client) Delete(ctx context.Context, key string) error {
	_, err := c.client.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(c.bucket),
		Key:    aws.String(key),
	})
	return err
}

// PublicURL generates a public-accessible URL for an object key.
func (c *Client) PublicURL(endpoint string, key string) string {
	return fmt.Sprintf("%s/%s/%s", strings.TrimRight(endpoint, "/"), strings.Trim(c.bucket, "/"), strings.TrimLeft(key, "/"))
}

// GenerateKey builds an S3 object key with prefix and timestamp.
func GenerateKey(prefix string, filename string) string {
	ext := filepath.Ext(filename)
	now := time.Now().Format("20060102/150405")
	return fmt.Sprintf("%s/%s_%d%s", prefix, now, time.Now().UnixNano(), ext)
}
