package main

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/PlatanosVerdes/wallapop-manager/internal/commands"
	"github.com/PlatanosVerdes/wallapop-manager/internal/config"
	"github.com/PlatanosVerdes/wallapop-manager/internal/telegram"
	"github.com/PlatanosVerdes/wallapop-manager/internal/users"
)

const (
	ownerChat  = "100"
	friendChat = "200"
	motos      = "https://es.wallapop.com/search?brand=Yamaha&model=XSR+900&category_id=14000"
)

func newBot(t *testing.T) *botState {
	t.Helper()
	cfg := config.Config{DataDir: t.TempDir(), TelegramChat: ownerChat, MaxSearches: 3, MaxUsers: 3}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	people, err := loadUsers(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return &botState{cfg: cfg, people: people, log: log}
}

func friend() commands.Request {
	return commands.Request{Chat: telegram.Chat{ID: 200, FirstName: "Ana"}}
}

// The bot is public: /start is all it takes, up to the cap.
func TestStartLetsAnybodyIn(t *testing.T) {
	b := newBot(t)
	ctx := context.Background()

	if _, err := b.start(ctx, friend()); err != nil {
		t.Fatal(err)
	}
	if !b.people.IsActive(friendChat) {
		t.Fatal("/start did not let the friend in")
	}

	// The owner and Ana make two of three; the third joins and the fourth finds it full.
	_, _ = b.start(ctx, commands.Request{Chat: telegram.Chat{ID: 300}})
	reply, _ := b.start(ctx, commands.Request{Chat: telegram.Chat{ID: 400}})
	if b.people.IsActive("400") || !strings.Contains(reply.Text, "lleno") {
		t.Fatalf("the cap was not kept: %q", reply.Text)
	}
}

// The owner is created before writing anything, so the name comes with the first /start.
func TestStartKeepsTheNameUpToDate(t *testing.T) {
	b := newBot(t)
	_, _ = b.start(context.Background(), commands.Request{Chat: telegram.Chat{ID: 100, FirstName: "Jorge"}})
	if user, _ := b.people.Get(ownerChat); user.Name != "Jorge" {
		t.Fatalf("the owner is still called %q", user.Name)
	}
}

func TestAPastedAddressBecomesASearch(t *testing.T) {
	b := newBot(t)
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
	b := newBot(t)
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
	b := newBot(t)
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
	b := newBot(t)
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

// The name can go before the address or after it, pasted or behind /nueva.
func TestTheNameGoesAroundTheAddress(t *testing.T) {
	b := newBot(t)
	coches := "https://es.wallapop.com/search?category_id=100&brand=Citroen&order_by=closest"
	for _, cmd := range b.commands(&commands.Listener{}) {
		if cmd.Name != "nueva" {
			continue
		}
		if _, err := cmd.Run(context.Background(), commands.Request{Chat: telegram.Chat{ID: 100}, Args: "coches top " + coches}); err != nil {
			t.Fatal(err)
		}
	}
	user, _ := b.people.Get(ownerChat)
	if len(user.Searches) != 1 || user.Searches[0].Name != "coches top" {
		t.Fatalf("stored %+v", user.Searches)
	}
	if got := user.Searches[0].Values().Get("order_by"); got != "newest" {
		t.Errorf("order_by = %q", got)
	}
}

// The pencil makes the next message the new name, and only the next one.
func TestThePencilRenames(t *testing.T) {
	b := newBot(t)
	ctx := context.Background()
	me := commands.Request{Chat: telegram.Chat{ID: 100}}
	search, _ := addSearch(b.cfg, b.people, ownerChat, motos, "Motos")

	if _, _, err := b.onButton(ctx, ownerChat, buttonRename+search.ID); err != nil {
		t.Fatal(err)
	}
	me.Args = "  las   motos  "
	if _, err := b.onText(ctx, me); err != nil {
		t.Fatal(err)
	}
	if got, _ := b.people.Search(ownerChat, search.ID); got.Name != "las motos" {
		t.Fatalf("the search is called %q", got.Name)
	}

	// The question has been answered: the next message is not a name any more.
	me.Args = "otra cosa"
	_, _ = b.onText(ctx, me)
	if got, _ := b.people.Search(ownerChat, search.ID); got.Name != "las motos" {
		t.Fatalf("a second message renamed it again to %q", got.Name)
	}
}

// An address sent while a name is expected is still a new search.
func TestAnAddressIsNotAName(t *testing.T) {
	b := newBot(t)
	ctx := context.Background()
	search, _ := addSearch(b.cfg, b.people, ownerChat, motos, "Motos")
	_, _, _ = b.onButton(ctx, ownerChat, buttonRename+search.ID)

	_, err := b.onText(ctx, commands.Request{Chat: telegram.Chat{ID: 100},
		Args: "https://es.wallapop.com/search?keywords=kallax"})
	if err != nil {
		t.Fatal(err)
	}
	user, _ := b.people.Get(ownerChat)
	if len(user.Searches) != 2 || user.Searches[0].Name != "Motos" {
		t.Fatalf("searches are %+v", user.Searches)
	}
}

// Nobody renames another chat's search with a forged pencil.
func TestAForgedPencilRenamesNothing(t *testing.T) {
	b := newBot(t)
	ctx := context.Background()
	_, _ = b.people.Request(friendChat, "Ana", true, time.Now())
	search, _ := addSearch(b.cfg, b.people, ownerChat, motos, "Motos")

	_, _, _ = b.onButton(ctx, friendChat, buttonRename+search.ID)
	_, _ = b.onText(ctx, commands.Request{Chat: telegram.Chat{ID: 200}, Args: "mia"})
	if got, _ := b.people.Search(ownerChat, search.ID); got.Name != "Motos" {
		t.Fatalf("another chat renamed the search to %q", got.Name)
	}
}
