// Package mail is orbit's mailbox: the reply address and Message-ID that name a conversation on the mails orbit sends, and the poller that turns a reply (or a new mail) into a message.
package mail

import (
	"crypto/hmac"
	"crypto/rand"
	"encoding/hex"
	"regexp"
	"strings"

	"github.com/kensac/orbit/internal/secret"
)

// sigLen is the hex characters of the HMAC a token carries: 48 bits, so a guess at another conversation's address is refused, while the plus address stays inside a local part's 64 characters ("orbit+" + 32 + 12).
const sigLen = 12

// Token names a conversation in a reply address or Message-ID: its id's 32 hex digits and an HMAC of it under the deployment secret, so a token cannot be made for a conversation from its id alone.
func Token(key secret.Key, conversationID string) string {
	id := strings.ReplaceAll(strings.ToLower(conversationID), "-", "")
	return id + sig(key, id)
}

func sig(key secret.Key, id string) string {
	return hex.EncodeToString(key.Derive(secret.Mail(id)))[:sigLen]
}

var tokenRE = regexp.MustCompile(`^[0-9a-f]{32}[0-9a-f]{12}$`)

// Conversation is the conversation id a token names, or "" for a token that is malformed or not signed by secret.
func Conversation(key secret.Key, token string) string {
	token = strings.ToLower(token)
	if !tokenRE.MatchString(token) {
		return ""
	}
	id, s := token[:32], token[32:]
	if !hmac.Equal([]byte(s), []byte(sig(key, id))) {
		return ""
	}
	return id[:8] + "-" + id[8:12] + "-" + id[12:16] + "-" + id[16:20] + "-" + id[20:]
}

// ReplyAddress is address's plus form for a conversation: local+<token>@domain.
func ReplyAddress(address, token string) string {
	at := strings.LastIndexByte(address, '@')
	if at < 0 {
		return address
	}
	return address[:at] + "+" + token + address[at:]
}

// MessageID is a new Message-ID naming a conversation, unique per mail: <orbit.<token>.<random>@domain>.
func MessageID(address, token string) string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return "<orbit." + token + "." + hex.EncodeToString(b) + "@" + domain(address) + ">"
}

// ThreadRef is the conversation's stable reference, sent in References on every mail of it, so a mail client threads them together.
func ThreadRef(address, token string) string {
	return "<orbit." + token + "@" + domain(address) + ">"
}

func domain(address string) string {
	if at := strings.LastIndexByte(address, '@'); at >= 0 {
		return address[at+1:]
	}
	return "orbit.invalid"
}

var refRE = regexp.MustCompile(`orbit\.([0-9a-fA-F]{44})[.@]`)

// tokensIn returns the tokens a header value names in Message-IDs.
func tokensIn(v string) []string {
	var out []string
	for _, m := range refRE.FindAllStringSubmatch(v, -1) {
		out = append(out, m[1])
	}
	return out
}
