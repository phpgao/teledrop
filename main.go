// teledrop downloads files sent to a Telegram bot, organizes them locally,
// and optionally uploads them to an S3-compatible object store (COS/MinIO/...).
// Captions and plain-text messages are saved as .txt files alongside.
package main

import (
	"context"
	"flag"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

// version is injected at build time via -ldflags="-X main.version=...".
var version = "dev"

func main() {
	configPath := flag.String("config", "config.yaml", "path to config file")
	flag.Parse()

	cfg, err := Load(*configPath)
	if err != nil {
		log.Fatalf("load config failed: %v", err)
	}

	api, err := tgbotapi.NewBotAPI(cfg.Telegram.Token)
	if err != nil {
		log.Fatalf("init bot failed: %v", err)
	}
	me, err := api.GetMe()
	if err != nil {
		log.Fatalf("get bot info failed: %v", err)
	}
	log.Printf("teledrop started, bot=@%s (id=%d) version=%s", me.UserName, me.ID, version)

	// Optional MTProto client using bot token for large file download.
	sessionDir := filepath.Join(cfg.Download.BaseDir, ".mtproto")
	mtClient := NewMTProtoClient(context.Background(), cfg.Telegram.MTProto, cfg.Telegram.Token, sessionDir)

	dl, err := NewDownloader(api, cfg.Download.BaseDir, mtClient)
	if err != nil {
		log.Fatalf("init downloader failed: %v", err)
	}
	org := NewOrganizer(cfg.Download.Organize, cfg.Download.SeparateForwards)

	uploadType := cfg.Upload.Type
	uploadEnabled := cfg.Upload.Enabled
	if !uploadEnabled {
		uploadType = "none"
	}
	up, err := NewUploader(BackendConfig{
		Type:  uploadType,
		S3:    cfg.Upload.S3,
		Local: cfg.Upload.Local,
	})
	if err != nil {
		log.Printf("warn: uploader init failed, falling back to noop: %v", err)
		up = &NoopUploader{}
		uploadEnabled = false
	}

	st, err := NewState(cfg.Download.BaseDir)
	if err != nil {
		log.Fatalf("init state failed: %v", err)
	}
	defer func() { _ = st.Close() }()

	b := NewBot(api, dl, org, up, st, uploadEnabled, cfg.Upload.Overwrite, cfg.Telegram.AllowedUsers)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if err = b.Start(ctx); err != nil {
		log.Fatalf("bot runtime error: %v", err)
	}
	os.Exit(0)
}
