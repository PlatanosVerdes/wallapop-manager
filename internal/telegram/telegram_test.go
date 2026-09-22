package telegram

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestEnabled(t *testing.T) {
	if New("", "").Enabled() {
		t.Error("a bot with no token reported itself ready")
	}
	if New("token", "").Enabled() {
		t.Error("a bot with no chat reported itself ready")
	}
	if !New("token", "chat").Enabled() {
		t.Error("a configured bot reported itself unusable")
	}
}

// Telegram refuses a photo it cannot fetch. The listing still has to arrive.
func TestPhotoFallsBackToText(t *testing.T) {
	var called []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		called = append(called, r.URL.Path)
		if strings.HasSuffix(r.URL.Path, "sendPhoto") {
			http.Error(w, `{"ok":false,"description":"wrong file identifier"}`, http.StatusBadRequest)
			return
		}
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	bot := New("token", "chat")
	bot.APIURL = srv.URL + "/bot"
	if err := bot.Photo(context.Background(), "https://example.test/a.jpg", "un anuncio", nil); err != nil {
		t.Fatalf("the message did not get through: %v", err)
	}
	if len(called) != 2 || !strings.HasSuffix(called[1], "sendMessage") {
		t.Fatalf("calls were %v, expected the photo then the text", called)
	}
}

// A caption longer than Telegram's limit goes as a message, not as a rejected photo.
func TestLongCaptionSkipsThePhoto(t *testing.T) {
	var called []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = append(called, r.URL.Path)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	bot := New("token", "chat")
	bot.APIURL = srv.URL + "/bot"
	if err := bot.Photo(context.Background(), "https://example.test/a.jpg", strings.Repeat("x", captionLimit+1), nil); err != nil {
		t.Fatal(err)
	}
	if len(called) != 1 || !strings.HasSuffix(called[0], "sendMessage") {
		t.Fatalf("calls were %v, expected only the text", called)
	}
}

func TestNothingIsSentWithoutAToken(t *testing.T) {
	if err := New("", "").Photo(context.Background(), "x", "y", nil); err != nil {
		t.Errorf("an unconfigured bot returned an error instead of staying quiet: %v", err)
	}
}

func TestEscape(t *testing.T) {
	if got := Escape(`Kallax <b>barata</b> & buena`); got != "Kallax &lt;b&gt;barata&lt;/b&gt; &amp; buena" {
		t.Errorf("escaped to %q", got)
	}
}

// The token travels in the URL, so it must never reach a log line.
func TestErrorsDoNotCarryTheToken(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", http.StatusForbidden)
	}))
	defer srv.Close()

	bot := New("secret-token", "chat")
	bot.APIURL = srv.URL + "/bot"
	err := bot.Text(context.Background(), "hola", nil)
	if err == nil {
		t.Fatal("expected an error")
	}
	if strings.Contains(err.Error(), "secret-token") {
		t.Fatalf("the error carries the token: %v", err)
	}
}
