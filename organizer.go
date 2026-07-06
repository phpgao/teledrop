package main

import (
	"fmt"
	"path"
	"strconv"
	"strings"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

// Rule is an organization rule.
type Rule string

const (
	RuleFlat       Rule = "flat"
	RuleByDate     Rule = "by_date"
	RuleByType     Rule = "by_type"
	RuleByChat     Rule = "by_chat"
	RuleByChatDate Rule = "by_chat_date"
)

// Organizer computes where a file lands.
type Organizer struct {
	rule             Rule
	separateForwards bool
}

// NewOrganizer builds an Organizer; an invalid rule falls back to by_chat_date.
func NewOrganizer(rule string, separateForwards bool) *Organizer {
	r := Rule(rule)
	switch r {
	case RuleFlat, RuleByDate, RuleByType, RuleByChat, RuleByChatDate:
	default:
		r = RuleByChatDate
	}
	return &Organizer{rule: r, separateForwards: separateForwards}
}

// Dir returns the directory relative to base_dir (without the filename).
// Parts are filesystem-safe; the same dimensions always yield the same prefix,
// so listing order stays stable.
func (o *Organizer) Dir(msg tgbotapi.Message) string {
	var parts []string
	forwarded := isForwarded(msg)

	if o.separateForwards && forwarded {
		parts = append(parts, "forwarded")
	}

	label := chatLabel(msg)
	t := msg.Time()
	if forwarded {
		label = forwardChatLabel(msg)
		t = time.Now()
	}

	switch o.rule {
	case RuleFlat:
		// no extra directory
	case RuleByDate:
		parts = append(parts, dateParts(t)...)
	case RuleByType:
		parts = append(parts, byTypeDir(msg))
	case RuleByChat:
		parts = append(parts, label)
	case RuleByChatDate:
		parts = append(parts, label)
		parts = append(parts, dateParts(t)...)
	}

	clean := make([]string, 0, len(parts))
	for _, p := range parts {
		if p != "" {
			clean = append(clean, p)
		}
	}
	return path.Join(clean...)
}

// isForwarded reports whether the message was forwarded (to separate from direct sends).
func isForwarded(msg tgbotapi.Message) bool {
	return msg.ForwardFrom != nil || msg.ForwardFromChat != nil || msg.ForwardSenderName != ""
}

// chatLabel builds a safe chat directory name: prefer username/title, else chat id.
func chatLabel(msg tgbotapi.Message) string {
	c := msg.Chat
	switch {
	case c.UserName != "":
		return sanitize(c.UserName)
	case c.Title != "":
		return sanitize(c.Title)
	default:
		return "chat_" + strconv.FormatInt(c.ID, 10)
	}
}

// forwardChatLabel builds a safe directory name from the forward source,
// falling back to the receiving chat when no source is available.
func forwardChatLabel(msg tgbotapi.Message) string {
	if msg.ForwardFromChat != nil {
		c := msg.ForwardFromChat
		switch {
		case c.UserName != "":
			return sanitize(c.UserName)
		case c.Title != "":
			return sanitize(c.Title)
		}
	}
	if msg.ForwardFrom != nil {
		u := msg.ForwardFrom
		switch {
		case u.UserName != "":
			return sanitize(u.UserName)
		case u.FirstName != "":
			return sanitize(u.FirstName)
		}
	}
	if msg.ForwardSenderName != "" {
		return sanitize(msg.ForwardSenderName)
	}
	return chatLabel(msg)
}

// dateParts returns year/month/day segments.
func dateParts(t time.Time) []string {
	return []string{
		strconv.Itoa(t.Year()),
		fmt.Sprintf("%02d", int(t.Month())),
		fmt.Sprintf("%02d", t.Day()),
	}
}

// byTypeDir groups files by broad type.
func byTypeDir(msg tgbotapi.Message) string {
	switch {
	case msg.Photo != nil:
		return "images"
	case msg.Video != nil || msg.VideoNote != nil:
		return "videos"
	case msg.Voice != nil || msg.Audio != nil:
		return "audio"
	case msg.Sticker != nil:
		return "stickers"
	case msg.Animation != nil:
		return "animations"
	case msg.Document != nil:
		return "documents"
	default:
		return "files"
	}
}

// sanitize strips filesystem/object-store unsafe characters and collapses whitespace.
func sanitize(s string) string {
	s = strings.TrimSpace(s)
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
			b.WriteRune(r)
		case r == ' ' || r == '/' || r == '\\':
			b.WriteRune('_')
		default:
			// drop other characters to avoid path traversal and encoding issues
		}
	}
	out := b.String()
	if out == "" {
		out = "unknown"
	}
	return out
}
