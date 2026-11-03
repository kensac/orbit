package store_test

import (
	"context"
	"testing"

	"github.com/kensac/orbit/internal/store"
	"github.com/kensac/orbit/internal/testdb"
)

func TestTelegramIngestIsIdempotentAndContinuesConversation(t *testing.T) {
	s := testdb.New(t)
	if err := s.SyncAgents(context.Background(), map[string]store.AgentSpec{"a": {SHA: "sha-a", JSON: []byte(`{"name":"a"}`), Enabled: true}}); err != nil {
		t.Fatal(err)
	}
	first, replayed, err := s.IngestTelegram(context.Background(), store.TelegramIn{UpdateID: 101, ChatID: 7, UserID: 9, Agent: "a", SpecSHA: "sha-a", Text: "hello", Title: "Telegram"})
	if err != nil || replayed || first.Outcome != "posted" {
		t.Fatalf("first=%+v replayed=%v err=%v", first, replayed, err)
	}
	again, replayed, err := s.IngestTelegram(context.Background(), store.TelegramIn{UpdateID: 101, ChatID: 7, UserID: 9, Agent: "a", SpecSHA: "sha-a", Text: "hello", Title: "Telegram"})
	if err != nil || !replayed || again.Outcome != "seen" {
		t.Fatalf("retry=%+v replayed=%v err=%v", again, replayed, err)
	}
	second, _, err := s.IngestTelegram(context.Background(), store.TelegramIn{UpdateID: 102, ChatID: 7, UserID: 9, Agent: "a", SpecSHA: "sha-a", Text: "again", Title: "Telegram"})
	if err != nil || second.ConversationID != first.ConversationID {
		t.Fatalf("second=%+v err=%v", second, err)
	}
	third, _, err := s.IngestTelegram(context.Background(), store.TelegramIn{UpdateID: 103, ChatID: 7, UserID: 9, Agent: "a", SpecSHA: "sha-a", Text: "new", Title: "Telegram", NewConversation: true})
	if err != nil || third.ConversationID == first.ConversationID {
		t.Fatalf("new=%+v err=%v", third, err)
	}
	telegram, err := s.TelegramForRun(context.Background(), second.RunID)
	if err != nil || telegram == nil || telegram.ChatID != 7 {
		t.Fatalf("route=%+v err=%v", telegram, err)
	}
}
