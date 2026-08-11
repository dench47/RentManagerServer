package service

import (
	"bytes"
	"context"
	"fmt"
	"log"

	"rentmanager-server/internal/config"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

type S3Service struct {
	client    *minio.Client
	bucket    string
	publicURL string
}

func NewS3Service(cfg config.S3Config) *S3Service {
	endpoint := cfg.Endpoint
	secure := true
	if len(endpoint) > 8 && endpoint[:8] == "https://" {
		endpoint = endpoint[8:]
	} else if len(endpoint) > 7 && endpoint[:7] == "http://" {
		endpoint = endpoint[7:]
		secure = false
	}

	client, err := minio.New(endpoint, &minio.Options{
		Creds:  credentials.NewStaticV4(cfg.AccessKey, cfg.SecretKey, ""),
		Secure: secure,
		Region: cfg.Region,
	})
	if err != nil {
		log.Printf("WARNING: Failed to create S3 client: %v (uploads will fallback to local disk)", err)
		return nil
	}

	exists, err := client.BucketExists(context.Background(), cfg.Bucket)
	if err != nil || !exists {
		log.Printf("WARNING: S3 bucket %s not accessible: %v (uploads will fallback to local disk)", cfg.Bucket, err)
		return nil
	}

	log.Printf("S3 connected: bucket=%s publicURL=%s", cfg.Bucket, cfg.PublicURL)
	return &S3Service{
		client:    client,
		bucket:    cfg.Bucket,
		publicURL: cfg.PublicURL,
	}
}

// Upload uploads file content to S3 and returns the public URL.
// key is the object key (e.g., "avatars/abc.jpg").
func (s *S3Service) Upload(ctx context.Context, key string, content []byte) (string, error) {
	if s == nil {
		return "", fmt.Errorf("s3 service not initialized")
	}

	reader := bytes.NewReader(content)
	_, err := s.client.PutObject(ctx, s.bucket, key, reader, int64(len(content)), minio.PutObjectOptions{})
	if err != nil {
		return "", fmt.Errorf("s3 upload failed: %w", err)
	}

	publicURL := fmt.Sprintf("%s/%s", s.publicURL, key)
	return publicURL, nil
}

// Delete removes an object from S3 by its key.
func (s *S3Service) Delete(ctx context.Context, key string) error {
	if s == nil {
		return fmt.Errorf("s3 service not initialized")
	}
	err := s.client.RemoveObject(ctx, s.bucket, key, minio.RemoveObjectOptions{})
	if err != nil {
		return fmt.Errorf("s3 delete failed: %w", err)
	}
	return nil
}

// ExtractKey extracts the S3 object key from a public URL.
func (s *S3Service) ExtractKey(url string) string {
	prefix := s.publicURL + "/"
	if len(url) > len(prefix) && url[:len(prefix)] == prefix {
		return url[len(prefix):]
	}
	return ""
}

// BuildKey builds a storage key for an uploaded file.
func BuildKey(subfolder, filename string) string {
	return subfolder + "/" + filename
}
