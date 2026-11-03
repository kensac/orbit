package spec

import (
	"slices"
	"sort"
	"time"
)

// BuiltinName is the agent every deployment has: a plain Claude Code
// session a person reaches from anywhere, and the one a new chat opens with
// when no agent is named.
const BuiltinName = "orbit"

// AllTopics in an agent's topics is every memory topic there is, now and
// later: the general agent is the person's own session, so what any agent
// was taught is in reach of it.
const AllTopics = "*"

// ReadsAllTopics reports whether the agent reads every topic.
func (a *Agent) ReadsAllTopics() bool { return slices.Contains(a.Topics, AllTopics) }

// builtinCharter is deliberately short: the general agent is a Claude Code
// session, not a role, so it is told where it is and nothing about what to
// care about.
const builtinCharter = `You are orbit, a general assistant your person reaches from anywhere, in the way they would use Claude Code. Do what they ask in this conversation. Reply as you would in a terminal session: direct, in Markdown, with what you did and what you found. Your reply is the whole answer, written out in full in your final message: never a pointer to something you wrote earlier, such as "see above" or "listed in the chat".`

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
		Delegates:         []string{"*"},
		DelegateTemplates: []string{"*"},
		// It is the person's general session: every topic's entries,
		// within the index budget (engine/known.go).
		Topics:          []string{AllTopics},
		MessagesTrigger: true,
		Session:         sess,
		// It may define agents, itself included (docs/api.md, Defining
		// agents): each change is a new, revertible version. It delegates
		// under approve: directly in its person's chat, after approval
		// when unattended. It creates agents from templates, each one an
		// agent:define a person approves. GitHub writes follow the rules below.
		Actions: builtinActions(d),
		// Replies stay in orbit; failures and approvals still notify.
		Notify: NotifySpec{When: "never"},
		Origin: OriginBuiltin,
	}}
}

func builtinActions(d Deployment) ActionSet {
	// It delegates, changes schedules and starts runs under approve:
	// directly in its person's chat, after approval when unattended.
	// agent:save is always proposed for approval (engine/save.go). Its
	// skill writes wait for a person: a skill steers every later run it
	// is given to.
	rules := ActionSet{"agent:define": RuleAuto, "skill:write": RuleApprove, "agent:save": RuleApprove, "template:create": RuleApprove, "continue": RuleAuto, "delegate": RuleApprove,
		"schedule:add": RuleApprove, "schedule:pause": RuleApprove, "schedule:resume": RuleApprove,
		"schedule:remove": RuleApprove, "run:now": RuleApprove}
	if d.GitHub != nil {
		rules["github:clone"] = RuleAuto
		rules["github:pr"] = RuleApprove
		rules["github:merge"] = RuleApprove
	}
	return rules
}
