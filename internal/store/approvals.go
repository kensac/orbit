package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// Approval states and scopes (docs/api.md, Approvals).
const (
	ApprovalPending   = "pending"
	ApprovalAllowed   = "allowed"
	ApprovalDenied    = "denied"
	ApprovalExpired   = "expired"
	ApprovalWithdrawn = "withdrawn"

	ScopeOnce         = "once"
	ScopeConversation = "conversation"

	ApprovalKindTool = "tool"
	ApprovalKindPlan = "plan"
)

// Approval is one permission request waiting for, or answered by, a person.
type Approval struct {
	ID             string          `json:"id"`
	RunID          string          `json:"run_id"`
	Attempt        int             `json:"attempt"`
	ConversationID string          `json:"conversation_id"`
	Agent          string          `json:"agent"`
	RequestID      string          `json:"request_id"`
	Kind           string          `json:"kind"`
	Tool           string          `json:"tool"`
	Input          json.RawMessage `json:"input"`
	// Level is the run's permission level when the CLI asked.
	Level              string          `json:"level"`
	DecisionReason     string          `json:"decision_reason"`
	DecisionReasonType string          `json:"decision_reason_type"`
	BlockedPath        string          `json:"blocked_path"`
	MatchedRule        json.RawMessage `json:"matched_rule"`
	Suggestions        json.RawMessage `json:"suggestions"`
	Title              string          `json:"title"`
	Description        string          `json:"description"`
	// ConversationGrant is false for an ask that may be allowed only once: a plan, or one the CLI marks as granting more than itself.
	ConversationGrant bool       `json:"conversation_grant"`
	State             string     `json:"state"`
	Scope             *string    `json:"scope"`
	Message           string     `json:"message"`
	DecidedBy         *string    `json:"decided_by"`
	DecidedAt         *time.Time `json:"decided_at"`
	ExpiresAt         time.Time  `json:"expires_at"`
	CreatedAt         time.Time  `json:"created_at"`
}

const approvalCols = `id::text, run_id::text, attempt, conversation_id::text, agent, request_id, kind, tool, input, level,
	decision_reason, decision_reason_type, blocked_path, matched_rule, suggestions, title, description, conversation_grant,
	state, scope, message, decided_by, decided_at, expires_at, created_at`

// scanApproval reads a row; a pending one past its expiry reads as expired, which it is whether or not its run got to record it.
func scanApproval(row pgx.Row) (Approval, error) {
	var a Approval
	err := row.Scan(&a.ID, &a.RunID, &a.Attempt, &a.ConversationID, &a.Agent, &a.RequestID, &a.Kind, &a.Tool, &a.Input, &a.Level,
		&a.DecisionReason, &a.DecisionReasonType, &a.BlockedPath, &a.MatchedRule, &a.Suggestions, &a.Title, &a.Description, &a.ConversationGrant,
		&a.State, &a.Scope, &a.Message, &a.DecidedBy, &a.DecidedAt, &a.ExpiresAt, &a.CreatedAt)
	if err == nil && a.State == ApprovalPending && !time.Now().Before(a.ExpiresAt) {
		a.State = ApprovalExpired
	}
	return a, err
}

// NewApproval is a permission request to put to a person.
type NewApproval struct {
	RunID, ConversationID, Agent, RequestID string
	Attempt                                 int
	Kind, Tool, Level                       string
	Input, MatchedRule, Suggestions         json.RawMessage
	DecisionReason, DecisionReasonType      string
	BlockedPath, Title, Description         string
	ConversationGrant                       bool
	ExpiresAt                               time.Time
}

// jsonOrNull stores a CLI value as given, an absent or null one as NULL.
func jsonOrNull(b json.RawMessage) any {
	if string(b) == "null" {
		return nil
	}
	return nullJSON(b)
}

// CreateApproval records a pending request. The same request of the same attempt is one row: a repeat returns it.
func (s *Store) CreateApproval(ctx context.Context, n NewApproval) (Approval, error) {
	if len(n.Input) == 0 {
		n.Input = json.RawMessage(`{}`)
	}
	return scanApproval(s.Pool.QueryRow(ctx, `INSERT INTO approvals (run_id, attempt, conversation_id, agent, request_id, kind, tool, input, level,
		decision_reason, decision_reason_type, blocked_path, matched_rule, suggestions, title, description, conversation_grant, expires_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18)
		ON CONFLICT (run_id, attempt, request_id) DO UPDATE SET request_id = EXCLUDED.request_id
		RETURNING `+approvalCols,
		n.RunID, n.Attempt, n.ConversationID, n.Agent, n.RequestID, n.Kind, n.Tool, n.Input, n.Level,
		n.DecisionReason, n.DecisionReasonType, n.BlockedPath, jsonOrNull(n.MatchedRule), jsonOrNull(n.Suggestions), n.Title, n.Description,
		n.ConversationGrant, n.ExpiresAt))
}

// GetApproval reads one approval.
func (s *Store) GetApproval(ctx context.Context, id string) (Approval, error) {
	if !IsUUID(id) {
		return Approval{}, ErrNotFound
	}
	a, err := scanApproval(s.Pool.QueryRow(ctx, `SELECT `+approvalCols+` FROM approvals WHERE id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return Approval{}, ErrNotFound
	}
	return a, err
}

// DecideApproval records a person's answer, once: ErrConflict when the approval was answered, withdrawn or expired first, ErrInvalid for a conversation scope it does not allow.
func (s *Store) DecideApproval(ctx context.Context, id string, allow bool, scope, message, by string) (Approval, error) {
	cur, err := s.GetApproval(ctx, id)
	if err != nil {
		return Approval{}, err
	}
	if scope == ScopeConversation && (!allow || !cur.ConversationGrant) {
		return Approval{}, fmt.Errorf("this request can only be decided once: %w", ErrInvalid)
	}
	state := ApprovalDenied
	if allow {
		state = ApprovalAllowed
	}
	a, err := scanApproval(s.Pool.QueryRow(ctx, `UPDATE approvals SET state = $2, scope = $3, message = $4, decided_by = $5, decided_at = now()
		WHERE id = $1 AND state = 'pending' AND expires_at > now() RETURNING `+approvalCols, id, state, scope, message, by))
	if errors.Is(err, pgx.ErrNoRows) {
		return Approval{}, ErrConflict
	}
	return a, err
}

// CloseApproval ends a pending approval nobody answered: expired (its time ran out) or withdrawn (the CLI or the run gave it up). It returns the row as it stands, whoever ended it.
func (s *Store) CloseApproval(ctx context.Context, id, state, message string) (Approval, error) {
	a, err := scanApproval(s.Pool.QueryRow(ctx, `UPDATE approvals SET state = $2, message = $3, decided_by = 'orbit', decided_at = now()
		WHERE id = $1 AND state = 'pending' RETURNING `+approvalCols, id, state, message))
	if errors.Is(err, pgx.ErrNoRows) {
		return s.GetApproval(ctx, id)
	}
	return a, err
}

// ListApprovals are a conversation's approvals, newest first, in one state when state is set ("pending" excludes expired ones); limit defaults to 50, at most 200.
func (s *Store) ListApprovals(ctx context.Context, conversationID, state string, limit int) ([]Approval, error) {
	limit = clampLimit(limit, 50, 200)
	out := []Approval{}
	if !IsUUID(conversationID) {
		return out, nil
	}
	q := `SELECT ` + approvalCols + ` FROM approvals WHERE conversation_id = $1 AND ($3 = ''
		OR ($3 = 'pending' AND state = 'pending' AND expires_at > now())
		OR ($3 = 'expired' AND (state = 'expired' OR (state = 'pending' AND expires_at <= now())))
		OR ($3 NOT IN ('pending', 'expired') AND state = $3))`
	rows, err := s.Pool.Query(ctx, q+` ORDER BY created_at DESC, id LIMIT $2`, conversationID, limit, state)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		a, err := scanApproval(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// ConversationGrants are the requests a person allowed for the rest of the conversation, oldest first.
func (s *Store) ConversationGrants(ctx context.Context, conversationID string) ([]Approval, error) {
	out := []Approval{}
	rows, err := s.Pool.Query(ctx, `SELECT `+approvalCols+` FROM approvals
		WHERE conversation_id = $1 AND state = 'allowed' AND scope = 'conversation' ORDER BY created_at`, conversationID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		a, err := scanApproval(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}
