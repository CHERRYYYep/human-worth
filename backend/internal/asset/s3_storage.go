package asset

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/KDZZZZZZ/human-worth/backend/internal/platform"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/smithy-go"
)

// S3Storage uses private, pre-provisioned buckets. It never creates a bucket,
// changes an ACL, returns a public URL, or falls back to a local directory.
type S3Storage struct {
	client     *s3.Client
	bucket     string
	writeSlots chan struct{}
}
type S3Config struct {
	Endpoint       string `json:"endpoint"`
	Bucket         string `json:"bucket"`
	Region         string `json:"region"`
	AccessKey      string `json:"access_key"`
	SecretKey      string `json:"secret_key"`
	ForcePathStyle bool   `json:"force_path_style"`
}

func NewS3Storage(config S3Config) (*S3Storage, error) {
	endpoint, err := url.Parse(config.Endpoint)
	if err != nil || endpoint.Host == "" || (endpoint.Scheme != "http" && endpoint.Scheme != "https") || endpoint.User != nil || endpoint.RawQuery != "" || endpoint.Fragment != "" || (endpoint.Path != "" && endpoint.Path != "/") || config.Bucket == "" || strings.ContainsAny(config.Bucket, "/\\") || config.Region == "" || config.AccessKey == "" || config.SecretKey == "" {
		return nil, errors.New("invalid S3 configuration")
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.MaxIdleConnsPerHost = 16
	client := s3.New(s3.Options{
		Region: config.Region, BaseEndpoint: aws.String(strings.TrimRight(config.Endpoint, "/")),
		Credentials:  credentials.NewStaticCredentialsProvider(config.AccessKey, config.SecretKey, ""),
		UsePathStyle: config.ForcePathStyle, RetryMaxAttempts: 1,
		HTTPClient: &http.Client{Transport: transport, Timeout: 30 * time.Second},
	})
	return &S3Storage{client: client, bucket: config.Bucket, writeSlots: make(chan struct{}, 2)}, nil
}

func StorageFromEnv() (Storage, error) {
	switch platform.Value("ASSET_STORAGE_BACKEND", "s3") {
	case "s3":
		access, err := platform.Secret("ASSET_S3_ACCESS_KEY_FILE")
		if err != nil {
			return nil, err
		}
		secret, err := platform.Secret("ASSET_S3_SECRET_KEY_FILE")
		if err != nil {
			return nil, err
		}
		style := os.Getenv("ASSET_S3_FORCE_PATH_STYLE")
		if style != "" && style != "true" && style != "false" {
			return nil, errors.New("invalid S3 path-style configuration")
		}
		return NewS3Storage(S3Config{Endpoint: os.Getenv("ASSET_S3_ENDPOINT"), Bucket: os.Getenv("ASSET_S3_BUCKET"), Region: os.Getenv("ASSET_S3_REGION"), AccessKey: access, SecretKey: secret, ForcePathStyle: style == "true"})
	case "filesystem":
		if os.Getenv("ASSET_ALLOW_LOCAL_STORAGE") != "true" || os.Getenv("ASSET_STORAGE_DIR") == "" {
			return nil, errors.New("filesystem storage requires explicit single-process test opt-in")
		}
		return FileStorage{Root: os.Getenv("ASSET_STORAGE_DIR")}, nil
	default:
		return nil, errors.New("unsupported object storage backend")
	}
}

// CheckStorage leaves the five-method Storage contract unchanged.
func CheckStorage(ctx context.Context, storage Storage) error {
	if storage == nil {
		return errors.New("object storage unavailable")
	}
	if remote, ok := storage.(*S3Storage); ok {
		_, err := remote.client.HeadBucket(ctx, &s3.HeadBucketInput{Bucket: aws.String(remote.bucket)})
		return err
	}
	local, ok := storage.(FileStorage)
	if !ok {
		return errors.New("unknown storage backend")
	}
	info, err := os.Stat(local.Root)
	if err == nil && !info.IsDir() {
		return errors.New("storage directory unavailable")
	}
	return err
}
func s3Error(err error) error {
	var api smithy.APIError
	if errors.As(err, &api) && (api.ErrorCode() == "NoSuchKey" || api.ErrorCode() == "NotFound") {
		return os.ErrNotExist
	}
	return err
}

// ponytail: bounded prefix rewrite, not multipart. At 32 MiB, 256 KiB chunks
// cause ~2 GiB of writes per full upload. Revisit with multipart when increasing
// capacity. The caller must hold the database row lock across WriteAt+commit.
// Rewriting from the authoritative offset also discards an uncommitted tail
// left by a successful PUT followed by a failed database commit.
func (s *S3Storage) WriteAt(ctx context.Context, key string, offset int64, data []byte) error {
	if offset < 0 || len(data) == 0 || len(data) > maxChunkBytes || offset > maxAssetBytes-int64(len(data)) {
		return errors.New("invalid object write")
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	select {
	case s.writeSlots <- struct{}{}:
		defer func() { <-s.writeSlots }()
	case <-ctx.Done():
		return ctx.Err()
	}
	body := make([]byte, int(offset)+len(data))
	if offset > 0 {
		reader, err := s.Open(ctx, key)
		if err != nil {
			return err
		}
		_, readErr := io.ReadFull(reader, body[:int(offset)])
		closeErr := reader.Close()
		if readErr != nil {
			return readErr
		}
		if closeErr != nil {
			return closeErr
		}
	}
	copy(body[int(offset):], data)
	_, err := s.client.PutObject(ctx, &s3.PutObjectInput{Bucket: aws.String(s.bucket), Key: aws.String(key), Body: bytes.NewReader(body), ContentLength: aws.Int64(int64(len(body)))})
	return err
}
func (s *S3Storage) Stat(ctx context.Context, key string) (int64, error) {
	result, err := s.client.HeadObject(ctx, &s3.HeadObjectInput{Bucket: aws.String(s.bucket), Key: aws.String(key)})
	if err != nil {
		return 0, s3Error(err)
	}
	return aws.ToInt64(result.ContentLength), nil
}
func (s *S3Storage) Open(ctx context.Context, key string) (io.ReadCloser, error) {
	result, err := s.client.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(s.bucket), Key: aws.String(key)})
	if err != nil {
		return nil, s3Error(err)
	}
	return result.Body, nil
}

// Move is copy+delete, NOT rename. Copy completes before deleting staging.
// On retry, repeat the copy if staging exists; if it is already gone, the
// server verifies final's full size, SHA-256 and media type before committing.
func (s *S3Storage) Move(ctx context.Context, from, to string) error {
	source, err := s.client.HeadObject(ctx, &s3.HeadObjectInput{Bucket: aws.String(s.bucket), Key: aws.String(from)})
	if errors.Is(s3Error(err), os.ErrNotExist) {
		_, err = s.Stat(ctx, to)
		return err
	}
	if err != nil {
		return err
	}
	_, err = s.client.CopyObject(ctx, &s3.CopyObjectInput{Bucket: aws.String(s.bucket), Key: aws.String(to), CopySource: aws.String(url.PathEscape(s.bucket + "/" + from)), CopySourceIfMatch: source.ETag})
	if err != nil {
		return err
	}
	size, err := s.Stat(ctx, to)
	if err != nil {
		return err
	}
	if size != aws.ToInt64(source.ContentLength) {
		return errors.New("object copy incomplete")
	}
	return s.Delete(ctx, from)
}
func (s *S3Storage) Delete(ctx context.Context, key string) error {
	_, err := s.client.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String(s.bucket), Key: aws.String(key)})
	return err
}
