package asset

import (
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestFilenameAndMediaValidation(t *testing.T) {
	name, err := cleanFilename(`dir\\report.txt`)
	if err != nil || name != "report.txt" {
		t.Fatalf("filename=%q err=%v", name, err)
	}
	if _, err = cleanFilename("\x00"); err == nil {
		t.Fatal("accepted control-only filename")
	}
	if _, err = mediaType("application/x-executable"); err == nil {
		t.Fatal("accepted non-whitelisted media type")
	}
	if got, err := mediaType("TEXT/PLAIN; charset=utf-8"); err != nil || got != "text/plain" {
		t.Fatalf("media=%q err=%v", got, err)
	}
}

func TestFileStorageStagingLifecycle(t *testing.T) {
	storage := FileStorage{Root: t.TempDir()}
	ctx := context.Background()
	key := "assets/ast_test/staging/upload"
	if err := storage.WriteAt(ctx, key, 0, []byte("hello ")); err != nil {
		t.Fatal(err)
	}
	if err := storage.WriteAt(ctx, key, 6, []byte("world")); err != nil {
		t.Fatal(err)
	}
	if size, err := storage.Stat(ctx, key); err != nil || size != 11 {
		t.Fatalf("size=%d err=%v", size, err)
	}
	if err := storage.Move(ctx, key, "assets/ast_test/content"); err != nil {
		t.Fatal(err)
	}
	reader, err := storage.Open(ctx, "assets/ast_test/content")
	if err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(reader)
	reader.Close()
	if err != nil || string(data) != "hello world" {
		t.Fatalf("data=%q err=%v", data, err)
	}
	if err := storage.Delete(ctx, "assets/ast_test/content"); err != nil {
		t.Fatal(err)
	}
	if err := storage.Delete(ctx, "assets/ast_test/content"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(storage.Root, "assets/ast_test/content")); !os.IsNotExist(err) {
		t.Fatalf("object remains: %v", err)
	}
}

func TestAssetGuards(t *testing.T) {
	if !bytesEqual([]byte("same"), []byte("same")) || bytesEqual([]byte("same"), []byte("different")) {
		t.Fatal("unexpected byte comparison")
	}
	if err := storageReady(&Server{}); err == nil {
		t.Fatal("missing storage accepted")
	}
	if _, _, err := decodeAssetCursor("not-a-cursor", "acct_alice"); err == nil {
		t.Fatal("malformed cursor accepted")
	}
}

func TestObjectDigestUsesStoredBytes(t *testing.T) {
	storage := FileStorage{Root: t.TempDir()}
	ctx := t.Context()
	data := []byte("<html><body>not a PNG</body></html>")
	key := "assets/ast_test/staging/sniff"
	if err := storage.WriteAt(ctx, key, 0, data); err != nil {
		t.Fatal(err)
	}
	server := &Server{Storage: storage}
	digest, size, detected, err := server.objectDigest(ctx, key)
	expected := sha256.Sum256(data)
	if err != nil || size != int64(len(data)) || !bytesEqual(digest, expected[:]) || detected != "text/html" {
		t.Fatalf("stored digest=%x size=%d detected=%q err=%v", digest, size, detected, err)
	}
	if err = validateDetectedMediaType("image/png", detected); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("HTML accepted: %v", err)
	}
	if err = validateDetectedMediaType("image/png", "text/plain"); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("mismatch accepted: %v", err)
	}
	if err = validateDetectedMediaType("image/png", "image/png"); err != nil {
		t.Fatal(err)
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, _, _, err = server.objectDigest(cancelled, key); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation ignored: %v", err)
	}
}
