package claudeproto

import (
	"regexp"
	"strings"
)

// Redacted replaces a credential ScrubCredentials finds. It holds no quote or backslash, so JSON text it lands in stays valid.
const Redacted = "[REDACTED]"

// secretNameRE is a variable or field name that holds a credential: the CLI's own (authNames) and any name ending in a credential word.
var secretNameRE = `(?:` + strings.Join(authNames, "|") + `|[A-Za-z0-9_]*(?i:token|secret|password|passwd|api_?key|private_?key|access_?key|client_?secret|passphrase))`

// credentialPatterns find credentials in text. Each keeps what names the credential (submatch 1, when there is one) and replaces only the value, so a transcript still says what was there. Values are matched without quotes, backslashes or whitespace, so a match never runs past the JSON string it sits in.
var credentialPatterns = []struct {
	re   *regexp.Regexp
	keep bool // replace submatch 2, keeping submatch 1
}{
	// NAME=value, NAME: value, "NAME": "value" (also inside a JSON string, quotes escaped). A value of fewer than 8 characters or a ${VAR} reference is not a credential.
	{regexp.MustCompile(`(\b` + secretNameRE + `\\?["']?\s*[:=]\s*\\?["']?)([^\s"'\\$][^\s"'\\]{7,})`), true},
	// An Authorization header's credential.
	{regexp.MustCompile(`(?i)((?:authorization|proxy-authorization)\\?["']?\s*[:=]\s*\\?["']?\s*(?:bearer|basic|token)\s+)([A-Za-z0-9._~+/=-]{8,})`), true},
	{regexp.MustCompile(`(?i)(\bbearer\s+)([A-Za-z0-9._~+/=-]{16,})`), true},
	// A password in a URL: scheme://user:password@host.
	{regexp.MustCompile(`([a-zA-Z][a-zA-Z0-9+.-]*://[^\s:/@"'\\]+:)([^\s@/"'\\]{3,})(?:@)`), true},
	// Tokens in their providers' formats.
	{regexp.MustCompile(`\bsk-ant-[A-Za-z0-9_-]{16,}`), false},
	{regexp.MustCompile(`\bsk-(?:proj-)?[A-Za-z0-9_-]{32,}`), false},
	{regexp.MustCompile(`\bgh[pousr]_[A-Za-z0-9]{30,}`), false},
	{regexp.MustCompile(`\bgithub_pat_[A-Za-z0-9_]{40,}`), false},
	{regexp.MustCompile(`\bglpat-[A-Za-z0-9_-]{20,}`), false},
	{regexp.MustCompile(`\bxox[abprs]-[A-Za-z0-9-]{10,}`), false},
	{regexp.MustCompile(`\bAKIA[0-9A-Z]{16}\b`), false},
	{regexp.MustCompile(`\bAIza[0-9A-Za-z_-]{35}`), false},
	// A private key block, whole.
	{regexp.MustCompile(`-----BEGIN [A-Z0-9 ]*PRIVATE KEY-----[\s\S]*?-----END [A-Z0-9 ]*PRIVATE KEY-----`), false},
}

// ScrubCredentials returns text with every credential it recognises replaced by Redacted: the values of the variables the CLI authenticates with and of any secret-named assignment, Authorization and bearer credentials, passwords in URLs, provider-format tokens and private key blocks. It is how a transcript written outside orbit (a person's own Claude Code sessions) is stored without the secrets that passed through it. It is a pattern match, so it removes what it recognises, not every secret.
func ScrubCredentials(text string) string {
	for _, p := range credentialPatterns {
		if !p.keep {
			text = p.re.ReplaceAllLiteralString(text, Redacted)
			continue
		}
		text = p.re.ReplaceAllStringFunc(text, func(m string) string {
			sub := p.re.FindStringSubmatchIndex(m)
			// Keep the name, replace the value, keep anything the pattern matched after it (the @ of a URL).
			return m[:sub[3]] + Redacted + m[sub[5]:]
		})
	}
	return text
}
