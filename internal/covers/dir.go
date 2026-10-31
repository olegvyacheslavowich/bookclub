package covers

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// Dir stores covers as files in a local directory: for development and for a VPS.
type Dir struct{ path string }

// NewDir creates the directory if needed.
func NewDir(path string) (*Dir, error) {
	if err := os.MkdirAll(path, 0o700); err != nil {
		return nil, fmt.Errorf("covers: create %s: %w", path, err)
	}
	return &Dir{path: path}, nil
}

func (d *Dir) Put(_ context.Context, name string, data []byte) error {
	if err := checkName(name); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(d.path, name+".*.tmp")
	if err != nil {
		return fmt.Errorf("covers: %w", err)
	}
	defer os.Remove(tmp.Name()) // no-op after a successful rename
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("covers: write: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("covers: write: %w", err)
	}
	if err := os.Rename(tmp.Name(), filepath.Join(d.path, name)); err != nil {
		return fmt.Errorf("covers: %w", err)
	}
	return nil
}

func (d *Dir) Get(_ context.Context, name string) ([]byte, error) {
	if err := checkName(name); err != nil {
		return nil, err
	}
	data, err := os.ReadFile(filepath.Join(d.path, name))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, ErrNotFound
	}
	return data, err
}

func (d *Dir) Delete(_ context.Context, name string) error {
	if err := checkName(name); err != nil {
		return err
	}
	err := os.Remove(filepath.Join(d.path, name))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return err
}
