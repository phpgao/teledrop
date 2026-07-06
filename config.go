package main

import (
	"fmt"
	"os"
	"regexp"

	"gopkg.in/yaml.v3"
)

// Config is the top-level configuration.
type Config struct {
	Telegram TelegramConfig `yaml:"telegram"`
	Download DownloadConfig `yaml:"download"`
	Upload   UploadConfig   `yaml:"upload"`
}

// TelegramConfig holds bot-related settings.
type TelegramConfig struct {
	Token        string        `yaml:"token"`
	AllowedUsers []int64       `yaml:"allowed_users"` // non-empty = whitelist; empty = allow everyone
	MTProto      MTProtoConfig `yaml:"mtproto"`       // optional: MTProto client for >20MB files
}

// DownloadConfig holds local organization settings.
type DownloadConfig struct {
	BaseDir          string `yaml:"base_dir"`
	Organize         string `yaml:"organize"` // flat|by_date|by_type|by_chat|by_chat_date
	SeparateForwards bool   `yaml:"separate_forwards"`
}

// UploadConfig holds upload backend settings.
type UploadConfig struct {
	Enabled   bool   `yaml:"enabled"`
	Overwrite bool   `yaml:"overwrite"`
	Type      string `yaml:"type"` // s3|local|none
	S3        S3Config
	Local     LocalConfig
}

var envRe = regexp.MustCompile(`\$\{(\w+)\}`)

// expandEnv replaces ${VAR} with the env value (empty when unset).
func expandEnv(in []byte) []byte {
	return envRe.ReplaceAllFunc(in, func(m []byte) []byte {
		name := envRe.FindSubmatch(m)[1]
		return []byte(os.Getenv(string(name)))
	})
}

// Load reads YAML from path, expands env vars, and parses it.
func Load(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("config: read failed: %w", err)
	}
	raw = expandEnv(raw)

	var cfg Config
	if err := yaml.Unmarshal(raw, &cfg); err != nil {
		return nil, fmt.Errorf("config: parse failed: %w", err)
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

// Validate performs basic fail-fast checks.
func (c *Config) Validate() error {
	if c.Telegram.Token == "" {
		return fmt.Errorf("config: telegram.token must not be empty")
	}
	if c.Download.BaseDir == "" {
		c.Download.BaseDir = "./downloads"
	}
	switch c.Download.Organize {
	case "", "flat", "by_date", "by_type", "by_chat", "by_chat_date":
	default:
		return fmt.Errorf("config: unknown organize rule %q", c.Download.Organize)
	}
	if c.Upload.Enabled {
		switch c.Upload.Type {
		case "s3", "local":
		case "none", "":
			return fmt.Errorf("config: upload.enabled=true but type=none")
		default:
			return fmt.Errorf("config: unknown upload.type %q", c.Upload.Type)
		}
	}
	return nil
}
