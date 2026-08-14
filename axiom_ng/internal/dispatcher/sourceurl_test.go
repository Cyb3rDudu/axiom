package dispatcher

// Source-URL request-build tests: the dispatcher must attach a verifiable
// HMAC source_url when remote delivery is configured, build none without
// it, and stay not-processable only when NEITHER local_path nor delivery
// is available.

import (
	"encoding/json"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/sourceurl"
)

func srcFrozenInput(t *testing.T, localPath string) json.RawMessage {
	t.Helper()
	f := map[string]any{
		"contract_version": "1.0",
		"job_id":           "job-src-1",
		"idempotency_key":  "idem-1",
		"source":           map[string]any{"type": "zotero", "source_id": "s1", "server_id": "srv"},
		"document": map[string]any{
			"document_id": "d1", "zotero_key": "K1", "zotero_version": 1,
		},
		"attachment": map[string]any{
			"attachment_id": "a1", "zotero_key": "K1", "zotero_version": 1,
			"content_type": "application/pdf",
			"filename":     "book.pdf",
			"content_hash": "sha256:abc",
		},
		"processing": map[string]any{"profile": "full-rag-v1"},
	}
	if localPath != "" {
		f["attachment"].(map[string]any)["local_path"] = localPath
	}
	b, err := json.Marshal(f)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestBuildRequestSourceURLAttachedAndVerifiable(t *testing.T) {
	const secret = "topsecret"
	// +90s (not minutes): leaves the exp in the near past for any mutation
	// that replaces the lease source with a constant now+5min — a mutated
	// build would then fail the exp echo below.
	lease := time.Now().Add(90 * time.Second).Truncate(time.Second)
	req, err := buildRequest(srcFrozenInput(t, "/data/book.pdf"), SourceURLOptions{
		BaseURL:    "http://100.79.104.120:8011",
		Secret:     secret,
		LeaseUntil: lease,
	})
	if err != nil {
		t.Fatalf("buildRequest: %v", err)
	}
	u := req.Attachment.SourceURL
	if u == "" {
		t.Fatal("source_url must be attached when configured")
	}
	if !strings.HasPrefix(u, "http://100.79.104.120:8011/api/processor/source/job-src-1?") {
		t.Fatalf("source_url = %q, wrong base/path", u)
	}
	// exp and sig are deterministically verifiable with the shared secret.
	if !strings.Contains(u, "exp="+strconv.FormatInt(lease.Unix(), 10)) {
		t.Fatalf("source_url = %q, exp mismatch", u)
	}
	sig := extractQueryParam(t, u, "sig")
	if !sourceurl.Verify(secret, "job-src-1", lease.Unix(), sig) {
		t.Fatalf("sig %q does not verify", sig)
	}
	// local_path stays for local operation & fallback.
	if req.Attachment.LocalPath != "/data/book.pdf" {
		t.Fatalf("local_path = %q, must stay", req.Attachment.LocalPath)
	}
}

func TestBuildRequestNoSourceURLOptionsKeepsLocalOnly(t *testing.T) {
	req, err := buildRequest(srcFrozenInput(t, "/data/book.pdf"), SourceURLOptions{})
	if err != nil {
		t.Fatalf("buildRequest: %v", err)
	}
	if req.Attachment.SourceURL != "" {
		t.Fatalf("source_url = %q, want empty without options", req.Attachment.SourceURL)
	}
}

func TestBuildRequestRemoteWithoutLocalPathIsProcessable(t *testing.T) {
	// No local_path, but remote delivery configured => processable via
	// source_url (the runner pulls).
	req, err := buildRequest(srcFrozenInput(t, ""), SourceURLOptions{
		BaseURL:    "http://mac:8011",
		Secret:     "s",
		LeaseUntil: time.Now().Add(time.Minute),
	})
	if err != nil {
		t.Fatalf("buildRequest: %v (remote must make missing local_path ok)", err)
	}
	if req.Attachment.SourceURL == "" {
		t.Fatal("source_url must be attached")
	}
}

func TestBuildRequestNoLocalPathNoRemoteNotProcessable(t *testing.T) {
	_, err := buildRequest(srcFrozenInput(t, ""), SourceURLOptions{})
	if err == nil || !strings.Contains(err.Error(), "local_path") {
		t.Fatalf("err = %v, want not-processable local_path", err)
	}
}

func TestBuildRequestBaseWithoutSecretNotProcessable(t *testing.T) {
	// Half configuration (base URL set, secret empty) must NOT silently
	// produce an unusable request: without a secret no source_url is built,
	// so a missing local_path stays not-processable.
	_, err := buildRequest(srcFrozenInput(t, ""), SourceURLOptions{
		BaseURL: "http://mac:8011",
		Secret:  "",
	})
	if err == nil || !strings.Contains(err.Error(), "local_path") {
		t.Fatalf("err = %v, want not-processable local_path (base without secret)", err)
	}
}

func TestBuildURLTrimsTrailingSlash(t *testing.T) {
	u := sourceurl.BuildURL("http://mac:8011/", "job-1", 42, "sig")
	if strings.Contains(u, "//api/") {
		t.Fatalf("source_url = %q, doubled slash from trailing base slash", u)
	}
	if !strings.HasPrefix(u, "http://mac:8011/api/processor/source/job-1?") {
		t.Fatalf("source_url = %q, wrong shape", u)
	}
}

func extractQueryParam(t *testing.T, rawURL, key string) string {
	t.Helper()
	i := strings.Index(rawURL, key+"=")
	if i < 0 {
		t.Fatalf("query param %q missing in %q", key, rawURL)
	}
	rest := rawURL[i+len(key)+1:]
	if j := strings.Index(rest, "&"); j >= 0 {
		rest = rest[:j]
	}
	return rest
}
