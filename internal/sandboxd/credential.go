package sandboxd

import (
	"errors"
	"fmt"
	"net/netip"
	"strconv"
	"time"

	"github.com/kensac/orbit/internal/credproxy"
)

// sealCredentials returns env with each backend credential replaced by a credproxy token: the credential sealed under CredProxyKey, usable only from this container's address on the sandbox network and only until the container's expiry. The process never sees the credential; code it runs can read the token, which the credential proxy alone can open and which is useless from anywhere else or later.
func (s *Server) sealCredentials(c containerState, env map[string]string) (map[string]string, error) {
	out := make(map[string]string, len(env))
	for k, v := range env {
		if k != credproxy.KindOAuth && k != credproxy.KindAPIKey {
			out[k] = v
			continue
		}
		if s.sealer == nil {
			return nil, errors.New("no credential proxy key: refusing to hand a sandbox the backend credential")
		}
		// The container's own network (a profile may choose one), on which the credential proxy sees its address.
		network := c.HostConfig.NetworkMode
		n, ok := c.NetworkSettings.Networks[network]
		if !ok {
			return nil, fmt.Errorf("sandbox is not on %s", network)
		}
		addr, err := netip.ParseAddr(n.IPAddress)
		if err != nil {
			return nil, fmt.Errorf("sandbox has no address on %s", network)
		}
		exp, err := strconv.ParseInt(c.Config.Labels[LabelExpires], 10, 64)
		if err != nil {
			return nil, errors.New("sandbox has no expiry")
		}
		tok, err := s.sealer.Seal(credproxy.Grant{Kind: k, Credential: v, Addr: addr, Expires: time.Unix(exp, 0),
			Sandbox: c.ID, RunID: c.Config.Labels[LabelRunID]})
		if err != nil {
			return nil, err
		}
		out[k] = tok
	}
	return out, nil
}
