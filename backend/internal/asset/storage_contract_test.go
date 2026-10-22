package asset

import (
	"context"
	"io"
	"testing"
)

// Run the same observable WriteAt contract against FileStorage and S3.
func storageWriteContract(t *testing.T, storage Storage) {
	t.Helper()
	ctx := t.Context()
	key := "contract/prefix"
	if err := storage.WriteAt(ctx, key, 0, []byte("hello wrong tail")); err != nil {
		t.Fatal(err)
	}
	if err := storage.WriteAt(ctx, key, 5, []byte(" world")); err != nil {
		t.Fatal(err)
	}
	if n, err := storage.Stat(ctx, key); err != nil || n != 11 {
		t.Fatalf("truncate: size=%d err=%v", n, err)
	}
	if err := storage.WriteAt(ctx, key, 12, []byte("hole")); err == nil {
		t.Fatal("accepted a hole")
	}
	r, err := storage.Open(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	b, err := io.ReadAll(r)
	closeErr := r.Close()
	if err != nil || closeErr != nil || string(b) != "hello world" {
		t.Fatalf("prefix/content changed: %q %v %v", b, err, closeErr)
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if err := storage.WriteAt(cancelled, key, 0, []byte("bad")); err == nil {
		t.Fatal("cancelled write accepted")
	}
	for _, offset := range []int64{-1, maxAssetBytes} {
		if err := storage.WriteAt(ctx, key, offset, []byte("bad")); err == nil {
			t.Fatal("invalid bounds accepted")
		}
	}
	if err := storage.Delete(ctx, key); err != nil {
		t.Fatal(err)
	}
}
func TestFileStorageWriteContract(t *testing.T) {
	storageWriteContract(t, FileStorage{Root: t.TempDir()})
}
