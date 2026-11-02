package notify

import (
	"bufio"
	"context"
	"net"
	"strconv"
	"strings"
	"testing"
)

// fakeSMTP accepts one mail without TLS or auth and returns its DATA.
func fakeSMTP(t *testing.T) (host string, port int, data <-chan string) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	ch := make(chan string, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		r := bufio.NewReader(c)
		say := func(s string) { _, _ = c.Write([]byte(s + "\r\n")) }
		say("220 fake")
		for {
			line, err := r.ReadString('\n')
			if err != nil {
				return
			}
			switch cmd := strings.ToUpper(strings.TrimSpace(line)); {
			case strings.HasPrefix(cmd, "EHLO"), strings.HasPrefix(cmd, "HELO"):
				say("250 fake")
			case cmd == "DATA":
				say("354 go")
				var b strings.Builder
				for {
					l, err := r.ReadString('\n')
					if err != nil || l == ".\r\n" {
						break
					}
					b.WriteString(l)
				}
				ch <- b.String()
				say("250 ok")
			case cmd == "QUIT":
				say("221 bye")
				return
			default:
				say("250 ok")
			}
		}
	}()
	h, p, _ := net.SplitHostPort(ln.Addr().String())
	n, _ := strconv.Atoi(p)
	return h, n, ch
}

func TestSMTPMailHeaders(t *testing.T) {
	host, port, data := fakeSMTP(t)
	ch := &smtpChannel{host: host, port: port, from: "orbit <orbit@example.com>", to: []string{"me@example.com"}}
	err := ch.Send(context.Background(), Message{Title: "finance: October\r\nBcc: x@evil.test", Body: "You spent $84.",
		ReplyTo: "orbit+tok@example.com", MessageID: "<orbit.tok.1@example.com>", InReplyTo: "<r1@mail>",
		References: "<orbit.tok@example.com> <r1@mail>"})
	if err != nil {
		t.Fatal(err)
	}
	got := <-data
	for _, want := range []string{"\r\nReply-To: orbit+tok@example.com\r\n", "\r\nMessage-ID: <orbit.tok.1@example.com>\r\n",
		"\r\nIn-Reply-To: <r1@mail>\r\n", "\r\nReferences: <orbit.tok@example.com> <r1@mail>\r\n", "\r\nSubject: finance: October Bcc: x@evil.test\r\n"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in\n%s", want, got)
		}
	}
	if strings.Contains(got, "\r\nBcc:") {
		t.Fatalf("a title added a header:\n%s", got)
	}
}
