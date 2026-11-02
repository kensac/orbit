package mail

import (
	"bytes"
	"errors"
	"fmt"
	"html"
	"io"
	"mime"
	"regexp"
	"strings"

	_ "github.com/emersion/go-message/charset" // decode every charset a mail client sends
	gomail "github.com/emersion/go-message/mail"
)

// Inbound is a mail as orbit acts on it.
type Inbound struct {
	MessageID string
	// From is the sender's address, lower-cased.
	From    string
	Subject string
	// Text is what the person wrote: the plain text (or the HTML as text), with the quoted mail it replies to and the signature removed.
	Text        string
	Attachments []Attachment
	// Tokens are the conversation tokens the mail names, most specific first: a recipient's plus address, then In-Reply-To, then References.
	Tokens []string
	// Rejected is why the mail must not be acted on even from an allowed sender (an automatic reply, a failed DMARC check); "" when none.
	Rejected string
}

// Attachment is a part of a mail that is not its text. Its bytes stay in the raw mail (mail_in.raw).
type Attachment struct {
	Name string `json:"name"`
	Type string `json:"type"`
	Size int    `json:"size"`
}

// maxText bounds the text taken from a mail, as /api/v1 bounds a message body (64 KiB); the raw mail keeps the rest.
const maxText = 60 << 10

// Parse reads a raw mail. address is orbit's own (spec.Mail.Address), whose plus addresses carry tokens.
func Parse(raw []byte, address string) (*Inbound, error) {
	mr, err := gomail.CreateReader(bytes.NewReader(raw))
	if err != nil && mr == nil {
		return nil, fmt.Errorf("parse: %w", err)
	}
	h := mr.Header
	in := &Inbound{}
	in.MessageID = strings.TrimSpace(h.Get("Message-Id"))
	if in.MessageID == "" {
		return nil, errors.New("parse: no Message-ID")
	}
	if from, err := h.AddressList("From"); err == nil && len(from) > 0 {
		in.From = strings.ToLower(from[0].Address)
	}
	in.Subject, _ = h.Subject()

	for _, k := range []string{"To", "Cc", "Delivered-To", "X-Original-To"} {
		for _, v := range h.Values(k) {
			for _, a := range addresses(v) {
				if t := plusToken(a, address); t != "" {
					in.Tokens = append(in.Tokens, t)
				}
			}
		}
	}
	in.Tokens = append(in.Tokens, tokensIn(h.Get("In-Reply-To"))...)
	in.Tokens = append(in.Tokens, tokensIn(h.Get("References"))...)
	in.Rejected = rejected(&h)

	var plain, htm string
	for {
		p, err := mr.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			// A malformed part ends the walk; what was read stands.
			break
		}
		b, _ := io.ReadAll(io.LimitReader(p.Body, 32<<20))
		switch ph := p.Header.(type) {
		case *gomail.InlineHeader:
			ct, _, _ := ph.ContentType()
			switch {
			case ct == "text/plain" && plain == "":
				plain = string(b)
			case ct == "text/html" && htm == "":
				htm = string(b)
			case strings.HasPrefix(ct, "text/"):
			default:
				in.Attachments = append(in.Attachments, Attachment{Name: partName(ph), Type: ct, Size: len(b)})
			}
		case *gomail.AttachmentHeader:
			name, _ := ph.Filename()
			ct, _, _ := ph.ContentType()
			in.Attachments = append(in.Attachments, Attachment{Name: name, Type: ct, Size: len(b)})
		}
	}
	text := plain
	if strings.TrimSpace(text) == "" && htm != "" {
		text = htmlText(htm)
	}
	in.Text = StripQuoted(text)
	if len(in.Text) > maxText {
		in.Text = in.Text[:maxText] + "\n\n(the rest of this mail is in orbit's record of it)"
	}
	return in, nil
}

// headerGetter is the textproto header go-message wraps.
type headerGetter interface{ Get(string) string }

// rejected says why a mail is never acted on: automatic mail (an out-of-office reply to orbit's own mail would otherwise answer it back, forever) and a sender the receiving server's DMARC check failed. A mail with no Authentication-Results is accepted: the allow list is then the only check.
func rejected(h headerGetter) string {
	if v := strings.ToLower(strings.TrimSpace(h.Get("Auto-Submitted"))); v != "" && v != "no" {
		return "automatic mail (Auto-Submitted: " + v + ")"
	}
	switch strings.ToLower(strings.TrimSpace(h.Get("Precedence"))) {
	case "bulk", "junk", "list", "auto_reply":
		return "automatic mail (Precedence)"
	}
	if h.Get("X-Autoreply") != "" || h.Get("X-Autorespond") != "" {
		return "automatic mail (an auto-reply)"
	}
	if ar := strings.ToLower(h.Get("Authentication-Results")); strings.Contains(ar, "dmarc=fail") {
		return "the sender failed DMARC (Authentication-Results)"
	}
	return ""
}

func partName(h headerGetter) string {
	if _, p, err := mime.ParseMediaType(h.Get("Content-Disposition")); err == nil && p["filename"] != "" {
		return p["filename"]
	}
	if _, p, err := mime.ParseMediaType(h.Get("Content-Type")); err == nil {
		return p["name"]
	}
	return ""
}

var addrRE = regexp.MustCompile(`[^\s<>,;"]+@[^\s<>,;"]+`)

func addresses(v string) []string { return addrRE.FindAllString(v, -1) }

// plusToken is the token in a's plus part when a is a plus form of address; "" otherwise.
func plusToken(a, address string) string {
	a, address = strings.ToLower(a), strings.ToLower(address)
	at, bat := strings.LastIndexByte(a, '@'), strings.LastIndexByte(address, '@')
	if at < 0 || bat < 0 || a[at:] != address[bat:] {
		return ""
	}
	local, base := a[:at], address[:bat]
	if !strings.HasPrefix(local, base+"+") {
		return ""
	}
	return local[len(base)+1:]
}

// ---------------------------------------------------------------- quoting

var (
	wroteRE     = regexp.MustCompile(`(?i)^\s*(On\s.+|Le\s.+|Am\s.+|El\s.+)$`)
	wroteEndRE  = regexp.MustCompile(`(?i)(wrote|a écrit|schrieb|escribió)\s*:\s*$`)
	originalRE  = regexp.MustCompile(`(?i)^\s*-{2,}\s*(Original Message|Forwarded message)\s*-{2,}`)
	outlookRE   = regexp.MustCompile(`^\s*_{10,}\s*$`)
	headerRE    = regexp.MustCompile(`(?i)^\s*\*?From:\*?\s.+`)
	headerOneRE = regexp.MustCompile(`(?i)^\s*\*?(Sent|Date|To|Subject):\*?\s`)
)

// StripQuoted returns what the person wrote above the mail they reply to: the text before the first attribution line ("On <date>, <who> wrote:", wrapped over up to three lines), an "Original Message" or Outlook separator, or a From:/Sent: header block; then without a signature ("-- ") and trailing "> " lines. Quoted lines the person wrote between stay, as the context of their answers. Code, not a model, so the same mail always gives the same message.
func StripQuoted(text string) string {
	text = strings.ReplaceAll(text, "\r\n", "\n")
	lines := strings.Split(text, "\n")
	cut := len(lines)
	for i := 0; i < len(lines) && cut == len(lines); i++ {
		l := lines[i]
		switch {
		case originalRE.MatchString(l), outlookRE.MatchString(l):
			cut = i
		case wroteRE.MatchString(l):
			for j := i; j < len(lines) && j < i+3; j++ {
				if wroteEndRE.MatchString(lines[j]) {
					cut = i
					break
				}
			}
		case headerRE.MatchString(l):
			for j := i + 1; j < len(lines) && j < i+4; j++ {
				if headerOneRE.MatchString(lines[j]) {
					cut = i
					break
				}
			}
		case l == "-- ":
			cut = i
		}
	}
	lines = lines[:cut]
	// Trailing quoted lines (a reply written above a bare "> " quote).
	end := len(lines)
	for end > 0 {
		t := strings.TrimSpace(lines[end-1])
		if t == "" || strings.HasPrefix(t, ">") {
			end--
			continue
		}
		break
	}
	return strings.TrimSpace(strings.Join(lines[:end], "\n"))
}

var (
	quoteDivRE = regexp.MustCompile(`(?is)<div[^>]*class="[^"]*(gmail_quote|gmail_attr)[^"]*".*$`)
	blockRE    = regexp.MustCompile(`(?is)<blockquote.*?</blockquote>`)
	dropRE     = regexp.MustCompile(`(?is)<(style|script|head)[^>]*>.*?</(style|script|head)>`)
	breakRE    = regexp.MustCompile(`(?i)<(br|/p|/div|/li|/tr|/h[1-6])[^>]*>`)
	tagRE      = regexp.MustCompile(`(?s)<[^>]*>`)
	blankRE    = regexp.MustCompile(`\n{3,}`)
)

// htmlText turns an HTML-only mail into text: the quoted part a mail client marks (Gmail's gmail_quote, any blockquote) dropped, blocks as lines, tags removed, entities decoded.
func htmlText(h string) string {
	h = quoteDivRE.ReplaceAllString(h, "")
	h = blockRE.ReplaceAllString(h, "")
	h = dropRE.ReplaceAllString(h, "")
	h = breakRE.ReplaceAllString(h, "\n")
	h = tagRE.ReplaceAllString(h, "")
	h = strings.ReplaceAll(html.UnescapeString(h), "\u00a0", " ")
	var lines []string
	for _, l := range strings.Split(h, "\n") {
		lines = append(lines, strings.TrimRight(l, " \t "))
	}
	return strings.TrimSpace(blankRE.ReplaceAllString(strings.Join(lines, "\n"), "\n\n"))
}
