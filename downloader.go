package main

import (
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

// 20 MB threshold: files larger than this use heavyClient with longer timeout.
const largeFileThreshold = 20 * 1024 * 1024

// FileItem is one file to download.
type FileItem struct {
	FileID    string
	UniqueID  string
	MimeType  string
	Kind      string // document/photo/video/audio/voice/videonote/animation/sticker
	Name      string // final on-disk filename (with extension)
	Size      int64  // file size in bytes (0 if unknown)
	ChatID    int64  // source chat (for MTProto fallback)
	MessageID int64  // source message (for MTProto fallback)
}

// Downloader pulls files to the local filesystem.
type Downloader struct {
	api         *tgbotapi.BotAPI
	baseDir     string
	client      *http.Client   // normal files (<= 20 MB)
	heavyClient *http.Client   // large files (> 20 MB), 30 min timeout
	mt          *MTProtoClient // optional MTProto fallback for >20 MB Bot API limit
}

// NewDownloader constructs a Downloader; baseDir falls back to ./downloads when empty.
// mt may be nil; when set, >20 MB files are routed through MTProto instead of Bot API.
func NewDownloader(api *tgbotapi.BotAPI, baseDir string, mt *MTProtoClient) (*Downloader, error) {
	if baseDir == "" {
		baseDir = "./downloads"
	}
	if err := os.MkdirAll(baseDir, 0o755); err != nil {
		return nil, fmt.Errorf("downloader: create download dir failed: %w", err)
	}
	return &Downloader{
		api:         api,
		baseDir:     baseDir,
		client:      &http.Client{Timeout: 10 * time.Minute},
		heavyClient: &http.Client{Timeout: 30 * time.Minute},
		mt:          mt,
	}, nil
}

// Extract pulls every file out of a message, deduplicated by FileUniqueID.
func Extract(msg tgbotapi.Message) []FileItem {
	seen := map[string]bool{}
	var items []FileItem

	add := func(fi FileItem) {
		if fi.FileID == "" {
			return
		}
		key := fi.UniqueID
		if key == "" {
			key = fi.FileID
		}
		if seen[key] {
			return
		}
		seen[key] = true
		if fi.Name == "" {
			fi.Name = buildName(fi)
		}
		items = append(items, fi)
	}

	if d := msg.Document; d != nil {
		add(FileItem{FileID: d.FileID, UniqueID: d.FileUniqueID, MimeType: d.MimeType, Kind: "document", Name: sanitizeName(d.FileName), Size: int64(d.FileSize)})
	}
	if len(msg.Photo) > 0 {
		p := largestPhoto(msg.Photo)
		add(FileItem{FileID: p.FileID, UniqueID: p.FileUniqueID, MimeType: "image/jpeg", Kind: "photo", Size: int64(p.FileSize)})
	}
	if v := msg.Video; v != nil {
		add(FileItem{FileID: v.FileID, UniqueID: v.FileUniqueID, MimeType: v.MimeType, Kind: "video", Name: sanitizeName(v.FileName), Size: int64(v.FileSize)})
	}
	if a := msg.Audio; a != nil {
		add(FileItem{FileID: a.FileID, UniqueID: a.FileUniqueID, MimeType: a.MimeType, Kind: "audio", Name: sanitizeName(a.FileName), Size: int64(a.FileSize)})
	}
	if vn := msg.VideoNote; vn != nil {
		add(FileItem{FileID: vn.FileID, UniqueID: vn.FileUniqueID, Kind: "videonote", Size: int64(vn.FileSize)})
	}
	if vc := msg.Voice; vc != nil {
		add(FileItem{FileID: vc.FileID, UniqueID: vc.FileUniqueID, MimeType: vc.MimeType, Kind: "voice", Size: int64(vc.FileSize)})
	}
	if an := msg.Animation; an != nil {
		add(FileItem{FileID: an.FileID, UniqueID: an.FileUniqueID, MimeType: an.MimeType, Kind: "animation", Name: sanitizeName(an.FileName), Size: int64(an.FileSize)})
	}
	if st := msg.Sticker; st != nil {
		mime := "image/webp"
		if st.IsAnimated {
			mime = "application/x-tgsticker"
		}
		add(FileItem{FileID: st.FileID, UniqueID: st.FileUniqueID, MimeType: mime, Kind: "sticker", Size: int64(st.FileSize)})
	}

	// Record source chat and message on every item (needed for MTProto fallback).
	for i := range items {
		items[i].ChatID = msg.Chat.ID
		items[i].MessageID = int64(msg.MessageID)
	}
	return items
}

// largestPhoto picks the highest-resolution photo (Telegram usually puts the
// original last, but an explicit comparison is more robust).
func largestPhoto(photos []tgbotapi.PhotoSize) tgbotapi.PhotoSize {
	best := photos[0]
	bestArea := best.Width * best.Height
	for _, p := range photos[1:] {
		if area := p.Width * p.Height; area > bestArea {
			best, bestArea = p, area
		}
	}
	return best
}

// Save downloads the file to baseDir/dir/Name and returns the local absolute path.
// When the file exceeds the Bot API 20 MB limit ("file is too big") and an MTProto
// client is configured, falls back to MTProto user-client download.
// An existing target gets a numeric suffix to avoid silent overwrite.
func (d *Downloader) Save(ctx context.Context, item FileItem, dir string) (string, error) {
	url, err := d.api.GetFileDirectURL(item.FileID)
	if err != nil {
		// Bot API refuses large files -> try MTProto fallback.
		if d.mt != nil && needFallback(err) {
			return d.mtSave(ctx, item, dir)
		}
		return "", fmt.Errorf("downloader: get direct url failed: %w", err)
	}

	localDir := filepath.Join(d.baseDir, filepath.FromSlash(dir))
	if err := os.MkdirAll(localDir, 0o755); err != nil {
		return "", fmt.Errorf("downloader: create dir failed: %w", err)
	}

	dst := avoidCollision(filepath.Join(localDir, item.Name))
	out, err := os.Create(dst)
	if err != nil {
		return "", fmt.Errorf("downloader: create file failed: %w", err)
	}
	defer func() { _ = out.Close() }()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", fmt.Errorf("downloader: build request failed: %w", err)
	}

	c := d.client
	if item.Size > largeFileThreshold {
		c = d.heavyClient
	}
	resp, err := c.Do(req)
	if err != nil {
		return "", fmt.Errorf("downloader: download failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("downloader: download failed http %d", resp.StatusCode)
	}
	if _, err := io.Copy(out, resp.Body); err != nil {
		return "", fmt.Errorf("downloader: write file failed: %w", err)
	}
	return dst, nil
}

// mtSave downloads a file via MTProto client (user account) and writes it to disk.
func (d *Downloader) mtSave(ctx context.Context, item FileItem, dir string) (string, error) {
	if !d.mt.IsReady() {
		log.Println("downloader: waiting for mtproto client to be ready...")
		waitCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		for !d.mt.IsReady() {
			select {
			case <-waitCtx.Done():
				return "", fmt.Errorf("downloader: mtproto client not ready after 30s")
			case <-time.After(time.Second):
			}
		}
	}
	localDir := filepath.Join(d.baseDir, filepath.FromSlash(dir))
	log.Printf("downloader: mtproto downloading chat=%d msg=%d dir=%s name=%s", item.ChatID, item.MessageID, dir, item.Name)
	name, _, err := d.mt.DownloadFile(ctx, item.ChatID, item.MessageID, localDir, item.Name)
	if err != nil {
		return "", fmt.Errorf("downloader: mtproto download: %w", err)
	}
	dst := filepath.Join(localDir, name)
	log.Printf("downloader: mtproto downloaded %s (%s)", name, formatSize(item.Size))
	return dst, nil
}

// SaveText writes content as a UTF-8 text file at baseDir/dir/name and returns the local path.
// An existing target gets a numeric suffix to avoid silent overwrite.
func (d *Downloader) SaveText(dir, name, content string) (string, error) {
	localDir := filepath.Join(d.baseDir, filepath.FromSlash(dir))
	if err := os.MkdirAll(localDir, 0o755); err != nil {
		return "", fmt.Errorf("downloader: create dir failed: %w", err)
	}
	dst := avoidCollision(filepath.Join(localDir, name))
	if err := os.WriteFile(dst, []byte(content), 0o644); err != nil {
		return "", fmt.Errorf("downloader: write text failed: %w", err)
	}
	return dst, nil
}

// buildName uses uniqueID + extension when no original name is present, guaranteeing uniqueness.
func buildName(fi FileItem) string {
	base := fi.UniqueID
	if base == "" {
		base = fi.FileID
	}
	return base + mimeToExt(fi)
}

// avoidCollision inserts a _N suffix if the target already exists, avoiding silent overwrite.
func avoidCollision(dst string) string {
	if _, err := os.Stat(dst); os.IsNotExist(err) {
		return dst
	}
	ext := filepath.Ext(dst)
	stem := strings.TrimSuffix(dst, ext)
	for i := 1; ; i++ {
		cand := fmt.Sprintf("%s_%d%s", stem, i, ext)
		if _, err := os.Stat(cand); os.IsNotExist(err) {
			return cand
		}
	}
}

// sanitizeName cleans a user-supplied filename, stripping path components to avoid traversal.
// Empty input falls back to buildName.
func sanitizeName(name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return ""
	}
	return filepath.Base(name)
}

// mimeToExt infers an extension from MIME type or file kind.
func mimeToExt(fi FileItem) string {
	switch fi.MimeType {
	case "image/jpeg":
		return ".jpg"
	case "image/png":
		return ".png"
	case "image/gif":
		return ".gif"
	case "image/webp":
		return ".webp"
	case "video/mp4":
		return ".mp4"
	case "video/webm":
		return ".webm"
	case "audio/mpeg":
		return ".mp3"
	case "audio/ogg":
		return ".ogg"
	case "application/x-tgsticker":
		return ".tgs"
	case "application/zip":
		return ".zip"
	case "application/pdf":
		return ".pdf"
	}
	switch fi.Kind {
	case "photo":
		return ".jpg"
	case "video", "videonote", "animation":
		return ".mp4"
	case "voice", "audio":
		return ".ogg"
	case "sticker":
		return ".webp"
	case "document":
		return ".bin"
	}
	return ".bin"
}
