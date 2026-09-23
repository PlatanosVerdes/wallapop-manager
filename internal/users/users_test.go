package users

import (
	"errors"
	"net/url"
	"testing"
	"time"
)

func TestAWaitingUserCannotAddSearches(t *testing.T) {
	store, err := Load(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	if created, _ := store.Request("42", "Ana", false, now); !created {
		t.Fatal("the first request was not recorded")
	}
	if created, _ := store.Request("42", "Ana", false, now); created {
		t.Fatal("a second /start counted as a new request")
	}

	query := url.Values{"keywords": {"kallax"}}
	if _, err := store.Add("42", "kallax", query, 3, now); !errors.Is(err, ErrUnknown) {
		t.Fatalf("a user waiting for approval added a search: %v", err)
	}
	if _, err := store.Approve("42"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Add("42", "kallax", query, 3, now); err != nil {
		t.Fatal(err)
	}
}

func TestSearchLimitAndRepeats(t *testing.T) {
	dir := t.TempDir()
	store, _ := Load(dir)
	now := time.Now()
	_, _ = store.Request("42", "Ana", true, now)

	first, err := store.Add("42", "kallax", url.Values{"keywords": {"kallax"}}, 2, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Add("42", "otra", url.Values{"keywords": {"kallax"}}, 2, now); !errors.Is(err, ErrAlreadyExists) {
		t.Fatalf("the same query was stored twice: %v", err)
	}
	if _, err := store.Add("42", "motos", url.Values{"brand": {"Yamaha"}}, 2, now); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Add("42", "bici", url.Values{"keywords": {"bici"}}, 2, now); !errors.Is(err, ErrTooMany) {
		t.Fatalf("the limit was not kept: %v", err)
	}

	if _, err := store.SetMuted("42", first.ID, true); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Delete("42", first.ID); err != nil {
		t.Fatal(err)
	}

	// What was written is what comes back.
	again, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	user, ok := again.Get("42")
	if !ok || len(user.Searches) != 1 || user.Searches[0].Name != "motos" {
		t.Fatalf("reloaded as %+v", user)
	}
	if got := user.Searches[0].Values().Get("brand"); got != "Yamaha" {
		t.Errorf("brand = %q", got)
	}
}

func TestSearchesOfOneChatAreNotAnother(t *testing.T) {
	store, _ := Load(t.TempDir())
	now := time.Now()
	_, _ = store.Request("1", "Ana", true, now)
	_, _ = store.Request("2", "Luis", true, now)
	search, _ := store.Add("1", "kallax", url.Values{"keywords": {"kallax"}}, 3, now)

	if _, err := store.Delete("2", search.ID); !errors.Is(err, ErrNoSuchSearch) {
		t.Fatalf("one chat deleted another's search: %v", err)
	}
	if _, err := store.SetMuted("2", search.ID, true); !errors.Is(err, ErrNoSuchSearch) {
		t.Fatalf("one chat silenced another's search: %v", err)
	}
}
