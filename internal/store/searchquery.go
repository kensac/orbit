package store

import (
	"fmt"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"
)

// The query language of Store.Search: websearch_to_tsquery's syntax (words,
// "quoted phrases", or between alternatives, -excluded) plus operators,
// key:value terms code takes out of the query before Postgres sees it.

// searchOps are the operators a query may carry. kind may repeat (or list
// kinds with commas); the others name one value.
var searchOps = []string{"agent", "kind", "repo", "path"}

// searchTerm is one term of a query: a word, a quoted phrase (its words), or
// the or operator.
type searchTerm struct {
	words  []string
	phrase bool
	neg    bool
	or     bool
}

// parsedSearch is a query split into its terms and operators.
type parsedSearch struct {
	terms []searchTerm
	agent string
	kinds []string
	repo  string
	path  string
	// ops are the operator tokens as written, for a repaired query.
	ops []string
}

// parseSearch reads q. A token is an operator when it is a whole unquoted
// token whose key is one of searchOps (agent:finance, kind:report,tool_call,
// path:"a dir/x.go"); any other key:value is text, so a URL stays a URL.
func parseSearch(q string) (parsedSearch, error) {
	var p parsedSearch
	seen := map[string]bool{}
	for i := 0; i < len(q); {
		r, size := utf8.DecodeRuneInString(q[i:])
		if unicode.IsSpace(r) {
			i += size
			continue
		}
		neg := false
		if q[i] == '-' && i+1 < len(q) && !unicode.IsSpace(rune(q[i+1])) {
			neg = true
			i++
		}
		if q[i] == '"' {
			rest := q[i+1:]
			body := rest
			if end := strings.IndexByte(rest, '"'); end >= 0 {
				body = rest[:end]
				i += end + 2
			} else {
				i = len(q)
			}
			p.terms = append(p.terms, searchTerm{words: strings.Fields(body), phrase: true, neg: neg})
			continue
		}
		start := i
		for i < len(q) {
			r, size := utf8.DecodeRuneInString(q[i:])
			if unicode.IsSpace(r) {
				break
			}
			if q[i] == '"' {
				// An operator's quoted value (path:"a dir/x") is part of
				// its token; anywhere else a quote starts a phrase.
				if k, v, ok := strings.Cut(q[start:i], ":"); ok && v == "" && !neg && slices.Contains(searchOps, strings.ToLower(k)) {
					end := strings.IndexByte(q[i+1:], '"')
					if end < 0 {
						i = len(q)
					} else {
						i += end + 2
					}
					continue
				}
				break
			}
			i += size
		}
		tok := q[start:i]
		if k, v, ok := strings.Cut(tok, ":"); ok && !neg && slices.Contains(searchOps, strings.ToLower(k)) {
			v = strings.Trim(v, `"`)
			k = strings.ToLower(k)
			if strings.TrimSpace(v) == "" {
				return p, fmt.Errorf("%w: %s: needs a value", ErrInvalid, k)
			}
			if k != "kind" && seen[k] {
				return p, fmt.Errorf("%w: %s: may appear once", ErrInvalid, k)
			}
			seen[k] = true
			p.ops = append(p.ops, tok)
			switch k {
			case "agent":
				p.agent = v
			case "kind":
				for kind := range strings.SplitSeq(v, ",") {
					if kind = strings.TrimSpace(kind); kind != "" && !slices.Contains(p.kinds, kind) {
						p.kinds = append(p.kinds, kind)
					}
				}
			case "repo":
				p.repo = strings.ToLower(v)
			case "path":
				p.path = v
			}
			continue
		}
		if !neg && strings.EqualFold(tok, "or") {
			p.terms = append(p.terms, searchTerm{or: true})
			continue
		}
		p.terms = append(p.terms, searchTerm{words: []string{tok}, neg: neg})
	}
	return p, nil
}

// hasWords says whether the query has anything to match besides operators.
func (p parsedSearch) hasWords() bool {
	for _, t := range p.terms {
		if !t.or && len(t.words) > 0 {
			return true
		}
	}
	return false
}

// text is the query for the english index: the terms as written, without
// the operators.
func (p parsedSearch) text() string {
	parts := make([]string, 0, len(p.terms))
	for _, t := range p.terms {
		if t.or || len(t.words) > 0 {
			parts = append(parts, t.String())
		}
	}
	return strings.Join(parts, " ")
}

// String writes a term back in websearch syntax.
func (t searchTerm) String() string {
	if t.or {
		return "or"
	}
	s := strings.Join(t.words, " ")
	if t.phrase {
		s = `"` + strings.ReplaceAll(s, `"`, "") + `"`
	}
	if t.neg {
		s = "-" + s
	}
	return s
}

// codeText is the query for the code index: every word as its identifier
// pieces (a word of several is a phrase of them), and a plain word that is an
// English stop word (stop, from the database) left out, since the simple
// configuration keeps every word and would match it everywhere.
func (p parsedSearch) codeText(stop map[string]bool) string {
	var parts []string
	for _, t := range p.terms {
		if t.or {
			parts = append(parts, "or")
			continue
		}
		var pieces []string
		for _, w := range t.words {
			for _, pc := range codePieces(w) {
				pieces = append(pieces, w[pc[0]:pc[1]])
			}
		}
		if len(pieces) == 0 || (len(pieces) == 1 && !t.phrase && stop[strings.ToLower(pieces[0])]) {
			continue
		}
		s := strings.Join(pieces, " ")
		if len(pieces) > 1 {
			s = `"` + s + `"`
		}
		if t.neg {
			s = "-" + s
		}
		parts = append(parts, s)
	}
	// A dangling or (its neighbour was a stop word) is harmless:
	// websearch_to_tsquery ignores an or with nothing on one side.
	return strings.Join(parts, " ")
}

// negated is the terms the query excludes, as a query matching a document
// that has any of them, so the document is excluded whichever index matched
// it.
func (p parsedSearch) negated() parsedSearch {
	var n parsedSearch
	for _, t := range p.terms {
		if t.neg && len(t.words) > 0 {
			if len(n.terms) > 0 {
				n.terms = append(n.terms, searchTerm{or: true})
			}
			t.neg = false
			n.terms = append(n.terms, t)
		}
	}
	return n
}

// codePieces returns the byte spans of w's identifier pieces: w split at every
// ASCII punctuation character and at camelCase boundaries (a lowercase letter
// or digit, then a capital; a capital, then a capital and a lowercase letter).
// orbit_search_code_text is the same rule in SQL (the search_code migration).
func codePieces(w string) [][2]int {
	var out [][2]int
	start := -1
	for i := 0; i < len(w); i++ {
		c := w[i]
		if c < utf8.RuneSelf && (unicode.IsPunct(rune(c)) || unicode.IsSymbol(rune(c)) || unicode.IsSpace(rune(c))) {
			if start >= 0 {
				out = append(out, [2]int{start, i})
				start = -1
			}
			continue
		}
		if start >= 0 && i > start && isUpper(c) {
			prev := w[i-1]
			if isLower(prev) || isDigit(prev) || (isUpper(prev) && i+1 < len(w) && isLower(w[i+1])) {
				out = append(out, [2]int{start, i})
				start = i
				continue
			}
		}
		if start < 0 {
			start = i
		}
	}
	if start >= 0 {
		out = append(out, [2]int{start, len(w)})
	}
	return out
}

func isUpper(c byte) bool { return c >= 'A' && c <= 'Z' }
func isLower(c byte) bool { return c >= 'a' && c <= 'z' }
func isDigit(c byte) bool { return c >= '0' && c <= '9' }
