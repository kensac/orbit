package usage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/kensac/orbit/internal/spec"
	"github.com/kensac/orbit/internal/store"
)

// oauthBeta is the beta header the endpoint requires of an OAuth token, as
// Claude Code sends it with every subscription request.
const oauthBeta = "oauth-2025-04-20"

// pollTimeout bounds one request: the endpoint answers in well under a
// second, and a poll that hangs only delays the next.
const pollTimeout = 20 * time.Second

// maxPollBody bounds the body kept: the answer is a few kilobytes.
const maxPollBody = 256 << 10

// idleCheck is how often a poller with no usage_poll configured looks
// again, so turning it on takes effect on reload without a restart.
const idleCheck = time.Minute

// Poller polls the subscription usage endpoint while usage_poll is
// configured and records every answer as a sample. Best effort: the
// endpoint is undocumented, and a failed poll is a recorded sample, never a
// failure of anything else.
type Poller struct {
	Store  *store.Store
	Client *http.Client
	Env    func(string) (string, bool)
	// Config is the deployment in force (it may change on reload).
	Config  func() *spec.Deployment
	Worker  string
	Version string
	Log     *slog.Logger
	Now     func() time.Time
}

// Run polls until ctx ends.
func (p *Poller) Run(ctx context.Context) {
	for {
		wait := idleCheck
		if cfg := p.Config().UsagePoll; cfg != nil {
			wait = time.Duration(cfg.Every)
			if err := p.maybePoll(ctx, cfg); err != nil && ctx.Err() == nil {
				p.Log.Warn("usage poll", "err", err)
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
	}
}

// maybePoll polls unless another instance did within half the interval
// (two run at once during a rollout), so the endpoint sees one caller.
func (p *Poller) maybePoll(ctx context.Context, cfg *spec.UsagePoll) error {
	_, last, err := p.Store.LatestUsageSamples(ctx)
	if err != nil {
		return err
	}
	if last != nil && p.now().Sub(last.PolledAt) < time.Duration(cfg.Every)/2 {
		return nil
	}
	status, body, pollErr := p.Poll(ctx, cfg)
	if pollErr != nil {
		p.Log.Warn("usage poll failed", "status", status, "err", pollErr)
	}
	return p.Store.RecordUsageSample(context.WithoutCancel(ctx), p.Worker, status, body, pollErr)
}

// Poll asks the endpoint once. It returns the HTTP status (0 when no
// response came) and the body when it is a JSON object.
func (p *Poller) Poll(ctx context.Context, cfg *spec.UsagePoll) (int, json.RawMessage, error) {
	tok, ok := p.Env(cfg.TokenEnv)
	if !ok || tok == "" {
		return 0, nil, fmt.Errorf("%s is not set", cfg.TokenEnv)
	}
	ctx, cancel := context.WithTimeout(ctx, pollTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, cfg.URL, nil)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("anthropic-beta", oauthBeta)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "orbit/"+p.Version)
	resp, err := p.Client.Do(req)
	if err != nil {
		// The error names the URL, never the header carrying the token.
		return 0, nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxPollBody+1))
	if err != nil {
		return resp.StatusCode, nil, err
	}
	if len(body) > maxPollBody {
		return resp.StatusCode, nil, fmt.Errorf("body is over %d bytes", maxPollBody)
	}
	if resp.StatusCode != http.StatusOK {
		return resp.StatusCode, nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, clip(body))
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(body, &obj); err != nil {
		return resp.StatusCode, nil, errors.New("body is not a JSON object")
	}
	return resp.StatusCode, body, nil
}

func (p *Poller) now() time.Time {
	if p.Now != nil {
		return p.Now()
	}
	return time.Now()
}

// clip keeps an error body short enough for a log line and a sample.
func clip(b []byte) string {
	const most = 300
	if len(b) > most {
		return string(b[:most]) + "..."
	}
	return string(b)
}
