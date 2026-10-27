package store

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
)

// OpenFile keeps the data in a local JSON file, starting empty if it does not exist yet.
// The directory must exist. Only one process may use the file.
func OpenFile(path string) (*Store, error) {
	return open(fileBackend{path: path}, nil)
}

type fileBackend struct{ path string }

func (f fileBackend) shared() bool { return false }

func (f fileBackend) load(_ context.Context, _ string) ([]byte, string, error) {
	raw, err := os.ReadFile(f.path)
	if err != nil {
		return nil, "", fmt.Errorf("store: read %s: %w", f.path, err) // wraps fs.ErrNotExist
	}
	return raw, "", nil
}

// save writes atomically: temp file in the same directory, fsync, rename.
// A crash mid-write leaves the previous file intact.
func (f fileBackend) save(_ context.Context, data []byte, _ string) (string, error) {
	if err := writeAtomic(f.path, data); err != nil {
		return "", fmt.Errorf("store: persist: %w", err)
	}
	return "", nil
}

// backup is a no-op for files: back up the file itself (cp from cron).
func (f fileBackend) backup(context.Context, string, []byte) error { return nil }

func writeAtomic(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".bookclub-*.tmp") // created with 0600
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op once the rename has succeeded

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}
