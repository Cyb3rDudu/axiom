package zotero

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// uploadServer fakes the Zotero local API for CreateAttachmentWithFile plus
// (optionally) a separate "foreign" upload target used to prove credentials
// never travel to a non-local host.
type uploadServer struct {
	t        *testing.T
	mu       sync.Mutex
	authForm map[string]string // authorize form fields (phase 1)
	regForm  map[string]string // register form fields (phase 3)
	upFile   string            // filename received in the multipart "file" part
	upParams map[string]string // leading (register-params) form fields on the upload
	authed   []string          // auth headers seen by the foreign upload target
	nUploads int
}

// phase switch: items POST -> authorize POST -> upload POST -> register POST.
func newUploadServer(t *testing.T, authorizeBody string, foreign *httptest.Server) (*uploadServer, *httptest.Server) {
	t.Helper()
	s := &uploadServer{t: t, authForm: map[string]string{}, regForm: map[string]string{}, upParams: map[string]string{}}
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/users/0/items":
			w.Write([]byte(`{"successful":{"0":{"key":"ATT1"}},"unchanged":{},"failed":{}}`))
		case r.Method == http.MethodPost && r.URL.Path == "/api/users/0/items/ATT1/file":
			// authorize and register share path + If-None-Match; the BODY
			// discriminates (register is exactly upload=<key>).
			b, _ := io.ReadAll(r.Body)
			body := string(b)
			if strings.HasPrefix(body, "upload=") {
				for _, kv := range strings.Split(body, "&") {
					p := strings.SplitN(kv, "=", 2)
					if len(p) == 2 {
						s.regForm[p[0]] = p[1]
					}
				}
				w.WriteHeader(http.StatusNoContent)
				return
			}
			for _, kv := range strings.Split(body, "&") {
				p := strings.SplitN(kv, "=", 2)
				if len(p) == 2 {
					s.authForm[p[0]] = p[1]
				}
			}
			if authorizeBody != "" {
				w.Header().Set("Content-Type", "application/json")
				w.Write([]byte(authorizeBody))
				return
			}
			upURL := srv.URL + "/api/users/0/uploads"
			if foreign != nil {
				upURL = foreign.URL + "/upload"
			}
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"url":%q,"uploadKey":"UK1","params":{"x-min-part-size":"1"}}`, upURL)
		case r.Method == http.MethodPost && r.URL.Path == "/api/users/0/uploads":
			s.serveUpload(w, r, true)
		case r.Method == http.MethodPost && r.URL.Path == "/upload":
			s.serveUpload(w, r, false) // only reached when foreign == nil (loopback to self)
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return s, srv
}

func (s *uploadServer) serveUpload(w http.ResponseWriter, r *http.Request, local bool) {
	if local {
		if got := r.Header.Get("Zotero-API-Key"); got != "wkey" {
			s.t.Errorf("local upload must carry the API key header, got %q", got)
		}
	} else {
		s.authed = append(s.authed, r.Header.Get("Zotero-API-Key"), r.Header.Get("Zotero-Server-ID"))
	}
	mr, err := r.MultipartReader()
	if err != nil {
		s.t.Errorf("upload body is not multipart: %v", err)
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	for {
		part, err := mr.NextPart()
		if err != nil {
			break
		}
		if part.FormName() == "file" {
			s.upFile = part.FileName()
			io.Copy(io.Discard, part)
		} else {
			b, _ := io.ReadAll(part)
			s.upParams[part.FormName()] = string(b)
		}
	}
	s.nUploads++
	w.WriteHeader(http.StatusCreated)
}

func newTestWriteClient(srv *httptest.Server) *WriteClient {
	return &WriteClient{BaseURL: srv.URL, ServerID: "sid", APIKey: "wkey",
		HTTP: srv.Client()}
}

// TestCreateAttachmentHappyPath pins OUR write contract: authorize form gets
// the plain-hex md5 + mtime in ms, the multipart upload carries the register
// params as leading fields and escapes the filename, register posts
// upload=<key> with If-None-Match, and the CREATED ITEM KEY is returned.
func TestCreateAttachmentHappyPath(t *testing.T) {
	s, srv := newUploadServer(t, "", nil)
	key, err := newTestWriteClient(srv).CreateAttachmentWithFile("", `he"ck.pdf`, []byte("PDFBYTES"))
	if err != nil {
		t.Fatalf("CreateAttachmentWithFile: %v", err)
	}
	if key != "ATT1" {
		t.Fatalf("returned key = %q, want the CREATED item key ATT1 (not the upload key)", key)
	}
	if s.nUploads != 1 {
		t.Fatalf("uploads = %d, want 1", s.nUploads)
	}
	md5hex := s.authForm["md5"]
	if len(md5hex) != 32 || strings.Trim(md5hex, "0123456789abcdef") != "" {
		t.Errorf("authorize md5 must be the PLAIN-HEX digest (32 hex chars), got %q", md5hex)
	}
	if s.authForm["filesize"] != "8" {
		t.Errorf("authorize filesize = %q, want 8", s.authForm["filesize"])
	}
	if s.authForm["filename"] == "" {
		t.Error("authorize form must carry the (urlencoded) filename")
	}
	if s.regForm["upload"] != "UK1" {
		t.Errorf("register must post upload=UK1, got %v", s.regForm)
	}
	if s.upParams["x-min-part-size"] != "1" {
		t.Errorf("register-response params must be forwarded as form fields, got %v", s.upParams)
	}
	if s.upFile != `he"ck.pdf` {
		t.Errorf("multipart filename round-trip failed: got %q, want %q", s.upFile, `he"ck.pdf`)
	}
}

// TestCreateAttachmentExistsObject: {"exists":1} ends the flow — no upload,
// no register, the created item key is returned.
func TestCreateAttachmentExistsObject(t *testing.T) {
	s, srv := newUploadServer(t, `{"exists":1}`, nil)
	key, err := newTestWriteClient(srv).CreateAttachmentWithFile("", "ok.pdf", []byte("X"))
	if err != nil {
		t.Fatalf("CreateAttachmentWithFile: %v", err)
	}
	if key != "ATT1" {
		t.Fatalf("key = %q, want ATT1", key)
	}
	if s.nUploads != 0 {
		t.Errorf("exists must skip the upload, got %d", s.nUploads)
	}
	if len(s.regForm) != 0 {
		t.Errorf("exists must skip the register, got %v", s.regForm)
	}
}

// TestCreateAttachmentGarbageAuthorize: an authorize response without a
// usable key ({url,uploadKey} / {exists:1} / bare quoted string) must fail
// LOUDLY, quoting the body — never post a garbage string as the upload key.
func TestCreateAttachmentGarbageAuthorize(t *testing.T) {
	_, srv := newUploadServer(t, `{"quota":0}`, nil)
	_, err := newTestWriteClient(srv).CreateAttachmentWithFile("", "ok.pdf", []byte("X"))
	if err == nil || !strings.Contains(err.Error(), `{"quota":0}`) {
		t.Fatalf("garbage authorize must fail loudly quoting the body, got err=%v", err)
	}
	if s, srv2 := newUploadServer(t, `[1,2]`, nil); true {
		_, err = newTestWriteClient(srv2).CreateAttachmentWithFile("", "ok.pdf", []byte("X"))
		if err == nil || !strings.Contains(err.Error(), "unerwartete Antwortform") {
			t.Fatalf("non-object authorize must hit the shape error, got err=%v", err)
		}
		_ = s
	}
}

// TestUploadCredentialsStayLocal: when the authorize response points at a
// DIFFERENT host, the upload request must not carry Zotero-API-Key or
// Zotero-Server-ID (pre-signed URLs must not receive credentials).
func TestUploadCredentialsStayLocal(t *testing.T) {
	foreign := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Zotero-API-Key") != "" || r.Header.Get("Zotero-Server-ID") != "" {
			t.Errorf("foreign-host upload leaked credentials: key=%q server-id=%q",
				r.Header.Get("Zotero-API-Key"), r.Header.Get("Zotero-Server-ID"))
		}
		w.WriteHeader(http.StatusCreated)
	}))
	defer foreign.Close()
	s, srv := newUploadServer(t, "", foreign)
	if _, err := newTestWriteClient(srv).CreateAttachmentWithFile("", "ok.pdf", []byte("X")); err != nil {
		t.Fatalf("CreateAttachmentWithFile: %v", err)
	}
	if s.nUploads != 0 {
		t.Fatalf("foreign server handled %d uploads; local must not receive one too", s.nUploads)
	}
}

// TestDeleteAttachmentItemVersionConflict: a 412 on the guarded DELETE must
// surface as IsVersionConflict (also through fmt.Errorf wrapping).
func TestDeleteAttachmentItemVersionConflict(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			w.Header().Set("Last-Modified-Version", "7")
			w.Write([]byte(`{}`))
			return
		}
		w.WriteHeader(http.StatusPreconditionFailed)
	}))
	defer srv.Close()
	err := newTestWriteClient(srv).DeleteAttachmentItem("K1")
	if err == nil {
		t.Fatal("expected 412 error")
	}
	if !IsVersionConflict(err) {
		t.Fatalf("IsVersionConflict(raw) = false, err=%v", err)
	}
	if !IsVersionConflict(fmt.Errorf("delete: %w", err)) {
		t.Fatal("IsVersionConflict must survive error wrapping (errors.As)")
	}
}

// TestAuthorizeKeyFlow pins the key-flow response shape {key, remember}.
func TestAuthorizeKeyFlow(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Zotero-Server-ID") != "sid" {
			t.Errorf("authorize needs Zotero-Server-ID, got %q", r.Header.Get("Zotero-Server-ID"))
		}
		w.Write([]byte(`{"key":"K","remember":true}`))
	}))
	defer srv.Close()
	key, remember, err := newTestWriteClient(srv).Authorize("axiom")
	if err != nil || key != "K" || !remember {
		t.Fatalf("Authorize = %q,%v,%v; want K,true,nil", key, remember, err)
	}
}
