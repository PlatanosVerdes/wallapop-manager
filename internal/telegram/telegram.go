// Package telegram carries the one thing this service is asked to say out loud: a listing
// has just appeared. Everything else it knows is a metric.
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
	MessageID int64  `json:"message_id"`
	Date      int64  `json:"date"`
	Text      string `json:"text"`
	Chat      Chat   `json:"chat"`
}

type Update struct {
	UpdateID      int64          `json:"update_id"`
	Message       *Message       `json:"message"`
	CallbackQuery *CallbackQuery `json:"callback_query"`
}

// CallbackQuery is a button press. Message is the one the button hangs from, which is
// what an edit needs to redraw it.
type CallbackQuery struct {
	ID      string   `json:"id"`
	Data    string   `json:"data"`
	Message *Message `json:"message"`
}

// Updates asks for everything after offset, waiting for it. An offset of -1 answers the
// last update alone, which is how a restart learns where it is without replaying a day of
// commands.
func (b *Bot) Updates(ctx context.Context, offset int64) ([]Update, error) {
	if !b.Enabled() {
		return nil, nil
	}

	form := url.Values{
		"timeout": {strconv.Itoa(pollSeconds)},
		// A button press arrives as a callback_query, and an update type left out of this
		// list is never delivered at all.
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

// Button is one key of an inline keyboard. Data is what comes back when it is pressed and
// is capped by Telegram at 64 bytes, so it carries an id and never a payload.
type Button struct {
	Text string `json:"text"`
	Data string `json:"callback_data,omitempty"`
	// URL turns the key into a link, which is tidier than a raw address in the text.
	URL string `json:"url,omitempty"`
	// Style is "danger", "success" or "primary". Empty is the plain button.
	Style string `json:"style,omitempty"`
	// Disabled draws the key as a label that does nothing, which is what a switch already
	// in the position asked for should look like.
	Disabled *Disabled `json:"disabled,omitempty"`
}

type Disabled struct{}

// Off is the button that says what happened and cannot be pressed again.
func Off(text string) Button { return Button{Text: text, Disabled: &Disabled{}} }

type Keyboard struct {
	Rows [][]Button `json:"inline_keyboard"`
}

// DataLimit is Telegram's ceiling on callback_data. Going over it does not drop the
// button: the whole message is refused.
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

// Answer closes a button press. Telegram spins on the phone until this arrives and then
// gives up on it, so it is sent whatever the outcome was.
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

// EditKeys redraws the buttons of a message in place, which is how a switch shows the
// position it was just moved to.
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
	// An empty keyboard has to go as an explicit object, or the buttons stay where they
	// were.
	if form.Get("reply_markup") == "" {
		form.Set("reply_markup", `{"inline_keyboard":[]}`)
	}
	return b.call(ctx, "editMessageReplyMarkup", form)
}

// noticeLimit is the length of the little banner a press raises on the phone.
const noticeLimit = 200

func truncate(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	return s[:limit-1] + "…"
}
