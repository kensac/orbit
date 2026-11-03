// Package telegram implements the bounded Telegram Bot API client and the
// update reader that turns allowed private messages into Orbit conversations.
package telegram

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
)

const (
	apiBase      = "https://api.telegram.org"
	maxResponse  = 4 << 20
	longPollSec  = 25
	maxTextRunes = 4096
)

var tokenRE = regexp.MustCompile(`^[0-9]{5,}:[A-Za-z0-9_-]{20,}$`)

// Client talks only to Telegram's fixed HTTPS API. It never follows redirects
// because the bot token is part of the request path.
type Client struct {
	token string
	http  *http.Client
}

func NewClient(token string, client *http.Client) (*Client, error) {
	if !tokenRE.MatchString(token) {
		return nil, errors.New("telegram bot token has an invalid format")
	}
	if client == nil {
		client = http.DefaultClient
	}
	c := *client
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &Client{token: token, http: &c}, nil
}

// User is the Telegram sender. IDs, not usernames, are used for authorization.
type User struct {
	ID    int64 `json:"id"`
	IsBot bool  `json:"is_bot"`
}

type Chat struct {
	ID   int64  `json:"id"`
	Type string `json:"type"`
}

type Reply struct {
	MessageID int64 `json:"message_id"`
}

type Message struct {
	MessageID int64  `json:"message_id"`
	Date      int64  `json:"date"`
	Text      string `json:"text"`
	From      *User  `json:"from"`
	Chat      Chat   `json:"chat"`
	ReplyTo   *Reply `json:"reply_to_message"`
}

type Update struct {
	ID      int64    `json:"update_id"`
	Message *Message `json:"message"`
}

// ReceivedUpdate holds the typed update and its original JSON for the append-only inbox record.
type ReceivedUpdate struct {
	Update Update
	Raw    []byte
}

type MessageRef struct {
	ChatID    string
	MessageID int64
}

func (c *Client) GetUpdates(ctx context.Context, offset int64) ([]ReceivedUpdate, error) {
	q := url.Values{
		"offset":          {strconv.FormatInt(offset, 10)},
		"timeout":         {strconv.Itoa(longPollSec)},
		"allowed_updates": {`["message"]`},
	}
	raw, err := c.call(ctx, "getUpdates", q, nil)
	if err != nil {
		return nil, err
	}
	var response struct {
		OK     bool              `json:"ok"`
		Result []json.RawMessage `json:"result"`
	}
	if err := json.Unmarshal(raw, &response); err != nil || !response.OK {
		return nil, errors.New("telegram getUpdates returned an invalid response")
	}
	updates := make([]ReceivedUpdate, 0, len(response.Result))
	for _, b := range response.Result {
		var update Update
		if err := json.Unmarshal(b, &update); err != nil {
			return nil, errors.New("telegram getUpdates returned an invalid update")
		}
		updates = append(updates, ReceivedUpdate{Update: update, Raw: append([]byte(nil), b...)})
	}
	return updates, nil
}

func (c *Client) SendMessage(ctx context.Context, chatID, text string, replyTo int64) (MessageRef, error) {
	if chatID == "" {
		return MessageRef{}, errors.New("telegram chat id is empty")
	}
	text = truncateRunes(text, maxTextRunes)
	body := map[string]any{"chat_id": chatID, "text": text, "disable_web_page_preview": true}
	if replyTo > 0 {
		body["reply_parameters"] = map[string]any{"message_id": replyTo, "allow_sending_without_reply": true}
	}
	b, err := json.Marshal(body)
	if err != nil {
		return MessageRef{}, errors.New("telegram message could not be encoded")
	}
	raw, err := c.call(ctx, "sendMessage", nil, b)
	if err != nil {
		return MessageRef{}, err
	}
	var response struct {
		OK     bool `json:"ok"`
		Result struct {
			MessageID int64 `json:"message_id"`
			Chat      Chat  `json:"chat"`
		} `json:"result"`
	}
	if err := json.Unmarshal(raw, &response); err != nil || !response.OK || response.Result.MessageID <= 0 {
		return MessageRef{}, errors.New("telegram sendMessage returned an invalid response")
	}
	return MessageRef{ChatID: strconv.FormatInt(response.Result.Chat.ID, 10), MessageID: response.Result.MessageID}, nil
}

func (c *Client) call(ctx context.Context, method string, query url.Values, body []byte) ([]byte, error) {
	u := apiBase + "/bot" + c.token + "/" + method
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, reader)
	if err != nil {
		return nil, fmt.Errorf("telegram %s request could not be built", method)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		// The standard transport error includes the full request URL, which has the bot token.
		return nil, fmt.Errorf("telegram %s request failed", method)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponse+1))
	if err != nil || len(raw) > maxResponse {
		return nil, fmt.Errorf("telegram %s response was unreadable or too large", method)
	}
	if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("telegram %s failed with HTTP %d", method, resp.StatusCode)
	}
	var ok struct {
		OK bool `json:"ok"`
	}
	if json.Unmarshal(raw, &ok) != nil || !ok.OK {
		return nil, fmt.Errorf("telegram %s was rejected", method)
	}
	return raw, nil
}

func truncateRunes(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return strings.TrimRight(string(r[:max-1]), " \n\t") + "…"
}
