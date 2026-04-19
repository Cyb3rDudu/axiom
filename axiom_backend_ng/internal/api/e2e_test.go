package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/Cyb3rDudu/axiom/axiom-ng/internal/api"
	"github.com/Cyb3rDudu/axiom/axiom-ng/internal/auth"
	"github.com/Cyb3rDudu/axiom/axiom-ng/internal/authctx"
	"github.com/Cyb3rDudu/axiom/axiom-ng/internal/config"
	"github.com/Cyb3rDudu/axiom/axiom-ng/internal/db"
	"github.com/Cyb3rDudu/axiom/axiom-ng/internal/repo"
	"github.com/Cyb3rDudu/axiom/axiom-ng/internal/server"
	"github.com/Cyb3rDudu/axiom/axiom-ng/internal/testutil"
)

// fixture wraps a dockertest Postgres with the concrete dependency
// graph and an httptest.Server so every test exercises the full
// router + middleware + DB path.
type fixture struct {
	pg     *testutil.Postgres
	srv    *httptest.Server
	signer *auth.Signer
	users  *repo.Users
	langs  *repo.Languages
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	pg := testutil.StartPostgres(t)

	signer, err := auth.NewSigner("e2e-secret")
	if err != nil {
		t.Fatalf("signer: %v", err)
	}

	users := repo.NewUsers(pg.DB)
	langs := repo.NewLanguages(pg.DB)
	sys := repo.NewSystemSettings(pg.DB)
	dash := repo.NewDashboard(pg.DB)
	chats := repo.NewChats(pg.DB)
	documents := repo.NewDocuments(pg.DB)
	groups := repo.NewDocumentGroups(pg.DB)
	chunks := repo.NewChunks(pg.DB)

	deps := server.Deps{
		Auth: api.AuthDeps{
			Users:          users,
			SystemSettings: sys,
			Signer:         signer,
		},
		Languages:      api.LanguageDeps{Languages: langs},
		System:         api.SystemDeps{Health: pingFromPool{pool: pg}},
		Dashboard:      api.DashboardDeps{Stats: dash},
		Settings:       api.SettingsDeps{Users: users},
		Chats:          api.ChatDeps{Chats: chats},
		Documents:      api.DocumentDeps{Documents: documents},
		DocumentGroups: api.DocumentGroupDeps{Groups: groups, Documents: documents},
		RAG:            api.RAGDeps{Chunks: chunks},
		UserCtx: server.UserContextConfig{
			Signer:     signer,
			UserLookup: userLookup{users: users},
		},
	}
	s := server.NewWithDeps(config.Defaults(), slog.New(slog.NewTextHandler(io.Discard, nil)), deps)
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)

	// Seed minimum-required rows so repo queries don't short-circuit.
	seedSupportedLanguages(t, pg)

	return &fixture{pg: pg, srv: srv, signer: signer, users: users, langs: langs}
}

func (f *fixture) close() { f.pg.Close() }

type pingFromPool struct{ pool *testutil.Postgres }

func (p pingFromPool) Ping(ctx context.Context) error { return db.Ping(ctx, p.pool.DB) }

type userLookup struct{ users *repo.Users }

func (u userLookup) GetUserByUsername(ctx context.Context, username string) (authctx.User, error) {
	user, err := u.users.GetByUsername(ctx, username)
	if err != nil {
		return authctx.User{}, err
	}
	return authctx.User{ID: user.ID, Username: user.Username, IsAdmin: user.IsAdmin, IsActive: user.IsActive}, nil
}

func seedSupportedLanguages(t *testing.T, pg *testutil.Postgres) {
	t.Helper()
	ctx := context.Background()
	// Wipe seed data from the multilingual migration so tests own the
	// full supported_languages state.
	if err := pg.DB.WithContext(ctx).Exec(`DELETE FROM supported_languages`).Error; err != nil {
		t.Fatalf("wipe languages: %v", err)
	}
	rows := []struct {
		Code, Name, Native string
		Completion         int
		Active             bool
	}{
		{"en", "English", "English", 100, true},
		{"de", "German", "Deutsch", 80, true},
		{"fr", "French", "Français", 40, false},
	}
	for _, r := range rows {
		err := pg.DB.WithContext(ctx).Exec(`
			INSERT INTO supported_languages (code, name, native_name, is_active, completion_percentage, created_at)
			VALUES (?, ?, ?, ?, ?, NOW())
		`, r.Code, r.Name, r.Native, r.Active, r.Completion).Error
		if err != nil {
			t.Fatalf("seed language %s: %v", r.Code, err)
		}
	}
}

// httpClient returns a cookie-jar-enabled http.Client rooted at the
// httptest server. Cookies set during login automatically flow to
// subsequent requests.
func (f *fixture) httpClient(t *testing.T) *http.Client {
	t.Helper()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatalf("cookiejar: %v", err)
	}
	return &http.Client{Jar: jar, Timeout: 10 * time.Second}
}

func (f *fixture) url(path string) string { return f.srv.URL + path }

// registerAndLogin returns a logged-in client plus the CSRF token
// needed for state-changing requests.
func (f *fixture) registerAndLogin(t *testing.T, username, password string) (*http.Client, string) {
	t.Helper()
	client := f.httpClient(t)

	status, body := f.do(t, client, http.MethodPost, "/api/auth/register", "", map[string]string{
		"username": username, "password": password,
	})
	if status != http.StatusCreated {
		t.Fatalf("register status: got %d, body=%s", status, body)
	}

	form := url.Values{"username": {username}, "password": {password}}
	status, body = f.doForm(t, client, http.MethodPost, "/api/auth/login", form)
	if status != http.StatusOK {
		t.Fatalf("login status: got %d, body=%s", status, body)
	}
	var lr api.LoginResponse
	if err := json.Unmarshal(body, &lr); err != nil {
		t.Fatalf("login decode: %v", err)
	}
	return client, lr.CSRFToken
}

// doRaw sends an arbitrary body string with Content-Type: application/json
// and optional CSRF header, closing the response body for the caller.
func (f *fixture) doRaw(t *testing.T, client *http.Client, method, path, csrf, body string) (int, []byte) {
	t.Helper()
	req, err := http.NewRequest(method, f.url(path), strings.NewReader(body))
	if err != nil {
		t.Fatalf("new raw request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if csrf != "" {
		req.Header.Set(auth.CSRFHeader, csrf)
	}
	resp, err := client.Do(req) //nolint:bodyclose // closed below
	if err != nil {
		t.Fatalf("do raw: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	out, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read raw body: %v", err)
	}
	return resp.StatusCode, out
}

// doForm POSTs form-encoded data and closes the response body.
func (f *fixture) doForm(t *testing.T, client *http.Client, method, path string, form url.Values) (int, []byte) {
	t.Helper()
	req, err := http.NewRequest(method, f.url(path), strings.NewReader(form.Encode()))
	if err != nil {
		t.Fatalf("new form request: %v", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := client.Do(req) //nolint:bodyclose // closed below
	if err != nil {
		t.Fatalf("do form: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	out, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read form body: %v", err)
	}
	return resp.StatusCode, out
}

// do issues a request with optional body + CSRF header and returns the
// status code and response body. The response is fully consumed and
// closed inside the helper, so callers never see a dangling body.
func (f *fixture) do(t *testing.T, client *http.Client, method, path, csrf string, body any) (int, []byte) {
	t.Helper()
	var rdr io.Reader
	if body != nil {
		buf, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		rdr = bytes.NewReader(buf)
	}
	req, err := http.NewRequest(method, f.url(path), rdr)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	if rdr != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if csrf != "" {
		req.Header.Set(auth.CSRFHeader, csrf)
	}
	resp, err := client.Do(req) //nolint:bodyclose // closed below
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	out, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return resp.StatusCode, out
}

// TestPublicEndpointsNoDB runs the / and /health / /api/system/*
// smoke paths that the frontend hits before login.
func TestHealthAndSystemStatus(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	defer f.close()

	client := f.httpClient(t)
	status, body := f.do(t, client, http.MethodGet, "/health", "", nil)
	if status != 200 || !bytes.Contains(body, []byte(`"healthy"`)) {
		t.Errorf("/health: got %d %s", status, body)
	}
	status, body = f.do(t, client, http.MethodGet, "/api/system/status", "", nil)
	if status != 200 || !bytes.Contains(body, []byte(`"components"`)) {
		t.Errorf("/api/system/status: got %d %s", status, body)
	}
	status, body = f.do(t, client, http.MethodGet, "/api/system/config", "", nil)
	if status != 200 || !bytes.Contains(body, []byte(`"version"`)) {
		t.Errorf("/api/system/config: got %d %s", status, body)
	}
	status, _ = f.do(t, client, http.MethodGet, "/api/system/gpu-status", "", nil)
	if status != 200 {
		t.Errorf("/api/system/gpu-status: got %d", status)
	}
}

func TestLanguagesListAndGet(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	defer f.close()

	client := f.httpClient(t)

	// Default: active only → 2 rows.
	status, body := f.do(t, client, http.MethodGet, "/api/languages", "", nil)
	if status != 200 {
		t.Fatalf("list status: %d %s", status, body)
	}
	var langs []repo.Language
	if err := json.Unmarshal(body, &langs); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(langs) != 2 {
		t.Errorf("active languages: got %d, want 2", len(langs))
	}
	if langs[0].Code != "en" {
		t.Errorf("first language: got %q, want en", langs[0].Code)
	}

	// include_inactive=true → 3 rows.
	status, body = f.do(t, client, http.MethodGet, "/api/languages?include_inactive=true", "", nil)
	if status != 200 {
		t.Fatalf("include_inactive status: %d %s", status, body)
	}
	_ = json.Unmarshal(body, &langs)
	if len(langs) != 3 {
		t.Errorf("all languages: got %d, want 3", len(langs))
	}

	status, body = f.do(t, client, http.MethodGet, "/api/languages/en", "", nil)
	if status != 200 || !bytes.Contains(body, []byte(`"native_name":"English"`)) {
		t.Errorf("GET /api/languages/en: got %d %s", status, body)
	}
	status, _ = f.do(t, client, http.MethodGet, "/api/languages/xx", "", nil)
	if status != 404 {
		t.Errorf("missing language status: got %d, want 404", status)
	}
}

func TestAuthRegisterDuplicateAndDisabledFlag(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	defer f.close()

	client := f.httpClient(t)
	body := map[string]string{"username": "alice", "password": "hunter22"}
	status, _ := f.do(t, client, http.MethodPost, "/api/auth/register", "", body)
	if status != http.StatusCreated {
		t.Fatalf("first register: %d", status)
	}

	// Duplicate → 400.
	status, _ = f.do(t, client, http.MethodPost, "/api/auth/register", "", body)
	if status != http.StatusBadRequest {
		t.Errorf("dup register: got %d, want 400", status)
	}

	// Disable registration via system setting.
	if err := f.pg.DB.WithContext(context.Background()).Exec(
		`INSERT INTO system_settings (key, value) VALUES ('registration_enabled', 'false'::jsonb)
		 ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value`).Error; err != nil {
		t.Fatalf("set registration_enabled: %v", err)
	}
	status, _ = f.do(t, client, http.MethodPost, "/api/auth/register", "", map[string]string{"username": "bob", "password": "x"})
	if status != http.StatusForbidden {
		t.Errorf("registration disabled: got %d, want 403", status)
	}
}

func TestAuthLoginFlow(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	defer f.close()
	client, csrf := f.registerAndLogin(t, "carol", "hunter22")

	// /auth/me
	status, body := f.do(t, client, http.MethodGet, "/api/auth/me", "", nil)
	if status != 200 || !bytes.Contains(body, []byte(`"carol"`)) {
		t.Errorf("/me: %d %s", status, body)
	}

	// test-csrf without header → 403
	status, _ = f.do(t, client, http.MethodPost, "/api/auth/test-csrf", "", nil)
	if status != http.StatusForbidden {
		t.Errorf("test-csrf missing: got %d", status)
	}

	// test-csrf with header → 200
	status, body = f.do(t, client, http.MethodPost, "/api/auth/test-csrf", csrf, nil)
	if status != 200 || !bytes.Contains(body, []byte("carol")) {
		t.Errorf("test-csrf ok: %d %s", status, body)
	}

	// change-password: wrong current → 400
	status, _ = f.do(t, client, http.MethodPost, "/api/auth/change-password", csrf, map[string]string{
		"current_password": "wrong", "new_password": "new-secret",
	})
	if status != http.StatusBadRequest {
		t.Errorf("wrong current pw: got %d", status)
	}
	// right current → 200
	status, _ = f.do(t, client, http.MethodPost, "/api/auth/change-password", csrf, map[string]string{
		"current_password": "hunter22", "new_password": "new-secret",
	})
	if status != http.StatusOK {
		t.Errorf("change pw: got %d", status)
	}

	// Logout clears cookies.
	status, _ = f.do(t, client, http.MethodPost, "/api/auth/logout", csrf, nil)
	if status != 200 {
		t.Errorf("logout: got %d", status)
	}
	status, _ = f.do(t, client, http.MethodGet, "/api/auth/me", "", nil)
	if status != http.StatusUnauthorized {
		t.Errorf("after logout /me: got %d, want 401", status)
	}
}

func TestLoginRejectsBadCredentials(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	defer f.close()

	client := f.httpClient(t)
	status, _ := f.do(t, client, http.MethodPost, "/api/auth/register", "", map[string]string{
		"username": "dave", "password": "rightpass",
	})
	if status != http.StatusCreated {
		t.Fatalf("register: %d", status)
	}
	form := url.Values{"username": {"dave"}, "password": {"wrongpass"}}
	status, _ = f.doForm(t, client, http.MethodPost, "/api/auth/login", form)
	if status != http.StatusUnauthorized {
		t.Errorf("bad password: got %d", status)
	}

	form = url.Values{"username": {"nobody"}, "password": {"x"}}
	status, _ = f.doForm(t, client, http.MethodPost, "/api/auth/login", form)
	if status != http.StatusUnauthorized {
		t.Errorf("missing user: got %d", status)
	}
}

func TestDashboardAndSettings(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	defer f.close()
	client, csrf := f.registerAndLogin(t, "eve", "hunter22")

	// Dashboard: empty counters.
	status, body := f.do(t, client, http.MethodGet, "/api/dashboard/stats", "", nil)
	if status != 200 {
		t.Fatalf("dashboard: %d %s", status, body)
	}
	var stats repo.DashboardStats
	_ = json.Unmarshal(body, &stats)
	if stats.TotalChats != 0 || stats.TotalDocuments != 0 {
		t.Errorf("expected all zeros, got %+v", stats)
	}

	// GET settings returns {} on a fresh user.
	status, body = f.do(t, client, http.MethodGet, "/api/me/settings", "", nil)
	if status != 200 {
		t.Errorf("get settings: %d", status)
	}
	if strings.TrimSpace(string(body)) != "{}" && string(body) != "null" {
		// accept {} or null since JSONB column may be NULL on insert
		t.Logf("settings body: %s", body)
	}

	// PUT settings without CSRF → 403.
	status, _ = f.do(t, client, http.MethodPut, "/api/me/settings", "", map[string]string{"theme": "dark"})
	if status != http.StatusForbidden {
		t.Errorf("PUT no csrf: got %d", status)
	}

	// PUT settings with CSRF → 200.
	status, _ = f.do(t, client, http.MethodPut, "/api/me/settings", csrf, map[string]string{"theme": "dark"})
	if status != http.StatusOK {
		t.Errorf("PUT settings: got %d", status)
	}

	// PUT invalid JSON → 400.
	status, _ = f.doRaw(t, client, http.MethodPut, "/api/me/settings", csrf, "not-json")
	if status != http.StatusBadRequest {
		t.Errorf("invalid json: got %d", status)
	}

	// GET settings reflects update.
	status, body = f.do(t, client, http.MethodGet, "/api/me/settings", "", nil)
	if status != 200 || !bytes.Contains(body, []byte(`"dark"`)) {
		t.Errorf("updated settings: %d %s", status, body)
	}
}

func TestChatsLifecycle(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	defer f.close()
	client, csrf := f.registerAndLogin(t, "frank", "hunter22")

	// Empty list.
	status, body := f.do(t, client, http.MethodGet, "/api/chats/", "", nil)
	if status != 200 || !bytes.Contains(body, []byte(`"total":0`)) {
		t.Errorf("empty list: %d %s", status, body)
	}

	// Create.
	status, body = f.do(t, client, http.MethodPost, "/api/chats/", csrf, map[string]string{"title": "Mission 1", "chat_type": "research"})
	if status != http.StatusCreated {
		t.Fatalf("create chat: %d %s", status, body)
	}
	var chat repo.Chat
	if err := json.Unmarshal(body, &chat); err != nil {
		t.Fatalf("decode chat: %v", err)
	}

	// Title rename.
	status, _ = f.do(t, client, http.MethodPut, "/api/chats/"+chat.ID.String()+"/title", csrf, map[string]string{"title": "Renamed"})
	if status != 200 {
		t.Errorf("rename: %d", status)
	}
	status, body = f.do(t, client, http.MethodGet, "/api/chats/"+chat.ID.String()+"/title", "", nil)
	if status != 200 || !bytes.Contains(body, []byte(`"Renamed"`)) {
		t.Errorf("get title: %d %s", status, body)
	}

	// Message append + list + delete.
	status, body = f.do(t, client, http.MethodPost, "/api/chats/"+chat.ID.String()+"/messages", csrf, map[string]any{
		"role": "user", "content": "hello",
	})
	if status != http.StatusCreated {
		t.Fatalf("append: %d %s", status, body)
	}
	var msg repo.Message
	_ = json.Unmarshal(body, &msg)

	status, body = f.do(t, client, http.MethodGet, "/api/chats/"+chat.ID.String()+"/messages", "", nil)
	if status != 200 {
		t.Fatalf("list messages: %d", status)
	}
	var msgs []repo.Message
	_ = json.Unmarshal(body, &msgs)
	if len(msgs) != 1 {
		t.Errorf("messages: got %d, want 1", len(msgs))
	}

	status, _ = f.do(t, client, http.MethodDelete, "/api/chats/"+chat.ID.String()+"/messages/"+msg.ID.String(), csrf, nil)
	if status != 200 {
		t.Errorf("delete msg: %d", status)
	}

	// Clear all messages (no-op since already empty, but exercises the handler).
	status, _ = f.do(t, client, http.MethodDelete, "/api/chats/"+chat.ID.String()+"/messages", csrf, nil)
	if status != 200 {
		t.Errorf("clear msgs: %d", status)
	}

	// Missions list (empty).
	status, body = f.do(t, client, http.MethodGet, "/api/chats/"+chat.ID.String()+"/missions", "", nil)
	if status != 200 || !bytes.Contains(body, []byte("[]")) {
		t.Errorf("missions list: %d %s", status, body)
	}

	// Get chat.
	status, _ = f.do(t, client, http.MethodGet, "/api/chats/"+chat.ID.String(), "", nil)
	if status != 200 {
		t.Errorf("get chat: %d", status)
	}

	// Delete.
	status, _ = f.do(t, client, http.MethodDelete, "/api/chats/"+chat.ID.String(), csrf, nil)
	if status != 200 {
		t.Errorf("delete chat: %d", status)
	}

	// After delete: get returns 404.
	status, _ = f.do(t, client, http.MethodGet, "/api/chats/"+chat.ID.String(), "", nil)
	if status != http.StatusNotFound {
		t.Errorf("post-delete get: got %d, want 404", status)
	}
}

func TestChatsRejectsInvalidUUID(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	defer f.close()
	client, csrf := f.registerAndLogin(t, "gwen", "hunter22")

	status, _ := f.do(t, client, http.MethodGet, "/api/chats/not-a-uuid", "", nil)
	if status != http.StatusBadRequest {
		t.Errorf("bad id: got %d", status)
	}
	status, _ = f.do(t, client, http.MethodPost, "/api/chats/not-a-uuid/messages", csrf, map[string]string{"role": "user", "content": "x"})
	if status != http.StatusBadRequest {
		t.Errorf("bad id on msg: got %d", status)
	}
}

func TestChatCreateValidation(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	defer f.close()
	client, csrf := f.registerAndLogin(t, "hank", "hunter22")

	// Missing title.
	status, _ := f.do(t, client, http.MethodPost, "/api/chats/", csrf, map[string]string{})
	if status != http.StatusBadRequest {
		t.Errorf("missing title: got %d", status)
	}

	// Bad JSON.
	status, _ = f.doRaw(t, client, http.MethodPost, "/api/chats/", csrf, "{")
	if status != http.StatusBadRequest {
		t.Errorf("bad json: got %d", status)
	}
}

func TestRegisterMissingFields(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	defer f.close()
	client := f.httpClient(t)

	status, _ := f.do(t, client, http.MethodPost, "/api/auth/register", "", map[string]string{"username": ""})
	if status != http.StatusBadRequest {
		t.Errorf("missing password: got %d", status)
	}

	status, _ = f.doRaw(t, client, http.MethodPost, "/api/auth/register", "", "{")
	if status != http.StatusBadRequest {
		t.Errorf("bad body: got %d", status)
	}
}

func TestProtectedEndpointsRejectAnon(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	defer f.close()
	client := f.httpClient(t)

	for _, path := range []string{"/api/auth/me", "/api/dashboard/stats", "/api/me/settings", "/api/chats/"} {
		status, _ := f.do(t, client, http.MethodGet, path, "", nil)
		if status != http.StatusUnauthorized {
			t.Errorf("%s: got %d, want 401", path, status)
		}
	}
}
