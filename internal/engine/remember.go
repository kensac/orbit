package engine

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"unicode"

	"github.com/kensac/orbit/internal/spec"
	"github.com/kensac/orbit/internal/store"
)

// /remember (docs/ARCHITECTURE.md, Memory entries): a person's message that starts with /remember is written as a memory entry by code when the run reads it, without waiting for the learning pass, and the reply says what was saved. A run woken only by such messages ends there, with no model call.

// rememberCommand is the word a message starts with.
const rememberCommand = "/remember"

// rememberUsage is how the reply names the command's form when it cannot be followed.
const rememberUsage = "/remember [--agent | --topic <name>] [--feedback | --user | --project | --reference] <what to remember>"

// rememberTitleMax bounds the title made from the text: an index line, not the fact.
const rememberTitleMax = 80

// isRemember reports whether a message body is a /remember command.
func isRemember(body string) bool {
	rest, ok := strings.CutPrefix(strings.TrimSpace(body), rememberCommand)
	return ok && (rest == "" || unicode.IsSpace(rune(rest[0])))
}

// rememberCmd is a parsed /remember: where the entry goes and what it says.
type rememberCmd struct {
	// Agent is --agent: this agent's own; Topic is --topic <name>; neither is the profile.
	Agent bool
	Topic string
	// Type is a type flag's, or "" to infer it (rememberType).
	Type string
	Text string
}

// parseRemember reads the flags after /remember, each a word of its own before the text.
func parseRemember(body string) (rememberCmd, error) {
	rest, _ := strings.CutPrefix(strings.TrimSpace(body), rememberCommand)
	var c rememberCmd
	for {
		word, after := nextWord(rest)
		if !strings.HasPrefix(word, "--") {
			break
		}
		flag, value, hasValue := strings.Cut(word, "=")
		switch {
		case flag == "--agent" && !hasValue:
			c.Agent = true
		case flag == "--topic":
			if !hasValue {
				value, after = nextWord(after)
			}
			if value == "" {
				return c, fmt.Errorf("--topic needs a topic's name")
			}
			c.Topic = value
		case !hasValue && slices.Contains(store.EntryTypes, strings.TrimPrefix(flag, "--")):
			if c.Type != "" {
				return c, fmt.Errorf("give at most one type")
			}
			c.Type = strings.TrimPrefix(flag, "--")
		default:
			return c, fmt.Errorf("%s is not a flag of /remember", word)
		}
		rest = after
	}
	if c.Agent && c.Topic != "" {
		return c, fmt.Errorf("--agent and --topic are two places; give one")
	}
	c.Text = strings.TrimSpace(rest)
	if c.Text == "" {
		return c, fmt.Errorf("there is nothing to remember")
	}
	if c.Type == "" {
		c.Type = rememberType(c.Text)
	}
	return c, nil
}

// nextWord splits s, after leading space, at its first word.
func nextWord(s string) (word, rest string) {
	s = strings.TrimLeftFunc(s, unicode.IsSpace)
	if i := strings.IndexFunc(s, unicode.IsSpace); i >= 0 {
		return s[:i], s[i:]
	}
	return s, ""
}

var (
	// rememberUserRE: the person saying who they are.
	rememberUserRE = regexp.MustCompile(`(?i)^(i am|i'm|im|my name|i live|i work|i was born|i grew up|i speak|my (job|role|wife|husband|partner|birthday|family|kids?|son|daughter))\b`)
	// rememberProjectRE: a fact about their work and world, not how they want it done.
	rememberProjectRE = regexp.MustCompile(`(?i)^(we|our|the project|the deadline)\b|\b(deadline|is due|due on|due by|launches on|ships on)\b`)
	// rememberReferenceRE: where something is.
	rememberReferenceRE = regexp.MustCompile(`(?i)https?://|\b(can be found|is documented|docs are|lives in the repo|is in the repo)\b`)
)

// rememberType infers an entry's type from simple cues in its text: a link or "can be found" is a reference, "I am", "I live", "my job" the person (user), "we", "our", a deadline a fact (project); anything else is how they want work done (feedback), the type that changes how an agent works.
func rememberType(text string) string {
	switch {
	case rememberReferenceRE.MatchString(text):
		return "reference"
	case rememberUserRE.MatchString(text):
		return "user"
	case rememberProjectRE.MatchString(text):
		return "project"
	}
	return "feedback"
}

// rememberTitle is the text's first sentence or line, cut at a word within rememberTitleMax.
func rememberTitle(text string) string {
	t, _, _ := strings.Cut(strings.TrimSpace(text), "\n")
	for _, end := range []string{". ", "! ", "? ", "; "} {
		if i := strings.Index(t, end); i > 0 {
			t = t[:i+1]
		}
	}
	t = strings.Join(strings.Fields(strings.TrimRight(t, ".;")), " ")
	if len(t) <= rememberTitleMax {
		return t
	}
	cut := strings.LastIndexByte(t[:rememberTitleMax-3], ' ')
	if cut <= 0 {
		cut = rememberTitleMax - 3
		for cut > 0 && (t[cut]&0xC0) == 0x80 {
			cut--
		}
	}
	return strings.TrimRight(t[:cut], " ,:") + "..."
}

// remembered is what /remember did with a run's messages: the confirmation lines for the reply, and the messages left for the model.
type remembered struct {
	lines []string
	rest  []store.Message
}

// remember writes an entry for each /remember among the person's messages of an owner's conversation (task conversations, whose messages an agent wrote, are left alone). A message an earlier attempt already saved (the same text in the same place) is reported as saved, not written twice, so a retried run never doubles an entry.
func (e *Engine) remember(ctx context.Context, a *spec.Agent, c *store.ClaimedRun, msgs []store.Message) (remembered, error) {
	var r remembered
	for _, m := range msgs {
		if !isRemember(m.Body) || (m.Author != nil && strings.HasPrefix(*m.Author, store.AgentAuthorPrefix)) {
			r.rest = append(r.rest, m)
			continue
		}
		line, err := e.rememberOne(ctx, a, c, m)
		if err != nil {
			return r, err
		}
		r.lines = append(r.lines, line)
	}
	return r, nil
}

func (e *Engine) rememberOne(ctx context.Context, a *spec.Agent, c *store.ClaimedRun, m store.Message) (string, error) {
	cmd, err := parseRemember(m.Body)
	if err != nil {
		return "Not saved: " + err.Error() + ". Write " + rememberUsage + ".", nil
	}
	where, whom := store.EntryRef{Scope: store.ScopeProfile}, "for every agent"
	switch {
	case cmd.Agent:
		where, whom = store.EntryRef{Scope: store.ScopeAgent, Owner: a.Name}, "for "+a.Name+" only"
	case cmd.Topic != "":
		topics, err := e.agentTopics(ctx, a)
		if err != nil {
			return "", err
		}
		if !slices.Contains(topics, cmd.Topic) {
			reads := "none"
			if len(topics) > 0 {
				reads = strings.Join(topics, ", ")
			}
			return fmt.Sprintf("Not saved: %s does not read the topic %q (it reads: %s).", a.Name, cmd.Topic, reads), nil
		}
		where, whom = store.EntryRef{Scope: store.ScopeTopic, Owner: cmd.Topic}, "for the agents that read "+cmd.Topic
	}
	f := store.EntryFields{Type: cmd.Type, Title: rememberTitle(cmd.Text), Body: cmd.Text}
	existing, err := e.Store.ListEntries(ctx, store.EntryFilter{Scope: where.Scope, Owner: where.Owner, Query: cmd.Text, Limit: store.MaxEntryLimit})
	if err != nil {
		return "", err
	}
	for _, x := range existing {
		if x.Body == cmd.Text {
			return fmt.Sprintf("Already in memory: [%s] %s (%s), %s.", x.Type, x.Title, entryRef(x), whom), nil
		}
	}
	author := ""
	if m.Author != nil {
		author = *m.Author
	}
	saved, err := e.Store.CreateEntry(ctx, store.NewEntry{EntryRef: where, EntryFields: f},
		store.Provenance{Source: store.SourcePerson, RunID: c.ID, ConversationID: c.ConversationID, Author: author, Note: "/remember in a conversation"})
	if err != nil {
		if errors.Is(err, store.ErrInvalid) {
			return "Not saved: " + strings.TrimPrefix(err.Error(), "invalid: ") + ".", nil
		}
		return "", err
	}
	return fmt.Sprintf("Saved to memory: [%s] %s (%s), %s.", saved.Type, saved.Title, entryRef(*saved), whom), nil
}

// rememberReport is the reply's account of what /remember did.
func rememberReport(lines []string) string { return strings.Join(lines, "\n") }
