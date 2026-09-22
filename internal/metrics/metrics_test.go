package metrics

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestPushWritesTheExpositionFormat(t *testing.T) {
	var got, path, method string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw := make([]byte, r.ContentLength)
		_, _ = r.Body.Read(raw)
		got, path, method = string(raw), r.URL.Path, r.Method
	}))
	defer srv.Close()

	err := New(srv.URL+"/", "wallapop-manager").Push(context.Background(), []Gauge{
		{Name: "wallapop_last_run_status", Help: "0 fine", Value: 0},
		{Name: "wallapop_session_days_remaining", Help: "days", Value: 29.5},
	})
	if err != nil {
		t.Fatal(err)
	}
	if method != http.MethodPut {
		t.Errorf("pushed with %s, expected PUT so the group is replaced", method)
	}
	if path != "/metrics/job/wallapop-manager" {
		t.Errorf("pushed to %s", path)
	}
	for _, want := range []string{
		"# TYPE wallapop_last_run_status gauge",
		"wallapop_last_run_status 0",
		"wallapop_session_days_remaining 29.5",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("the body is missing %q:\n%s", want, got)
		}
	}
}

func TestPushWithoutAGatewayIsANoOp(t *testing.T) {
	if err := New("", "job").Push(context.Background(), []Gauge{{Name: "x"}}); err != nil {
		t.Fatalf("a disabled pusher must not fail: %v", err)
	}
}

func TestPushSurfacesARejection(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "text format parsing error", http.StatusBadRequest)
	}))
	defer srv.Close()

	if err := New(srv.URL, "job").Push(context.Background(), []Gauge{{Name: "x"}}); err == nil {
		t.Fatal("expected an error")
	}
}
