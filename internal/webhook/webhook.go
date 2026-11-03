// Package webhook verifies inbound webhook deliveries: the signature over the body, the sender's delivery id (orbit's dedupe key) and, where the scheme signs one, its timestamp (replay protection). It is pure: no store, no clock but the one passed in.
package webhook

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Delivery is what a verified request says about itself.
type Delivery struct {
	// ID is the sender's delivery id, the same on every retry of one delivery.
	ID string
	// Event is the sender's event name, when its scheme carries one (GitHub's X-GitHub-Event).
	Event string
	// SentAt is the sender's signed timestamp, when its scheme has one.
	SentAt *time.Time
}

// Errors Verify returns, which the route maps to a status.
var (
	// ErrMalformed is a request missing a header its scheme requires, or with one that does not parse.
	ErrMalformed = errors.New("malformed delivery")
	// ErrSignature is a signature that does not match the body under the secret.
	ErrSignature = errors.New("signature does not match")
	// ErrStale is a signed timestamp further than the tolerance from now: a replay, or a sender's clock far off.
	ErrStale = errors.New("timestamp outside the tolerance")
)

// maxDeliveryID bounds a delivery id: GitHub's and Standard Webhooks' are UUIDs or msg_ ids, far below it.
const maxDeliveryID = 200

// Verify checks a delivery's signature under secret, by scheme ("github" or "standard"), and returns what the request says about itself. tolerance bounds a signed timestamp's distance from now.
func Verify(scheme string, secret []byte, h http.Header, body []byte, now time.Time, tolerance time.Duration) (Delivery, error) {
	switch scheme {
	case "github":
		return verifyGitHub(secret, h, body)
	case "standard":
		return verifyStandard(secret, h, body, now, tolerance)
	}
	return Delivery{}, fmt.Errorf("unknown webhook scheme %q", scheme)
}

// verifyGitHub is GitHub's scheme: X-Hub-Signature-256 is "sha256=" and the hex HMAC-SHA256 of the body; X-GitHub-Delivery is the delivery's id (a GUID, the same on a redelivery). GitHub signs no timestamp, so the delivery id is the only replay protection.
func verifyGitHub(secret []byte, h http.Header, body []byte) (Delivery, error) {
	d := Delivery{ID: h.Get("X-GitHub-Delivery"), Event: h.Get("X-GitHub-Event")}
	if err := checkID(d.ID, "X-GitHub-Delivery"); err != nil {
		return Delivery{}, err
	}
	sig, ok := strings.CutPrefix(h.Get("X-Hub-Signature-256"), "sha256=")
	if !ok {
		return Delivery{}, fmt.Errorf("%w: X-Hub-Signature-256 is missing or not sha256=<hex>", ErrMalformed)
	}
	got, err := hex.DecodeString(sig)
	if err != nil {
		return Delivery{}, fmt.Errorf("%w: X-Hub-Signature-256 is not hex", ErrMalformed)
	}
	m := hmac.New(sha256.New, secret)
	m.Write(body)
	if !hmac.Equal(got, m.Sum(nil)) {
		return Delivery{}, ErrSignature
	}
	return d, nil
}

// verifyStandard is Standard Webhooks (standardwebhooks.com): webhook-signature lists space-separated "v1,<base64 HMAC-SHA256>" of "<webhook-id>.<webhook-timestamp>.<body>"; webhook-timestamp is Unix seconds. A secret written "whsec_<base64>" is the decoded bytes, as the specification's libraries read it.
func verifyStandard(secret []byte, h http.Header, body []byte, now time.Time, tolerance time.Duration) (Delivery, error) {
	id, ts := h.Get("webhook-id"), h.Get("webhook-timestamp")
	if err := checkID(id, "webhook-id"); err != nil {
		return Delivery{}, err
	}
	sec, err := strconv.ParseInt(ts, 10, 64)
	if err != nil {
		return Delivery{}, fmt.Errorf("%w: webhook-timestamp is not Unix seconds", ErrMalformed)
	}
	key := secret
	if b64, ok := strings.CutPrefix(string(secret), "whsec_"); ok {
		if key, err = base64.StdEncoding.DecodeString(b64); err != nil {
			return Delivery{}, errors.New("the webhook secret starts with whsec_ but is not base64 after it")
		}
	}
	m := hmac.New(sha256.New, key)
	m.Write([]byte(id + "." + ts + "."))
	m.Write(body)
	want := m.Sum(nil)
	matched := false
	for _, f := range strings.Fields(h.Get("webhook-signature")) {
		v, sig, ok := strings.Cut(f, ",")
		if !ok || v != "v1" {
			continue
		}
		got, err := base64.StdEncoding.DecodeString(sig)
		if err == nil && hmac.Equal(got, want) {
			matched = true
		}
	}
	if !matched {
		return Delivery{}, ErrSignature
	}
	// The timestamp is checked after the signature, so only a signed one is judged.
	at := time.Unix(sec, 0).UTC()
	if d := now.Sub(at); d > tolerance || d < -tolerance {
		return Delivery{}, fmt.Errorf("%w: signed at %s, %s from now (at most %s)", ErrStale, at.Format(time.RFC3339), d.Round(time.Second), tolerance)
	}
	return Delivery{ID: id, SentAt: &at}, nil
}

func checkID(id, header string) error {
	if id == "" {
		return fmt.Errorf("%w: %s is missing", ErrMalformed, header)
	}
	if len(id) > maxDeliveryID {
		return fmt.Errorf("%w: %s is longer than %d bytes", ErrMalformed, header, maxDeliveryID)
	}
	return nil
}
