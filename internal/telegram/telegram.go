// Package telegram carries the one thing this service is asked to say out loud: a listing
// has just appeared. Everything else it knows is a metric.
package telegram

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const api = "https://api.telegram.org/bot"

// captionLimit is Telegram's ceiling for a photo caption; a plain message allows four
// times more, which is why a long one goes without the picture.
const captionLimit = 1024

type Bot struct {
	Token  string
	Chat   string
	HTTP   *http.Client
	APIURL string
}

func New(token, chat string) *Bot {
	return &Bot{Token: token, Chat: chat, HTTP: &http.Client{Timeout: 20 * time.Second}, APIURL: api}
}

func (b *Bot) Enabled() bool { return b != nil && b.Token != "" && b.Chat != "" }

// Photo sends the picture with the text under it, and falls back to the text alone when
// Telegram will not take the image: a listing is worth sending without its photo.
func (b *Bot) Photo(ctx context.Context, photo, caption string) error {
	if !b.Enabled() {
		return nil
	}
	if photo == "" || len(caption) > captionLimit {
		return b.Text(ctx, caption)
	}
	err := b.call(ctx, "sendPhoto", url.Values{
		"chat_id":    {b.Chat},
		"photo":      {photo},
		"caption":    {caption},
		"parse_mode": {"HTML"},
	})
	if err == nil {
		return nil
	}
	return b.Text(ctx, caption)
}

func (b *Bot) Text(ctx context.Context, text string) error {
	if !b.Enabled() {
		return nil
	}
	return b.call(ctx, "sendMessage", url.Values{
		"chat_id":                  {b.Chat},
		"text":                     {text},
		"parse_mode":               {"HTML"},
		"disable_web_page_preview": {"true"},
	})
}

func (b *Bot) call(ctx context.Context, method string, form url.Values) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		b.APIURL+b.Token+"/"+method, strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := b.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("telegram %s: %w", method, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		// The token is in the URL, so the error says the method and never the target.
		return fmt.Errorf("telegram %s answered %d: %s", method, resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	return nil
}

// Escape protects the three characters HTML parse mode reads as markup. Listing titles
// are written by strangers and arrive full of them.
func Escape(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace(s)
}
