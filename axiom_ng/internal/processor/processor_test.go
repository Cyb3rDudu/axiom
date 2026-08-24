package processor

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// mustJSON writes v as a JSON response with the given status.
func mustJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func clientFor(t *testing.T, h http.Handler) *Client {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	c, err := New(Options{BaseURL: srv.URL, MaxBody: 1 << 20, ResultTimeout: 5 * time.Second})
	if err != nil {
		t.Fatalf("client: %v", err)
	}
	return c
}

func TestSubmitProcessRejectsWrongJobIDEcho(t *testing.T) {
	c := clientFor(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/process" {
			w.WriteHeader(404)
			return
		}
		mustJSON(w, 202, ProcessAccepted{ContractVersion: "1.0", JobID: "WRONG", Status: "accepted"})
	}))
	_, err := c.SubmitProcess(context.Background(), &ProcessRequest{ContractVersion: "1.0", JobID: "job-1", IdempotencyKey: "k"})
	if err == nil || !strings.Contains(err.Error(), "echoes job_id") {
		t.Fatalf("want job_id echo rejection, got %v", err)
	}
}

func TestSubmitProcessRejectsUnsupportedVersion(t *testing.T) {
	c := clientFor(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mustJSON(w, 202, ProcessAccepted{ContractVersion: "99.0", JobID: "job-1", Status: "accepted"})
	}))
	_, err := c.SubmitProcess(context.Background(), &ProcessRequest{ContractVersion: "1.0", JobID: "job-1", IdempotencyKey: "k"})
	if err == nil || !strings.Contains(err.Error(), "unsupported contract_version") {
		t.Fatalf("want version rejection, got %v", err)
	}
}

func TestSubmitProcessRejectsUnknownStatus(t *testing.T) {
	c := clientFor(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mustJSON(w, 202, ProcessAccepted{ContractVersion: "1.0", JobID: "job-1", Status: "bogus"})
	}))
	_, err := c.SubmitProcess(context.Background(), &ProcessRequest{ContractVersion: "1.0", JobID: "job-1", IdempotencyKey: "k"})
	if err == nil || !strings.Contains(err.Error(), "unknown acceptance status") {
		t.Fatalf("want unknown-status rejection, got %v", err)
	}
}

func TestJobStatusValidatesEchoAndStatus(t *testing.T) {
	// Valid status echoes fine.
	c := clientFor(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mustJSON(w, 200, JobStatus{ContractVersion: "1.0", JobID: "job-1", Status: "running"})
	}))
	st, err := c.JobStatus(context.Background(), "job-1")
	if err != nil {
		t.Fatalf("valid status failed: %v", err)
	}
	if st.Status != "running" {
		t.Fatalf("status = %q", st.Status)
	}

	// Wrong job_id echo is rejected.
	c2 := clientFor(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mustJSON(w, 200, JobStatus{ContractVersion: "1.0", JobID: "other", Status: "running"})
	}))
	if _, err := c2.JobStatus(context.Background(), "job-1"); err == nil {
		t.Fatal("want job_id echo rejection")
	}

	// Unknown status is rejected (would otherwise loop forever).
	c3 := clientFor(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mustJSON(w, 200, JobStatus{ContractVersion: "1.0", JobID: "job-1", Status: "wibble"})
	}))
	if _, err := c3.JobStatus(context.Background(), "job-1"); err == nil {
		t.Fatal("want unknown-status rejection")
	}
}

func TestStatusErrorMapping(t *testing.T) {
	c := clientFor(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":"boom"}`, http.StatusInternalServerError)
	}))
	err := c.Health(context.Background())
	if err == nil {
		t.Fatal("want error on 500")
	}
	se, ok := err.(*StatusError)
	if !ok {
		t.Fatalf("want *StatusError, got %T (%v)", err, err)
	}
	if se.Code != 500 {
		t.Fatalf("status code = %d, want 500", se.Code)
	}
}

func TestOversizedResponseRejected(t *testing.T) {
	big := strings.Repeat("x", (1<<20)+10000)
	c := clientFor(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"junk":"`))
		w.Write([]byte(big))
		w.Write([]byte(`"}`))
	}))
	// Do an untyped call so oversized-body detection fires (client max = 1MiB).
	err := c.Health(context.Background())
	if err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("want oversized-body rejection, got %v", err)
	}
}

// #216 honest readiness: a warmup-aware runner reports models_warmed (False
// while the BGE-M3/reranker preload is still in flight, True after). The RAG
// probe reads it to distinguish a genuinely-warm runner from one that only
// declares query_embedding/reranking. Older runners that omit the additive
// field decode to false — the honest cold default.
func TestCapabilitiesDecodesModelsWarmed(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		want bool
	}{
		{"warm", `{"models_warmed":true}`, true},
		{"cold/omitted defaults false", `{}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := clientFor(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mustJSON(w, 200, json.RawMessage(tc.body))
			}))
			caps, err := c.Capabilities(context.Background())
			if err != nil {
				t.Fatalf("capabilities: %v", err)
			}
			if caps.ModelsWarmed != tc.want {
				t.Fatalf("ModelsWarmed = %v, want %v", caps.ModelsWarmed, tc.want)
			}
		})
	}
}
