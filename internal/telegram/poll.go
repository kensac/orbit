package telegram

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/kensac/orbit/internal/spec"
	"github.com/kensac/orbit/internal/store"
)

const maxUpdateBytes = 128 << 10

// Poller receives Telegram updates and stores each before advancing its offset.
type Poller struct {
	Store  *store.Store
	Config func() *spec.Config
	Env    func(string) (string, bool)
	HTTP   *http.Client
	Log    *slog.Logger
	Wake   func()
}

func (p *Poller) Run(ctx context.Context) {
	var offset int64
	var lastToken string
	var lock *store.TelegramLock
	defer func() {
		if lock != nil {
			lock.Release(context.Background())
		}
	}()
	for ctx.Err() == nil {
		cfg := p.Config()
		tg := cfg.Deployment.Telegram
		if tg == nil {
			if !sleep(ctx, time.Minute) {
				return
			}
			continue
		}
		if lock == nil {
			var err error
			lock, err = p.Store.TryTelegramLock(ctx)
			if err != nil {
				p.log().Warn("telegram poll lock", "err", err)
				if !sleep(ctx, 10*time.Second) {
					return
				}
				continue
			}
			if lock == nil {
				if !sleep(ctx, 10*time.Second) {
					return
				}
				continue
			}
		}
		channel := cfg.Deployment.Channels[tg.Channel]
		token, ok := p.Env(channel.TokenEnv)
		if !ok || token == "" {
			p.log().Warn("telegram poll", "err", fmt.Sprintf("%s is not set", channel.TokenEnv))
			if !sleep(ctx, 10*time.Second) {
				return
			}
			continue
		}
		if token != lastToken {
			offset, lastToken = 0, token
		}
		bot, err := NewClient(token, p.HTTP)
		if err != nil {
			p.log().Warn("telegram poll", "err", err)
			if !sleep(ctx, 10*time.Second) {
				return
			}
			continue
		}
		updates, err := bot.GetUpdates(ctx, offset)
		if err != nil {
			if ctx.Err() == nil {
				p.log().Warn("telegram poll", "err", err)
			}
			if !sleep(ctx, 10*time.Second) {
				return
			}
			continue
		}
		if err := lock.Ping(ctx); err != nil {
			p.log().Warn("telegram poll lock", "err", err)
			lock.Release(context.Background())
			lock = nil
			continue
		}
		for _, update := range updates {
			done, err := p.Handle(ctx, cfg, update)
			if err != nil {
				if ctx.Err() == nil {
					p.log().Warn("telegram update", "update_id", update.Update.ID, "err", err)
				}
				break
			}
			if done && update.Update.ID >= offset {
				offset = update.Update.ID + 1
			}
		}
	}
}

// Handle records one update. An unsupported or unauthorized update is kept as
// ignored; a storage failure returns done=false so getUpdates retries it.
func (p *Poller) Handle(ctx context.Context, cfg *spec.Config, received ReceivedUpdate) (done bool, err error) {
	u := received.Update
	in := store.TelegramIn{UpdateID: u.ID, Raw: received.Raw}
	ignore := func(reason string) (bool, error) {
		if in.Raw == nil {
			in.Raw = []byte{}
		}
		if len(in.Raw) > maxUpdateBytes {
			in.Raw = in.Raw[:maxUpdateBytes]
			reason = "update exceeded the storage limit"
		}
		if err := p.Store.IgnoreTelegram(ctx, in, reason); err != nil {
			return false, err
		}
		p.log().Info("telegram update ignored", "update_id", u.ID, "reason", reason)
		return true, nil
	}
	if len(in.Raw) > maxUpdateBytes {
		return ignore("update exceeded the storage limit")
	}
	if _, seen, err := p.Store.TelegramSeen(ctx, u.ID); err != nil || seen {
		return err == nil, err
	}
	if u.Message == nil {
		return ignore("not a message update")
	}
	m := u.Message
	in.ChatID = strconv.FormatInt(m.Chat.ID, 10)
	in.MessageID = m.MessageID
	if m.From != nil {
		in.FromUserID = m.From.ID
	}
	telegramCfg := cfg.Deployment.Telegram
	if telegramCfg == nil {
		return ignore("Telegram reader is disabled")
	}
	if m.From == nil || m.From.IsBot {
		return ignore("sender is not a person")
	}
	if m.Chat.Type != "private" || m.Chat.ID != m.From.ID {
		return ignore("only private chats are accepted")
	}
	if !telegramCfg.AllowsUser(m.From.ID) {
		return ignore("sender is not in telegram.allow_users")
	}
	body := strings.TrimSpace(m.Text)
	if body == "" {
		return ignore("message has no text")
	}
	a := cfg.Agents[telegramCfg.Agent]
	if a == nil {
		return ignore("configured agent is unavailable")
	}
	conversationID := ""
	if m.ReplyTo != nil && m.ReplyTo.MessageID > 0 {
		conversationID, err = p.Store.TelegramConversation(ctx, in.ChatID, m.ReplyTo.MessageID)
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			return false, err
		}
		if conversationID != "" {
			conversation, err := p.Store.GetConversation(ctx, conversationID)
			if err != nil {
				if errors.Is(err, store.ErrNotFound) {
					return ignore("the replied-to conversation no longer exists")
				}
				return false, err
			}
			a = cfg.Agents[conversation.Agent]
			if a == nil {
				return ignore("the replied-to conversation's agent is not configured")
			}
		}
	}
	in.Agent, in.SpecSHA = a.Name, a.SHA
	in.ConversationID = conversationID
	in.Title = store.DefaultTitle(body)
	in.Body = body
	in.Author = "telegram:" + strconv.FormatInt(m.From.ID, 10)
	in.Wake = a.MessagesTrigger
	result, replayed, err := p.Store.IngestTelegram(ctx, in)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) || errors.Is(err, store.ErrInvalid) {
			return ignore(err.Error())
		}
		return false, err
	}
	p.log().Info("telegram message posted", "update_id", u.ID, "agent", a.Name,
		"conversation", result.ConversationID, "run", result.RunID, "replayed", replayed)
	if result.RunID != "" && !replayed && p.Wake != nil {
		p.Wake()
	}
	return true, nil
}

func sleep(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

func (p *Poller) log() *slog.Logger {
	if p.Log != nil {
		return p.Log
	}
	return slog.Default()
}
