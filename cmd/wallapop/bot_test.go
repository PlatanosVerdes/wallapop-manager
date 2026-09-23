package main

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/PlatanosVerdes/wallapop-manager/internal/commands"
	"github.com/PlatanosVerdes/wallapop-manager/internal/config"
	"github.com/PlatanosVerdes/wallapop-manager/internal/session"
	"github.com/PlatanosVerdes/wallapop-manager/internal/telegram"
	"github.com/PlatanosVerdes/wallapop-manager/internal/users"
)

const (
	ownerChat  = "100"
	friendChat = "200"
	motos      = "https://es.wallapop.com/search?brand=Yamaha&model=XSR+900&category_id=14000"
)

// sent records who was told what.
type sent struct {
	mu   sync.Mutex
	msgs []string
}

func (s *sent) to(chat string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for _, m := range s.msgs {
		if to, text, _ := strings.Cut(m, "|"); to == chat {
			out = append(out, text)
		}
	}
	return out
}

func newBot(t *testing.T) (*botState, *sent) {
	t.Helper()
	box := &sent{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		if strings.HasSuffix(r.URL.Path, "sendMessage") {
			box.mu.Lock()
			box.msgs = append(box.msgs, r.Form.Get("chat_id")+"|"+r.Form.Get("text"))
			box.mu.Unlock()
		}
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	t.Cleanup(srv.Close)

	dir := t.TempDir()
	cfg := config.Config{DataDir: dir, TelegramChat: ownerChat, TelegramToken: "token", MaxSearches: 3}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	people, err := loadUsers(cfg, log)
	if err != nil {
		t.Fatal(err)
	}
	bot := telegram.New("token", ownerChat)
	bot.APIURL = srv.URL + "/bot"
	return &botState{
		cfg: cfg, store: session.NewStore(dir), people: people, log: log, bot: bot,
		nextWatch: time.Now, nextRun: time.Now,
	}, box
}

func friend() commands.Request {
	return commands.Request{Chat: telegram.Chat{ID: 200, FirstName: "Ana"}}
}

func TestAFriendGetsInOnlyWhenTheOwnerSaysSo(t *testing.T) {
	b, box := newBot(t)
	ctx := context.Background()

	if _, err := b.start(ctx, friend()); err != nil {
		t.Fatal(err)
	}
	if b.people.IsActive(friendChat) {
		t.Fatal("a stranger was let in by asking")
	}
	if asked := box.to(ownerChat); len(asked) != 1 || !strings.Contains(asked[0], "Ana") {
		t.Fatalf("the owner was told %v", asked)
	}

	// A second /start does not bother the owner again.
	_, _ = b.start(ctx, friend())
	if asked := box.to(ownerChat); len(asked) != 1 {
		t.Fatalf("the owner was asked %d times", len(asked))
	}

	// Only the owner's press counts, even if somebody forges the button.
	if _, _, err := b.onButton(ctx, friendChat, buttonApprove+friendChat); err != nil {
		t.Fatal(err)
	}
	if b.people.IsActive(friendChat) {
		t.Fatal("a chat approved itself")
	}

	if _, _, err := b.onButton(ctx, ownerChat, buttonApprove+friendChat); err != nil {
		t.Fatal(err)
	}
	if !b.people.IsActive(friendChat) {
		t.Fatal("the owner's yes did not let the friend in")
	}
	if told := box.to(friendChat); len(told) == 0 || !strings.Contains(told[len(told)-1], "dentro") {
		t.Fatalf("the friend was told %v", told)
	}
}

func TestAPastedAddressBecomesASearch(t *testing.T) {
	b, _ := newBot(t)
	ctx := context.Background()

	reply, err := b.onText(ctx, commands.Request{Chat: telegram.Chat{ID: 100}, Args: "mira " + motos + " la moto"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(reply.Text, "Guardada") || !strings.Contains(reply.Text, "toda España") {
		t.Fatalf("the answer was %q", reply.Text)
	}
	user, _ := b.people.Get(ownerChat)
	if len(user.Searches) != 1 || user.Searches[0].Name != "mira la moto" {
		t.Fatalf("stored %+v", user.Searches)
	}
}

// Every press is looked up inside the chat it came from.
func TestNobodyTouchesAnotherChatsSearch(t *testing.T) {
	b, _ := newBot(t)
	ctx := context.Background()
	_, _ = b.people.Request(friendChat, "Ana", true, time.Now())
	search, err := addSearch(b.cfg, b.people, ownerChat, motos, "")
	if err != nil {
		t.Fatal(err)
	}

	for _, data := range []string{buttonToggle, buttonMute, buttonDelete} {
		if _, _, err := b.onButton(ctx, friendChat, data+search.ID); err != nil {
			t.Fatal(err)
		}
	}
	got, err := b.people.Search(ownerChat, search.ID)
	if err != nil || got.Muted {
		t.Fatalf("another chat reached the owner's search: %+v, %v", got, err)
	}
}

func TestDeletingAsksFirst(t *testing.T) {
	b, _ := newBot(t)
	ctx := context.Background()
	search, _ := addSearch(b.cfg, b.people, ownerChat, motos, "Motos")

	_, keys, _ := b.onButton(ctx, ownerChat, buttonAskDelete+search.ID)
	if _, err := b.people.Search(ownerChat, search.ID); err != nil {
		t.Fatal("the bin deleted without asking")
	}
	if keys == nil || len(keys.Rows) != 1 || keys.Rows[0][0].Data != buttonDelete+search.ID {
		t.Fatalf("the question was drawn as %+v", keys)
	}

	_, _, _ = b.onButton(ctx, ownerChat, buttonDelete+search.ID)
	if _, err := b.people.Search(ownerChat, search.ID); err == nil {
		t.Fatal("the search survived its confirmed deletion")
	}
}

func TestLeavingRemovesEverything(t *testing.T) {
	b, _ := newBot(t)
	ctx := context.Background()
	_, _ = b.people.Request(friendChat, "Ana", true, time.Now())
	_, _ = addSearch(b.cfg, b.people, friendChat, motos, "")

	if _, _, err := b.onButton(ctx, friendChat, buttonLeave); err != nil {
		t.Fatal(err)
	}
	if _, ok := b.people.Get(friendChat); ok {
		t.Fatal("the chat is still there after leaving")
	}
	if _, err := users.Load(b.cfg.DataDir); err != nil {
		t.Fatal(err)
	}
}
