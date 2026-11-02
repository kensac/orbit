package engine

import (
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/kensac/orbit/internal/store"
)

// conversationBudget bounds the thread a fresh run is shown, in bytes:
// about 6k tokens at 4 bytes a token, room for the last few exchanges with
// whole reports. It sits beside memory (16 KB by default) in every prompt,
// so the two together stay a small part of the window the run's inputs
// need.
const conversationBudget = 24 << 10

// conversationItemMax bounds one item, so one long report cannot crowd out
// every exchange before it: a quarter of the budget keeps at least four.
const conversationItemMax = conversationBudget / 4

// conversationRows is how many items are read before the byte budget is
// applied: the smallest rendered item is about 100 bytes, so the budget,
// not this, decides what is shown.
const conversationRows = conversationBudget / 100

// conversationText packs the thread into conversationBudget, newest items
// first, and returns it oldest first with the items it holds. Items
// arrive newest first (store.Conversation). Every step is deterministic:
// the same thread gives the same bytes.
func conversationText(items []store.ConversationItem, loc *time.Location) (string, []store.ConversationItem) {
	var chosen []store.ConversationItem
	used := 0
	for _, it := range items {
		it.Body = clip(it.Body, conversationItemMax)
		n := len(renderItem(it, loc))
		if used+n > conversationBudget {
			break
		}
		used += n
		chosen = append(chosen, it)
	}
	slices.Reverse(chosen)
	return renderConversation(chosen, loc), chosen
}

func renderConversation(items []store.ConversationItem, loc *time.Location) string {
	var b strings.Builder
	for _, it := range items {
		b.WriteString(renderItem(it, loc))
	}
	return b.String()
}

func renderItem(it store.ConversationItem, loc *time.Location) string {
	at := it.At.In(loc).Format("2006-01-02 15:04")
	var head string
	switch it.Kind {
	case "message":
		head = "person"
		if strings.HasPrefix(it.Summary, store.AgentAuthorPrefix) {
			head = strings.TrimPrefix(it.Summary, store.AgentAuthorPrefix) + ", the agent that delegated this task"
		}
	case "report":
		head = "you, report"
		if it.Summary != "" {
			head += ": " + it.Summary
		}
	case "failure":
		head = "you, run failed"
	case "question":
		head = "you asked"
	default:
		head = it.Kind
	}
	return fmt.Sprintf("## %s, %s\n\n%s\n\n", at, head, strings.TrimSpace(it.Body))
}

// clip cuts s to at most n bytes on a rune boundary and says it did.
func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	// The note is at most 64 bytes, reserved before cutting.
	cut := n - 64
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + fmt.Sprintf("\n\n[truncated: %d of %d bytes shown]", cut, len(s))
}
