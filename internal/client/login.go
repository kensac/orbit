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
	return login(ctx, hc, issuer, clientID, notice, waitPoll)
}

func waitPoll(ctx context.Context, interval time.Duration) error {
	timer := time.NewTimer(interval)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func login(ctx context.Context, hc *http.Client, issuer, clientID string, notice io.Writer, wait func(context.Context, time.Duration) error) (string, error) {
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
	if err := form(ctx, hc, doc.Device, url.Values{"client_id": {clientID}, "scope": {"openid profile email"}}, &device, false); err != nil {
		return "", err
	}
	if device.Code == "" || device.User == "" || device.Expires <= 0 || device.Expires > int(maxOAuthSeconds) || device.Interval < 0 || device.Interval > int(maxOAuthSeconds) || ValidateURL(device.URI) != nil {
		return "", errors.New("invalid device authorization response")
	}
	if _, err := fmt.Fprintf(notice, "Open %s and enter code %s\n", device.URI, device.User); err != nil {
		return "", err
	}
	interval := time.Duration(device.Interval) * time.Second
	if interval < 5*time.Second {
		interval = 5 * time.Second
	} // RFC 8628 defaults to five seconds.
	ctx, cancel := context.WithTimeout(ctx, time.Duration(device.Expires)*time.Second)
	defer cancel()
	for {
		if err := wait(ctx, interval); err != nil {
			return "", err
		}
		var token struct {
			Access string `json:"access_token"`
			Type   string `json:"token_type"`
			Error  string `json:"error"`
		}
		if err := form(ctx, hc, doc.Token, url.Values{"client_id": {clientID}, "device_code": {device.Code}, "grant_type": {"urn:ietf:params:oauth:grant-type:device_code"}}, &token, true); err != nil {
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

// maxOAuthSeconds leaves room for RFC 8628 slow-down increments without duration overflow.
const maxOAuthSeconds = int64((1<<63-1)/int64(time.Second)) / 2

func form(ctx context.Context, hc *http.Client, endpoint string, values url.Values, out any, allowOAuthError bool) error {
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
	var payload json.RawMessage
	if json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&payload) != nil {
		return errors.New("invalid authorization response")
	}
	if resp.StatusCode != http.StatusOK {
		var envelope struct {
			Error  string `json:"error"`
			Access string `json:"access_token"`
			Device string `json:"device_code"`
		}
		if !allowOAuthError || json.Unmarshal(payload, &envelope) != nil || envelope.Error == "" || envelope.Access != "" || envelope.Device != "" {
			return errors.New("invalid OAuth error response")
		}
	}
	if json.Unmarshal(payload, out) != nil {
		return errors.New("invalid authorization response")
	}
	return nil
}
