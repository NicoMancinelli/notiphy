package api

import (
	"crypto/subtle"
	"net/http"
	"strings"
)

// adminCookie holds the admin token after a successful ?token= handoff, so a
// phone only has to paste the token once.
const adminCookie = "notiphy_admin"

// requireAdmin gates the operator surface — the dashboard, device
// registration, and token management.
//
// These endpoints are deliberately *not* protected by a webhook token: they are
// how a device is added in the first place. Without an admin token anyone who
// can reach the server can register their own device and start receiving your
// notifications, so exposing notiphy beyond a private network without setting
// one is unsafe. The server says so loudly at boot.
//
// The webhook (/hooks/:token), approval (/a/:secret), and live
// (/live/:id) paths are excluded: each carries its own capability token in the
// URL, which is what makes them safe to hand to CI or paste into a notification.
func (s *Server) requireAdmin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.cfg.AdminToken == "" {
			next.ServeHTTP(w, r)
			return
		}

		// A one-time ?token= in the URL sets a cookie, so the subscribe page's
		// later fetch() calls authenticate without embedding the token in JS.
		if q := r.URL.Query().Get("token"); q != "" && s.adminTokenOK(q) {
			http.SetCookie(w, &http.Cookie{
				Name:     adminCookie,
				Value:    q,
				Path:     "/",
				HttpOnly: true,
				SameSite: http.SameSiteLaxMode,
				Secure:   strings.HasPrefix(s.cfg.BaseURL, "https://"),
				MaxAge:   60 * 60 * 24 * 365,
			})
			// Redirect to drop the token from the address bar and history.
			clean := *r.URL
			q := clean.Query()
			q.Del("token")
			clean.RawQuery = q.Encode()
			http.Redirect(w, r, clean.RequestURI(), http.StatusSeeOther)
			return
		}

		if s.adminAuthorized(r) {
			next.ServeHTTP(w, r)
			return
		}

		if acceptsHTML(r) {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.WriteHeader(http.StatusUnauthorized)
			s.render(w, "unauthorized.html", map[string]any{"BaseURL": s.cfg.BaseURL})
			return
		}
		s.writeError(w, http.StatusUnauthorized,
			"admin token required: send Authorization: Bearer <token>, or open this URL with ?token=<token>")
	})
}

func (s *Server) adminAuthorized(r *http.Request) bool {
	if auth := r.Header.Get("Authorization"); strings.HasPrefix(auth, "Bearer ") {
		if s.adminTokenOK(strings.TrimPrefix(auth, "Bearer ")) {
			return true
		}
	}
	if c, err := r.Cookie(adminCookie); err == nil {
		if s.adminTokenOK(c.Value) {
			return true
		}
	}
	return false
}

// adminTokenOK compares in constant time so the token cannot be recovered by
// timing the comparison.
func (s *Server) adminTokenOK(got string) bool {
	want := s.cfg.AdminToken
	if want == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1
}
