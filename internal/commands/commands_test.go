package commands

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/PlatanosVerdes/wallapop-manager/internal/telegram"
)

func TestParse(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"/wp_status", "wp_status"},
		{"  /wp_status  ", "wp_status"},
		{"/WP_Status", "wp_status"},
		{"/wp_status@PlatanosServicesBot", "wp_status"},
		{"/wp_check ahora", "wp_check"},
		{"hola", ""},
		{"", ""},
	} {
		if got := parse(tc.in); got != tc.want {
			t.Errorf("parse(%q) = %q, expected %q", tc.in, got, tc.want)
		}
	}
}

// telegramFake answers the three calls the listener makes and records what was sent.
type telegramFake struct {
	mu       sync.Mutex
	updates  []telegram.Update
	sent     []string
	keys     []string
	answered []string
	edited   []string
	menu     string
	served   chan struct{}
	requests int
	// serveOn is the poll that hands the queue over. One is the bootstrap read, so a
	// fixture placed there is what a restart finds waiting.
	serveOn int
}

func (f *telegramFake) server(t *testing.T) *telegram.Bot {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		f.mu.Lock()
		defer f.mu.Unlock()

		switch {
		case strings.HasSuffix(r.URL.Path, "setMyCommands"):
			f.menu = r.Form.Get("commands")
		case strings.HasSuffix(r.URL.Path, "getUpdates"):
			f.requests++
			// The first read is the bootstrap; the queue is handed over on the second and
			// stays empty after that, which is what a long poll looks like.
			out := []telegram.Update{}
			if f.requests == f.serveOn {
				out = f.updates
			}
			if f.requests > f.serveOn {
				select {
				case <-f.served:
				default:
					close(f.served)
				}
				time.Sleep(10 * time.Millisecond)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": out})
			return
		case strings.HasSuffix(r.URL.Path, "sendMessage"):
			f.sent = append(f.sent, r.Form.Get("text"))
			f.keys = append(f.keys, r.Form.Get("reply_markup"))
		case strings.HasSuffix(r.URL.Path, "answerCallbackQuery"):
			f.answered = append(f.answered, r.Form.Get("text"))
		case strings.HasSuffix(r.URL.Path, "editMessageReplyMarkup"):
			f.edited = append(f.edited, r.Form.Get("reply_markup"))
		}
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	t.Cleanup(srv.Close)

	bot := telegram.New("token", "1308329178")
	bot.APIURL = srv.URL + "/bot"
	return bot
}

func message(id int64, chat int64, text string) telegram.Update {
	return telegram.Update{UpdateID: id, Message: &telegram.Message{Text: text, Chat: telegram.Chat{ID: chat, Type: "private"}}}
}

func listen(t *testing.T, fake *telegramFake, cmds []Command) []string {
	t.Helper()
	fake.served = make(chan struct{})
	if fake.serveOn == 0 {
		fake.serveOn = 2
	}
	listener := &Listener{
		Bot:      fake.server(t),
		Chat:     "1308329178",
		Commands: cmds,
		Log:      slog.New(slog.NewTextHandler(io.Discard, nil)),
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		_ = listener.Serve(ctx)
		close(done)
	}()

	select {
	case <-fake.served:
	case <-time.After(5 * time.Second):
		t.Fatal("the listener never drained the queue")
	}
	cancel()
	<-done

	fake.mu.Lock()
	defer fake.mu.Unlock()
	return append([]string(nil), fake.sent...)
}

func TestCommandIsAnswered(t *testing.T) {
	fake := &telegramFake{updates: []telegram.Update{
		message(2, 1308329178, "/wp_status"),
	}}
	sent := listen(t, fake, []Command{
		{Name: "wp_status", Help: "estado", Run: func(context.Context) (Reply, error) { return Say("todo en orden"), nil }},
	})

	if len(sent) != 1 || sent[0] != "todo en orden" {
		t.Fatalf("answers were %v", sent)
	}
	if !strings.Contains(fake.menu, "wp_status") {
		t.Errorf("the menu was published as %q", fake.menu)
	}
}

// A bot is public. Anybody can write to it, and nobody else gets an answer.
func TestAnotherChatIsIgnored(t *testing.T) {
	fake := &telegramFake{updates: []telegram.Update{
		message(2, 999999, "/wp_status"),
	}}
	sent := listen(t, fake, []Command{
		{Name: "wp_status", Run: func(context.Context) (Reply, error) { return Say("secreto"), nil }},
	})
	if len(sent) != 0 {
		t.Fatalf("a stranger was answered: %v", sent)
	}
}

// The bot is shared, so a command that is not ours belongs to somebody else.
func TestUnknownCommandStaysQuiet(t *testing.T) {
	fake := &telegramFake{updates: []telegram.Update{
		message(2, 1308329178, "/pf_offers"),
		message(3, 1308329178, "hola que tal"),
	}}
	if sent := listen(t, fake, []Command{{Name: "wp_status"}}); len(sent) != 0 {
		t.Fatalf("answered something that was not for us: %v", sent)
	}
}

func TestFailedCommandAnswersWhy(t *testing.T) {
	fake := &telegramFake{updates: []telegram.Update{message(2, 1308329178, "/wp_check")}}
	sent := listen(t, fake, []Command{
		{Name: "wp_check", Run: func(context.Context) (Reply, error) { return Reply{}, ErrBusy }},
	})
	if len(sent) != 1 || !strings.Contains(sent[0], "ronda en marcha") {
		t.Fatalf("answers were %v", sent)
	}
}

func TestHelpIsBuiltFromTheTable(t *testing.T) {
	got := Help([]Command{
		{Name: "wp_status", Help: "estado"},
		{Name: "wp_check", Help: "mira ahora"},
	})
	for _, want := range []string{"/wp_status — estado", "/wp_check — mira ahora"} {
		if !strings.Contains(got, want) {
			t.Errorf("help does not carry %q:\n%s", want, got)
		}
	}
}

// The first read answers whatever was queued before the process started, which on a
// restart is this morning's command. Running it would act on a message nobody sent again.
func TestNothingIsReplayedAfterARestart(t *testing.T) {
	fake := &telegramFake{
		serveOn: 1,
		updates: []telegram.Update{message(9, 1308329178, "/wp_check")},
	}
	var ran int
	sent := listen(t, fake, []Command{
		{Name: "wp_check", Run: func(context.Context) (Reply, error) { ran++; return Say("hecho"), nil }},
	})
	if ran != 0 || len(sent) != 0 {
		t.Fatalf("a queued command was replayed on startup: ran %d, sent %v", ran, sent)
	}
}

func TestButtonIsAnsweredAndRedrawn(t *testing.T) {
	fake := &telegramFake{updates: []telegram.Update{{
		UpdateID: 2,
		CallbackQuery: &telegram.CallbackQuery{
			ID:      "q1",
			Data:    DataPrefix + "t:c6ae82bf-ccbe-4a8a-9981-5c7487a05940",
			Message: &telegram.Message{MessageID: 77, Chat: telegram.Chat{ID: 1308329178}},
		},
	}}}
	fake.served = make(chan struct{})
	fake.serveOn = 2

	listener := &Listener{
		Bot:  fake.server(t),
		Chat: "1308329178",
		Log:  slog.New(slog.NewTextHandler(io.Discard, nil)),
		OnButton: func(_ context.Context, data string) (string, *telegram.Keyboard, error) {
			return "Silenciada Motos", &telegram.Keyboard{Rows: [][]telegram.Button{{{Text: "🔕 Motos", Data: data}}}}, nil
		},
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = listener.Serve(ctx); close(done) }()
	select {
	case <-fake.served:
	case <-time.After(5 * time.Second):
		t.Fatal("the press never arrived")
	}
	cancel()
	<-done

	fake.mu.Lock()
	defer fake.mu.Unlock()
	// Telegram spins on the phone until the query is closed, whatever the outcome.
	if len(fake.answered) != 1 || fake.answered[0] != "Silenciada Motos" {
		t.Fatalf("the press was answered with %v", fake.answered)
	}
	if len(fake.edited) != 1 || !strings.Contains(fake.edited[0], "🔕 Motos") {
		t.Fatalf("the buttons were redrawn as %v", fake.edited)
	}
}

func TestButtonFromAnotherChatIsIgnored(t *testing.T) {
	fake := &telegramFake{updates: []telegram.Update{{
		UpdateID: 2,
		CallbackQuery: &telegram.CallbackQuery{
			ID:      "q1",
			Data:    DataPrefix + "t:whatever",
			Message: &telegram.Message{MessageID: 5, Chat: telegram.Chat{ID: 999}},
		},
	}}}
	fake.served = make(chan struct{})
	fake.serveOn = 2

	var ran bool
	listener := &Listener{
		Bot:  fake.server(t),
		Chat: "1308329178",
		Log:  slog.New(slog.NewTextHandler(io.Discard, nil)),
		OnButton: func(context.Context, string) (string, *telegram.Keyboard, error) {
			ran = true
			return "no deberia", nil, nil
		},
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = listener.Serve(ctx); close(done) }()
	select {
	case <-fake.served:
	case <-time.After(5 * time.Second):
		t.Fatal("the press never arrived")
	}
	cancel()
	<-done

	if ran {
		t.Fatal("a stranger's button press was acted on")
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.edited) != 0 {
		t.Fatalf("something was redrawn for a stranger: %v", fake.edited)
	}
}
