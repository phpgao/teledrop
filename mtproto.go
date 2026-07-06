package main

import (
	"context"
	"fmt"
	"log"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/gotd/td/session"
	"github.com/gotd/td/telegram"
	"github.com/gotd/td/telegram/dcs"
	"github.com/gotd/td/telegram/downloader"
	"github.com/gotd/td/tg"
	"golang.org/x/net/proxy"
)

// MTProtoClient logs into Telegram MTProto using the bot token — same identity as Bot API.
// Message IDs are identical, no peer mapping needed.
type MTProtoClient struct {
	api *tg.Client
	dl  *downloader.Downloader
	mu  sync.Mutex
}

type MTProtoConfig struct {
	AppID   int    `yaml:"app_id"`
	AppHash string `yaml:"app_hash"`
	Socks5  string `yaml:"socks5"`
}

func NewMTProtoClient(ctx context.Context, cfg MTProtoConfig, botToken, sessionDir string) *MTProtoClient {
	if cfg.AppID == 0 || cfg.AppHash == "" || botToken == "" {
		return nil
	}

	m := &MTProtoClient{}
	if err := os.MkdirAll(sessionDir, 0o700); err != nil {
		log.Printf("warn: mtproto session dir: %v", err)
		return nil
	}
	opts := buildOpts(cfg, filepath.Join(sessionDir, "session.json"))
	client := telegram.NewClient(cfg.AppID, cfg.AppHash, opts)

	go func() {
		err := client.Run(ctx, func(runCtx context.Context) error {
			// Bot token auth — no phone, no code, no 2FA.
			if _, err := client.Auth().Bot(runCtx, botToken); err != nil {
				return fmt.Errorf("mtproto: bot auth: %w", err)
			}

			log.Println("mtproto: bot client ready")
			m.mu.Lock()
			m.api = client.API()
			m.dl = downloader.NewDownloader()
			m.mu.Unlock()

			<-runCtx.Done()
			log.Println("mtproto: bot client stopped")
			return nil
		})
		if err != nil {
			log.Printf("mtproto: client error: %v", err)
		}
	}()
	return m
}

func (m *MTProtoClient) IsReady() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.api != nil
}

func (m *MTProtoClient) DownloadFile(ctx context.Context, botChatID int64, messageID int64, dir, fallbackName string) (string, int64, error) {
	m.mu.Lock()
	api := m.api
	dl := m.dl
	m.mu.Unlock()

	if api == nil || dl == nil {
		return "", 0, fmt.Errorf("mtproto: client not ready")
	}

	// Try MessagesGetMessages first (direct ID lookup).
	msgs, err := api.MessagesGetMessages(ctx, []tg.InputMessageClass{
		&tg.InputMessageID{ID: int(messageID)},
	})
	if err == nil {
		if msg := findFirstRealMessage(msgs); msg != nil {
			return m.downloadFromMessage(ctx, api, dl, msg, dir, fallbackName)
		}
	}

	// Fallback: MessagesGetHistory with peer.
	peer := peerFromBotChatID(botChatID)
	if peer == nil {
		return "", 0, fmt.Errorf("mtproto: cannot build peer for chat %d", botChatID)
	}

	resp, err := api.MessagesGetHistory(ctx, &tg.MessagesGetHistoryRequest{
		Peer:     peer,
		OffsetID: int(messageID) + 1,
		Limit:    1,
	})
	if err != nil {
		return "", 0, fmt.Errorf("mtproto: get history: %w", err)
	}
	msg, err := firstMessage(resp)
	if err != nil {
		return "", 0, fmt.Errorf("mtproto: message %d not found: %w", messageID, err)
	}
	return m.downloadFromMessage(ctx, api, dl, msg, dir, fallbackName)
}

func (m *MTProtoClient) downloadFromMessage(ctx context.Context, api *tg.Client, dl *downloader.Downloader, msg tg.MessageClass, dir, fallbackName string) (string, int64, error) {
	loc, size, realName, err := extractFileInfo(msg)
	if err != nil {
		return "", 0, fmt.Errorf("mtproto: extract file: %w", err)
	}

	name := realName
	if name == "" {
		name = fallbackName
	}

	localPath := filepath.Join(dir, filepath.Base(name))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", 0, fmt.Errorf("mtproto: create dir: %w", err)
	}
	dst := avoidCollision(localPath)

	out, err := os.Create(dst)
	if err != nil {
		return "", 0, fmt.Errorf("mtproto: create output: %w", err)
	}
	defer func() { _ = out.Close() }()

	_, err = dl.Download(api, loc).Stream(ctx, out)
	if err != nil {
		_ = os.Remove(dst)
		return "", 0, fmt.Errorf("mtproto: download: %w", err)
	}

	return filepath.Base(dst), size, nil
}

// -- helpers ----------------------------------------------------------------

func peerFromBotChatID(chatID int64) tg.InputPeerClass {
	if chatID > 0 {
		return &tg.InputPeerUser{UserID: chatID}
	}
	absChatID := -chatID
	if absChatID < 1000000000000 {
		return &tg.InputPeerChat{ChatID: absChatID}
	}
	return &tg.InputPeerChannel{ChannelID: absChatID - 1000000000000}
}

func findFirstRealMessage(resp tg.MessagesMessagesClass) tg.MessageClass {
	switch r := resp.(type) {
	case *tg.MessagesMessages:
		if len(r.Messages) > 0 {
			if m, ok := r.Messages[0].(*tg.Message); ok {
				return m
			}
		}
	case *tg.MessagesChannelMessages:
		if len(r.Messages) > 0 {
			if m, ok := r.Messages[0].(*tg.Message); ok {
				return m
			}
		}
	}
	return nil
}

func firstMessage(resp tg.MessagesMessagesClass) (tg.MessageClass, error) {
	switch r := resp.(type) {
	case *tg.MessagesChannelMessages:
		if len(r.Messages) == 0 {
			return nil, fmt.Errorf("not found")
		}
		return r.Messages[0].(*tg.Message), nil
	case *tg.MessagesMessages, *tg.MessagesMessagesSlice:
		var msgs []tg.MessageClass
		switch v := r.(type) {
		case *tg.MessagesMessages:
			msgs = v.Messages
		case *tg.MessagesMessagesSlice:
			msgs = v.Messages
		}
		if len(msgs) == 0 {
			return nil, fmt.Errorf("not found")
		}
		return msgs[0].(*tg.Message), nil
	default:
		return nil, fmt.Errorf("unexpected type: %T", r)
	}
}

func extractFileInfo(msg tg.MessageClass) (tg.InputFileLocationClass, int64, string, error) {
	m, ok := msg.(*tg.Message)
	if !ok || m.Media == nil {
		return nil, 0, "", fmt.Errorf("no media")
	}
	switch media := m.Media.(type) {
	case *tg.MessageMediaDocument:
		doc, ok := media.Document.(*tg.Document)
		if !ok {
			return nil, 0, "", fmt.Errorf("unsupported document")
		}
		name := ""
		for _, attr := range doc.Attributes {
			if fn, ok := attr.(*tg.DocumentAttributeFilename); ok {
				name = fn.FileName
			}
		}
		return &tg.InputDocumentFileLocation{
			ID:            doc.ID,
			AccessHash:    doc.AccessHash,
			FileReference: doc.FileReference,
		}, doc.Size, name, nil
	case *tg.MessageMediaPhoto:
		photo, ok := media.Photo.(*tg.Photo)
		if !ok {
			return nil, 0, "", fmt.Errorf("unsupported photo")
		}
		var bestPixels int32
		var bestSize int
		var bestType string
		for _, s := range photo.Sizes {
			ps, ok := s.(*tg.PhotoSize)
			if !ok {
				continue
			}
			if p := int32(ps.W) * int32(ps.H); p > bestPixels {
				bestPixels, bestSize, bestType = p, ps.Size, ps.Type
			}
		}
		return &tg.InputPhotoFileLocation{
			ID:            photo.ID,
			AccessHash:    photo.AccessHash,
			FileReference: photo.FileReference,
			ThumbSize:     bestType,
		}, int64(bestSize), "", nil
	default:
		return nil, 0, "", fmt.Errorf("unsupported media")
	}
}

func buildOpts(cfg MTProtoConfig, sessionPath string) telegram.Options {
	opts := telegram.Options{
		SessionStorage: &session.FileStorage{Path: sessionPath},
	}
	if cfg.Socks5 != "" {
		dialer, err := proxy.SOCKS5("tcp", cfg.Socks5, nil, proxy.Direct)
		if err != nil {
			log.Printf("warn: mtproto socks5: %v", err)
		} else {
			opts.Resolver = dcs.Plain(dcs.PlainOptions{
				Dial: func(ctx context.Context, network, addr string) (net.Conn, error) {
					return dialer.Dial(network, addr)
				},
			})
			log.Printf("mtproto: using SOCKS5 proxy %s", cfg.Socks5)
		}
	}
	return opts
}

func needFallback(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(err.Error(), "file is too big")
}
