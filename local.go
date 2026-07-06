package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// LocalUploader copies files into another local directory (mirror),
// handy for local testing or as a prototype for other backends.
type LocalUploader struct {
	mirrorDir string
}

// NewLocalUploader builds the local mirror uploader, creating mirrorDir if missing.
func NewLocalUploader(cfg LocalConfig) (*LocalUploader, error) {
	if cfg.MirrorDir == "" {
		return nil, fmt.Errorf("uploader: local uploader missing mirror_dir")
	}
	if err := os.MkdirAll(cfg.MirrorDir, 0o755); err != nil {
		return nil, fmt.Errorf("uploader: create mirror_dir failed: %w", err)
	}
	return &LocalUploader{mirrorDir: cfg.MirrorDir}, nil
}

// Upload copies src to mirrorDir/key. When overwrite=false and the target exists, returns ErrExists.
func (u *LocalUploader) Upload(_ context.Context, src, key string, overwrite bool) error {
	dst := filepath.Join(u.mirrorDir, filepath.FromSlash(key))

	if !overwrite {
		if _, err := os.Stat(dst); err == nil {
			return ErrExists
		} else if !os.IsNotExist(err) {
			return fmt.Errorf("uploader: check local target failed: %w", err)
		}
	}

	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return fmt.Errorf("uploader: create local dir failed: %w", err)
	}

	in, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("uploader: open source failed: %w", err)
	}
	defer func() { _ = in.Close() }()

	out, err := os.Create(dst)
	if err != nil {
		return fmt.Errorf("uploader: create target failed: %w", err)
	}
	defer func() { _ = out.Close() }()

	if _, err := io.Copy(out, in); err != nil {
		return fmt.Errorf("uploader: copy file failed: %w", err)
	}
	return nil
}
