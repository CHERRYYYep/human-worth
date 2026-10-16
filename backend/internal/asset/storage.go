package asset

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
)

// Storage contains bytes only. The database remains the source of asset state.
type Storage interface {
	WriteAt(context.Context, string, int64, []byte) error
	Stat(context.Context, string) (int64, error)
	Open(context.Context, string) (io.ReadCloser, error)
	Move(context.Context, string, string) error
	Delete(context.Context, string) error
}

// FileStorage is deliberately a local-test adapter. It is not safe as the
// backing store for multiple Asset replicas.
type FileStorage struct{ Root string }

func (s FileStorage) path(key string) string { return filepath.Join(s.Root, filepath.FromSlash(key)) }
func (s FileStorage) WriteAt(ctx context.Context, key string, offset int64, data []byte) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}
	path := s.path(key)
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	_, writeErr := f.WriteAt(data, offset)
	closeErr := f.Close()
	if writeErr != nil {
		return writeErr
	}
	return closeErr
}
func (s FileStorage) Stat(ctx context.Context, key string) (int64, error) {
	select {
	case <-ctx.Done():
		return 0, ctx.Err()
	default:
	}
	info, err := os.Stat(s.path(key))
	if err != nil {
		return 0, err
	}
	return info.Size(), nil
}
func (s FileStorage) Open(ctx context.Context, key string) (io.ReadCloser, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	default:
	}
	return os.Open(s.path(key))
}
func (s FileStorage) Move(ctx context.Context, from, to string) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}
	source, destination := s.path(from), s.path(to)
	if _, err := os.Stat(destination); err == nil {
		return s.Delete(ctx, from)
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(destination), 0700); err != nil {
		return err
	}
	return os.Rename(source, destination)
}
func (s FileStorage) Delete(ctx context.Context, key string) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}
	err := os.Remove(s.path(key))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}
