package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/Cyb3rDudu/axiom/axiom-ng/internal/auth"
	"github.com/Cyb3rDudu/axiom/axiom-ng/internal/repo"
)

// AuthDeps is what the auth handler set needs. The concrete repos live
// elsewhere; the interfaces keep handler tests independent of the
// database.
type AuthDeps struct {
	Users          UserStore
	SystemSettings SystemSettingsStore
	Signer         *auth.Signer
}

// UserStore is the subset of repo.Users the auth handlers need.
type UserStore interface {
	GetByUsername(ctx context.Context, username string) (repo.User, error)
	GetByID(ctx context.Context, id int32) (repo.User, error)
	Create(ctx context.Context, in repo.CreateInput) (repo.User, error)
	UpdatePassword(ctx context.Context, id int32, hashed string) error
	Count(ctx context.Context) (int64, error)
}

// SystemSettingsStore feeds the registration-enabled gate.
type SystemSettingsStore interface {
	RegistrationEnabled(ctx context.Context) (bool, error)
}

// RegisterRequest is the POST /api/auth/register body.
type RegisterRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
	Email    string `json:"email,omitempty"`
}

// LoginRequest accepts either form or JSON input. OAuth2PasswordRequestForm
// from FastAPI is x-www-form-urlencoded.
type LoginRequest struct {
	Username   string `json:"username" form:"username"`
	Password   string `json:"password" form:"password"`
	RememberMe bool   `json:"remember_me" form:"remember_me"`
}

// LoginResponse mirrors Python's login JSON body.
type LoginResponse struct {
	Message     string `json:"message"`
	CSRFToken   string `json:"csrf_token"`
	AccessToken string `json:"access_token"`
	TokenType   string `json:"token_type"`
	ExpiresIn   int    `json:"expires_in"`
	RememberMe  bool   `json:"remember_me"`
}

// ChangePasswordRequest is the JSON body for /api/auth/change-password.
type ChangePasswordRequest struct {
	CurrentPassword string `json:"current_password"`
	NewPassword     string `json:"new_password"`
}

// Register handles POST /api/auth/register.
func (d AuthDeps) Register(w http.ResponseWriter, r *http.Request) {
	var req RegisterRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "Invalid request body")
		return
	}
	if req.Username == "" || req.Password == "" {
		writeError(w, http.StatusBadRequest, "username and password are required")
		return
	}

	ok, err := d.SystemSettings.RegistrationEnabled(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "registration check failed")
		return
	}
	if !ok {
		writeError(w, http.StatusForbidden, "User registration is disabled")
		return
	}

	if _, err := d.Users.GetByUsername(r.Context(), req.Username); err == nil {
		writeError(w, http.StatusBadRequest, "Username already registered")
		return
	} else if !errors.Is(err, repo.ErrNotFound) {
		writeError(w, http.StatusInternalServerError, "username lookup failed")
		return
	}

	hash, err := auth.HashPassword(req.Password)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "password hashing failed")
		return
	}

	isFirst := false
	if n, err := d.Users.Count(r.Context()); err == nil && n == 0 {
		// Bootstrap: first user is always admin. Matches
		// axiom_backend/setup_first_user.py.
		isFirst = true
	}

	email := req.Email
	if email == "" {
		email = req.Username + "@axiom.local"
	}

	user, err := d.Users.Create(r.Context(), repo.CreateInput{
		Username:       req.Username,
		Email:          email,
		HashedPassword: hash,
		IsAdmin:        isFirst,
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "user creation failed")
		return
	}
	writeJSON(w, http.StatusCreated, user)
}

// Login handles POST /api/auth/login.
func (d AuthDeps) Login(w http.ResponseWriter, r *http.Request) {
	req, err := readLoginRequest(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "Invalid login request")
		return
	}

	user, err := d.Users.GetByUsername(r.Context(), req.Username)
	if errors.Is(err, repo.ErrNotFound) {
		writeError(w, http.StatusUnauthorized, "Incorrect username or password")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "login lookup failed")
		return
	}
	ok, err := auth.VerifyPassword(req.Password, user.HashedPassword)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "password verification failed")
		return
	}
	if !ok {
		writeError(w, http.StatusUnauthorized, "Incorrect username or password")
		return
	}

	ttl := auth.LifetimeFor(req.RememberMe)
	token, err := d.Signer.Issue(user.Username, req.RememberMe, ttl)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "token issuance failed")
		return
	}
	csrf, err := auth.NewCSRFToken()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "csrf generation failed")
		return
	}

	opts := auth.CookieOptionsFromRequest(r)
	auth.SetAccessCookie(w, token, ttl, opts)
	auth.SetCSRFCookie(w, csrf, ttl, opts)

	writeJSON(w, http.StatusOK, LoginResponse{
		Message:     "Login successful",
		CSRFToken:   csrf,
		AccessToken: token,
		TokenType:   "bearer",
		ExpiresIn:   int(ttl.Seconds()),
		RememberMe:  req.RememberMe,
	})
}

// Logout handles POST /api/auth/logout.
func (d AuthDeps) Logout(w http.ResponseWriter, r *http.Request) {
	auth.ClearAuthCookies(w, auth.CookieOptionsFromRequest(r))
	writeJSON(w, http.StatusOK, map[string]string{"message": "Logout successful"})
}

// Me handles GET /api/auth/me.
func (d AuthDeps) Me(w http.ResponseWriter, r *http.Request) {
	username, ok := usernameFromRequest(r)
	if !ok {
		writeError(w, http.StatusUnauthorized, "Not authenticated")
		return
	}
	user, err := d.Users.GetByUsername(r.Context(), username)
	if err != nil {
		writeError(w, http.StatusUnauthorized, "User not found")
		return
	}
	writeJSON(w, http.StatusOK, user)
}

// TestCSRF handles POST /api/auth/test-csrf. CSRF middleware has
// already validated by the time we get here.
func (d AuthDeps) TestCSRF(w http.ResponseWriter, r *http.Request) {
	username, _ := usernameFromRequest(r)
	writeJSON(w, http.StatusOK, map[string]string{
		"message": "CSRF protection working for user: " + username,
	})
}

// ChangePassword handles POST /api/auth/change-password.
func (d AuthDeps) ChangePassword(w http.ResponseWriter, r *http.Request) {
	var req ChangePasswordRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "Invalid request body")
		return
	}
	if req.NewPassword == "" {
		writeError(w, http.StatusBadRequest, "new_password is required")
		return
	}
	username, ok := usernameFromRequest(r)
	if !ok {
		writeError(w, http.StatusUnauthorized, "Not authenticated")
		return
	}
	user, err := d.Users.GetByUsername(r.Context(), username)
	if err != nil {
		writeError(w, http.StatusUnauthorized, "User not found")
		return
	}
	ok, err = auth.VerifyPassword(req.CurrentPassword, user.HashedPassword)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "password verification failed")
		return
	}
	if !ok {
		writeError(w, http.StatusBadRequest, "Current password is incorrect")
		return
	}
	hash, err := auth.HashPassword(req.NewPassword)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "password hashing failed")
		return
	}
	if err := d.Users.UpdatePassword(r.Context(), user.ID, hash); err != nil {
		writeError(w, http.StatusInternalServerError, "password update failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"message": "Password changed successfully"})
}
