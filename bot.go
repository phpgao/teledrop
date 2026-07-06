package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"path"
	"path/filepath"
	"strings"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

const maxConcurrent = 4

// Bot wires the modules together to process messages.
type Bot struct {
	api           *tgbotapi.BotAPI
	org           *Organizer
	dl            *Downloader
	up            Uploader
	state         *State
	uploadEnabled bool
	overwrite     bool
	allowed       map[int64]struct{}
	sem           chan struct{}
}

// NewBot assembles a Bot.
func NewBot(api *tgbotapi.BotAPI, dl *Downloader, org *Organizer, up Uploader, st *State, uploadEnabled, overwrite bool, allowed []int64) *Bot {
	m := make(map[int64]struct{}, len(allowed))
	for _, id := range allowed {
		m[id] = struct{}{}
	}
	return &Bot{
		api:           api,
		org:           org,
		dl:            dl,
		up:            up,
		state:         st,
		uploadEnabled: uploadEnabled,
		overwrite:     overwrite,
		allowed:       m,
		sem:           make(chan struct{}, maxConcurrent),
	}
}

// Start replays any previously failed downloads, then runs long polling (getUpdates).
func (b *Bot) Start(ctx context.Context) error {
	b.retryFailures(ctx, 0)
	return b.startPoll(ctx)
}

// startPoll pulls updates via long polling.
func (b *Bot) startPoll(ctx context.Context) error {
	u := tgbotapi.NewUpdate(0)
	u.Timeout = 60
	ch := b.api.GetUpdatesChan(u)

	log.Println("teledrop: long polling started")

	for {
		select {
		case <-ctx.Done():
			log.Println("teledrop: received stop signal, halting poll")
			return nil
		case upd := <-ch:
			if upd.Message == nil {
				continue
			}
			log.Printf("teledrop: msg received chat=%d msg=%d", upd.Message.Chat.ID, upd.Message.MessageID)
			b.sem <- struct{}{}
			go func(m tgbotapi.Message) {
				defer func() { <-b.sem }()
				b.handle(ctx, m)
			}(*upd.Message)
		}
	}
}

// handle processes one message:
// commands -> auth -> dedup -> notify-start -> download files + record -> upload -> notify-done.
func (b *Bot) handle(ctx context.Context, m tgbotapi.Message) {
	if m.Command() == "start" {
		b.reply(m.Chat.ID, "teledrop is ready: send me a file (and optional caption) and I will download and organize it. Send /retry to replay failed downloads.")
		return
	}
	if m.Command() == "retry" {
		b.retryFailures(ctx, m.Chat.ID)
		return
	}

	if !b.authorized(m) {
		log.Printf("handle: unauthorized chat=%d", m.Chat.ID)
		b.reply(m.Chat.ID, "⛔ You are not allowed to use this bot.")
		return
	}

	if b.state.IsProcessed(m.Chat.ID, int64(m.MessageID)) {
		log.Printf("skip already-processed message chat=%d msg=%d", m.Chat.ID, m.MessageID)
		return
	}

	dir := b.org.Dir(m)
	items := Extract(m)
	log.Printf("handle: chat=%d msg=%d dir=%s items=%d", m.Chat.ID, m.MessageID, dir, len(items))
	fromWho := chatDisplayName(m)

	// -- Plain-text message (no file): save as .txt ---------------------------------
	if len(items) == 0 {
		if strings.TrimSpace(m.Text) == "" {
			return
		}
		startTime := time.Now()
		name := fmt.Sprintf("text_%d.txt", m.MessageID)
		content := formatText(m, m.Text, "")
		localPath, err := b.saveTextWithRetry(dir, name, content, m, "text")
		dur := time.Since(startTime)
		size := int64(len(content))

		dr := DownloadRecord{
			ChatID: m.Chat.ID, From: fromWho, MessageID: int64(m.MessageID),
			Name: name, Size: size, Kind: "text", Dir: dir,
			LocalPath: localPath, DurationMs: dur.Milliseconds(), Uploaded: false,
		}
		if err != nil {
			dr.Status = "failed"
			dr.Error = err.Error()
		} else {
			dr.Status = "ok"
		}
		b.state.RecordDownload(dr)

		if err != nil {
			b.replyQuote(m, fmt.Sprintf("❌ %s failed: %v", name, err))
			return
		}

		key := path.Join(dir, name)
		b.uploadText(ctx, m, key)
		b.state.MarkProcessed(m.Chat.ID, int64(m.MessageID))
		b.replyQuote(m, fmt.Sprintf("✅ %s (%s, %s)", name, formatSize(size), formatDuration(dur)))
		return
	}

	// -- Start notification (replyQuote the original message) ----------------------
	b.replyQuote(m, formatStartMsg(items))

	// -- Process files -------------------------------------------------------------
	startTime := time.Now()
	var ok, fail int
	var totalSize int64
	var lines []string

	for _, item := range items {
		// Already-seen file (by Telegram FileUniqueID) -> skip download.
		if b.state.Seen(item.UniqueID) {
			log.Printf("skip already-seen file uid=%s name=%s", item.UniqueID, item.Name)
			dr := DownloadRecord{
				ChatID: m.Chat.ID, From: fromWho, MessageID: int64(m.MessageID),
				UniqueID: item.UniqueID, FileID: item.FileID,
				Name: item.Name, Size: item.Size, Kind: item.Kind, Dir: dir,
				Status: "skipped",
			}
			b.state.RecordDownload(dr)
			lines = append(lines, fmt.Sprintf("⏭️ %s (%s) — already downloaded", item.Name, formatSize(item.Size)))
			ok++
			continue
		}

		fileStart := time.Now()
		localPath, err := b.downloadWithRetry(ctx, item, dir)
		fileDur := time.Since(fileStart)
		b.state.MarkSeen(item.UniqueID)

		key := path.Join(dir, item.Name) // remote key mirrors local structure, always with /
		var line string

		dr := DownloadRecord{
			ChatID: m.Chat.ID, From: fromWho, MessageID: int64(m.MessageID),
			UniqueID: item.UniqueID, FileID: item.FileID,
			Name: item.Name, Size: item.Size, Kind: item.Kind, Dir: dir,
			LocalPath: localPath, DurationMs: fileDur.Milliseconds(),
		}

		if err != nil {
			dr.Status = "failed"
			dr.Error = err.Error()
			log.Printf("download failed chat=%d file=%s: %v", m.Chat.ID, item.FileID, err)
			b.state.AddFailure(Failure{FileID: item.FileID, ChatID: m.Chat.ID, MessageID: int64(m.MessageID), Dir: dir, Name: item.Name, Kind: "file"})
			line = fmt.Sprintf("❌ %s (%s) — download failed: %v", item.Name, formatSize(item.Size), err)
			fail++
		} else {
			dr.Status = "ok"
			line = fmt.Sprintf("✅ %s (%s, %s)", item.Name, formatSize(item.Size), formatDuration(fileDur))

			if b.uploadEnabled {
				switch e := b.up.Upload(ctx, localPath, key, b.overwrite); {
				case e == nil:
					line += " ☁️"
					dr.Uploaded = true
				case errors.Is(e, ErrExists):
					line += " ☁️ skipped (exists)"
					dr.Uploaded = true
				default:
					log.Printf("upload failed chat=%d key=%s: %v", m.Chat.ID, key, e)
					line += fmt.Sprintf(" ☁️ upload failed: %v", e)
				}
			}
			totalSize += item.Size
			ok++
		}
		b.state.RecordDownload(dr)
		lines = append(lines, line)
	}

	// -- Save caption ---------------------------------------------------------------
	if caption := strings.TrimSpace(m.Caption); caption != "" {
		if saved := b.saveCaption(ctx, m, items, dir); saved != "" {
			lines = append(lines, fmt.Sprintf("💬 %s", saved))
		}
	}

	// -- Completion notification (replyQuote the original message) ------------------
	totalDur := time.Since(startTime)
	summary := fmt.Sprintf("✅ teledrop — %d/%d success", ok, ok+fail)
	if totalSize > 0 {
		summary += fmt.Sprintf(" (total %s", formatSize(totalSize))
		if totalDur > time.Second {
			summary += fmt.Sprintf(", %s", formatDuration(totalDur))
		}
		summary += ")"
	}
	msg := summary
	if len(lines) > 0 {
		msg += "\n\n" + strings.Join(lines, "\n")
	}
	b.replyQuote(m, msg)

	if fail == 0 {
		b.state.MarkProcessed(m.Chat.ID, int64(m.MessageID))
	}
	log.Printf("done chat=%d ok=%d fail=%d", m.Chat.ID, ok, fail)
}

// -- helpers ------------------------------------------------------------------

func formatStartMsg(items []FileItem) string {
	var b strings.Builder
	fmt.Fprintf(&b, "📥 processing %d files", len(items))
	if len(items) <= 8 {
		b.WriteString(":\n")
		for _, item := range items {
			fmt.Fprintf(&b, "  • %s (%s)\n", item.Name, formatSize(item.Size))
		}
	} else {
		b.WriteString("...")
	}
	return strings.TrimSpace(b.String())
}

func formatSize(n int64) string {
	if n <= 0 {
		return "? B"
	}
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for n >= div && exp < 3 {
		div *= unit
		exp++
	}
	div /= unit
	return fmt.Sprintf("%.1f %s", float64(n)/float64(div), []string{"KB", "MB", "GB"}[exp-1])
}

func formatDuration(d time.Duration) string {
	if d < time.Second {
		return "0s"
	}
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	m := int(d.Minutes())
	s := int(d.Seconds()) % 60
	if s == 0 {
		return fmt.Sprintf("%dm", m)
	}
	return fmt.Sprintf("%dm%ds", m, s)
}

// -- download ----------------------------------------------------------------

// downloadWithRetry downloads a file with bounded exponential backoff.
func (b *Bot) downloadWithRetry(ctx context.Context, item FileItem, dir string) (string, error) {
	var lastErr error
	for i := 0; i < maxRetries; i++ {
		if i > 0 {
			time.Sleep(backoff(i))
		}
		localPath, err := b.dl.Save(ctx, item, dir)
		if err == nil {
			return localPath, nil
		}
		lastErr = err
		log.Printf("download attempt %d/%d failed file=%s: %v", i+1, maxRetries, item.FileID, err)
	}
	return "", lastErr
}

// saveTextWithRetry writes a text file with retries, recording a Failure on persistent error.
func (b *Bot) saveTextWithRetry(dir, name, content string, m tgbotapi.Message, kind string) (string, error) {
	var lastErr error
	for i := 0; i < maxRetries; i++ {
		if i > 0 {
			time.Sleep(backoff(i))
		}
		if localPath, err := b.dl.SaveText(dir, name, content); err == nil {
			return localPath, nil
		} else {
			lastErr = err
		}
	}
	b.state.AddFailure(Failure{ChatID: m.Chat.ID, MessageID: int64(m.MessageID), Dir: dir, Name: name, Caption: content, Kind: kind})
	return "", lastErr
}

// backoff returns an exponential delay before the i-th retry (i is 1-based).
func backoff(attempt int) time.Duration {
	return time.Duration(1<<uint(attempt-1)) * 200 * time.Millisecond
}

// -- retry -------------------------------------------------------------------

// retryFailures replays the failed-download queue via the stored file_id (updates are one-shot,
// but file_id stays valid for the bot). When notifyChat is 0 it runs silently, e.g. at startup.
func (b *Bot) retryFailures(ctx context.Context, notifyChat int64) {
	fs := b.state.Failures()
	if len(fs) == 0 {
		if notifyChat != 0 {
			b.reply(notifyChat, "✅ no failed downloads to retry")
		}
		return
	}
	log.Printf("teledrop: retrying %d failed downloads", len(fs))
	var remaining []Failure
	var ok, fail int
	for _, f := range fs {
		log.Printf("retry: attempting chat=%d name=%s", f.ChatID, f.Name)
		var err error
		switch f.Kind {
		case "text", "caption":
			_, err = b.dl.SaveText(f.Dir, f.Name, f.Caption)
		default: // file
			_, err = b.dl.Save(ctx, FileItem{FileID: f.FileID, Name: f.Name, ChatID: f.ChatID, MessageID: f.MessageID}, f.Dir)
		}
		if err != nil {
			log.Printf("retry failed chat=%d name=%s: %v", f.ChatID, f.Name, err)
			remaining = append(remaining, f)
			fail++
			continue
		}
		// Re-upload the recovered file when upload is enabled.
		if b.uploadEnabled {
			key := path.Join(f.Dir, f.Name)
			src := filepath.Join(b.dl.baseDir, filepath.FromSlash(key))
			switch e := b.up.Upload(ctx, src, key, b.overwrite); {
			case e == nil:
			case errors.Is(e, ErrExists):
			default:
				log.Printf("retry upload failed chat=%d key=%s: %v", f.ChatID, key, e)
			}
		}
		ok++
	}
	b.state.ReplaceFailures(remaining)
	if notifyChat != 0 {
		b.reply(notifyChat, fmt.Sprintf("✅ retried %d, ok=%d fail=%d", len(fs), ok, fail))
	}
	log.Printf("teledrop: retry done total=%d ok=%d fail=%d", len(fs), ok, fail)
}

// -- caption -----------------------------------------------------------------

// saveCaption persists the message caption as a sidecar .txt next to the file(s).
// Returns the saved key, or "" on failure.
func (b *Bot) saveCaption(ctx context.Context, m tgbotapi.Message, items []FileItem, dir string) string {
	var name, ref string
	switch len(items) {
	case 1:
		stem := strings.TrimSuffix(items[0].Name, filepath.Ext(items[0].Name))
		name = stem + ".txt"
		ref = items[0].Name
	default:
		name = "caption.txt"
		ref = fmt.Sprintf("%d files", len(items))
	}
	if _, err := b.dl.SaveText(dir, name, formatText(m, m.Caption, ref)); err != nil {
		log.Printf("save caption failed chat=%d: %v", m.Chat.ID, err)
		return ""
	}
	key := path.Join(dir, name)
	b.uploadText(ctx, m, key)
	return key
}

// uploadText uploads a previously-saved text file when upload is enabled.
func (b *Bot) uploadText(ctx context.Context, m tgbotapi.Message, key string) {
	if !b.uploadEnabled {
		return
	}
	src := filepath.Join(b.dl.baseDir, filepath.FromSlash(key))
	switch err := b.up.Upload(ctx, src, key, b.overwrite); {
	case err == nil:
	case errors.Is(err, ErrExists):
	default:
		log.Printf("upload text failed chat=%d key=%s: %v", m.Chat.ID, key, err)
	}
}

// -- auth / reply ------------------------------------------------------------

// authorized allows everyone when the whitelist is empty.
func (b *Bot) authorized(m tgbotapi.Message) bool {
	if len(b.allowed) == 0 {
		return true
	}
	id := m.Chat.ID
	if m.From != nil {
		id = m.From.ID
	}
	_, ok := b.allowed[id]
	return ok
}

// reply sends a text reply; a failure is only logged, not fatal.
func (b *Bot) reply(chatID int64, text string) {
	msg := tgbotapi.NewMessage(chatID, text)
	if _, err := b.api.Send(msg); err != nil {
		log.Printf("reply failed chat=%d: %v", chatID, err)
	}
}

// replyQuote sends a text reply referencing the original message.
func (b *Bot) replyQuote(m tgbotapi.Message, text string) {
	msg := tgbotapi.NewMessage(m.Chat.ID, text)
	msg.ReplyToMessageID = m.MessageID
	if _, err := b.api.Send(msg); err != nil {
		log.Printf("reply failed chat=%d: %v", m.Chat.ID, err)
	}
}

// -- text formatting ---------------------------------------------------------

// formatText wraps raw content with a metadata header.
// ref is the associated file name for captions, or "" for plain text messages.
func formatText(m tgbotapi.Message, body, ref string) string {
	var b strings.Builder
	b.WriteString("From: ")
	b.WriteString(chatDisplayName(m))
	b.WriteByte('\n')
	b.WriteString("Date: ")
	b.WriteString(m.Time().Format("2006-01-02 15:04:05"))
	b.WriteByte('\n')
	fmt.Fprintf(&b, "MsgID: %d\n", m.MessageID)
	if ref != "" {
		fmt.Fprintf(&b, "File: %s\n", ref)
	}
	b.WriteString("---\n")
	b.WriteString(body)
	return b.String()
}

// chatDisplayName builds a human-readable label for the chat.
func chatDisplayName(m tgbotapi.Message) string {
	c := m.Chat
	switch {
	case c.UserName != "":
		return "@" + c.UserName
	case c.Title != "":
		return c.Title
	case c.FirstName != "":
		return c.FirstName
	case c.LastName != "":
		return c.LastName
	default:
		return fmt.Sprintf("chat_%d", c.ID)
	}
}
