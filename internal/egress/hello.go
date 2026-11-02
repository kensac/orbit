package egress

import (
	"bufio"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"time"
)

// helloTimeout bounds the wait for a client's first bytes on a 443 tunnel.
const helloTimeout = 10 * time.Second

// maxHello bounds what is read looking for a ClientHello: a real one is a few KB (post-quantum key shares included).
const maxHello = 64 << 10

var errSeen = errors.New("client hello seen")

// recorder keeps every byte read through it, so what the TLS parser consumed can be sent on unchanged.
type recorder struct {
	r   io.Reader
	got []byte
}

func (c *recorder) Read(p []byte) (int, error) {
	if len(c.got) >= maxHello {
		return 0, errors.New("client hello too large")
	}
	if room := maxHello - len(c.got); len(p) > room {
		p = p[:room]
	}
	n, err := c.r.Read(p)
	c.got = append(c.got, p[:n]...)
	return n, err
}

// helloConn is the net.Conn crypto/tls reads the ClientHello from: reads come from the recorder, writes are dropped (the handshake is abandoned before any reply).
type helloConn struct {
	net.Conn
	rec *recorder
}

func (h helloConn) Read(p []byte) (int, error)  { return h.rec.Read(p) }
func (h helloConn) Write(p []byte) (int, error) { return len(p), nil }

// peekHello reads a tunnel's opening bytes and, when they are a TLS ClientHello, its server name. It returns every byte it read, to be sent upstream first. crypto/tls parses the hello: the callback records the name and stops the handshake.
func peekHello(c net.Conn, br *bufio.Reader) (first []byte, sni string, isTLS bool) {
	_ = c.SetReadDeadline(time.Now().Add(helloTimeout))
	defer func() { _ = c.SetReadDeadline(time.Time{}) }()
	b, err := br.Peek(1)
	if err != nil || b[0] != 0x16 {
		// Not a TLS handshake record: nothing has been consumed.
		return nil, "", false
	}
	rec := &recorder{r: br}
	srv := tls.Server(helloConn{Conn: c, rec: rec}, &tls.Config{
		// The callback runs on the ClientHello, before any version is negotiated, so this floor never refuses a hello it reads.
		MinVersion: tls.VersionTLS12,
		GetConfigForClient: func(hi *tls.ClientHelloInfo) (*tls.Config, error) {
			sni, isTLS = hi.ServerName, true
			return nil, errSeen
		},
	})
	_ = srv.Handshake()
	return rec.got, sni, isTLS
}
