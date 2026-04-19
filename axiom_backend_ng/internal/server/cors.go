package server

import (
	"net/http"
	"os"
	"strings"
)

// corsOrigins mirrors axiom_backend/main.py:get_cors_origins(). Two
// independent triggers enable a wildcard: ALLOW_CORS_WILDCARD=true
// OR CORS_ALLOWED_ORIGINS=*. Otherwise the default localhost list is
// unioned with the comma-separated CORS_ALLOWED_ORIGINS env value.
//
// Returning a single "*" entry means "allow all" and disables the
// per-origin equality check downstream.
func corsOrigins(envLookup func(string) string) []string {
	if strings.EqualFold(envLookup("ALLOW_CORS_WILDCARD"), "true") {
		return []string{"*"}
	}
	custom := envLookup("CORS_ALLOWED_ORIGINS")
	if custom == "*" {
		return []string{"*"}
	}
	defaults := []string{
		"http://localhost",
		"http://localhost:80",
		"http://localhost:3000",
		"http://localhost:3030",
		"http://localhost:5173",
		"http://localhost:8001",
		"http://127.0.0.1",
		"http://127.0.0.1:80",
		"http://127.0.0.1:3000",
		"http://127.0.0.1:3030",
		"http://127.0.0.1:8001",
	}
	seen := map[string]struct{}{}
	var out []string
	push := func(o string) {
		o = strings.TrimSpace(o)
		if o == "" {
			return
		}
		if _, ok := seen[o]; ok {
			return
		}
		seen[o] = struct{}{}
		out = append(out, o)
	}
	for _, o := range defaults {
		push(o)
	}
	for _, o := range strings.Split(custom, ",") {
		push(o)
	}
	return out
}

// CORS returns a middleware reproducing the Python backend's FastAPI
// CORSMiddleware behavior: credentials enabled, wildcard methods/headers,
// 24h preflight cache. Env-var parity with Python is validated via
// corsOrigins.
func CORS(envLookup func(string) string) func(http.Handler) http.Handler {
	if envLookup == nil {
		envLookup = os.Getenv
	}
	origins := corsOrigins(envLookup)
	allowAll := len(origins) == 1 && origins[0] == "*"
	allowed := map[string]struct{}{}
	for _, o := range origins {
		allowed[o] = struct{}{}
	}

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			origin := r.Header.Get("Origin")
			h := w.Header()

			if origin != "" {
				if allowAll {
					// FastAPI mirrors the request origin back when
					// allow_credentials=true + allow_origins=["*"]
					// — a raw "*" would be rejected by browsers.
					h.Set("Access-Control-Allow-Origin", origin)
					h.Add("Vary", "Origin")
				} else if _, ok := allowed[origin]; ok {
					h.Set("Access-Control-Allow-Origin", origin)
					h.Add("Vary", "Origin")
				}
			}
			h.Set("Access-Control-Allow-Credentials", "true")

			if r.Method == http.MethodOptions && r.Header.Get("Access-Control-Request-Method") != "" {
				h.Set("Access-Control-Allow-Methods", "*")
				h.Set("Access-Control-Allow-Headers", "*")
				h.Set("Access-Control-Expose-Headers", "*")
				h.Set("Access-Control-Max-Age", "86400")
				w.WriteHeader(http.StatusNoContent)
				return
			}
			h.Set("Access-Control-Expose-Headers", "*")
			next.ServeHTTP(w, r)
		})
	}
}
