package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestLivenessAlwaysOK(t *testing.T) {
	h := NewHealth(nil)
	srv := httptest.NewServer(NewRouter(h))
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/healthz")
	if err != nil {
		t.Fatalf("healthz request: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("healthz status = %d, want 200", resp.StatusCode)
	}
	var body map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body["status"] != "alive" || body["probe"] != "healthz" {
		t.Fatalf("healthz body = %v", body)
	}
}

func TestReadinessFollowsReadyFunc(t *testing.T) {
	cases := []struct {
		name  string
		ready bool
		want  int
	}{
		{"ready", true, http.StatusOK},
		{"not-ready", false, http.StatusServiceUnavailable},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := NewHealth(func() bool { return tc.ready })
			srv := httptest.NewServer(NewRouter(h))
			defer srv.Close()

			resp, err := http.Get(srv.URL + "/readyz")
			if err != nil {
				t.Fatalf("readyz request: %v", err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != tc.want {
				t.Fatalf("readyz status = %d, want %d", resp.StatusCode, tc.want)
			}
		})
	}
}

func TestNotFoundEnvelope(t *testing.T) {
	h := NewHealth(nil)
	srv := httptest.NewServer(NewRouter(h))
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/no/such/route")
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
	var body map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body["error"] != "not_found" {
		t.Fatalf("error envelope = %v", body)
	}
}
