package auth

import (
	"net/http"
	"time"
)

// Cookie names used by axiom-ng. Must match the Python backend byte-for-byte.
const (
	AccessTokenCookie = "access_token"
	CSRFCookie        = "csrf_token"
	CSRFHeader        = "X-CSRF-Token"
	RememberMeHeader  = "X-Remember-Me"
)

// CookieOptions captures the deployment-dependent subset of cookie flags.
type CookieOptions struct {
	// Secure marks cookies as HTTPS-only.
	Secure bool
	// SameSite is "lax" on HTTP, "none" on HTTPS. See
	// axiom_backend/api/auth.py for the matching rule.
	SameSite http.SameSite
}

// CookieOptionsFromRequest reproduces the Python backend's scheme-based
// rule: HTTP → SameSite=Lax, Secure=false; HTTPS → SameSite=None,
// Secure=true. It inspects the request's TLS, X-Forwarded-Proto header,
// and URL scheme in that order.
func CookieOptionsFromRequest(r *http.Request) CookieOptions {
	if requestIsHTTPS(r) {
		return CookieOptions{Secure: true, SameSite: http.SameSiteNoneMode}
	}
	return CookieOptions{Secure: false, SameSite: http.SameSiteLaxMode}
}

func requestIsHTTPS(r *http.Request) bool {
	if r.TLS != nil {
		return true
	}
	if proto := r.Header.Get("X-Forwarded-Proto"); proto == "https" {
		return true
	}
	return r.URL.Scheme == "https"
}

// SetAccessCookie writes the HttpOnly JWT cookie with the Python backend's
// exact attributes (Domain empty, Path=/, max-age in seconds).
func SetAccessCookie(w http.ResponseWriter, token string, ttl time.Duration, opts CookieOptions) {
	http.SetCookie(w, &http.Cookie{
		Name:     AccessTokenCookie,
		Value:    token,
		MaxAge:   int(ttl.Seconds()),
		HttpOnly: true,
		Secure:   opts.Secure,
		SameSite: opts.SameSite,
		Path:     "/",
	})
}

// SetCSRFCookie writes the CSRF token cookie. HttpOnly is false because
// the frontend must read it and echo it back in the X-CSRF-Token header.
func SetCSRFCookie(w http.ResponseWriter, token string, ttl time.Duration, opts CookieOptions) {
	http.SetCookie(w, &http.Cookie{
		Name:     CSRFCookie,
		Value:    token,
		MaxAge:   int(ttl.Seconds()),
		HttpOnly: false,
		Secure:   opts.Secure,
		SameSite: opts.SameSite,
		Path:     "/",
	})
}

// ClearAuthCookies deletes both cookies by setting MaxAge=-1. SameSite
// and Secure must match the cookie that was originally issued.
func ClearAuthCookies(w http.ResponseWriter, opts CookieOptions) {
	for _, name := range []string{AccessTokenCookie, CSRFCookie} {
		http.SetCookie(w, &http.Cookie{
			Name:     name,
			Value:    "",
			MaxAge:   -1,
			Path:     "/",
			HttpOnly: name == AccessTokenCookie,
			Secure:   opts.Secure,
			SameSite: opts.SameSite,
		})
	}
}

// LifetimeFor returns the correct cookie lifetime for a login.
func LifetimeFor(rememberMe bool) time.Duration {
	if rememberMe {
		return RememberMeLifetime
	}
	return AccessTokenLifetime
}
