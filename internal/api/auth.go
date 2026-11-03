package api

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/kensac/orbit/internal/auth"
)

// principal is who a request is for.
type principal struct {
	// Person is the authentik user (preferred_username). Empty for the
	// operator token, which acts for no one in particular.
	Person string
	// Groups are the person's groups, from their token.
	Groups []string
}

type principalKey struct{}

func principalOf(r *http.Request) principal {
	p, _ := r.Context().Value(principalKey{}).(principal)
	return p
}

// authenticate guards /api/v1. Every listener accepts an authentik token;
// the operator token is accepted only when internal is true, on the
// listener traefik never routes to (ORBIT_INTERNAL_LISTEN). Which listener
// a connection arrived on is a fact of the socket, unlike a forwarded
// header, which anyone can send.
func (s *Server) authenticate(internal bool, next http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, ok := bearer(r)
		if !ok {
			unauthorized(w, "token", "missing bearer token")
			return
		}
		if internal && s.Token != "" && subtle.ConstantTimeCompare([]byte(raw), []byte(s.Token)) == 1 {
			next(w, r.WithContext(context.WithValue(r.Context(), principalKey{}, principal{})))
			return
		}
		if s.Verifier == nil {
			unauthorized(w, "token", "authentik tokens are not configured on this server (ORBIT_OIDC_ISSUER)")
			return
		}
		id, err := s.Verifier.Verify(r.Context(), raw)
		var ae *auth.Error
		switch {
		case errors.As(err, &ae):
			reason := ae.Error()
			if !internal && ae.Check == "token" {
				reason += "; the operator token is accepted only on the internal listener"
			}
			unauthorized(w, ae.Check, reason)
			return
		case err != nil:
			s.Log.Warn("token verification unavailable", "err", err)
			writeErr(w, http.StatusServiceUnavailable, "cannot verify tokens right now: "+err.Error())
			return
		}
		next(w, r.WithContext(context.WithValue(r.Context(), principalKey{}, principal{Person: id.Person, Groups: id.Groups})))
	})
}

func bearer(r *http.Request) (string, bool) {
	h := r.Header.Get("Authorization")
	const prefix = "bearer "
	if len(h) <= len(prefix) || !strings.EqualFold(h[:len(prefix)], prefix) {
		return "", false
	}
	return strings.TrimSpace(h[len(prefix):]), true
}

// unauthorized writes a 401 naming the check that failed, in the body and
// in WWW-Authenticate (RFC 6750).
func unauthorized(w http.ResponseWriter, check, reason string) {
	w.Header().Set("WWW-Authenticate", `Bearer error="invalid_token", error_description="`+strings.ReplaceAll(reason, `"`, `'`)+`"`)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusUnauthorized)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": reason, "check": check})
}
