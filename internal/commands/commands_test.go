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
	for _, tc := range []struct{ in, name, args string }{
		{"/estado", "estado", ""},
		{"  /estado  ", "estado", ""},
		{"/Estado", "estado", ""},
		{"/estado@PlatanosWallapopBot", "estado", ""},
		{"/nueva https://es.wallapop.com/search?keywords=kallax", "nueva", "https://es.wallapop.com/search?keywords=kallax"},
		{"hola", "", "hola"},
		{"", "", ""},
	} {
		if name, args := parse(tc.in); name != tc.name || args != tc.args {
			t.Errorf("parse(%q) = %q, %q; expected %q, %q", tc.in, name, args, tc.name, tc.args)
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

const member = "1308329178"

func onlyMember(chat string) bool { return chat == member }

func listen(t *testing.T, fake *telegramFake, cmds []Command) []string {
	t.Helper()
	return listenWith(t, fake, &Listener{Commands: cmds})
}

func listenWith(t *testing.T, fake *telegramFake, listener *Listener) []string {
	t.Helper()
	fake.served = make(chan struct{})
	if fake.serveOn == 0 {
		fake.serveOn = 2
	}
	listener.Bot = fake.server(t)
	listener.Allowed = onlyMember
	listener.Log = slog.New(slog.NewTextHandler(io.Discard, nil))

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
		message(2, 1308329178, "/estado"),
	}}
	sent := listen(t, fake, []Command{
		{Name: "estado", Help: "estado", Run: func(context.Context, Request) (Reply, error) { return Say("todo en orden"), nil }},
	})

	if len(sent) != 1 || sent[0] != "todo en orden" {
		t.Fatalf("answers were %v", sent)
	}
	if !strings.Contains(fake.menu, "estado") {
		t.Errorf("the menu was published as %q", fake.menu)
	}
}

// A bot is public. Anybody can write to it, and a stranger gets the open commands alone.
func TestAnotherChatIsIgnored(t *testing.T) {
	fake := &telegramFake{updates: []telegram.Update{
		message(2, 999999, "/estado"),
	}}
	sent := listen(t, fake, []Command{
		{Name: "estado", Run: func(context.Context, Request) (Reply, error) { return Say("secreto"), nil }},
	})
	if len(sent) != 0 {
		t.Fatalf("a stranger was answered: %v", sent)
	}
}

func TestStrangerCanOnlyStart(t *testing.T) {
	fake := &telegramFake{updates: []telegram.Update{
		message(2, 999999, "/estado"),
		message(3, 999999, "https://es.wallapop.com/search?keywords=kallax"),
		message(4, 999999, "/start"),
	}}
	var texts int
	sent := listenWith(t, fake, &Listener{
		Commands: []Command{
			{Name: "estado", Run: func(context.Context, Request) (Reply, error) { return Say("secreto"), nil }},
			{Name: "start", Open: true, Run: func(_ context.Context, req Request) (Reply, error) {
				return Say("hola " + req.ChatID()), nil
			}},
		},
		OnText: func(context.Context, Request) (Reply, error) { texts++; return Say("guardada"), nil },
	})
	if len(sent) != 1 || sent[0] != "hola 999999" || texts != 0 {
		t.Fatalf("a stranger got %v, and %d texts were handled", sent, texts)
	}
}

// A message that is not a command goes to OnText, with the whole text as its arguments:
// that is how a pasted address becomes a search.
func TestTextGoesToOnText(t *testing.T) {
	fake := &telegramFake{updates: []telegram.Update{
		message(2, 1308329178, "https://es.wallapop.com/search?keywords=kallax"),
	}}
	var got string
	sent := listenWith(t, fake, &Listener{
		OnText: func(_ context.Context, req Request) (Reply, error) { got = req.Args; return Say("guardada"), nil },
	})
	if got != "https://es.wallapop.com/search?keywords=kallax" || len(sent) != 1 {
		t.Fatalf("OnText saw %q and the answers were %v", got, sent)
	}
}

func TestUnknownCommandPointsAtHelp(t *testing.T) {
	fake := &telegramFake{updates: []telegram.Update{message(2, 1308329178, "/nada")}}
	sent := listen(t, fake, []Command{{Name: "estado"}})
	if len(sent) != 1 || !strings.Contains(sent[0], "/ayuda") {
		t.Fatalf("answers were %v", sent)
	}
}

func TestFailedCommandAnswersWhy(t *testing.T) {
	fake := &telegramFake{updates: []telegram.Update{message(2, 1308329178, "/ahora")}}
	sent := listen(t, fake, []Command{
		{Name: "ahora", Run: func(context.Context, Request) (Reply, error) { return Reply{}, ErrBusy }},
	})
	if len(sent) != 1 || !strings.Contains(sent[0], "ronda en marcha") {
		t.Fatalf("answers were %v", sent)
	}
}

func TestHelpIsBuiltFromTheTable(t *testing.T) {
	got := Help([]Command{
		{Name: "estado", Help: "estado"},
		{Name: "ahora", Help: "mira ahora"},
	})
	for _, want := range []string{"/estado\n<i>estado</i>", "/ahora\n<i>mira ahora</i>"} {
		if !strings.Contains(got, want) {
			t.Errorf("help does not carry %q:\n%s", want, got)
		}
	}
}

// Telegram keeps updates for 24 hours, so the first read after a long stop hands over
// commands nobody is waiting for any more.
func TestNothingIsReplayedAfterARestart(t *testing.T) {
	fake := &telegramFake{
		serveOn: 1,
		updates: []telegram.Update{{UpdateID: 9, Message: &telegram.Message{
			Text: "/ahora", Date: time.Now().Add(-24 * time.Hour).Unix(),
			Chat: telegram.Chat{ID: 1308329178},
		}}},
	}
	var ran int
	sent := listen(t, fake, []Command{
		{Name: "ahora", Run: func(context.Context, Request) (Reply, error) { ran++; return Say("hecho"), nil }},
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
			Data:    "t:c6ae82bf",
			Message: &telegram.Message{MessageID: 77, Chat: telegram.Chat{ID: 1308329178}},
		},
	}}}
	fake.served = make(chan struct{})
	fake.serveOn = 2

	listener := &Listener{
		Bot:     fake.server(t),
		Allowed: onlyMember,
		Log:     slog.New(slog.NewTextHandler(io.Discard, nil)),
		OnButton: func(_ context.Context, _, data string) (string, *telegram.Keyboard, error) {
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
			Data:    "t:whatever",
			Message: &telegram.Message{MessageID: 5, Chat: telegram.Chat{ID: 999}},
		},
	}}}
	fake.served = make(chan struct{})
	fake.serveOn = 2

	var ran bool
	listener := &Listener{
		Bot:     fake.server(t),
		Allowed: onlyMember,
		Log:     slog.New(slog.NewTextHandler(io.Discard, nil)),
		OnButton: func(context.Context, string, string) (string, *telegram.Keyboard, error) {
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

// A command sent seconds before a restart is one its sender is still waiting for, so the
// queue handed over on the first read is judged by age and not thrown away whole.
func TestFreshCommandQueuedDuringARestartIsRun(t *testing.T) {
	fake := &telegramFake{serveOn: 1, updates: []telegram.Update{
		{UpdateID: 9, Message: &telegram.Message{
			Text: "/ahora", Date: time.Now().Add(-5 * time.Second).Unix(),
			Chat: telegram.Chat{ID: 1308329178},
		}},
	}}
	var ran int
	sent := listen(t, fake, []Command{
		{Name: "ahora", Run: func(context.Context, Request) (Reply, error) { ran++; return Say("hecho"), nil }},
	})
	if ran != 1 || len(sent) != 1 {
		t.Fatalf("a command sent five seconds ago was dropped: ran %d, sent %v", ran, sent)
	}
}

func TestStaleCommandIsDropped(t *testing.T) {
	fake := &telegramFake{serveOn: 1, updates: []telegram.Update{
		{UpdateID: 9, Message: &telegram.Message{
			Text: "/ahora", Date: time.Now().Add(-6 * time.Hour).Unix(),
			Chat: telegram.Chat{ID: 1308329178},
		}},
	}}
	var ran int
	sent := listen(t, fake, []Command{
		{Name: "ahora", Run: func(context.Context, Request) (Reply, error) { ran++; return Say("hecho"), nil }},
	})
	if ran != 0 || len(sent) != 0 {
		t.Fatalf("this morning's command was replayed: ran %d, sent %v", ran, sent)
	}
}

// A press has no time of its own, so the queue found on the first read is left alone.
func TestQueuedPressIsNotReplayed(t *testing.T) {
	fake := &telegramFake{serveOn: 1, updates: []telegram.Update{{
		UpdateID: 9,
		CallbackQuery: &telegram.CallbackQuery{
			ID:      "q1",
			Data:    "t:whatever",
			Message: &telegram.Message{MessageID: 5, Chat: telegram.Chat{ID: 1308329178}},
		},
	}}}
	fake.served = make(chan struct{})

	var ran bool
	listener := &Listener{
		Bot:     fake.server(t),
		Allowed: onlyMember,
		Log:     slog.New(slog.NewTextHandler(io.Discard, nil)),
		OnButton: func(context.Context, string, string) (string, *telegram.Keyboard, error) {
			ran = true
			return "silenciada", nil, nil
		},
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = listener.Serve(ctx); close(done) }()
	select {
	case <-fake.served:
	case <-time.After(5 * time.Second):
		t.Fatal("the listener never got going")
	}
	cancel()
	<-done

	if ran {
		t.Fatal("a press queued before the restart was acted on")
	}
}
