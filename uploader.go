package main

import (
	"context"
	"errors"
)

// ErrExists is returned by Upload when overwrite=false and the remote key already exists,
// so the caller can skip it.
var ErrExists = errors.New("uploader: object already exists")

// Uploader abstracts delivering a local file to a remote store.
//   - src: absolute path of the already-downloaded local file
//   - key: remote object key (without prefix; the implementation appends its own prefix)
//   - overwrite: when false and the key exists, return ErrExists so the caller can skip
type Uploader interface {
	Upload(ctx context.Context, src, key string, overwrite bool) error
}

// S3Config configures an S3-compatible store (AWS S3 / Tencent COS / MinIO / ...).
type S3Config struct {
	Endpoint     string `yaml:"endpoint"`
	Region       string `yaml:"region"`
	Bucket       string `yaml:"bucket"`
	Prefix       string `yaml:"prefix"`
	AccessKey    string `yaml:"access_key"`
	SecretKey    string `yaml:"secret_key"`
	UsePathStyle bool   `yaml:"use_path_style"`
	HealthCheck  bool   `yaml:"health_check"` // optional: ping bucket on startup
}

// LocalConfig configures the local mirror uploader (used when type=local).
type LocalConfig struct {
	MirrorDir string `yaml:"mirror_dir"`
}

// BackendConfig selects the upload backend.
type BackendConfig struct {
	Type  string
	S3    S3Config
	Local LocalConfig
}

// NewUploader dispatches a concrete implementation by Type.
func NewUploader(cfg BackendConfig) (Uploader, error) {
	switch cfg.Type {
	case "", "none":
		return &NoopUploader{}, nil
	case "local":
		return NewLocalUploader(cfg.Local)
	case "s3":
		return NewS3Uploader(cfg.S3)
	default:
		return nil, errors.New("uploader: unknown upload type " + cfg.Type)
	}
}
