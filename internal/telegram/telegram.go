// Package telegram carries the one thing this service is asked to say out loud: a listing
// has just appeared. Everything else it knows is a metric.
package telegram

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const api = "https://api.telegram.org/bot"

// captionLimit is Telegram's ceiling for a photo caption; a plain message allows four
// times more, which is why a long one goes without the picture.
const captionLimit = 1024

type Bot struct {
	Token string
	Chat  string
	HTTP  *http.Client
	// Poll is a second client for long polling, which holds a request open on purpose and
	// would trip the timeout of the one used for sending.
	Poll   *http.Client
	APIURL string
}

func New(token, chat string) *Bot {
	return &Bot{
		Token:  token,
		Chat:   chat,
		HTTP:   &http.Client{Timeout: 20 * time.Second},
		Poll:   &http.Client{Timeout: 2 * pollSeconds * time.Second},
		APIURL: api,
	}
}

// pollSeconds is how long Telegram holds an empty getUpdates open before answering. One
// long poll is one request a minute or two, rather than a poll every few seconds.
const pollSeconds = 30

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

// A bot token has exactly one reader: Telegram hands each update to whoever asks first and
// answers a second caller with 409. So one process owns the updates, and any other service
// sharing this bot may only send.

type Chat struct {
	ID   int64  `json:"id"`
	Type string `json:"type"`
}

type Message struct {
	Date int64  `json:"date"`
	Text string `json:"text"`
	Chat Chat   `json:"chat"`
}

type Update struct {
	UpdateID int64    `json:"update_id"`
	Message  *Message `json:"message"`
}

// Updates asks for everything after offset, waiting for it. An offset of -1 answers the
// last update alone, which is how a restart learns where it is without replaying a day of
// commands.
func (b *Bot) Updates(ctx context.Context, offset int64) ([]Update, error) {
	if !b.Enabled() {
		return nil, nil
	}

	form := url.Values{
		"timeout":         {strconv.Itoa(pollSeconds)},
		"allowed_updates": {`["message"]`},
	}
	if offset != 0 {
		form.Set("offset", strconv.FormatInt(offset, 10))
	}

	var answer struct {
		OK     bool     `json:"ok"`
		Result []Update `json:"result"`
	}
	if err := b.get(ctx, "getUpdates", form, &answer); err != nil {
		return nil, err
	}
	return answer.Result, nil
}

// Command is one entry of the menu Telegram shows next to the text box.
type Command struct {
	Name        string `json:"command"`
	Description string `json:"description"`
}

// SetCommands publishes the menu. The list belongs to the bot, so the process that owns
// the updates is the one that sets it.
func (b *Bot) SetCommands(ctx context.Context, commands []Command) error {
	if !b.Enabled() {
		return nil
	}
	encoded, err := json.Marshal(commands)
	if err != nil {
		return err
	}
	return b.call(ctx, "setMyCommands", url.Values{"commands": {string(encoded)}})
}

func (b *Bot) get(ctx context.Context, method string, form url.Values, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		b.APIURL+b.Token+"/"+method+"?"+form.Encode(), nil)
	if err != nil {
		return err
	}

	resp, err := b.Poll.Do(req)
	if err != nil {
		return fmt.Errorf("telegram %s: %w", method, err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode >= 400 {
		return fmt.Errorf("telegram %s answered %d: %s", method, resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	return json.Unmarshal(raw, out)
}
