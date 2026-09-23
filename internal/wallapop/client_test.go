package wallapop

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

type fakeSession struct {
	access  string
	cookie  string
	expires time.Time
	updates int
}

func (f *fakeSession) AccessToken() string { return f.access }

func (f *fakeSession) SessionCookie() (string, string) {
	return "__Secure-next-auth.session-token", f.cookie
}

func (f *fakeSession) Update(access, cookie string, expires time.Time) error {
	f.access = access
	if cookie != "" {
		f.cookie = cookie
	}
	f.expires = expires
	f.updates++
	return nil
}

// One server stands in for both hosts: the API and the site that hands out sessions.
func newTestClient(url string, sess *fakeSession) *Client {
	client := New(sess)
	client.BaseURL = url
	client.WebURL = url
	client.DeviceID = "dev-1"
	return client
}

func TestMyItemsFollowsTheCursor(t *testing.T) {
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer tok" {
			t.Errorf("Authorization was %q", got)
		}
		if got := r.Header.Get("X-DeviceId"); got != "dev-1" {
			t.Errorf("X-DeviceId was %q", got)
		}
		if got := r.Header.Get("X-Signature"); got != "" {
			t.Errorf("nothing should be signed by default, got %q", got)
		}
		seen = append(seen, r.URL.Query().Get("next_page"))

		w.Header().Set("Content-Type", "application/json")
		if r.URL.Query().Get("next_page") == "" {
			w.Header().Set(HeaderNextPage, "page-2")
			fmt.Fprint(w, `{"data":[{"id":"a","title":"one","expired":{"flag":true}}],"meta":{}}`)
			return
		}
		fmt.Fprint(w, `{"data":[{"id":"b","title":"two"}],"meta":{}}`)
	}))
	defer srv.Close()

	items, err := newTestClient(srv.URL, &fakeSession{access: "tok"}).MyItems(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 2 {
		t.Fatalf("expected both pages, got %d items", len(items))
	}
	if len(seen) != 2 || seen[0] != "" || seen[1] != "page-2" {
		t.Fatalf("cursor was not followed: %#v", seen)
	}
}

func TestReactivateAcceptsNoContent(t *testing.T) {
	var method, path string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method, path = r.Method, r.URL.Path
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	if err := newTestClient(srv.URL, &fakeSession{access: "tok"}).Reactivate(context.Background(), "abc"); err != nil {
		t.Fatal(err)
	}
	if method != "PUT" || path != "/api/v3/items/abc/reactivate" {
		t.Fatalf("called %s %s", method, path)
	}
}

func TestSpentAccessTokenIsRenewedAndRetried(t *testing.T) {
	var calls []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, r.Method+" "+r.URL.Path)

		if r.URL.Path == PathSession {
			if got := r.Header.Get("Cookie"); got != "__Secure-next-auth.session-token=cookie-1" {
				t.Errorf("Cookie was %q", got)
			}
			http.SetCookie(w, &http.Cookie{Name: "__Secure-next-auth.session-token", Value: "cookie-2"})
			fmt.Fprint(w, `{"token":"fresh-access","expires":"2026-10-03T08:00:00.000Z"}`)
			return
		}

		if r.Header.Get("Authorization") != "Bearer fresh-access" {
			w.Header().Set(headerUnauthorized, reasonAccessExpired)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		fmt.Fprint(w, `{"data":[{"id":"a","title":"one"}],"meta":{}}`)
	}))
	defer srv.Close()

	sess := &fakeSession{access: "spent", cookie: "cookie-1"}
	items, err := newTestClient(srv.URL, sess).MyItems(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 {
		t.Fatalf("expected the retry to succeed, got %d items", len(items))
	}
	if sess.access != "fresh-access" || sess.cookie != "cookie-2" {
		t.Fatalf("the rolled cookie was not stored: %+v", sess)
	}
	if sess.expires.IsZero() {
		t.Error("the session deadline was not stored")
	}
	want := []string{"GET " + PathItems, "GET " + PathSession, "GET " + PathItems}
	if fmt.Sprint(calls) != fmt.Sprint(want) {
		t.Fatalf("calls were %v, expected %v", calls, want)
	}
}

func TestDeadRefreshTokenNeedsAHuman(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(headerUnauthorized, reasonRefreshExpired)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	_, err := newTestClient(srv.URL, &fakeSession{access: "spent", cookie: "dead"}).MyItems(context.Background())
	if !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("expected ErrUnauthorized, got %v", err)
	}
	if errors.Is(err, ErrAccessExpired) {
		t.Fatal("a dead refresh token must not look retryable")
	}
}

func TestNoCookieStored(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	_, err := newTestClient(srv.URL, &fakeSession{access: "spent"}).MyItems(context.Background())
	if !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("expected ErrUnauthorized, got %v", err)
	}
}

// A revoked cookie gets a 200 with an empty session, not an error.
func TestEmptySessionAnswerNeedsAHuman(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == PathSession {
			fmt.Fprint(w, `{}`)
			return
		}
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	err := newTestClient(srv.URL, &fakeSession{cookie: "revoked"}).RenewSession(context.Background())
	if !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("expected ErrUnauthorized, got %v", err)
	}
}
