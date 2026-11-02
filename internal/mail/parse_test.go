package mail

import (
	"strings"
	"testing"
)

func TestStripQuoted(t *testing.T) {
	cases := []struct{ name, in, want string }{
		{"gmail", "Yes, the $500 was the deductible.\n\nOn Sun, Nov 1, 2026 at 9:00 PM orbit <orbit@example.com> wrote:\n> finance: October\n> You spent $84.\n",
			"Yes, the $500 was the deductible."},
		{"gmail wrapped", "Sounds good\n\nOn Sun, Nov 1, 2026 at 9:00 PM orbit <\norbit@example.com> wrote:\n\n> quoted\n", "Sounds good"},
		{"apple", "Thanks!\n\n> On Nov 1, 2026, at 21:00, orbit <orbit@example.com> wrote:\n>\n> report\n", "Thanks!"},
		{"outlook", "Approve it.\n\n________________________________\nFrom: orbit <orbit@example.com>\nSent: Sunday\n", "Approve it."},
		{"outlook headers", "Approve it.\n\nFrom: orbit <orbit@example.com>\nSent: Sunday, November 1, 2026 9:00 PM\nTo: me\n", "Approve it."},
		{"original", "ok\n-----Original Message-----\nFrom: x\n", "ok"},
		{"signature", "Do it tomorrow.\n\n-- \nKanishk\nSent from my phone", "Do it tomorrow."},
		// Answers written between the quoted lines keep them: they are the context.
		{"inline", "> Should I pay it?\nYes\n> And the other?\nNo\n", "> Should I pay it?\nYes\n> And the other?\nNo"},
		{"all quoted", "> only a quote\n", ""},
		{"crlf", "Fine.\r\n\r\nOn Mon, orbit wrote:\r\n> x\r\n", "Fine."},
		{"plain", "Just a new thought\nwith two lines", "Just a new thought\nwith two lines"},
		{"on without wrote", "On second thought, skip it.\nThanks", "On second thought, skip it.\nThanks"},
	}
	for _, c := range cases {
		if got := StripQuoted(c.in); got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}
}

func TestHTMLText(t *testing.T) {
	h := `<div dir="ltr">Pay the <b>$500</b> one&nbsp;now.<br>Thanks</div><br><div class="gmail_quote"><div dir="ltr" class="gmail_attr">On Sun orbit wrote:<br></div><blockquote>old</blockquote></div>`
	if got := htmlText(h); got != "Pay the $500 one now.\nThanks" {
		t.Fatalf("got %q", got)
	}
}

func TestTokens(t *testing.T) {
	key := []byte("s3cret")
	conv := "0b6f3c1e-2a4d-4c5e-9f00-123456789abc"
	tok := Token(key, conv)
	if len(tok) != 44 || Conversation(key, tok) != conv || Conversation(key, strings.ToUpper(tok)) != conv {
		t.Fatalf("token %q does not round-trip", tok)
	}
	flip := "a"
	if tok[43] == 'a' {
		flip = "b"
	}
	if Conversation([]byte("other"), tok) != "" || Conversation(key, tok[:43]+flip) != "" {
		t.Fatal("a token signed with another key, or altered, must not resolve")
	}
	addr := ReplyAddress("orbit@example.com", tok)
	if addr != "orbit+"+tok+"@example.com" || len(addr[:strings.IndexByte(addr, '@')]) > 64 {
		t.Fatalf("reply address %q", addr)
	}
	if got := tokensIn(MessageID("orbit@example.com", tok) + " " + ThreadRef("orbit@example.com", tok)); len(got) != 2 || got[0] != tok {
		t.Fatalf("tokens in ids = %v", got)
	}
}

func raw(headers map[string]string, body string) []byte {
	var b strings.Builder
	for k, v := range headers {
		b.WriteString(k + ": " + v + "\r\n")
	}
	b.WriteString("\r\n" + strings.ReplaceAll(strings.ReplaceAll(body, "\r\n", "\n"), "\n", "\r\n"))
	return []byte(b.String())
}

func TestParse(t *testing.T) {
	tok := Token([]byte("s"), "0b6f3c1e-2a4d-4c5e-9f00-123456789abc")
	in, err := Parse(raw(map[string]string{
		"Message-ID": "<abc@mail.gmail.com>", "From": "Kanishk <Kanishk@Example.COM>",
		"To": "orbit <orbit+" + tok + "@example.com>", "Subject": "Re: finance: October",
		"In-Reply-To": MessageID("orbit@example.com", tok), "Content-Type": "text/plain; charset=utf-8",
	}, "Yes.\n\nOn Sun, orbit wrote:\n> x\n"), "orbit@example.com")
	if err != nil {
		t.Fatal(err)
	}
	if in.MessageID != "<abc@mail.gmail.com>" || in.From != "kanishk@example.com" || in.Text != "Yes." ||
		len(in.Tokens) != 2 || in.Tokens[0] != tok || in.Rejected != "" || in.Subject != "Re: finance: October" {
		t.Fatalf("parsed %+v", in)
	}

	multi := "--b\r\nContent-Type: multipart/alternative; boundary=a\r\n\r\n--a\r\nContent-Type: text/plain\r\n\r\nSee attached\r\n--a\r\nContent-Type: text/html\r\n\r\n<p>See attached</p>\r\n--a--\r\n" +
		"--b\r\nContent-Type: application/pdf; name=bill.pdf\r\nContent-Disposition: attachment; filename=bill.pdf\r\nContent-Transfer-Encoding: base64\r\n\r\nJVBERi0=\r\n--b--\r\n"
	in, err = Parse(raw(map[string]string{"Message-ID": "<m2@x>", "From": "a@b.c", "Subject": "bill",
		"Content-Type": "multipart/mixed; boundary=b"}, multi), "orbit@example.com")
	if err != nil {
		t.Fatal(err)
	}
	if in.Text != "See attached" || len(in.Attachments) != 1 || in.Attachments[0].Name != "bill.pdf" || in.Attachments[0].Size != 5 {
		t.Fatalf("multipart %+v", in)
	}

	for hdr, v := range map[string]string{"Auto-Submitted": "auto-replied", "Precedence": "bulk",
		"Authentication-Results": "mail.example.com; dkim=none; dmarc=fail (p=none) header.from=gmail.com"} {
		in, _ = Parse(raw(map[string]string{"Message-ID": "<m3@x>", "From": "a@b.c", hdr: v}, "hi"), "orbit@example.com")
		if in.Rejected == "" {
			t.Errorf("%s: %s accepted", hdr, v)
		}
	}
	if _, err := Parse(raw(map[string]string{"From": "a@b.c"}, "hi"), "orbit@example.com"); err == nil {
		t.Fatal("a mail without a Message-ID must not parse")
	}
}

// TestTokenIsStable pins a token recorded before the key derivation moved to internal/secret: reply addresses already in people's mailboxes must keep resolving.
func TestTokenIsStable(t *testing.T) {
	key := []byte("0123456789abcdef0123456789abcdef-golden")
	conv := "7A1C2D3E-0000-4000-8000-000000000002"
	if got, want := Token(key, conv), "7a1c2d3e000040008000000000000002beda42d06e1b"; got != want {
		t.Fatalf("Token = %s, want %s", got, want)
	}
	if Conversation(key, "7a1c2d3e000040008000000000000002beda42d06e1b") != strings.ToLower(conv) {
		t.Fatal("a token signed before the change no longer resolves")
	}
}
