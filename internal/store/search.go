package store

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"
)

// Full-text search over search_docs (the search and search_code
// migrations): documents derived by triggers from messages, runs,
// run_events, memory_versions, memory_entry_versions, skill_versions and
// external_session_lines.

// SearchKinds are the kinds of document search returns, in the order the
// API documents them.
var SearchKinds = []string{"message", "question", "report", "error", "narration", "tool_call", "memory", "memory_entry", "skill", "session"}

// MaxSearchResults bounds one page of results: each carries a snippet, and a
// page is meant for a person or a model to read, not to export.
const MaxSearchResults = 100

// maxSearchOffset bounds paging into a ranking. Every page ranks every match
// again, so deep pages cost as much as the whole result set; past this a
// narrower query is the answer.
const maxSearchOffset = 1000

// SearchQuery is one search. Q is web-search syntax (websearch_to_tsquery:
// words, "quoted phrases", or, -excluded) with the operators agent:, kind:,
// repo: and path: (searchquery.go). Every other field narrows it; an operator
// and its field both apply.
type SearchQuery struct {
	Q     string
	Agent string
	// EntryTopics, with Agent, widen it to the memory entries the agent reads beside its own.
	EntryTopics    []string
	EntryProfile   bool
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
	Kind string `json:"kind"`
	// Agent is nil for a profile or topic memory entry, a skill, or an imported session line.
	Agent          *string `json:"agent"`
	ConversationID *string `json:"conversation_id"`
	RunID          *string `json:"run_id"`
	Attempt        *int    `json:"attempt"`
	Seq            *int    `json:"seq"`
	MessageID      *string `json:"message_id"`
	MemoryVersion  *int64  `json:"memory_version_id"`
	EntryID        *string `json:"entry_id"`
	EntryVersion   *int    `json:"entry_version"`
	Scope          *string `json:"scope"`
	Owner          *string `json:"owner"`
	Title          *string `json:"title"`
	Author         *string `json:"author"`
	Tool           *string `json:"tool"`
	// ExternalSessionID is the imported session a session line is of; Seq is then its line.
	ExternalSessionID *string `json:"external_session_id"`
	// Repo and Path are what a tool call's input names.
	Repo         *string       `json:"repo"`
	Path         *string       `json:"path"`
	Skill        *string       `json:"skill"`
	SkillVersion *int          `json:"skill_version"`
	At           time.Time     `json:"at"`
	Rank         float64       `json:"rank"`
	Snippet      string        `json:"snippet"`
	Parts        []SnippetPart `json:"snippet_parts"`
}

// SearchPage is a page of results, best first, and how many matched.
// Repaired is set when the query as written matched nothing and these
// results are for its repair.
type SearchPage struct {
	Total    int            `json:"total"`
	Results  []SearchResult `json:"results"`
	Repaired *SearchRepair  `json:"repaired"`
}

// SearchRepair is the query a search ran instead of one that matched
// nothing: Q, re-runnable as written, and each word it changed.
type SearchRepair struct {
	Q     string             `json:"q"`
	Words []SearchRepairWord `json:"words"`
}

// SearchRepairWord is one word of a repair, as written and as searched.
type SearchRepairWord struct {
	From string `json:"from"`
	To   string `json:"to"`
}

// Snippet delimiters ts_headline marks matches with: control characters,
// stripped from the text first, so a body can never forge a match.
const (
	snipStart = "\x01"
	snipStop  = "\x02"
)

// Search ranks the documents matching q.Q within q's filters. A document
// matches through either index: the english one (stemmed words) or the code
// one (identifier pieces, unstemmed: parse_config, parseConfig and "parse
// config" find each other); an excluded term excludes a document whichever
// matched. Rank is ts_rank_cd normalised by length, the better of the two,
// ties newest first. Of an agent's memory versions only the newest matching
// one is returned: each version is the whole document again, so older ones
// would crowd every result out. A memory entry or a skill is returned only as
// its current version, and not once retired. A query that matches nothing is
// repaired once, when a word of it is in no document and one close to it is
// (pg_trgm similarity against search_terms), and the page says so.
func (s *Store) Search(ctx context.Context, q SearchQuery) (*SearchPage, error) {
	p, err := parseSearch(q.Q)
	if err != nil {
		return nil, err
	}
	if !p.hasWords() {
		return nil, fmt.Errorf("%w: a query is required: words to find, besides any operators", ErrInvalid)
	}
	if p.agent != "" {
		if q.Agent != "" && q.Agent != p.agent {
			return nil, fmt.Errorf("%w: agent:%s and agent %s disagree", ErrInvalid, p.agent, q.Agent)
		}
		q.Agent = p.agent
	}
	for _, k := range slices.Concat(q.Kinds, p.kinds) {
		if !slices.Contains(SearchKinds, k) {
			return nil, fmt.Errorf("%w: kind %q is not one of %s", ErrInvalid, k, strings.Join(SearchKinds, ", "))
		}
	}
	if len(p.kinds) > 0 {
		if len(q.Kinds) > 0 {
			both := slices.DeleteFunc(slices.Clone(p.kinds), func(k string) bool { return !slices.Contains(q.Kinds, k) })
			if len(both) == 0 {
				return nil, fmt.Errorf("%w: kind:%s and kind %s share no kind", ErrInvalid,
					strings.Join(p.kinds, ","), strings.Join(q.Kinds, ","))
			}
			q.Kinds = both
		} else {
			q.Kinds = p.kinds
		}
	}
	page, err := s.search(ctx, q, p)
	if err != nil || page.Total > 0 || q.Offset > 0 {
		return page, err
	}
	fixed, words, err := s.repairSearch(ctx, p)
	if err != nil || len(words) == 0 {
		return page, err
	}
	again, err := s.search(ctx, q, fixed)
	if err != nil || again.Total == 0 {
		return page, err
	}
	again.Repaired = &SearchRepair{Q: strings.Join(slices.Concat([]string{fixed.text()}, fixed.ops), " "), Words: words}
	return again, nil
}

// search runs one parsed query.
func (s *Store) search(ctx context.Context, q SearchQuery, p parsedSearch) (*SearchPage, error) {
	limit := clampLimit(q.Limit, 20, MaxSearchResults)
	offset := min(max(q.Offset, 0), maxSearchOffset)
	var kinds []string
	if len(q.Kinds) > 0 {
		kinds = q.Kinds
	}
	entryTopics := q.EntryTopics
	if entryTopics == nil {
		entryTopics = []string{}
	}
	stop, err := s.stopWords(ctx, p)
	if err != nil {
		return nil, err
	}
	neg := p.negated()
	const headline = `'MaxFragments=2, MaxWords=30, MinWords=12, FragmentDelimiter=" ... ", StartSel=' || $10 || ', StopSel=' || $11`
	rows, err := s.Pool.Query(ctx, `
		WITH q AS (SELECT websearch_to_tsquery('english', $1) AS q, websearch_to_tsquery('simple', $12) AS c,
		                  websearch_to_tsquery('english', $13) AS ne, websearch_to_tsquery('simple', $14) AS nc,
		                  websearch_to_tsquery('simple', $1) AS s),
		m AS (
		    SELECT d.*, greatest(ts_rank_cd(d.tsv, q.q, 1), ts_rank_cd(d.code_tsv, q.c, 1)) AS rank,
		           CASE WHEN d.kind = 'memory'
		                THEN row_number() OVER (PARTITION BY d.agent, d.kind = 'memory' ORDER BY d.memory_version_id DESC)
		           END AS newest
		    FROM search_docs d, q
		    WHERE (d.tsv @@ q.q OR d.code_tsv @@ q.c)
		      AND NOT d.tsv @@ q.ne AND NOT d.code_tsv @@ q.nc
		      AND ($2 = '' OR d.agent = $2 OR (d.kind = 'memory_entry' AND d.agent IS NULL AND (
		          (d.scope = 'profile' AND $17::boolean) OR (d.scope = 'topic' AND d.owner = ANY ($18::text[])))))
		      AND ($3 = '' OR d.conversation_id = $3::uuid)
		      AND ($4 = '' OR d.run_id = $4::uuid)
		      AND ($5::text[] IS NULL OR d.kind = ANY ($5))
		      AND ($6::timestamptz IS NULL OR d.at >= $6)
		      AND ($7::timestamptz IS NULL OR d.at < $7)
		      AND ($15 = '' OR (regexp_replace(d.repo, '^.*/', '') = regexp_replace($15, '^.*/', '')
		                        AND (strpos(d.repo, '/') = 0 OR strpos($15, '/') = 0 OR d.repo = $15)))
		      AND ($16 = '' OR strpos(lower(d.path), lower($16)) > 0)
		      AND (d.kind <> 'memory_entry' OR EXISTS (
		          SELECT 1 FROM memory_entries e
		          JOIN memory_entry_versions v ON v.entry_id = e.id AND v.version = e.current_version
		          WHERE e.id = d.entry_id AND e.current_version = d.entry_version AND v.status = 'active'))
		      AND (d.kind <> 'skill' OR EXISTS (
		          SELECT 1 FROM skills k
		          JOIN skill_versions v ON v.skill = k.name AND v.version = k.current_version
		          WHERE k.name = d.skill AND k.current_version = d.skill_version AND v.status = 'active'))
		),
		page AS (
		    SELECT *, count(*) OVER () AS total FROM m WHERE newest IS NULL OR newest = 1
		    ORDER BY rank DESC, at DESC, id DESC LIMIT $8 OFFSET $9
		)
		SELECT p.total, p.kind, p.agent, p.conversation_id::text, p.run_id::text, p.attempt, p.seq,
		       p.message_id::text, p.memory_version_id, p.entry_id::text, p.entry_version, p.scope, p.owner, p.title,
		       p.author, p.tool, p.external_session_id::text, p.repo, p.path, p.skill, p.skill_version, p.at, p.rank::float8,
		       CASE WHEN strpos(h.e, $10) > 0 THEN h.e ELSE ts_headline('simple', b.t, q.c || q.s, `+headline+`) END
		FROM page p CROSS JOIN q
		CROSS JOIN LATERAL (SELECT translate(p.body, $10 || $11, '') AS t) b
		CROSS JOIN LATERAL (SELECT ts_headline('english', b.t, q.q, `+headline+`) AS e) h
		ORDER BY p.rank DESC, p.at DESC, p.id DESC`,
		p.text(), q.Agent, q.ConversationID, q.RunID, kinds, q.From, q.To, limit, offset, snipStart, snipStop,
		p.codeText(stop), neg.text(), neg.codeText(stop), p.repo, p.path, q.EntryProfile, entryTopics)
	if err != nil {
		return nil, err
	}
	page := &SearchPage{Results: []SearchResult{}}
	for rows.Next() {
		var r SearchResult
		var headline string
		if err := rows.Scan(&page.Total, &r.Kind, &r.Agent, &r.ConversationID, &r.RunID, &r.Attempt, &r.Seq,
			&r.MessageID, &r.MemoryVersion, &r.EntryID, &r.EntryVersion, &r.Scope, &r.Owner, &r.Title,
			&r.Author, &r.Tool, &r.ExternalSessionID, &r.Repo, &r.Path, &r.Skill, &r.SkillVersion, &r.At, &r.Rank, &headline); err != nil {
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
		return s.searchTotal(ctx, q, p, page)
	}
	return page, nil
}

// stopWords are the query's plain words that the english configuration
// drops, asked of the database so the list is the one the english index uses.
func (s *Store) stopWords(ctx context.Context, p parsedSearch) (map[string]bool, error) {
	var words []string
	for _, t := range p.terms {
		if !t.phrase && len(t.words) == 1 {
			words = append(words, strings.ToLower(t.words[0]))
		}
	}
	stop := map[string]bool{}
	if len(words) == 0 {
		return stop, nil
	}
	rows, err := s.Pool.Query(ctx, `SELECT w FROM unnest($1::text[]) w WHERE numnode(plainto_tsquery('english', w)) = 0`, words)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var w string
		if err := rows.Scan(&w); err != nil {
			return nil, err
		}
		stop[w] = true
	}
	return stop, rows.Err()
}

// repairSearch replaces each piece of the query's wanted words (not those it
// excludes) that is in no document with the term most like it (pg_trgm
// similarity, at least pg_trgm.similarity_threshold), keeping a leading
// capital. A piece is a candidate when it is 3 to 32 ASCII letters, as
// search_terms' rows are. No candidate, or none with a close term, is no
// repair.
func (s *Store) repairSearch(ctx context.Context, p parsedSearch) (parsedSearch, []SearchRepairWord, error) {
	var cands []string
	for _, t := range p.terms {
		if t.neg || t.or {
			continue
		}
		for _, w := range t.words {
			for _, pc := range codePieces(w) {
				if piece := strings.ToLower(w[pc[0]:pc[1]]); termShape(piece) && !slices.Contains(cands, piece) {
					cands = append(cands, piece)
				}
			}
		}
	}
	if len(cands) == 0 {
		return p, nil, nil
	}
	rows, err := s.Pool.Query(ctx, `
		SELECT w, b.term FROM unnest($1::text[]) w
		CROSS JOIN LATERAL (SELECT t.term FROM search_terms t WHERE t.term % w
		                    ORDER BY similarity(t.term, w) DESC, t.term LIMIT 1) b
		WHERE NOT EXISTS (SELECT 1 FROM search_terms t WHERE t.term = w)`, cands)
	if err != nil {
		return p, nil, err
	}
	fix := map[string]string{}
	for rows.Next() {
		var from, to string
		if err := rows.Scan(&from, &to); err != nil {
			rows.Close()
			return p, nil, err
		}
		fix[from] = to
	}
	if err := rows.Err(); err != nil || len(fix) == 0 {
		return p, nil, err
	}
	var words []SearchRepairWord
	out := p
	out.terms = slices.Clone(p.terms)
	for i, t := range out.terms {
		if t.neg || t.or {
			continue
		}
		t.words = slices.Clone(t.words)
		for j, w := range t.words {
			fixed := w
			spans := codePieces(w)
			for k := len(spans) - 1; k >= 0; k-- {
				pc := spans[k]
				to, ok := fix[strings.ToLower(w[pc[0]:pc[1]])]
				if !ok {
					continue
				}
				if isUpper(w[pc[0]]) {
					if pc[1]-pc[0] > 1 && isUpper(w[pc[1]-1]) {
						to = strings.ToUpper(to)
					} else {
						to = strings.ToUpper(to[:1]) + to[1:]
					}
				}
				fixed = fixed[:pc[0]] + to + fixed[pc[1]:]
			}
			if fixed != w {
				t.words[j] = fixed
				words = append(words, SearchRepairWord{From: w, To: fixed})
			}
		}
		out.terms[i] = t
	}
	return out, words, nil
}

// termShape is the shape of a search_terms row (the search_code migration).
func termShape(w string) bool {
	if len(w) < 3 || len(w) > 32 {
		return false
	}
	for i := 0; i < len(w); i++ {
		if !isLower(w[i]) {
			return false
		}
	}
	return true
}

// searchTotal counts q's matches for a page past the end, so total stays
// true whatever the offset.
func (s *Store) searchTotal(ctx context.Context, q SearchQuery, p parsedSearch, page *SearchPage) (*SearchPage, error) {
	q.Offset, q.Limit = 0, 1
	first, err := s.search(ctx, q, p)
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
