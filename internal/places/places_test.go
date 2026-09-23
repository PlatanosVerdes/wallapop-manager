package places

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestFindTakesTownsOnly(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Query().Get("q") {
		case "sant cugat":
			_, _ = w.Write([]byte(`[{"name":"Sant Cugat del Vallès","class":"boundary","lat":"41.47","lon":"2.08","address":{"province":"Barcelona"}}]`))
		case "piel":
			_, _ = w.Write([]byte(`[{"name":"Piel","class":"shop","lat":"40.4","lon":"-3.7"}]`))
		default:
			_, _ = w.Write([]byte(`[]`))
		}
	}))
	defer server.Close()
	finder := New(server.URL)

	place, found, err := finder.Find(context.Background(), "sant cugat")
	if err != nil || !found || place.Name != "Sant Cugat del Vallès, Barcelona" || place.Latitude != 41.47 {
		t.Fatalf("Find = %+v, %v, %v", place, found, err)
	}
	for _, name := range []string{"piel", "nowhere"} {
		if _, found, err := finder.Find(context.Background(), name); found || err != nil {
			t.Errorf("Find(%q) = %v, %v", name, found, err)
		}
	}
}
