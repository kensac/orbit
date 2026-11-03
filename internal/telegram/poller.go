package telegram

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/kensac/orbit/internal/spec"
	"github.com/kensac/orbit/internal/store"
)

const maxUpdates = 100

type Poller struct {
	Store  *store.Store
	Config func() *spec.Config
	Env    func(string) (string, bool)
	Client *http.Client
	Log    *slog.Logger
	Wake   func()
	API    string
}

type update struct {
	ID      int64    `json:"update_id"`
	Message *message `json:"message"`
}
type message struct {
	Chat struct {
		ID   int64  `json:"id"`
		Type string `json:"type"`
	} `json:"chat"`
	From *struct {
		ID  int64 `json:"id"`
		Bot bool  `json:"is_bot"`
	} `json:"from"`
	Text string `json:"text"`
}
type response struct {
	OK          bool     `json:"ok"`
	Description string   `json:"description"`
	Result      []update `json:"result"`
}

func (p *Poller) Run(ctx context.Context) {
	for ctx.Err() == nil {
		cfg := p.Config()
		t := cfg.Deployment.Telegram
		if t == nil {
			wait(ctx, time.Minute)
			continue
		}
		lock, err := p.Store.TryTelegramLock(ctx)
		if err != nil {
			p.Log.Warn("telegram lock", "err", err)
			wait(ctx, time.Duration(t.Poll))
			continue
		}
		if lock == nil {
			wait(ctx, time.Duration(t.Poll))
			continue
		}
		pollCtx, cancel := context.WithCancel(ctx)
		finished := make(chan struct{})
		go func() {
			defer close(finished)
			ticker := time.NewTicker(10 * time.Second)
			defer ticker.Stop()
			for {
				select {
				case <-pollCtx.Done():
					return
				case <-ticker.C:
					if lock.Ping(pollCtx) != nil {
						cancel()
						return
					}
				}
			}
		}()
		token, _ := p.Env(cfg.Deployment.Channels[t.Channel].TokenEnv)
		if token == "" {
			p.Log.Warn("telegram poll", "err", "bot token is not set")
			cancel()
			<-finished
			lock.Release(ctx)
			wait(ctx, time.Duration(t.Poll))
			continue
		}
		if err := p.Poll(pollCtx, cfg, token); err != nil && ctx.Err() == nil {
			p.Log.Warn("telegram poll", "err", err)
		}
		cancel()
		<-finished
		lock.Release(ctx)
		wait(ctx, time.Duration(t.Poll))
	}
}

func (p *Poller) Poll(ctx context.Context, cfg *spec.Config, token string) error {
	t := cfg.Deployment.Telegram
	offset, err := p.Store.TelegramOffset(ctx)
	if err != nil {
		return err
	}
	api := p.API
	if api == "" {
		api = "https://api.telegram.org"
	}
	q := url.Values{"offset": {fmt.Sprint(offset)}, "timeout": {"25"}, "limit": {fmt.Sprint(maxUpdates)}}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(api, "/")+"/bot"+token+"/getUpdates?"+q.Encode(), nil)
	if err != nil {
		return fmt.Errorf("telegram get updates request could not be built")
	}
	client := p.Client
	if client == nil {
		client = &http.Client{Timeout: 35 * time.Second}
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("telegram get updates request failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("telegram get updates: %s", resp.Status)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return err
	}
	var envelope response
	if err = json.Unmarshal(b, &envelope); err != nil {
		return fmt.Errorf("telegram response: %w", err)
	}
	if !envelope.OK {
		return fmt.Errorf("telegram API: %s", envelope.Description)
	}
	for _, u := range envelope.Result {
		if err := ctx.Err(); err != nil {
			return err
		}
		m := u.Message
		if m == nil || m.From == nil || m.Chat.Type != "private" || m.From.Bot || !t.AllowsTelegramUser(m.From.ID) || strings.TrimSpace(m.Text) == "" {
			chat, user := int64(0), int64(0)
			text := ""
			if m != nil {
				chat = m.Chat.ID
				text = m.Text
				if m.From != nil {
					user = m.From.ID
				}
			}
			reason := "unsupported update"
			if m != nil && m.Chat.Type != "private" {
				reason = "only private chats are accepted"
			} else if m != nil && m.From != nil && !t.AllowsTelegramUser(m.From.ID) {
				reason = "user is not in telegram.allow_users"
			}
			if err := p.Store.IgnoreTelegram(ctx, u.ID, chat, user, reason, text); err != nil {
				return err
			}
			continue
		}
		text := strings.TrimSpace(m.Text)
		fresh := text == "/new" || strings.HasPrefix(text, "/new@")
		if fresh {
			text = "Start a new conversation."
		}
		res, replayed, err := p.Store.IngestTelegram(ctx, store.TelegramIn{UpdateID: u.ID, ChatID: m.Chat.ID, UserID: m.From.ID, Agent: t.Agent, SpecSHA: cfg.Agents[t.Agent].SHA, Text: text, Title: store.DefaultTitle("Telegram"), NewConversation: fresh})
		if err != nil {
			return err
		}
		p.Log.Info("telegram message posted", "update_id", u.ID, "chat_id", m.Chat.ID, "conversation", res.ConversationID, "run", res.RunID, "replayed", replayed)
		if res.RunID != "" && !replayed && p.Wake != nil {
			p.Wake()
		}
	}
	return nil
}

func wait(ctx context.Context, d time.Duration) {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
	case <-timer.C:
	}
}
