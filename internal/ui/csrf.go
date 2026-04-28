package ui

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"html/template"
	"net/http"
)

const csrfCookieName = "__Host-csrf"

type csrfContextKey struct{}

// csrfToken returns the CSRF token for the current request. It reads the value
// stored in the request context by csrfMiddleware (preferred), falling back to
// reading the cookie directly.
func csrfToken(r *http.Request) string {
	if tok, ok := r.Context().Value(csrfContextKey{}).(string); ok {
		return tok
	}
	c, err := r.Cookie(csrfCookieName)
	if err != nil {
		return ""
	}
	return c.Value
}

// csrfField renders a hidden input element containing the CSRF token, suitable
// for embedding in POST forms.
func csrfField(token string) template.HTML {
	return template.HTML(`<input type="hidden" name="csrf" value="` + template.HTMLEscapeString(token) + `">`)
}

// generateCSRFToken generates a new cryptographically random 32-byte hex token.
func generateCSRFToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// csrfMiddleware wraps all UI handlers with CSRF protection using the
// double-submit cookie pattern. On every request it ensures the __Host-csrf
// cookie is set. On POST requests it validates that the "csrf" form field
// matches the cookie value, returning 403 on mismatch.
func (h *Handler) csrfMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Get or create the CSRF token.
		tok := ""
		if c, err := r.Cookie(csrfCookieName); err == nil {
			tok = c.Value
		} else {
			var genErr error
			tok, genErr = generateCSRFToken()
			if genErr != nil {
				http.Error(w, "internal server error", http.StatusInternalServerError)
				return
			}
			cookie := &http.Cookie{
				Name:     csrfCookieName,
				Value:    tok,
				Path:     "/",
				SameSite: http.SameSiteStrictMode,
				HttpOnly: false,
				Secure:   !h.devMode,
			}
			http.SetCookie(w, cookie)
		}

		// Validate POST requests.
		if r.Method == http.MethodPost {
			if r.FormValue("csrf") != tok {
				h.renderError(w, http.StatusForbidden, "CSRF validation failed")
				return
			}
		}

		// Store token in context so handlers can embed it in templates.
		ctx := context.WithValue(r.Context(), csrfContextKey{}, tok)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}
