package main

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

	"github.com/PlatanosVerdes/wallapop-manager/internal/commands"
	"github.com/PlatanosVerdes/wallapop-manager/internal/config"
	"github.com/PlatanosVerdes/wallapop-manager/internal/telegram"
	"github.com/PlatanosVerdes/wallapop-manager/internal/users"
	"github.com/PlatanosVerdes/wallapop-manager/internal/wallapop"
)

const (
	ownerChat  = "100"
	friendChat = "200"
	motos      = "https://es.wallapop.com/search?brand=Yamaha&model=XSR+900&category_id=14000"
)

func newBot(t *testing.T) *botState {
	t.Helper()
	cfg := config.Config{DataDir: t.TempDir(), TelegramChat: ownerChat, MaxSearches: 3, MaxUsers: 3,
		Scheme: wallapop.SchemeNone}
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

func TestStartLetsAnybodyIn(t *testing.T) {
	b := newBot(t)
	ctx := context.Background()

	if _, err := b.start(ctx, friend()); err != nil {
		t.Fatal(err)
	}
	if !b.people.IsActive(friendChat) {
		t.Fatal("/start did not let the friend in")
	}

	// Owner and Ana are two of three: the third fits, the fourth does not.
	_, _ = b.start(ctx, commands.Request{Chat: telegram.Chat{ID: 300}})
	reply, _ := b.start(ctx, commands.Request{Chat: telegram.Chat{ID: 400}})
	if b.people.IsActive("400") || !strings.Contains(reply.Text, "lleno") {
		t.Fatalf("the cap was not kept: %q", reply.Text)
	}
}

// The owner exists before writing, so the name arrives with the first /start.
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

func TestAWrittenSearchIsSaved(t *testing.T) {
	b := newBot(t)
	reply, err := b.onText(context.Background(), commands.Request{Chat: telegram.Chat{ID: 100}, Args: "bici 100-300"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(reply.Text, "Guardada") || !strings.Contains(reply.Text, "de 100 a 300") {
		t.Fatalf("the answer was %q", reply.Text)
	}
	user, _ := b.people.Get(ownerChat)
	if len(user.Searches) != 1 || user.Searches[0].Name != "bici" {
		t.Fatalf("stored %+v", user.Searches)
	}
}

func TestSmallTalkIsNoSearch(t *testing.T) {
	b := newBot(t)
	for _, text := range []string{"hola", "Gracias!!", "jajaja", "👍", "buenas noches"} {
		reply, _ := b.onText(context.Background(), commands.Request{Chat: telegram.Chat{ID: 100}, Args: text})
		if strings.Contains(reply.Text, "Guardada") || reply.Keys != nil {
			t.Errorf("%q was taken as a search: %q", text, reply.Text)
		}
	}
	for _, text := range []string{"bici", "ps5", "no frost"} {
		if smallTalk(text) {
			t.Errorf("%q was taken as small talk", text)
		}
	}
}

// The narrowed search gets a new id, so its first round stays silent.
func TestALocationNarrowsTheLatestSearch(t *testing.T) {
	b := newBot(t)
	ctx := context.Background()
	owner := telegram.Chat{ID: 100}
	_, _ = addSearch(ctx, b.cfg, b.people, ownerChat, "kallax", "")
	_, _ = addSearch(ctx, b.cfg, b.people, ownerChat, "bici a 10 km", "")
	before, _ := b.people.Get(ownerChat)

	reply, err := b.onText(ctx, commands.Request{Chat: owner, Location: &telegram.Location{Latitude: 41.38, Longitude: 2.17}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(reply.Text, "hasta 10 km") {
		t.Fatalf("the answer was %q", reply.Text)
	}
	after, _ := b.people.Get(ownerChat)
	if after.Searches[0] != before.Searches[0] {
		t.Errorf("the older search changed: %+v", after.Searches[0])
	}
	bici := after.Searches[1]
	if bici.ID == before.Searches[1].ID || wallapop.RadiusKm(bici.Values()) != "10" {
		t.Errorf("the latest search was not narrowed afresh: %+v", bici)
	}
}

func TestNobodyTouchesAnotherChatsSearch(t *testing.T) {
	b := newBot(t)
	ctx := context.Background()
	_, _ = b.people.Request(friendChat, "Ana", true, time.Now())
	search, err := addSearch(context.Background(), b.cfg, b.people, ownerChat, motos, "")
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
	search, _ := addSearch(context.Background(), b.cfg, b.people, ownerChat, motos, "Motos")

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
	_, _ = addSearch(context.Background(), b.cfg, b.people, friendChat, motos, "")

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

func TestThePencilRenames(t *testing.T) {
	b := newBot(t)
	ctx := context.Background()
	me := commands.Request{Chat: telegram.Chat{ID: 100}}
	search, _ := addSearch(context.Background(), b.cfg, b.people, ownerChat, motos, "Motos")

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

	me.Args = "otra cosa"
	_, _ = b.onText(ctx, me)
	if got, _ := b.people.Search(ownerChat, search.ID); got.Name != "las motos" {
		t.Fatalf("a second message renamed it again to %q", got.Name)
	}
}

func TestAnAddressIsNotAName(t *testing.T) {
	b := newBot(t)
	ctx := context.Background()
	search, _ := addSearch(context.Background(), b.cfg, b.people, ownerChat, motos, "Motos")
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

func TestAForgedPencilRenamesNothing(t *testing.T) {
	b := newBot(t)
	ctx := context.Background()
	_, _ = b.people.Request(friendChat, "Ana", true, time.Now())
	search, _ := addSearch(context.Background(), b.cfg, b.people, ownerChat, motos, "Motos")

	_, _, _ = b.onButton(ctx, friendChat, buttonRename+search.ID)
	_, _ = b.onText(ctx, commands.Request{Chat: telegram.Chat{ID: 200}, Args: "mia"})
	if got, _ := b.people.Search(ownerChat, search.ID); got.Name != "Motos" {
		t.Fatalf("another chat renamed the search to %q", got.Name)
	}
}

func fakeSearch(t *testing.T, b *botState) func() []string {
	t.Helper()
	var mu sync.Mutex
	var asked []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		asked = append(asked, r.URL.Query().Get("keywords"))
		mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"section": map[string]any{
			"payload": map[string]any{"items": []any{}}}}})
	}))
	t.Cleanup(srv.Close)
	b.cfg.BaseURL = srv.URL
	return func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), asked...)
	}
}

// A picked search is read even when silenced; "Todas" skips silenced ones, like a round.
func TestCheckReadsTheSearchPicked(t *testing.T) {
	b := newBot(t)
	asked := fakeSearch(t, b)
	ctx := context.Background()
	kallax, _ := addSearch(context.Background(), b.cfg, b.people, ownerChat, "https://es.wallapop.com/search?keywords=kallax", "")
	_, _ = addSearch(context.Background(), b.cfg, b.people, ownerChat, "https://es.wallapop.com/search?keywords=bici", "")
	_, _ = b.people.SetMuted(ownerChat, kallax.ID, true)

	_, keys, err := b.onButton(ctx, ownerChat, buttonCheck+kallax.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got := asked(); len(got) != 1 || got[0] != "kallax" {
		t.Fatalf("asked %v, expected kallax alone", got)
	}
	if keys == nil || !strings.Contains(keys.Rows[0][0].Text, "kallax: 0 mirados") {
		t.Fatalf("the receipt was %+v", keys)
	}

	_, _, _ = b.onButton(ctx, ownerChat, buttonCheck)
	if got := asked(); len(got) != 2 || got[1] != "bici" {
		t.Fatalf("asked %v, expected bici after kallax", got)
	}
}

func TestCheckOffersEverySearchAndAll(t *testing.T) {
	b := newBot(t)
	_, _ = addSearch(context.Background(), b.cfg, b.people, ownerChat, "https://es.wallapop.com/search?keywords=kallax", "")
	_, _ = addSearch(context.Background(), b.cfg, b.people, ownerChat, "https://es.wallapop.com/search?keywords=bici", "")
	user, _ := b.people.Get(ownerChat)
	keys := checkKeys(user)
	if len(keys.Rows) != 3 || keys.Rows[2][0].Data != buttonCheck {
		t.Fatalf("the choice was drawn as %+v", keys.Rows)
	}
}
