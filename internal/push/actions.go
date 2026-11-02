package push

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/kensac/orbit/internal/secret"
	"github.com/kensac/orbit/internal/store"
)

// Action is one button on a notification. URL, when set, is where the
// service worker POSTs to act without opening the app; open has none and
// opens the payload's url.
type Action struct {
	Action string `json:"action"`
	Title  string `json:"title"`
	URL    string `json:"url,omitempty"`
}

// Decisions a push action link carries.
const (
	DecideApprove = "approve" // an action approved, a permission request allowed once
	DecideReject  = "reject"  // an action rejected
	DecideDeny    = "deny"    // a permission request denied
)

// ActionToken is what a push action link says: decide this action or
// approval this way, as this person, before this time. Nonce is shared by
// every button of one notification, so the notification decides once.
type ActionToken struct {
	Kind     string `json:"k"` // "action" or "approval"
	ID       string `json:"i"`
	Decision string `json:"d"`
	Person   string `json:"p"`
	Nonce    string `json:"n"`
	Exp      int64  `json:"e"`
}

// ActionPath is the API route a push action link POSTs to.
const ActionPath = "/api/v1/push/act"

var (
	// ErrActionLink is a token that is malformed or not orbit's.
	ErrActionLink = errors.New("the action link is not valid")
	// ErrActionExpired is a token past its time.
	ErrActionExpired = errors.New("the action link has expired")
)

// SignAction is the token for t, keyed from the operator token's
// PushAction purpose.
func SignAction(key secret.Key, t ActionToken) string {
	b, _ := json.Marshal(t)
	body := base64.RawURLEncoding.EncodeToString(b)
	return body + "." + actionMAC(key, body)
}

// ParseAction checks a token and returns what it says.
func ParseAction(key secret.Key, tok string, now time.Time) (ActionToken, error) {
	var t ActionToken
	body, mac, ok := strings.Cut(tok, ".")
	if !ok || len(key) == 0 || !hmac.Equal([]byte(mac), []byte(actionMAC(key, body))) {
		return t, ErrActionLink
	}
	b, err := base64.RawURLEncoding.DecodeString(body)
	if err != nil || json.Unmarshal(b, &t) != nil || t.ID == "" || t.Nonce == "" || t.Person == "" {
		return ActionToken{}, ErrActionLink
	}
	switch {
	case t.Kind == "action" && (t.Decision == DecideApprove || t.Decision == DecideReject),
		t.Kind == "approval" && (t.Decision == DecideApprove || t.Decision == DecideDeny):
	default:
		return ActionToken{}, ErrActionLink
	}
	if now.Unix() >= t.Exp {
		return ActionToken{}, ErrActionExpired
	}
	return t, nil
}

func actionMAC(key secret.Key, body string) string {
	m := hmac.New(sha256.New, key.Derive(secret.PushAction))
	m.Write([]byte(body))
	return base64.RawURLEncoding.EncodeToString(m.Sum(nil))
}

// actions are the buttons for a push of one row to person: Approve and
// Reject on a pending action, Approve and Deny on a permission request,
// and Open on both. Other kinds, and a Sender without a key or public URL,
// get none (the notification opens the app when tapped).
func (s *Sender) actions(a store.Attention, person string, now time.Time) []Action {
	base := ""
	if s.PublicURL != nil {
		base = strings.TrimRight(s.PublicURL(), "/")
	}
	if len(s.Secret) == 0 || base == "" {
		return nil
	}
	var kind, id string
	var buttons [][2]string
	switch {
	case a.Kind == store.AttentionApproval && a.ActionID != nil:
		kind, id, buttons = "action", *a.ActionID, [][2]string{{DecideApprove, "Approve"}, {DecideReject, "Reject"}}
	case a.Kind == store.AttentionPermission && a.ApprovalID != nil:
		kind, id, buttons = "approval", *a.ApprovalID, [][2]string{{DecideApprove, "Approve"}, {DecideDeny, "Deny"}}
	default:
		return nil
	}
	n := make([]byte, 16)
	_, _ = rand.Read(n)
	nonce := hex.EncodeToString(n)
	var out []Action
	for _, b := range buttons {
		tok := SignAction(s.Secret, ActionToken{Kind: kind, ID: id, Decision: b[0], Person: person, Nonce: nonce, Exp: now.Add(MaxAge).Unix()})
		out = append(out, Action{Action: b[0], Title: b[1], URL: base + ActionPath + "?t=" + tok})
	}
	return append(out, Action{Action: "open", Title: "Open"})
}
