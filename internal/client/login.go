package client

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Login signs in with a public authentik device client and returns only its access token.
// The caller must configure this issuer and client ID in Orbit's accepted audience list.
func Login(ctx context.Context, hc *http.Client, issuer, clientID string, notice io.Writer) (string, error) {
	if err := ValidateURL(issuer); err != nil {
		return "", err
	}
	if clientID == "" {
		return "", errors.New("client ID is required")
	}
	var doc struct {
		Issuer string `json:"issuer"`
		Device string `json:"device_authorization_endpoint"`
		Token  string `json:"token_endpoint"`
	}
	discoveryCtx, cancelDiscovery := context.WithTimeout(ctx, 30*time.Second)
	defer cancelDiscovery()
	req, _ := http.NewRequestWithContext(discoveryCtx, "GET", strings.TrimRight(issuer, "/")+"/.well-known/openid-configuration", nil)
	resp, err := hc.Do(req)
	if err != nil {
		return "", errors.New("issuer discovery failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 || json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&doc) != nil || doc.Issuer != issuer {
		return "", errors.New("invalid issuer discovery")
	}
	for _, ep := range []string{doc.Device, doc.Token} {
		if ValidateURL(ep) != nil {
			return "", errors.New("issuer does not advertise secure device endpoints")
		}
		u, _ := url.Parse(ep)
		i, _ := url.Parse(issuer)
		if u.Host != i.Host {
			return "", errors.New("device endpoints must share issuer host")
		}
	}
	var device struct {
		Code     string `json:"device_code"`
		User     string `json:"user_code"`
		URI      string `json:"verification_uri"`
		Expires  int    `json:"expires_in"`
		Interval int    `json:"interval"`
	}
	if err := form(ctx, hc, doc.Device, url.Values{"client_id": {clientID}, "scope": {"openid profile email"}}, &device); err != nil {
		return "", err
	}
	if device.Code == "" || device.User == "" || device.Expires <= 0 || ValidateURL(device.URI) != nil {
		return "", errors.New("invalid device authorization response")
	}
	fmt.Fprintf(notice, "Open %s and enter code %s\n", device.URI, device.User)
	interval := time.Duration(device.Interval) * time.Second
	if interval < 5*time.Second {
		interval = 5 * time.Second
	} // RFC 8628 defaults to five seconds.
	ctx, cancel := context.WithTimeout(ctx, time.Duration(device.Expires)*time.Second)
	defer cancel()
	for {
		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return "", ctx.Err()
		case <-timer.C:
		}
		var token struct {
			Access string `json:"access_token"`
			Type   string `json:"token_type"`
			Error  string `json:"error"`
		}
		if err := form(ctx, hc, doc.Token, url.Values{"client_id": {clientID}, "device_code": {device.Code}, "grant_type": {"urn:ietf:params:oauth:grant-type:device_code"}}, &token); err != nil {
			return "", err
		}
		switch token.Error {
		case "authorization_pending":
			continue
		case "slow_down":
			interval += 5 * time.Second
			continue
		case "":
			if token.Access != "" && strings.EqualFold(token.Type, "Bearer") {
				return token.Access, nil
			}
			return "", errors.New("invalid token response")
		default:
			return "", errors.New("device authorization refused or expired")
		}
	}
}

func form(ctx context.Context, hc *http.Client, endpoint string, values url.Values, out any) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "POST", endpoint, strings.NewReader(values.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := hc.Do(req)
	if err != nil {
		return errors.New("authorization connection failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 && resp.StatusCode != 400 {
		return fmt.Errorf("authorization returned HTTP %d", resp.StatusCode)
	}
	if json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(out) != nil {
		return errors.New("invalid authorization response")
	}
	return nil
}
