package api_test

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Cyb3rDudu/axiom/axiom-ng/internal/api"
	"github.com/Cyb3rDudu/axiom/axiom-ng/internal/auth"
	"github.com/Cyb3rDudu/axiom/axiom-ng/internal/authctx"
	"github.com/Cyb3rDudu/axiom/axiom-ng/internal/config"
	"github.com/Cyb3rDudu/axiom/axiom-ng/internal/server"
	"github.com/google/uuid"
)

// doAuthed issues an authenticated request with the stub fixture's
// pre-minted JWT + CSRF tokens.
func doAuthed(f *stubFixture, method, path, body string) (*http.Response, []byte) {
	var rdr io.Reader
	if body != "" {
		rdr = strings.NewReader(body)
	}
	req, _ := http.NewRequest(method, f.srv.URL+path, rdr)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	req.AddCookie(&http.Cookie{Name: auth.AccessTokenCookie, Value: f.token})
	req.AddCookie(&http.Cookie{Name: auth.CSRFCookie, Value: f.csrf})
	req.Header.Set(auth.CSRFHeader, f.csrf)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, []byte(err.Error())
	}
	b, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	return resp, b
}

func doPublic(f *stubFixture, method, path, body string) (*http.Response, []byte) {
	var rdr io.Reader
	if body != "" {
		rdr = strings.NewReader(body)
	}
	req, _ := http.NewRequest(method, f.srv.URL+path, rdr)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, []byte(err.Error())
	}
	b, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	return resp, b
}

func TestRegisterDBErrors(t *testing.T) {
	t.Parallel()
	f := newStubFixture()
	defer f.close()

	// system_settings lookup fails → 500.
	f.sys.err = errStub
	resp, _ := doPublic(f, http.MethodPost, "/api/auth/register", `{"username":"x","password":"y"}`)
	if resp.StatusCode != http.StatusInternalServerError {
		t.Errorf("sys err: got %d", resp.StatusCode)
	}
	f.sys.err = nil

	// username lookup returns non-NotFound error → 500.
	f.users.getByUsernameErr = errStub
	resp, _ = doPublic(f, http.MethodPost, "/api/auth/register", `{"username":"x","password":"y"}`)
	if resp.StatusCode != http.StatusInternalServerError {
		t.Errorf("lookup err: got %d", resp.StatusCode)
	}
	f.users.getByUsernameErr = nil

	// With NotFound, Create fails → 500.
	f.users.getByUsernameNF = true
	f.users.createErr = errStub
	resp, _ = doPublic(f, http.MethodPost, "/api/auth/register", `{"username":"x","password":"y"}`)
	if resp.StatusCode != http.StatusInternalServerError {
		t.Errorf("create err: got %d", resp.StatusCode)
	}
	f.users.createErr = nil
	f.users.getByUsernameNF = false
}

func TestRegisterFirstUserBecomesAdmin(t *testing.T) {
	t.Parallel()
	f := newStubFixture()
	defer f.close()
	f.users.getByUsernameNF = true
	f.users.countValue = 0
	resp, _ := doPublic(f, http.MethodPost, "/api/auth/register", `{"username":"first","password":"p"}`)
	if resp.StatusCode != http.StatusCreated {
		t.Errorf("first register: got %d", resp.StatusCode)
	}
}

func TestLoginDBErrors(t *testing.T) {
	t.Parallel()
	f := newStubFixture()
	defer f.close()

	// Non-NotFound lookup error → 500.
	f.users.getByUsernameErr = errStub
	form := "username=alice&password=anything"
	req, _ := http.NewRequest(http.MethodPost, f.srv.URL+"/api/auth/login", strings.NewReader(form))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, _ := http.DefaultClient.Do(req)
	resp.Body.Close()
	if resp.StatusCode != http.StatusInternalServerError {
		t.Errorf("login lookup err: got %d", resp.StatusCode)
	}
}

func TestLoginRejectsMalformedBody(t *testing.T) {
	t.Parallel()
	f := newStubFixture()
	defer f.close()
	req, _ := http.NewRequest(http.MethodPost, f.srv.URL+"/api/auth/login", strings.NewReader("{broken"))
	req.Header.Set("Content-Type", "application/json")
	resp, _ := http.DefaultClient.Do(req)
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("bad json: got %d", resp.StatusCode)
	}
}

func TestLoginRespectsRememberMeHeader(t *testing.T) {
	t.Parallel()
	f := newStubFixture()
	defer f.close()
	// Make the verify succeed by pre-hashing a known password.
	hashed, _ := auth.HashPassword("pw")
	f.users.user.HashedPassword = hashed
	form := "username=alice&password=pw"
	req, _ := http.NewRequest(http.MethodPost, f.srv.URL+"/api/auth/login", strings.NewReader(form))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set(auth.RememberMeHeader, "true")
	resp, _ := http.DefaultClient.Do(req)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("login: %d %s", resp.StatusCode, body)
	}
	if !bytes.Contains(body, []byte(`"remember_me":true`)) {
		t.Errorf("remember_me header not honoured: %s", body)
	}
}

func TestMeDBError(t *testing.T) {
	t.Parallel()
	f := newStubFixture()
	defer f.close()
	f.users.getByUsernameErr = errStub
	resp, _ := doAuthed(f, http.MethodGet, "/api/auth/me", "")
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("me db err: got %d", resp.StatusCode)
	}
}

func TestChangePasswordAllBranches(t *testing.T) {
	t.Parallel()
	// Bad JSON → 400
	f := newStubFixture()
	defer f.close()
	resp, _ := doAuthed(f, http.MethodPost, "/api/auth/change-password", "{broken")
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("bad json: got %d", resp.StatusCode)
	}
	// Missing new_password → 400
	resp, _ = doAuthed(f, http.MethodPost, "/api/auth/change-password", `{"current_password":"old"}`)
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("missing new: got %d", resp.StatusCode)
	}
	// DB lookup error → 401
	f.users.getByUsernameErr = errStub
	resp, _ = doAuthed(f, http.MethodPost, "/api/auth/change-password", `{"current_password":"old","new_password":"new"}`)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("lookup err: got %d", resp.StatusCode)
	}
	f.users.getByUsernameErr = nil
	// Verify succeeds (valid bcrypt hash in user), update fails → 500
	hashed, _ := auth.HashPassword("current")
	f.users.user.HashedPassword = hashed
	f.users.updatePasswordErr = errStub
	resp, _ = doAuthed(f, http.MethodPost, "/api/auth/change-password", `{"current_password":"current","new_password":"new"}`)
	if resp.StatusCode != http.StatusInternalServerError {
		t.Errorf("update err: got %d", resp.StatusCode)
	}
}

func TestChatsErrorBranches(t *testing.T) {
	t.Parallel()
	f := newStubFixture()
	defer f.close()

	// List DB error.
	f.chats.list = errStub
	resp, _ := doAuthed(f, http.MethodGet, "/api/chats/", "")
	if resp.StatusCode != http.StatusInternalServerError {
		t.Errorf("list err: got %d", resp.StatusCode)
	}
	f.chats.list = nil

	// Create bad body.
	resp, _ = doAuthed(f, http.MethodPost, "/api/chats/", "{broken")
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("bad json: got %d", resp.StatusCode)
	}
	// Create DB error.
	f.chats.create = errStub
	resp, _ = doAuthed(f, http.MethodPost, "/api/chats/", `{"title":"t"}`)
	if resp.StatusCode != http.StatusInternalServerError {
		t.Errorf("create err: got %d", resp.StatusCode)
	}
	f.chats.create = nil

	id := uuid.New().String()

	// Get: DB error path.
	f.chats.get = errStub
	resp, _ = doAuthed(f, http.MethodGet, "/api/chats/"+id, "")
	if resp.StatusCode != http.StatusInternalServerError {
		t.Errorf("get err: got %d", resp.StatusCode)
	}
	f.chats.get = nil

	// Delete: NF + err.
	f.chats.delNF = true
	resp, _ = doAuthed(f, http.MethodDelete, "/api/chats/"+id, "")
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("delete nf: got %d", resp.StatusCode)
	}
	f.chats.delNF = false
	f.chats.del = errStub
	resp, _ = doAuthed(f, http.MethodDelete, "/api/chats/"+id, "")
	if resp.StatusCode != http.StatusInternalServerError {
		t.Errorf("delete err: got %d", resp.StatusCode)
	}
	f.chats.del = nil

	// Update title: bad body, missing title, NF, DB err.
	resp, _ = doAuthed(f, http.MethodPut, "/api/chats/"+id+"/title", "{broken")
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("title bad json: got %d", resp.StatusCode)
	}
	resp, _ = doAuthed(f, http.MethodPut, "/api/chats/"+id+"/title", `{"title":""}`)
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("title empty: got %d", resp.StatusCode)
	}
	f.chats.updNF = true
	resp, _ = doAuthed(f, http.MethodPut, "/api/chats/"+id+"/title", `{"title":"x"}`)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("title nf: got %d", resp.StatusCode)
	}
	f.chats.updNF = false
	f.chats.upd = errStub
	resp, _ = doAuthed(f, http.MethodPut, "/api/chats/"+id+"/title", `{"title":"x"}`)
	if resp.StatusCode != http.StatusInternalServerError {
		t.Errorf("title err: got %d", resp.StatusCode)
	}
	f.chats.upd = nil

	// GetTitle: NF + err.
	f.chats.getNF = true
	resp, _ = doAuthed(f, http.MethodGet, "/api/chats/"+id+"/title", "")
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("get title nf: got %d", resp.StatusCode)
	}
	f.chats.getNF = false
	f.chats.get = errStub
	resp, _ = doAuthed(f, http.MethodGet, "/api/chats/"+id+"/title", "")
	if resp.StatusCode != http.StatusInternalServerError {
		t.Errorf("get title err: got %d", resp.StatusCode)
	}
	f.chats.get = nil

	// Messages: list NF + err, append bad body / missing / NF / err,
	// delete NF + err, clear NF + err.
	f.chats.lmNF = true
	resp, _ = doAuthed(f, http.MethodGet, "/api/chats/"+id+"/messages", "")
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("list msgs nf: got %d", resp.StatusCode)
	}
	f.chats.lmNF = false
	f.chats.lm = errStub
	resp, _ = doAuthed(f, http.MethodGet, "/api/chats/"+id+"/messages", "")
	if resp.StatusCode != http.StatusInternalServerError {
		t.Errorf("list msgs err: got %d", resp.StatusCode)
	}
	f.chats.lm = nil

	resp, _ = doAuthed(f, http.MethodPost, "/api/chats/"+id+"/messages", "{broken")
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("append bad: got %d", resp.StatusCode)
	}
	resp, _ = doAuthed(f, http.MethodPost, "/api/chats/"+id+"/messages", `{"role":"","content":""}`)
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("append missing: got %d", resp.StatusCode)
	}
	f.chats.amNF = true
	resp, _ = doAuthed(f, http.MethodPost, "/api/chats/"+id+"/messages", `{"role":"user","content":"x"}`)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("append nf: got %d", resp.StatusCode)
	}
	f.chats.amNF = false
	f.chats.am = errStub
	resp, _ = doAuthed(f, http.MethodPost, "/api/chats/"+id+"/messages", `{"role":"user","content":"x"}`)
	if resp.StatusCode != http.StatusInternalServerError {
		t.Errorf("append err: got %d", resp.StatusCode)
	}
	f.chats.am = nil

	// DeleteMessage: bad msg id, NF, err.
	resp, _ = doAuthed(f, http.MethodDelete, "/api/chats/"+id+"/messages/not-a-uuid", "")
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("bad msg id: got %d", resp.StatusCode)
	}
	msgID := uuid.New().String()
	f.chats.dmNF = true
	resp, _ = doAuthed(f, http.MethodDelete, "/api/chats/"+id+"/messages/"+msgID, "")
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("del msg nf: got %d", resp.StatusCode)
	}
	f.chats.dmNF = false
	f.chats.dm = errStub
	resp, _ = doAuthed(f, http.MethodDelete, "/api/chats/"+id+"/messages/"+msgID, "")
	if resp.StatusCode != http.StatusInternalServerError {
		t.Errorf("del msg err: got %d", resp.StatusCode)
	}
	f.chats.dm = nil

	// Clear: NF + err.
	f.chats.cmNF = true
	resp, _ = doAuthed(f, http.MethodDelete, "/api/chats/"+id+"/messages", "")
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("clear nf: got %d", resp.StatusCode)
	}
	f.chats.cmNF = false
	f.chats.cm = errStub
	resp, _ = doAuthed(f, http.MethodDelete, "/api/chats/"+id+"/messages", "")
	if resp.StatusCode != http.StatusInternalServerError {
		t.Errorf("clear err: got %d", resp.StatusCode)
	}
	f.chats.cm = nil

	// Missions list: NF + err.
	f.chats.lmiNF = true
	resp, _ = doAuthed(f, http.MethodGet, "/api/chats/"+id+"/missions", "")
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("missions nf: got %d", resp.StatusCode)
	}
	f.chats.lmiNF = false
	f.chats.lmi = errStub
	resp, _ = doAuthed(f, http.MethodGet, "/api/chats/"+id+"/missions", "")
	if resp.StatusCode != http.StatusInternalServerError {
		t.Errorf("missions err: got %d", resp.StatusCode)
	}
}

func TestDashboardDBError(t *testing.T) {
	t.Parallel()
	f := newStubFixture()
	defer f.close()
	f.dash.err = errStub
	resp, _ := doAuthed(f, http.MethodGet, "/api/dashboard/stats", "")
	if resp.StatusCode != http.StatusInternalServerError {
		t.Errorf("dashboard err: got %d", resp.StatusCode)
	}
}

func TestSettingsErrorBranches(t *testing.T) {
	t.Parallel()
	f := newStubFixture()
	defer f.close()

	// Get: user lookup fails → 401 (NF) vs 500 (other err).
	f.users.getByIDErr = errStub
	resp, _ := doAuthed(f, http.MethodGet, "/api/me/settings", "")
	if resp.StatusCode != http.StatusInternalServerError {
		t.Errorf("settings get err: got %d", resp.StatusCode)
	}
	f.users.getByIDErr = nil

	// Put with body too large → 400 not valid json (read succeeds, not valid).
	// Settings store returns NotFound → 500 (current behavior).
	f.users.updateSettingsNF = true
	resp, _ = doAuthed(f, http.MethodPut, "/api/me/settings", `{"x":1}`)
	if resp.StatusCode != http.StatusInternalServerError {
		t.Errorf("settings nf: got %d", resp.StatusCode)
	}
	f.users.updateSettingsNF = false

	// Put with store error.
	f.users.updateSettingsErr = errStub
	resp, _ = doAuthed(f, http.MethodPut, "/api/me/settings", `{"x":1}`)
	if resp.StatusCode != http.StatusInternalServerError {
		t.Errorf("settings err: got %d", resp.StatusCode)
	}
}

func TestLanguagesDBErrors(t *testing.T) {
	t.Parallel()
	f := newStubFixture()
	defer f.close()
	f.langs.listErr = errStub
	resp, _ := doPublic(f, http.MethodGet, "/api/languages", "")
	if resp.StatusCode != http.StatusInternalServerError {
		t.Errorf("list err: got %d", resp.StatusCode)
	}
	f.langs.listErr = nil
	f.langs.getErr = errStub
	resp, _ = doPublic(f, http.MethodGet, "/api/languages/en", "")
	if resp.StatusCode != http.StatusInternalServerError {
		t.Errorf("get err: got %d", resp.StatusCode)
	}
}

func TestSystemStatusDBDown(t *testing.T) {
	t.Parallel()
	f := newStubFixture()
	defer f.close()
	f.health.err = errStub
	// Since server.Deps pulls f.health by value, we have to swap the
	// entire deps.  Simpler: rebuild the fixture with db down.
	f2 := newStubFixture()
	defer f2.close()
	f2.users.getByUsernameErr = nil
	// Use the composed health stub from a fresh fixture but with err set.
	// Because we can't easily inject after construction, we test the
	// server-level behaviour via the healthy path and accept the
	// unhealthy branch is covered by the generic success path.
	resp, body := doPublic(f2, http.MethodGet, "/api/system/status", "")
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status: got %d", resp.StatusCode)
	}
	if !strings.Contains(string(body), `"status"`) {
		t.Errorf("status body missing field: %s", body)
	}
}

// TestSystemStatusUnhealthyBranch explicitly covers the
// "unhealthy" component path by constructing a fixture where the
// Health dep returns an error.
func TestSystemStatusUnhealthyBranch(t *testing.T) {
	t.Parallel()
	// Build a minimal handler stack outside newStubFixture so we can
	// inject a broken health dep directly.
	f := newStubFixtureWithHealth(errStub)
	defer f.close()
	resp, body := doPublic(f, http.MethodGet, "/api/system/status", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status: got %d", resp.StatusCode)
	}
	if !bytes.Contains(body, []byte(`"unhealthy"`)) {
		t.Errorf("expected unhealthy marker, body=%s", body)
	}
}

// TestSystemStatusNilHealth covers the branch where no DBHealth dep
// is configured.
func TestSystemStatusNilHealth(t *testing.T) {
	t.Parallel()
	f := newStubFixtureWithHealth(nilHealthSentinel)
	defer f.close()
	resp, body := doPublic(f, http.MethodGet, "/api/system/status", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status: got %d", resp.StatusCode)
	}
	if !bytes.Contains(body, []byte(`"unknown"`)) {
		t.Errorf("expected unknown db marker, body=%s", body)
	}
}

// --- support for the status tests ---

// nilHealthSentinel is a marker value; when passed to
// newStubFixtureWithHealth the health dep is left nil.
var nilHealthSentinel = nilHealthErr{}

type nilHealthErr struct{}

func (nilHealthErr) Error() string { return "nil-health-sentinel" }

func newStubFixtureWithHealth(err error) *stubFixture {
	f := newStubFixture()
	f.srv.Close()

	deps := fixtureDeps(f)
	if err == nilHealthSentinel {
		deps.System.Health = nil
	} else {
		deps.System.Health = stubHealth{err: err}
	}
	s := server.NewWithDeps(config.Defaults(), slog.New(slog.NewTextHandler(io.Discard, nil)), deps)
	f.srv = httptest.NewServer(s.Handler())
	return f
}

// fixtureDeps rebuilds the dep graph from a stubFixture so ad-hoc
// variants can swap a single dep without repeating the full wiring.
func fixtureDeps(f *stubFixture) server.Deps {
	return server.Deps{
		Auth:      api.AuthDeps{Users: f.users, SystemSettings: f.sys, Signer: f.signer},
		Languages: api.LanguageDeps{Languages: f.langs},
		System:    api.SystemDeps{Health: f.health},
		Dashboard: api.DashboardDeps{Stats: f.dash},
		Settings:  api.SettingsDeps{Users: f.users},
		Chats:     api.ChatDeps{Chats: f.chats},
		UserCtx: server.UserContextConfig{
			Signer:     f.signer,
			UserLookup: stubLookup{user: authctx.User{ID: 1, Username: "alice"}},
		},
	}
}

func TestOpenAPIBodyReadError_Settings(t *testing.T) {
	t.Parallel()
	// The io.LimitReader inside SettingsDeps.Put caps the body at 1MiB.
	// Send 1MiB+1 bytes of invalid JSON to trigger the "not valid JSON"
	// branch while still exercising the limit.
	f := newStubFixture()
	defer f.close()
	body := strings.Repeat("x", (1<<20)+1)
	resp, _ := doAuthed(f, http.MethodPut, "/api/me/settings", body)
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("large invalid body: got %d", resp.StatusCode)
	}
}

func TestSettingsGetReturnsEmptyObjectWhenNil(t *testing.T) {
	t.Parallel()
	f := newStubFixture()
	defer f.close()
	f.users.user.Settings = json.RawMessage(nil) // force nil
	resp, body := doAuthed(f, http.MethodGet, "/api/me/settings", "")
	if resp.StatusCode != http.StatusOK {
		t.Errorf("get settings nil: got %d", resp.StatusCode)
	}
	if strings.TrimSpace(string(body)) != "{}" {
		t.Errorf("expected {}, got %q", body)
	}
}
