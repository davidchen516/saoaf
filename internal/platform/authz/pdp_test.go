package authz

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestEvaluateAllow(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"decision": true,
			"context":  map[string]any{"policy_decision_id": "decision:1"},
		})
	}))
	defer srv.Close()

	d, err := NewPDPClient(srv.URL).Evaluate(context.Background(), Evaluation{
		Subject: Entity{Type: "identity", ID: "user:alice"},
		Action:  Entity{ID: "resource.publish"},
	})
	if err != nil || !d.Allow || d.DecisionID != "decision:1" {
		t.Fatalf("d=%+v err=%v", d, err)
	}
}

// fail-closed 矩阵：deny / 非200 / 坏 body / 断连 / 超时全部 DENY。
func TestEvaluateFailsClosed(t *testing.T) {
	deny := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"decision": false})
	}))
	defer deny.Close()
	badStatus := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer badStatus.Close()
	badBody := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("<html>not json</html>"))
	}))
	defer badBody.Close()
	dead := "http://127.0.0.1:1" // closed port

	for name, url := range map[string]string{
		"explicit deny":   deny.URL,
		"pdp 500":         badStatus.URL,
		"malformed body":  badBody.URL,
		"pdp unreachable": dead,
	} {
		t.Run(name, func(t *testing.T) {
			d, err := NewPDPClient(url).Evaluate(context.Background(), Evaluation{Subject: Entity{ID: "x"}, Action: Entity{ID: "y"}})
			if err == nil {
				t.Fatal("evaluation unexpectedly succeeded")
			}
			if d.Allow {
				t.Fatal("fail-closed violated: Allow=true on error")
			}
		})
	}
}
