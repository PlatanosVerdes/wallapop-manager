// Package telegram talks to the Bot API: messages out, updates in.
package telegram

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const api = "https://api.telegram.org/bot"

// captionLimit is Telegram's cap on a photo caption; longer text goes without the photo.
const captionLimit = 1024

type Bot struct {
	Token string
	Chat  string
	HTTP  *http.Client
	// Poll has its own client: a long poll would trip the send timeout.
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

// pollSeconds is how long Telegram holds an empty getUpdates open.
const pollSeconds = 30

func (b *Bot) Enabled() bool { return b != nil && b.Token != "" && b.Chat != "" }

func (b *Bot) To(chat string) *Bot {
	other := *b
	other.Chat = chat
	return &other
}

// Photo falls back to text alone when Telegram refuses the image.
func (b *Bot) Photo(ctx context.Context, photo, caption string, keys *Keyboard) error {
	if !b.Enabled() {
		return nil
	}
	if photo == "" || len(caption) > captionLimit {
		return b.Text(ctx, caption, keys)
	}
	form := url.Values{
		"chat_id":    {b.Chat},
		"photo":      {photo},
		"caption":    {caption},
		"parse_mode": {"HTML"},
	}
	if err := withKeys(form, keys); err != nil {
		return err
	}
	if err := b.call(ctx, "sendPhoto", form); err == nil {
		return nil
	}
	return b.Text(ctx, caption, keys)
}

func (b *Bot) Text(ctx context.Context, text string, keys *Keyboard) error {
	if !b.Enabled() {
		return nil
	}
	form := url.Values{
		"chat_id":                  {b.Chat},
		"text":                     {text},
		"parse_mode":               {"HTML"},
		"disable_web_page_preview": {"true"},
	}
	if err := withKeys(form, keys); err != nil {
		return err
	}
	return b.call(ctx, "sendMessage", form)
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
		// The token is in the URL, so the error never includes it.
		return fmt.Errorf("telegram %s answered %d: %s", method, resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	return nil
}

// Escape protects what HTML parse mode reads as markup; listing titles are full of it.
func Escape(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace(s)
}

// A token has one reader: Telegram answers a second getUpdates caller with 409.

type Chat struct {
	ID        int64  `json:"id"`
	Type      string `json:"type"`
	Title     string `json:"title"`
	FirstName string `json:"first_name"`
	LastName  string `json:"last_name"`
	Username  string `json:"username"`
}

func (c Chat) Name() string {
	name := c.Title
	if name == "" {
		name = strings.TrimSpace(c.FirstName + " " + c.LastName)
	}
	if c.Username != "" {
		if name == "" {
			return "@" + c.Username
		}
		name += " (@" + c.Username + ")"
	}
	if name == "" {
		return strconv.FormatInt(c.ID, 10)
	}
	return name
}

type Message struct {
	MessageID int64     `json:"message_id"`
	Date      int64     `json:"date"`
	Text      string    `json:"text"`
	Chat      Chat      `json:"chat"`
	Location  *Location `json:"location"`
}

type Location struct {
	Latitude  float64 `json:"latitude"`
	Longitude float64 `json:"longitude"`
}

type Update struct {
	UpdateID      int64          `json:"update_id"`
	Message       *Message       `json:"message"`
	CallbackQuery *CallbackQuery `json:"callback_query"`
}

type CallbackQuery struct {
	ID      string   `json:"id"`
	Data    string   `json:"data"`
	Message *Message `json:"message"`
}

// Updates with offset -1 answers the last update alone, so a restart skips the backlog.
func (b *Bot) Updates(ctx context.Context, offset int64) ([]Update, error) {
	if !b.Enabled() {
		return nil, nil
	}

	form := url.Values{
		"timeout": {strconv.Itoa(pollSeconds)},
		// An update type left out of this list is never delivered.
		"allowed_updates": {`["message","callback_query"]`},
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

type Command struct {
	Name        string `json:"command"`
	Description string `json:"description"`
}

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

type Button struct {
	Text string `json:"text"`
	Data string `json:"callback_data,omitempty"`
	URL  string `json:"url,omitempty"`
	// Style is "danger", "success" or "primary".
	Style string `json:"style,omitempty"`
	// Disabled draws the key as a label that does nothing.
	Disabled *Disabled `json:"disabled,omitempty"`
}

type Disabled struct{}

func Off(text string) Button { return Button{Text: text, Disabled: &Disabled{}} }

type Keyboard struct {
	Rows [][]Button `json:"inline_keyboard"`
}

// DataLimit is Telegram's cap on callback_data; going over refuses the whole message.
const DataLimit = 64

var ErrDataTooLong = errors.New("telegram: callback data is over 64 bytes")

func withKeys(form url.Values, keys *Keyboard) error {
	if keys == nil || len(keys.Rows) == 0 {
		return nil
	}
	for _, row := range keys.Rows {
		for _, button := range row {
			if len(button.Data) > DataLimit {
				return fmt.Errorf("%w: %q", ErrDataTooLong, button.Data)
			}
		}
	}
	encoded, err := json.Marshal(keys)
	if err != nil {
		return err
	}
	form.Set("reply_markup", string(encoded))
	return nil
}

// Answer must be sent whatever the outcome: the phone spins until it arrives.
func (b *Bot) Answer(ctx context.Context, queryID, notice string) error {
	if !b.Enabled() {
		return nil
	}
	form := url.Values{"callback_query_id": {queryID}}
	if notice != "" {
		form.Set("text", truncate(notice, noticeLimit))
	}
	return b.call(ctx, "answerCallbackQuery", form)
}

func (b *Bot) EditKeys(ctx context.Context, messageID int64, keys *Keyboard) error {
	if !b.Enabled() || messageID == 0 {
		return nil
	}
	form := url.Values{
		"chat_id":    {b.Chat},
		"message_id": {strconv.FormatInt(messageID, 10)},
	}
	if keys == nil {
		keys = &Keyboard{}
	}
	if err := withKeys(form, keys); err != nil {
		return err
	}
	// An empty keyboard must be sent as an object, or the old buttons stay.
	if form.Get("reply_markup") == "" {
		form.Set("reply_markup", `{"inline_keyboard":[]}`)
	}
	return b.call(ctx, "editMessageReplyMarkup", form)
}

const noticeLimit = 200

func truncate(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	return s[:limit-1] + "…"
}
