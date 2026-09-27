// Package httpx has the small HTTP helpers shared by the fakeshop services.
package httpx

import (
	"encoding/json"
	"net"
	"net/http"
	"strings"
)

func WriteJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func Error(w http.ResponseWriter, status int, msg string) {
	WriteJSON(w, status, map[string]string{"error": msg})
}

// Host is the virtual host a request is for. In the lab Caddy passes the
// original Host header; tests that reach fakeshop by service name can use
// ?host= or X-Forwarded-Host instead.
func Host(r *http.Request) string {
	h := r.URL.Query().Get("host")
	if h == "" {
		h = r.Header.Get("X-Forwarded-Host")
	}
	if h == "" {
		h = r.Host
	}
	h, _, _ = strings.Cut(h, ",") // first proxy hop wins
	if host, _, err := net.SplitHostPort(strings.TrimSpace(h)); err == nil {
		h = host
	}
	return strings.ToLower(strings.TrimSpace(h))
}

// Scheme is the scheme the browser used, so that redirects between the
// virtual hosts keep working both behind Caddy (https) and over plain HTTP.
// A non-empty override wins.
func Scheme(r *http.Request, override string) string {
	if override != "" {
		return override
	}
	if p := r.Header.Get("X-Forwarded-Proto"); p != "" {
		p, _, _ = strings.Cut(p, ",")
		return strings.TrimSpace(p)
	}
	if r.TLS != nil {
		return "https"
	}
	return "http"
}

// AdminToken returns a check for the X-Admin-Token header.
func AdminToken(token string) func(*http.Request) bool {
	return func(r *http.Request) bool {
		return token != "" && r.Header.Get("X-Admin-Token") == token
	}
}
