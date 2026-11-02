package store

import (
	"context"
	"encoding/json"
)

// ConversationFacts is what a conversation did, read from its record rather than from a model: what "save this conversation as an agent" builds the agent from (engine/save.go).
type ConversationFacts struct {
	// Tools are the tools its runs called, by name, in the order first called.
	Tools []string `json:"tools"`
	// Fetched are the URLs its runs fetched with WebFetch, in the order first fetched.
	Fetched []string `json:"fetched"`
	// Approved are the action types a person approved in it (not a rule), sorted.
	Approved []string `json:"approved"`
	// Schedules are the schedule:add proposals made in it that no person rejected, oldest first.
	Schedules []ScheduleAsk `json:"schedules"`
}

// ScheduleAsk is one schedule:add proposal's params.
type ScheduleAsk struct {
	When   string `json:"when"`
	Prompt string `json:"prompt,omitempty"`
}

// ConversationFacts reads a conversation's facts. A tool call is a journal tool_call item's first revision; every run of the conversation counts, whatever its trigger.
func (s *Store) ConversationFacts(ctx context.Context, conversationID string) (*ConversationFacts, error) {
	f := &ConversationFacts{Tools: []string{}, Fetched: []string{}, Approved: []string{}, Schedules: []ScheduleAsk{}}
	rows, err := s.Pool.Query(ctx, `
		SELECT j.body->>'name', COALESCE(j.body->'input'->>'url', '')
		FROM journal_items j JOIN runs r ON r.id = j.run_id
		WHERE r.conversation_id = $1 AND j.kind = 'tool_call' AND j.revision = 1
		ORDER BY j.at, j.run_id, j.epoch, j.seq`, conversationID)
	if err != nil {
		return nil, err
	}
	seenTool, seenURL := map[string]bool{}, map[string]bool{}
	for rows.Next() {
		var name, url string
		if err := rows.Scan(&name, &url); err != nil {
			rows.Close()
			return nil, err
		}
		if name != "" && !seenTool[name] {
			seenTool[name] = true
			f.Tools = append(f.Tools, name)
		}
		if name == "WebFetch" && url != "" && !seenURL[url] {
			seenURL[url] = true
			f.Fetched = append(f.Fetched, url)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows, err = s.Pool.Query(ctx, `
		SELECT DISTINCT a.type FROM actions a JOIN runs r ON r.id = a.run_id
		WHERE r.conversation_id = $1 AND a.status IN ('approved', 'executing', 'executed', 'failed')
		  AND a.decided_by IS NOT NULL AND a.decided_by <> 'rule'
		ORDER BY a.type`, conversationID)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var t string
		if err := rows.Scan(&t); err != nil {
			rows.Close()
			return nil, err
		}
		f.Approved = append(f.Approved, t)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows, err = s.Pool.Query(ctx, `
		SELECT a.params FROM actions a JOIN runs r ON r.id = a.run_id
		WHERE r.conversation_id = $1 AND a.type = 'schedule:add' AND a.status <> 'rejected'
		ORDER BY a.created_at, a.id`, conversationID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var b []byte
		if err := rows.Scan(&b); err != nil {
			return nil, err
		}
		var sa ScheduleAsk
		if json.Unmarshal(b, &sa) == nil && sa.When != "" {
			f.Schedules = append(f.Schedules, sa)
		}
	}
	return f, rows.Err()
}
