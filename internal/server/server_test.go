package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/PlatanosVerdes/wallapop-manager/internal/reactivate"
	"github.com/PlatanosVerdes/wallapop-manager/internal/session"
)

func get(t *testing.T, h *Health) (int, map[string]any) {
	t.Helper()
	rec := httptest.NewRecorder()
	h.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))

	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("the body is not JSON: %v (%s)", err, rec.Body.String())
	}
	return rec.Code, body
}

func TestHealthAnswers200WithNoSession(t *testing.T) {
	dir := t.TempDir()
	code, body := get(t, &Health{Version: "test", DataDir: dir, Store: session.NewStore(dir)})

	if code != http.StatusOK {
		t.Fatalf("answered %d, expected 200 while the process is alive", code)
	}
	if body["status"] != "down" {
		t.Errorf("status is %q, expected down", body["status"])
	}
}

func TestHealthReportsTheSession(t *testing.T) {
	dir := t.TempDir()
	store := session.NewStore(dir)
	sess := session.New("a.b.c.d.e")
	sess.Expires = time.Now().Add(20 * 24 * time.Hour)
	if err := store.Save(sess); err != nil {
		t.Fatal(err)
	}

	code, body := get(t, &Health{Version: "test", DataDir: dir, Store: store, WarnBefore: 72 * time.Hour})
	if code != http.StatusOK {
		t.Fatalf("answered %d", code)
	}
	if body["status"] != "ok" {
		t.Errorf("status is %q, expected ok", body["status"])
	}
	if days, _ := body["renewable_days_left"].(float64); days < 19 || days > 20 {
		t.Errorf("renewable_days_left is %v, expected about 20", body["renewable_days_left"])
	}
}

func TestHealthWarnsOnAnExpiringSession(t *testing.T) {
	dir := t.TempDir()
	store := session.NewStore(dir)
	sess := session.New("a.b.c.d.e")
	sess.Expires = time.Now().Add(12 * time.Hour)
	if err := store.Save(sess); err != nil {
		t.Fatal(err)
	}

	_, body := get(t, &Health{DataDir: dir, Store: store, WarnBefore: 72 * time.Hour})
	if body["status"] != "warn" {
		t.Errorf("status is %q, expected warn", body["status"])
	}
}

func TestHealthKeepsTheSpecificReason(t *testing.T) {
	dir := t.TempDir()
	if err := reactivate.SaveResult(dir, reactivate.Result{Error: "boom", NeedsHuman: true}); err != nil {
		t.Fatal(err)
	}

	_, body := get(t, &Health{DataDir: dir, Store: session.NewStore(dir)})
	if body["status"] != "down" {
		t.Errorf("status is %q, expected down", body["status"])
	}
	if got, _ := body["session"].(string); !strings.Contains(got, "no session imported") {
		t.Errorf("session reads %q, expected the session's own reason", got)
	}
}

func TestHealthReportsAPassThatNeedsAHuman(t *testing.T) {
	dir := t.TempDir()
	store := session.NewStore(dir)
	sess := session.New("a.b.c.d.e")
	sess.Expires = time.Now().Add(20 * 24 * time.Hour)
	if err := store.Save(sess); err != nil {
		t.Fatal(err)
	}
	if err := reactivate.SaveResult(dir, reactivate.Result{Error: "rejected", NeedsHuman: true}); err != nil {
		t.Fatal(err)
	}

	_, body := get(t, &Health{DataDir: dir, Store: store, WarnBefore: 72 * time.Hour})
	if body["status"] != "down" {
		t.Errorf("status is %q, expected down", body["status"])
	}
	if got, _ := body["session"].(string); !strings.Contains(got, "rejected") {
		t.Errorf("session reads %q, expected the pass error", got)
	}
}

func TestHealthCarriesTheLastRun(t *testing.T) {
	dir := t.TempDir()
	store := session.NewStore(dir)
	if err := store.Save(session.New("a.b.c.d.e")); err != nil {
		t.Fatal(err)
	}
	if err := reactivate.SaveResult(dir, reactivate.Result{Catalogue: 11, Expired: 9}); err != nil {
		t.Fatal(err)
	}

	_, body := get(t, &Health{DataDir: dir, Store: store})
	last, ok := body["last_run"].(map[string]any)
	if !ok {
		t.Fatalf("no last_run in %v", body)
	}
	if last["catalogue"] != 11.0 || last["expired"] != 9.0 {
		t.Errorf("last_run came back as %v", last)
	}
}
