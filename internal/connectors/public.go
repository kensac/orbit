package connectors

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"syscall"
	"time"

	"github.com/kensac/orbit/internal/egress"
)

// PublicClient makes a request only to public IP addresses and follows no
// redirect. Catalog endpoints and their discovered OAuth endpoints are
// untrusted URLs, so DNS is checked at the connection itself.
func PublicClient(parent *http.Client) *http.Client {
	if parent == nil {
		parent = &http.Client{}
	}
	if parent.Transport != nil {
		client := *parent
		client.CheckRedirect = func(*http.Request, []*http.Request) error {
			return errors.New("connector endpoints do not redirect")
		}
		return &client
	}
	dialer := &net.Dialer{Timeout: 10 * time.Second, Control: func(_, address string, _ syscall.RawConn) error {
		host, _, err := net.SplitHostPort(address)
		if err != nil {
			return err
		}
		ip, err := netip.ParseAddr(host)
		if err != nil || !egress.Public(ip) {
			return fmt.Errorf("connector address %q is not public", host)
		}
		return nil
	}}
	base := http.DefaultTransport.(*http.Transport).Clone()
	base.Proxy = nil
	base.DialContext = dialer.DialContext
	return &http.Client{Transport: base, Timeout: parent.Timeout, CheckRedirect: func(*http.Request, []*http.Request) error {
		return errors.New("connector endpoints do not redirect")
	}}
}
