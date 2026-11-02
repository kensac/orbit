// Package client implements the public Orbit API client.
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

// Client keeps the person's access token in memory only.
type Client struct {
	BaseURL, Token string
	HTTP           *http.Client
}

// HTTPClient refuses redirects so credentials never follow an API redirect.
func HTTPClient() *http.Client {
	return &http.Client{Transport: &http.Transport{Proxy: http.ProxyFromEnvironment, TLSHandshakeTimeout: 10 * time.Second, ResponseHeaderTimeout: 30 * time.Second}, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

// ValidateURL requires TLS and forbids embedded credentials and query parameters.
func ValidateURL(raw string) error {
	u, e := url.Parse(raw)
	if e != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return errors.New("endpoint must be an HTTPS URL without credentials, query or fragment")
	}
	return nil
}

// Request writes a JSON response or a run's event stream to out.
func (c *Client) Request(ctx context.Context, method, path string, body io.Reader, out io.Writer) error {
	if err := ValidateURL(c.BaseURL); err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(c.BaseURL, "/")+"/api/v1/"+path, body)
	if err != nil {
		return errors.New("invalid API request")
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return errors.New("API connection failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("API returned HTTP %d", resp.StatusCode)
	}
	_, err = io.Copy(out, resp.Body)
	return err
}

// Segment validates an identifier before placing it in an API path.
func Segment(s string) (string, error) {
	if s == "" || strings.ContainsAny(s, "/\\?#%") || s == "." || s == ".." {
		return "", errors.New("invalid identifier")
	}
	return url.PathEscape(s), nil
}

// Message encodes a message without permitting an author override.
func Message(text, id string) io.Reader {
	b, _ := json.Marshal(map[string]any{"body": text, "wake": true, "client_message_id": id})
	return strings.NewReader(string(b))
}
