// Package secret derives every key orbit signs or seals with from one root, the operator token (ORBIT_API_TOKEN), so each use has its own key and rotating the token voids all of them together.
package secret

import (
	"crypto/hmac"
	"crypto/sha256"
)

// Purposes Derive is called with. Each is a distinct label so no two uses share a key; a label is never reused or changed, because changing one voids everything signed under it (approval links, mail reply addresses, upload URLs and sealed OAuth state already sent out).
const (
	// UploadURL keys signed upload URLs (internal/api uploads.go).
	UploadURL = "orbit upload url"
	// MCPOAuth keys the MCP OAuth proxy's sealed state and codes (internal/api oauth.go).
	MCPOAuth = "orbit mcp oauth"
	// PushAction keys the action links in a push (Approve, Reject, Deny; internal/push actions.go).
	PushAction = "orbit push action"
)

// Approval is the purpose that signs the approval link of one action: the link's signature is Derive(Approval(id)) itself, in hex.
func Approval(actionID string) string { return "action:" + actionID }

// Mail is the purpose that signs one conversation's mail token (internal/mail): the token carries a prefix of Derive(Mail(id)) in hex, id being the conversation's 32 hex digits.
func Mail(conversationHex string) string { return "mail:" + conversationHex }

// Key is the root secret: the operator token's bytes.
type Key []byte

// Derive is HMAC-SHA256 of purpose under the root: a 32-byte key (or, for the per-item purposes, the signature itself) that is useless for any other purpose. Purposes are the constants and functions above, whose outputs never collide: the fixed labels contain a space, the per-item ones a colon prefix and no space.
func (k Key) Derive(purpose string) []byte {
	m := hmac.New(sha256.New, k)
	m.Write([]byte(purpose))
	return m.Sum(nil)
}
