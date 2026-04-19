package api_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http/httptest"

	"github.com/Cyb3rDudu/axiom/axiom-ng/internal/api"
	"github.com/Cyb3rDudu/axiom/axiom-ng/internal/auth"
	"github.com/Cyb3rDudu/axiom/axiom-ng/internal/authctx"
	"github.com/Cyb3rDudu/axiom/axiom-ng/internal/config"
	"github.com/Cyb3rDudu/axiom/axiom-ng/internal/repo"
	"github.com/Cyb3rDudu/axiom/axiom-ng/internal/server"
	"github.com/google/uuid"
)

// errStub is the single sentinel returned by every stub method when
// an injection flag is set.
var errStub = errors.New("stub: boom")

// --- UserStore ---

type stubUsers struct {
	user repo.User

	getByUsernameErr   error
	getByUsernameNF    bool // return ErrNotFound
	getByIDErr         error
	createErr          error
	updatePasswordErr  error
	updatePasswordNF   bool
	updateSettingsErr  error
	updateSettingsNF   bool
	countErr           error
	countValue         int64
}

func (s *stubUsers) GetByUsername(_ context.Context, _ string) (repo.User, error) {
	if s.getByUsernameNF {
		return repo.User{}, repo.ErrNotFound
	}
	if s.getByUsernameErr != nil {
		return repo.User{}, s.getByUsernameErr
	}
	return s.user, nil
}

func (s *stubUsers) GetByID(_ context.Context, _ int32) (repo.User, error) {
	if s.getByIDErr != nil {
		return repo.User{}, s.getByIDErr
	}
	return s.user, nil
}

func (s *stubUsers) Create(_ context.Context, _ repo.CreateInput) (repo.User, error) {
	if s.createErr != nil {
		return repo.User{}, s.createErr
	}
	return s.user, nil
}

func (s *stubUsers) UpdatePassword(_ context.Context, _ int32, _ string) error {
	if s.updatePasswordNF {
		return repo.ErrNotFound
	}
	return s.updatePasswordErr
}

func (s *stubUsers) UpdateSettings(_ context.Context, _ int32, _ json.RawMessage) error {
	if s.updateSettingsNF {
		return repo.ErrNotFound
	}
	return s.updateSettingsErr
}

func (s *stubUsers) Count(_ context.Context) (int64, error) {
	if s.countErr != nil {
		return 0, s.countErr
	}
	return s.countValue, nil
}

// --- SystemSettingsStore ---

type stubSettings struct {
	enabled bool
	err     error
}

func (s *stubSettings) RegistrationEnabled(_ context.Context) (bool, error) {
	return s.enabled, s.err
}

// --- LanguageStore ---

type stubLangs struct {
	list    []repo.Language
	listErr error
	get     repo.Language
	getErr  error
	getNF   bool
}

func (s *stubLangs) List(_ context.Context, _ bool) ([]repo.Language, error) {
	return s.list, s.listErr
}

func (s *stubLangs) Get(_ context.Context, _ string) (repo.Language, error) {
	if s.getNF {
		return repo.Language{}, repo.ErrNotFound
	}
	return s.get, s.getErr
}

// --- DashboardStore ---

type stubDash struct {
	stats repo.DashboardStats
	err   error
}

func (s *stubDash) ForUser(_ context.Context, _ int32) (repo.DashboardStats, error) {
	return s.stats, s.err
}

// --- DBHealth ---

type stubHealth struct{ err error }

func (s stubHealth) Ping(_ context.Context) error { return s.err }

// --- ChatStore ---

type stubChats struct {
	create, get, del, upd, list, lm, am, dm, cm, lmi error
	createNF, getNF, delNF, updNF, lmNF, amNF, dmNF, cmNF, lmiNF bool
	chat  repo.Chat
	page  repo.Paginated
	msgs  []repo.Message
	msg   repo.Message
	miss  []repo.MissionSummary
}

func (s *stubChats) Create(_ context.Context, _ int32, _, _ string) (repo.Chat, error) {
	if s.createNF {
		return repo.Chat{}, repo.ErrNotFound
	}
	return s.chat, s.create
}
func (s *stubChats) Get(_ context.Context, _ int32, _ uuid.UUID) (repo.Chat, error) {
	if s.getNF {
		return repo.Chat{}, repo.ErrNotFound
	}
	return s.chat, s.get
}
func (s *stubChats) Delete(_ context.Context, _ int32, _ uuid.UUID) error {
	if s.delNF {
		return repo.ErrNotFound
	}
	return s.del
}
func (s *stubChats) UpdateTitle(_ context.Context, _ int32, _ uuid.UUID, _ string) error {
	if s.updNF {
		return repo.ErrNotFound
	}
	return s.upd
}
func (s *stubChats) List(_ context.Context, _ int32, _ repo.ListOptions) (repo.Paginated, error) {
	return s.page, s.list
}
func (s *stubChats) ListMessages(_ context.Context, _ int32, _ uuid.UUID) ([]repo.Message, error) {
	if s.lmNF {
		return nil, repo.ErrNotFound
	}
	return s.msgs, s.lm
}
func (s *stubChats) AppendMessage(_ context.Context, _ int32, _ uuid.UUID, _, _ string, _ json.RawMessage) (repo.Message, error) {
	if s.amNF {
		return repo.Message{}, repo.ErrNotFound
	}
	return s.msg, s.am
}
func (s *stubChats) DeleteMessage(_ context.Context, _ int32, _, _ uuid.UUID) error {
	if s.dmNF {
		return repo.ErrNotFound
	}
	return s.dm
}
func (s *stubChats) ClearMessages(_ context.Context, _ int32, _ uuid.UUID) error {
	if s.cmNF {
		return repo.ErrNotFound
	}
	return s.cm
}
func (s *stubChats) ListMissions(_ context.Context, _ int32, _ uuid.UUID) ([]repo.MissionSummary, error) {
	if s.lmiNF {
		return nil, repo.ErrNotFound
	}
	return s.miss, s.lmi
}

// stubFixture wires a router with per-test stub repos, so error-path
// tests don't pay the dockertest container cost.
type stubFixture struct {
	srv     *httptest.Server
	signer  *auth.Signer
	users   *stubUsers
	sys     *stubSettings
	langs   *stubLangs
	dash    *stubDash
	chats   *stubChats
	health  stubHealth
	token   string
	csrf    string
}

func newStubFixture() *stubFixture {
	signer, _ := auth.NewSigner("stub-secret")
	users := &stubUsers{user: repo.User{
		ID: 1, Username: "alice", Email: "alice@local", HashedPassword: "$2a$12$dummy",
		IsActive: true, Role: "user",
	}}
	f := &stubFixture{
		signer: signer,
		users:  users,
		sys:    &stubSettings{enabled: true},
		langs:  &stubLangs{},
		dash:   &stubDash{},
		chats:  &stubChats{},
	}
	deps := server.Deps{
		Auth:      api.AuthDeps{Users: users, SystemSettings: f.sys, Signer: signer},
		Languages: api.LanguageDeps{Languages: f.langs},
		System:    api.SystemDeps{Health: f.health},
		Dashboard: api.DashboardDeps{Stats: f.dash},
		Settings:  api.SettingsDeps{Users: users},
		Chats:     api.ChatDeps{Chats: f.chats},
		UserCtx: server.UserContextConfig{
			Signer:     signer,
			UserLookup: stubLookup{user: authctx.User{ID: 1, Username: "alice"}},
		},
	}
	s := server.NewWithDeps(config.Defaults(), slog.New(slog.NewTextHandler(io.Discard, nil)), deps)
	f.srv = httptest.NewServer(s.Handler())

	// Pre-mint a valid token + CSRF cookie value so tests can authenticate
	// without going through the login handler.
	f.token, _ = signer.Issue("alice", false, auth.AccessTokenLifetime)
	f.csrf, _ = auth.NewCSRFToken()
	return f
}

func (f *stubFixture) close() { f.srv.Close() }

type stubLookup struct{ user authctx.User }

func (s stubLookup) GetUserByUsername(_ context.Context, _ string) (authctx.User, error) {
	return s.user, nil
}
