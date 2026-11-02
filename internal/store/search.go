package store

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"
)

// Full-text search over search_docs (the search migration): documents
// derived by triggers from messages, runs, run_events and memory_versions.

// SearchKinds are the kinds of document search returns, in the order the
// API documents them.
var SearchKinds = []string{"message", "question", "report", "error", "narration", "tool_call", "memory"}

// MaxSearchResults bounds one page of results: each carries a snippet, and a
// page is meant for a person or a model to read, not to export.
const MaxSearchResults = 100

// maxSearchOffset bounds paging into a ranking. Every page ranks every match
// again, so deep pages cost as much as the whole result set; past this a
// narrower query is the answer.
const maxSearchOffset = 1000

// SearchQuery is one search. Q is web-search syntax (websearch_to_tsquery:
// words, "quoted phrases", or, -excluded). Every other field narrows it.
type SearchQuery struct {
	Q              string
	Agent          string
	ConversationID string
	RunID          string
	Kinds          []string
	From, To       *time.Time // [From, To)
	Limit, Offset  int
}

// SnippetPart is a stretch of a snippet, matching the query or not.
type SnippetPart struct {
	Text  string `json:"text"`
	Match bool   `json:"match"`
}

// SearchResult is one matching document.
type SearchResult struct {
	Kind           string        `json:"kind"`
	Agent          string        `json:"agent"`
	ConversationID *string       `json:"conversation_id"`
	RunID          *string       `json:"run_id"`
	Attempt        *int          `json:"attempt"`
	Seq            *int          `json:"seq"`
	MessageID      *string       `json:"message_id"`
	MemoryVersion  *int64        `json:"memory_version_id"`
	Author         *string       `json:"author"`
	Tool           *string       `json:"tool"`
	At             time.Time     `json:"at"`
	Rank           float64       `json:"rank"`
	Snippet        string        `json:"snippet"`
	Parts          []SnippetPart `json:"snippet_parts"`
}

// SearchPage is a page of results, best first, and how many matched.
type SearchPage struct {
	Total   int            `json:"total"`
	Results []SearchResult `json:"results"`
}

// Snippet delimiters ts_headline marks matches with: control characters,
// stripped from the text first, so a body can never forge a match.
const (
	snipStart = "\x01"
	snipStop  = "\x02"
)

// Search ranks the documents matching q.Q (ts_rank_cd, normalised by length;
// ties newest first) within q's filters. Of an agent's memory versions only
// the newest matching one is returned: each version is the whole document
// again, so older ones would crowd every result out. A query that has no
// searchable words (only stop words) matches nothing.
func (s *Store) Search(ctx context.Context, q SearchQuery) (*SearchPage, error) {
	if strings.TrimSpace(q.Q) == "" {
		return nil, fmt.Errorf("%w: a query is required", ErrInvalid)
	}
	for _, k := range q.Kinds {
		if !slices.Contains(SearchKinds, k) {
			return nil, fmt.Errorf("%w: kind %q is not one of %s", ErrInvalid, k, strings.Join(SearchKinds, ", "))
		}
	}
	limit := clampLimit(q.Limit, 20, MaxSearchResults)
	offset := min(max(q.Offset, 0), maxSearchOffset)
	var kinds []string
	if len(q.Kinds) > 0 {
		kinds = q.Kinds
	}
	rows, err := s.Pool.Query(ctx, `
		WITH q AS (SELECT websearch_to_tsquery('english', $1) AS q),
		m AS (
		    SELECT d.*, ts_rank_cd(d.tsv, q.q, 1) AS rank,
		           CASE WHEN d.kind = 'memory'
		                THEN row_number() OVER (PARTITION BY d.agent, d.kind = 'memory' ORDER BY d.memory_version_id DESC)
		           END AS newest
		    FROM search_docs d, q
		    WHERE d.tsv @@ q.q
		      AND ($2 = '' OR d.agent = $2)
		      AND ($3 = '' OR d.conversation_id = $3::uuid)
		      AND ($4 = '' OR d.run_id = $4::uuid)
		      AND ($5::text[] IS NULL OR d.kind = ANY ($5))
		      AND ($6::timestamptz IS NULL OR d.at >= $6)
		      AND ($7::timestamptz IS NULL OR d.at < $7)
		),
		page AS (
		    SELECT *, count(*) OVER () AS total FROM m WHERE newest IS NULL OR newest = 1
		    ORDER BY rank DESC, at DESC, id DESC LIMIT $8 OFFSET $9
		)
		SELECT p.total, p.kind, p.agent, p.conversation_id::text, p.run_id::text, p.attempt, p.seq,
		       p.message_id::text, p.memory_version_id, p.author, p.tool, p.at, p.rank::float8,
		       ts_headline('english', translate(p.body, $10 || $11, ''), q.q,
		           'MaxFragments=2, MaxWords=30, MinWords=12, FragmentDelimiter=" ... ", StartSel=' || $10 || ', StopSel=' || $11)
		FROM page p, q
		ORDER BY p.rank DESC, p.at DESC, p.id DESC`,
		q.Q, q.Agent, q.ConversationID, q.RunID, kinds, q.From, q.To, limit, offset, snipStart, snipStop)
	if err != nil {
		return nil, err
	}
	page := &SearchPage{Results: []SearchResult{}}
	for rows.Next() {
		var r SearchResult
		var headline string
		if err := rows.Scan(&page.Total, &r.Kind, &r.Agent, &r.ConversationID, &r.RunID, &r.Attempt, &r.Seq,
			&r.MessageID, &r.MemoryVersion, &r.Author, &r.Tool, &r.At, &r.Rank, &headline); err != nil {
			rows.Close()
			return nil, err
		}
		r.Snippet, r.Parts = snippetParts(headline)
		page.Results = append(page.Results, r)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(page.Results) == 0 && offset > 0 {
		// Past the last page the window count is gone with the rows.
		return s.searchTotal(ctx, q, page)
	}
	return page, nil
}

// searchTotal counts q's matches for a page past the end, so total stays
// true whatever the offset.
func (s *Store) searchTotal(ctx context.Context, q SearchQuery, page *SearchPage) (*SearchPage, error) {
	q.Offset, q.Limit = 0, 1
	first, err := s.Search(ctx, q)
	if err != nil {
		return nil, err
	}
	page.Total = first.Total
	return page, nil
}

// snippetParts splits a ts_headline result at its match delimiters into the
// plain snippet and its parts.
func snippetParts(h string) (string, []SnippetPart) {
	parts := []SnippetPart{}
	var plain strings.Builder
	match := false
	for h != "" {
		delim := snipStart
		if match {
			delim = snipStop
		}
		i := strings.Index(h, delim)
		seg := h
		if i >= 0 {
			seg = h[:i]
		}
		// A stray delimiter of the other kind cannot be in the text (the
		// body was stripped of both); drop one defensively.
		seg = strings.NewReplacer(snipStart, "", snipStop, "").Replace(seg)
		if seg != "" {
			if n := len(parts); n > 0 && parts[n-1].Match == match {
				parts[n-1].Text += seg
			} else {
				parts = append(parts, SnippetPart{Text: seg, Match: match})
			}
			plain.WriteString(seg)
		}
		if i < 0 {
			break
		}
		h = h[i+len(delim):]
		match = !match
	}
	return plain.String(), parts
}

// SearchBound reads one end of a search's date range: a calendar day in loc
// (its midnight, or for an end the next one, so the day is included) or an
// RFC 3339 instant. Empty is no bound.
func SearchBound(v string, loc *time.Location, end bool) (*time.Time, error) {
	if v == "" {
		return nil, nil
	}
	if d, err := time.ParseInLocation(time.DateOnly, v, loc); err == nil {
		if end {
			d = d.AddDate(0, 0, 1)
		}
		return &d, nil
	}
	t, err := time.Parse(time.RFC3339, v)
	if err != nil {
		return nil, fmt.Errorf("want YYYY-MM-DD or an RFC 3339 time, got %q", v)
	}
	return &t, nil
}
