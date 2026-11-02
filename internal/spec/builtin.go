package spec

import (
	"sort"
	"time"
)

// BuiltinName is the agent every deployment has: a plain Claude Code
// session a person reaches from anywhere, and the one a new chat opens with
// when no agent is named.
const BuiltinName = "orbit"

// builtinCharter is deliberately short: the general agent is a Claude Code
// session, not a role, so it is told where it is and nothing about what to
// care about.
const builtinCharter = `You are orbit, a general assistant your person reaches from anywhere, in the
way they would use Claude Code. Do what they ask in this conversation. Reply
as you would in a terminal session: direct, in Markdown, with what you did
and what you found.`

// builtinIdle keeps a conversation's session for a day: a person comes
// back to a chat the next morning (MaxSessionIdle).
const builtinIdle = MaxSessionIdle

// builtinLive keeps the process open between messages that come in a
// burst, as in a terminal.
const builtinLive = 10 * time.Minute

// Builtins are the agents orbit defines in code, given a deployment. The
// general agent needs a claude-code runner: the default one if it is, else
// the first by name; without any it is not defined.
func Builtins(d Deployment) []*Agent {
	runner := ""
	if r, ok := d.Runners[d.Defaults.Runner]; ok && r.Kind == "claude-code" {
		runner = d.Defaults.Runner
	} else {
		names := make([]string, 0, len(d.Runners))
		for n, r := range d.Runners {
			if r.Kind == "claude-code" {
				names = append(names, n)
			}
		}
		sort.Strings(names)
		if len(names) > 0 {
			runner = names[0]
		}
	}
	if runner == "" {
		return nil
	}
	addr, _ := d.SandboxService()
	// In a workstation sandbox the session has every Claude Code tool
	// (tools is then unused). Without one it may not write or run
	// commands: on orbit's host they would reach orbit's own files.
	sandbox := ""
	tools := []string{"WebSearch", "WebFetch", "Task", "TodoWrite"}
	var needs []string
	if addr != "" {
		sandbox, tools = SandboxWorkstation, nil
		// Only hosts that reach sandboxd claim its runs.
		needs = []string{"sandbox"}
	}
	sess := &SessionSpec{Resume: true, Idle: Duration(builtinIdle), Live: Duration(builtinLive)}
	if d.Runners[runner].MaxBudgetUSD > 0 {
		// A live process's budget would cover every run fed into it.
		sess.Live = 0
	}
	return []*Agent{{
		Name:        BuiltinName,
		Description: "A Claude Code session you can reach from anywhere.",
		Charter:     builtinCharter,
		Runner:      runner,
		Tools:       tools,
		Chat:        true,
		Sandbox:     sandbox,
		Needs:       needs,
		// The general agent coordinates the others: in its person's chat a
		// delegation applies directly, unattended it waits for approval
		// (docs/ARCHITECTURE.md, Delegation).
		Delegates:       []string{"*"},
		MessagesTrigger: true,
		Session:         sess,
		// It may define agents, itself included (docs/api.md, Defining
		// agents): each change is a new, revertible version. It delegates
		// under approve: directly in its person's chat, after approval
		// when unattended.
		Actions: map[string]Rule{"agent:define": RuleAuto, "delegate": RuleApprove},
		// Replies stay in orbit; failures and approvals still notify.
		Notify: NotifySpec{When: "never"},
		Origin: OriginBuiltin,
	}}
}
