package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/kensac/orbit/internal/store"
)

// maxEntryWrites bounds the memory entry writes of one run: a conversation teaches a few durable things, not a form's worth, and a run that wants more writes the rest next time.
const maxEntryWrites = 10

// OutputEntry is one memory entry write in a run's output. Empty strings are absent fields: an update changes only the fields it fills.
type OutputEntry struct {
	// Op is add, update or retire.
	Op string `json:"op"`
	// Ref, for an add, is where the entry goes: profile, agent, or topic/<name>; for an update or retire, the entry's ref as the index shows it.
	Ref          string `json:"ref"`
	Type         string `json:"type"`
	Title        string `json:"title"`
	Description  string `json:"description"`
	Body         string `json:"body"`
	Why          string `json:"why"`
	HowToApply   string `json:"how_to_apply"`
	SupersededBy string `json:"superseded_by"`
	// Note says why, in a sentence: what the person said or did that this records.
	Note string `json:"note"`
}

// entriesSchema is the output's entries property. Every field is required (empty when unused), so backends whose structured output wants every property listed take it as is.
func entriesSchema() map[string]any {
	str := func(n int) map[string]any { return map[string]any{"type": "string", "maxLength": n} }
	return map[string]any{
		"type": "array", "maxItems": maxEntryWrites,
		"items": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"op":            map[string]any{"type": "string", "enum": []string{"add", "update", "retire"}},
				"ref":           str(200),
				"type":          map[string]any{"type": "string", "enum": append([]string{""}, store.EntryTypes...)},
				"title":         str(200),
				"description":   str(400),
				"body":          str(16 << 10),
				"why":           str(4 << 10),
				"how_to_apply":  str(4 << 10),
				"superseded_by": str(200),
				"note":          str(2 << 10),
			},
			"required":             []string{"op", "ref", "type", "title", "description", "body", "why", "how_to_apply", "superseded_by", "note"},
			"additionalProperties": false,
		},
	}
}

// withEntries adds the entries property to a schema; required makes the backend fill it (an empty list when there is nothing to keep), while the engine's own envelope leaves it optional.
func withEntries(schema json.RawMessage, required bool) json.RawMessage {
	var s map[string]any
	if json.Unmarshal(schema, &s) != nil {
		return schema
	}
	props, _ := s["properties"].(map[string]any)
	if props == nil {
		return schema
	}
	props["entries"] = entriesSchema()
	if req, _ := s["required"].([]any); required && !slices.Contains(req, any("entries")) {
		s["required"] = append(req, "entries")
	}
	b, _ := json.Marshal(s)
	return b
}

// refusedWrite is an entry write the engine refused before the store saw it.
type refusedWrite struct {
	Index int    `json:"index"`
	Op    string `json:"op"`
	Ref   string `json:"ref"`
	Error string `json:"error"`
}

// resolveEntryWrites turns a run's entry ops into store writes: an add's place must be the profile, the agent itself, or one of the topics it reads; an update or retire must name an entry the run was shown, at the version it was shown. What cannot be resolved is refused with its reason, and the rest still apply.
func resolveEntryWrites(agent string, topics []string, known []store.Entry, ops []OutputEntry) ([]*store.EntryWrite, []refusedWrite) {
	byRef := map[string]store.Entry{}
	for _, e := range known {
		byRef[entryRef(e)] = e
	}
	added := map[string]int{} // ref of an add of this run -> its write's index
	var writes []*store.EntryWrite
	var refused []refusedWrite
	refuse := func(i int, op OutputEntry, format string, args ...any) {
		refused = append(refused, refusedWrite{Index: i, Op: op.Op, Ref: op.Ref, Error: fmt.Sprintf(format, args...)})
	}
	opt := func(s string) *string {
		if strings.TrimSpace(s) == "" {
			return nil
		}
		return &s
	}
	for i, op := range ops {
		if i >= maxEntryWrites {
			refuse(i, op, "at most %d entry writes per run", maxEntryWrites)
			continue
		}
		ref := strings.TrimSpace(op.Ref)
		w := &store.EntryWrite{Op: op.Op, SupersededByIndex: -1, Note: op.Note}
		switch op.Op {
		case "add":
			var where store.EntryRef
			switch {
			case ref == store.ScopeProfile:
				where = store.EntryRef{Scope: store.ScopeProfile}
			case ref == store.ScopeAgent:
				where = store.EntryRef{Scope: store.ScopeAgent, Owner: agent}
			case strings.HasPrefix(ref, "topic/") && slices.Contains(topics, strings.TrimPrefix(ref, "topic/")):
				where = store.EntryRef{Scope: store.ScopeTopic, Owner: strings.TrimPrefix(ref, "topic/")}
			default:
				refuse(i, op, "an add's ref is profile, agent, or topic/<name> for a topic this agent reads (%s)", strings.Join(topics, ", "))
				continue
			}
			w.New = store.NewEntry{EntryRef: where, Key: store.EntryKey(op.Title), EntryFields: store.EntryFields{
				Type: op.Type, Title: op.Title, Description: op.Description, Body: op.Body, Why: op.Why, HowToApply: op.HowToApply}}
			added[entryRef(store.Entry{Scope: where.Scope, Topic: topicOf(where), Key: w.New.Key})] = len(writes)
		case "update", "retire":
			cur, ok := byRef[ref]
			if !ok {
				refuse(i, op, "no entry %q was shown to this run", ref)
				continue
			}
			w.ID, w.Expect = cur.ID, cur.Version
			if op.Op == "update" {
				w.Edit = store.EntryEdit{Type: opt(op.Type), Title: opt(op.Title), Description: opt(op.Description),
					Body: opt(op.Body), Why: opt(op.Why), HowToApply: opt(op.HowToApply)}
			} else if by := strings.TrimSpace(op.SupersededBy); by != "" {
				if e, ok := byRef[by]; ok {
					w.SupersededBy = &e.ID
				} else if j, ok := added[by]; ok {
					w.SupersededByIndex = j
				} else {
					refuse(i, op, "superseded_by %q is neither an entry shown to this run nor one it adds", by)
					continue
				}
			}
		default:
			refuse(i, op, "op must be add, update or retire")
			continue
		}
		writes = append(writes, w)
	}
	return writes, refused
}

func topicOf(r store.EntryRef) string {
	if r.Scope == store.ScopeTopic {
		return r.Owner
	}
	return ""
}

// entriesContract is the system prompt's part about writing entries.
func entriesContract(topics []string) string {
	where := "profile (about your person, read by every agent) or agent (yours alone)"
	if len(topics) > 0 {
		where = "profile (about your person, read by every agent), agent (yours alone), or topic/<name> for a topic you read (" + strings.Join(topics, ", ") + ")"
	}
	return `
- entries: durable things to remember beyond this run, written straight into orbit's memory entries (every version is kept; your person can see, edit and revert them). Add one when your person corrects you, states a lasting preference or decision, or tells you a fact about their world a later run would need. Not for anything about this run alone, and not a copy of your report. Return an empty list when there is nothing new.
  - add: ref is where it goes: ` + where + `. Give type (user: who they are; feedback: how they want work done; project: facts about their world and work; reference: where to find things), a short title, the body, a one-line description, and why and how_to_apply when you know them.
  - update: ref is the entry's ref from What you know; fill only the fields that change.
  - retire: ref as for update, when an entry is no longer true; superseded_by is the ref of the entry that replaces it (one you add here: its ref is the place, a slash, and its title in lower case with dashes).
  - Check What you know first: update an entry rather than add a near-duplicate. note says, in a sentence, what your person said or did that this records.
`
}

// recordEntryWrites stores what became of a run's entry writes as its entries.json artifact: each op with its outcome, in output order. The entries themselves name the run (GET /memory/entries?run_id=).
func (e *Engine) recordEntryWrites(ctx context.Context, c *store.ClaimedRun, ops []OutputEntry, writes []*store.EntryWrite, refused []refusedWrite) {
	if len(ops) == 0 {
		return
	}
	type row struct {
		Op     string                 `json:"op"`
		Ref    string                 `json:"ref"`
		Title  string                 `json:"title,omitempty"`
		Result store.EntryWriteResult `json:"result"`
	}
	rows := make([]row, len(ops))
	bad := map[int]string{}
	for _, r := range refused {
		bad[r.Index] = r.Error
	}
	next := 0
	for i, op := range ops {
		rows[i] = row{Op: op.Op, Ref: op.Ref, Title: op.Title}
		if msg, ok := bad[i]; ok {
			rows[i].Result = store.EntryWriteResult{Outcome: "refused", Error: msg}
		} else if next < len(writes) {
			rows[i].Result = writes[next].Result
			next++
		}
	}
	b, _ := json.MarshalIndent(rows, "", "  ")
	if err := e.Store.PutArtifact(context.WithoutCancel(ctx), c.ID, c.Attempt, "out", "entries.json", "application/json", b); err != nil {
		e.Log.Warn("record entry writes", "run", c.ID, "err", err)
	}
}
