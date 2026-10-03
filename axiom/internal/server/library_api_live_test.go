// library_api_live_test.go — the F06 (#300) import HTTP contract against
// the LIVE dev environment (source-mode dev-up.sh with
// AXIOM_LIBRARY_IMPORT_PROVIDERS=fake; the release-mode golden baseline
// stays freeze-only). Gated like the baseline live probes.
//
//	AXIOM_LIBRARY_LIVE=1 go test ./internal/server -run TestLibraryLive
package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"testing"
	"time"
)

func libraryLiveBase() string {
	if v := os.Getenv("AXIOM_LIBRARY_LIVE_BASE"); v != "" {
		return v
	}
	return "http://127.0.0.1:8111"
}

func libraryLiveEnabled(t *testing.T) {
	t.Helper()
	if os.Getenv("AXIOM_LIBRARY_LIVE") != "1" {
		t.Skip("AXIOM_LIBRARY_LIVE != 1 — library live probes need the source-mode dev env (dev-up.sh)")
	}
}

// livePostImport issues the multipart intake against the live server.
func livePostImport(t *testing.T, reqJSON, filename string, content []byte) (int, map[string]any) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, err := mw.CreateFormFile("file", filename)
	if err != nil {
		t.Fatal(err)
	}
	fw.Write(content)
	mw.WriteField("request", reqJSON)
	mw.Close()
	resp, err := http.Post(libraryLiveBase()+"/api/v1/library/imports", mw.FormDataContentType(), &buf)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var body map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&body)
	return resp.StatusCode, body
}

func liveGetStatus(t *testing.T, importID string) map[string]any {
	t.Helper()
	resp, err := http.Get(libraryLiveBase() + "/api/v1/library/imports/" + importID)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var op map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&op)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status route = %d (%v)", resp.StatusCode, op)
	}
	return op
}

// TestLibraryLiveContract — 202 → committed status with provenance,
// replay 200-same, payload mismatch 409, magic-byte 400, confirm round.
func TestLibraryLiveContract(t *testing.T) {
	libraryLiveEnabled(t)
	stamp := fmt.Sprintf("%d", time.Now().UnixNano())

	// 1. Fresh intake: 202 + status_url; polls to committed with
	// per-field provenance.
	code, body := livePostImport(t, importReqJSON("live-"+stamp, "Totally Unknown Work", ""),
		"live.pdf", []byte("%PDF-1.4\nTotally Unknown Work live probe.\n%AXIOM-LANG: en\n\nBody."))
	if code != http.StatusAccepted {
		t.Fatalf("fresh intake = %d (%v)", code, body)
	}
	importID, _ := body["import_id"].(string)
	if body["status_url"] != "/api/v1/library/imports/"+importID {
		t.Fatalf("202 body malformed: %v", body)
	}
	deadline := time.Now().Add(15 * time.Second)
	var op map[string]any
	for {
		op = liveGetStatus(t, importID)
		if s, _ := op["status"].(string); s == "committed" || s == "terminal_failed" || s == "retryable_failed" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("import did not settle: %v", op)
		}
		time.Sleep(100 * time.Millisecond)
	}
	if op["status"] != "committed" {
		t.Fatalf("live import = %v (%v)", op["status"], op)
	}
	if fields, _ := op["fields"].([]any); len(fields) == 0 {
		t.Fatalf("committed status carries no per-field provenance: %v", op)
	}

	// 2. Replay: 200 with the SAME import.
	code, body = livePostImport(t, importReqJSON("live-"+stamp, "Totally Unknown Work", ""),
		"live.pdf", []byte("%PDF-1.4\nTotally Unknown Work live probe.\n%AXIOM-LANG: en\n\nBody."))
	if code != http.StatusOK {
		t.Fatalf("replay = %d, want 200", code)
	}
	if id, _ := body["import_id"].(string); id != importID {
		t.Fatalf("live replay diverged: %v vs %v", id, importID)
	}

	// 3. Payload mismatch: 409.
	code, _ = livePostImport(t, importReqJSON("live-"+stamp, "A Different Title", ""),
		"live.pdf", []byte("%PDF-1.4\nTotally Unknown Work live probe.\n%AXIOM-LANG: en\n\nBody."))
	if code != http.StatusConflict {
		t.Fatalf("payload mismatch = %d, want 409", code)
	}

	// 4. Magic bytes: renamed non-PDF → 400.
	code, _ = livePostImport(t, importReqJSON("live-magic-"+stamp, "Some Title", ""),
		"evil.pdf", []byte("MZ\x90\x00 not a rendition"))
	if code != http.StatusBadRequest {
		t.Fatalf("foreign magic bytes = %d, want 400", code)
	}

	// 5. Confirm round: the ambiguous fixture → awaiting → confirm →
	// committed with the CHOSEN bibliography.
	code, body = livePostImport(t, importReqJSON("live-amb-"+stamp, "Network Effects", ""),
		"amb.pdf", []byte("%PDF-1.4\nNetwork Effects live probe.\n%AXIOM-LANG: en\n\nBody."))
	if code != http.StatusAccepted {
		t.Fatalf("ambiguous intake = %d (%v)", code, body)
	}
	ambID, _ := body["import_id"].(string)
	deadline = time.Now().Add(15 * time.Second)
	for {
		op = liveGetStatus(t, ambID)
		if s, _ := op["status"].(string); s != "resolving_metadata" && s != "inspecting" && s != "received" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("ambiguous import did not settle: %v", op)
		}
		time.Sleep(100 * time.Millisecond)
	}
	if op["status"] != "awaiting_confirmation" {
		t.Fatalf("ambiguous live import = %v, want awaiting_confirmation", op["status"])
	}
	decisions, _ := op["decisions"].([]any)
	if len(decisions) != 1 {
		t.Fatalf("live decisions: %v", op["decisions"])
	}
	dec := decisions[0].(map[string]any)
	cands, _ := dec["candidates"].([]any)
	if len(cands) != 2 {
		t.Fatalf("live candidates: %v", dec["candidates"])
	}
	confirmReq, _ := json.Marshal(map[string]string{
		"decision_id":  dec["decision_id"].(string),
		"candidate_id": cands[0].(map[string]any)["candidate_id"].(string),
	})
	resp, err := http.Post(libraryLiveBase()+"/api/v1/library/imports/"+ambID+"/confirm", "application/json", bytes.NewReader(confirmReq))
	if err != nil {
		t.Fatal(err)
	}
	var confirmed map[string]any
	_ = json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&confirmed)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || confirmed["status"] != "committed" {
		t.Fatalf("live confirm = %d %v", resp.StatusCode, confirmed)
	}
}
