package logging

import (
	"io"
	"os"
	"path/filepath"
)

func Open(path string) (func() error, io.Writer, error) {
	if path == "" {
		return func() error { return nil }, os.Stdout, nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, nil, err
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, nil, err
	}
	return file.Close, file, nil
}
