package asset

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/KDZZZZZZ/human-worth/backend/internal/platform"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

type s3RoundTrip func(*http.Request) (*http.Response, error)

func (f s3RoundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// SDK wire/error unit test only: this controlled transport is NOT an S3 store.
func TestS3StorageWireAndRecovery(t *testing.T) {
	store, err := NewS3Storage(S3Config{Endpoint: "http://s3.test", Bucket: "test-assets", Region: "us-east-1", AccessKey: "test-access", SecretKey: "test-secret", ForcePathStyle: true})
	if err != nil {
		t.Fatal(err)
	}
	objects := map[string][]byte{}
	var mu sync.Mutex
	failDelete := false
	options := store.client.Options()
	options.HTTPClient = &http.Client{Transport: s3RoundTrip(func(r *http.Request) (*http.Response, error) {
		mu.Lock()
		defer mu.Unlock()
		if !strings.HasPrefix(r.Header.Get("Authorization"), "AWS4-HMAC-SHA256 ") {
			t.Error("request is not SigV4 signed")
		}
		if r.URL.Host != "s3.test" || !strings.HasPrefix(r.URL.Path, "/test-assets") {
			t.Error("endpoint or path style changed")
		}
		key := strings.TrimPrefix(r.URL.Path, "/test-assets/")
		status, body := 200, []byte{}
		headers := http.Header{}
		if r.Method == "HEAD" && r.URL.Path == "/test-assets" { // readiness
		} else {
			switch r.Method {
			case "PUT":
				if source := r.Header.Get("X-Amz-Copy-Source"); source != "" {
					source = strings.TrimPrefix(source, "test-assets/")
					// SDK percent-encodes CopySource; test keys contain only slash/alphanumerics.
					source = strings.ReplaceAll(source, "%2F", "/")
					source = strings.TrimPrefix(source, "test-assets/")
					data, ok := objects[source]
					if !ok {
						return nil, fmt.Errorf("copy source missing")
					}
					objects[key] = bytes.Clone(data)
					body = []byte(`<CopyObjectResult><ETag>"test-etag"</ETag></CopyObjectResult>`)
				} else {
					data, e := io.ReadAll(r.Body)
					if e != nil {
						return nil, e
					}
					objects[key] = data
				}
			case "GET", "HEAD":
				data, ok := objects[key]
				if !ok {
					status = 404
					body = []byte(`<Error><Code>NoSuchKey</Code></Error>`)
				} else {
					headers.Set("Content-Length", fmt.Sprint(len(data)))
					headers.Set("ETag", `"test-etag"`)
					if r.Method == "GET" {
						body = bytes.Clone(data)
					}
				}
			case "DELETE":
				if failDelete {
					status = 503
					body = []byte(`<Error><Code>ServiceUnavailable</Code></Error>`)
				} else {
					delete(objects, key)
					status = 204
				}
			default:
				t.Errorf("unexpected operation %s", r.Method)
			}
		}
		return &http.Response{StatusCode: status, Header: headers, Body: io.NopCloser(bytes.NewReader(body)), Request: r}, nil
	})}
	store.client = s3.New(options)
	ctx := t.Context()
	if err = CheckStorage(ctx, store); err != nil {
		t.Fatal(err)
	}
	if err = store.WriteAt(ctx, "staging/upload", 0, []byte("hello wrong tail")); err != nil {
		t.Fatal(err)
	}
	// DB still says 5 received bytes: retry discards the uncommitted tail.
	if err = store.WriteAt(ctx, "staging/upload", 5, []byte(" world")); err != nil {
		t.Fatal(err)
	}
	if n, e := store.Stat(ctx, "staging/upload"); e != nil || n != 11 {
		t.Fatalf("size=%d err=%v", n, e)
	}
	failDelete = true
	if err = store.Move(ctx, "staging/upload", "final/content"); err == nil {
		t.Fatal("copy+failed delete reported success")
	}
	failDelete = false
	if err = store.Move(ctx, "staging/upload", "final/content"); err != nil {
		t.Fatal(err)
	}
	if err = store.Move(ctx, "staging/upload", "final/content"); err != nil {
		t.Fatal("move retry:", err)
	}
	reader, e := store.Open(ctx, "final/content")
	if e != nil {
		t.Fatal(e)
	}
	data, e := io.ReadAll(reader)
	reader.Close()
	if e != nil || string(data) != "hello world" {
		t.Fatalf("data=%q err=%v", data, e)
	}
	if _, e = store.Stat(ctx, "staging/upload"); !errors.Is(e, os.ErrNotExist) {
		t.Fatal("staging remains", e)
	}
	if err = store.Delete(ctx, "final/content"); err != nil {
		t.Fatal(err)
	}
	if err = store.Delete(ctx, "final/content"); err != nil {
		t.Fatal(err)
	}
	if _, e = store.Open(ctx, "final/content"); !errors.Is(e, os.ErrNotExist) {
		t.Fatal("missing object error", e)
	}
	for _, offset := range []int64{-1, maxAssetBytes} {
		if e = store.WriteAt(ctx, "bad", offset, []byte("x")); e == nil {
			t.Fatal("out of bounds write accepted")
		}
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if e = store.WriteAt(cancelled, "cancelled", 0, []byte("x")); e == nil {
		t.Fatal("cancelled write accepted")
	}
}

func TestStorageConfigurationFailsClosed(t *testing.T) {
	for _, key := range []string{"ASSET_STORAGE_BACKEND", "ASSET_STORAGE_DIR", "ASSET_ALLOW_LOCAL_STORAGE", "ASSET_S3_ACCESS_KEY_FILE", "ASSET_S3_SECRET_KEY_FILE", "ASSET_S3_ENDPOINT", "ASSET_S3_BUCKET", "ASSET_S3_REGION", "ASSET_S3_FORCE_PATH_STYLE"} {
		t.Setenv(key, "")
	}
	t.Setenv("ASSET_STORAGE_DIR", t.TempDir())
	storage, err := StorageFromEnv()
	if err == nil || storage != nil {
		t.Fatal("missing S3 config silently fell back")
	}
	runtime := platform.NewRuntime("asset")
	probe := runtime.Health(":0", func(ctx context.Context) error { return CheckStorage(ctx, storage) })
	response := httptest.NewRecorder()
	probe.Handler.ServeHTTP(response, httptest.NewRequest("GET", "/readyz", nil))
	if response.Code != 503 {
		t.Fatalf("missing storage readyz=%d", response.Code)
	}
	t.Setenv("ASSET_STORAGE_BACKEND", "filesystem")
	if _, err = StorageFromEnv(); err == nil {
		t.Fatal("filesystem lacks explicit single-process opt-in")
	}
	t.Setenv("ASSET_ALLOW_LOCAL_STORAGE", "true")
	if storage, err = StorageFromEnv(); err != nil || CheckStorage(t.Context(), storage) != nil {
		t.Fatal(err)
	}
	secretDir := t.TempDir()
	for _, key := range []string{"ASSET_S3_ACCESS_KEY_FILE", "ASSET_S3_SECRET_KEY_FILE"} {
		path := filepath.Join(secretDir, key)
		if err = os.WriteFile(path, []byte("test-secret"), 0600); err != nil {
			t.Fatal(err)
		}
		t.Setenv(key, path)
	}
	t.Setenv("ASSET_STORAGE_BACKEND", "s3")
	t.Setenv("ASSET_S3_ENDPOINT", "http://s3.test")
	t.Setenv("ASSET_S3_BUCKET", "test-assets")
	t.Setenv("ASSET_S3_REGION", "us-east-1")
	t.Setenv("ASSET_S3_FORCE_PATH_STYLE", "sometimes")
	if _, err = StorageFromEnv(); err == nil {
		t.Fatal("invalid boolean accepted")
	}
	t.Setenv("ASSET_S3_FORCE_PATH_STYLE", "true")
	if storage, err = StorageFromEnv(); err != nil || storage == nil {
		t.Fatal(err)
	}
}
