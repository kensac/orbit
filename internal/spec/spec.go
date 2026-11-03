// Package spec loads and validates orbit's configuration: the deployment file
// (orbit.yaml: channels, endpoints, defaults) and one file per agent
// (agents/*.yaml). Configuration lives in git next to the deployment; the
// database records the spec each run used, so a run stays explainable after
// its agent's file changes.
package spec

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"math"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/robfig/cron/v3"
	"gopkg.in/yaml.v3"

	"github.com/kensac/orbit/internal/rrule"
	"github.com/kensac/orbit/internal/transforms"
)

// Rule is what the harness does with an action an agent proposes.
type Rule string

const (
	// RuleAuto executes the action as soon as the run finishes.
	RuleAuto Rule = "auto"
	// RuleApprove holds the action until a person approves it.
	RuleApprove Rule = "approve"
	// RuleDeny records the proposal and never executes it. It is the default
	// for any action type an agent does not name.
	RuleDeny Rule = "deny"
)

// Config is the whole loaded configuration.
type Config struct {
	Deployment Deployment
	Agents     map[string]*Agent
}

// Deployment is orbit.yaml.
type Deployment struct {
	// Timezone schedules are read in unless an agent names its own.
	Timezone string `yaml:"timezone" json:"timezone"`
	// loc is Timezone parsed; set by Validate, which rejects a zone that does not parse.
	loc *time.Location
	// PublicURL is where a person reaches the API (links in notifications).
	PublicURL string                `yaml:"public_url" json:"public_url"`
	Defaults  Defaults              `yaml:"defaults" json:"defaults"`
	Channels  map[string]Channel    `yaml:"channels" json:"channels"`
	Endpoints map[string]Endpoint   `yaml:"endpoints" json:"endpoints"`
	Runners   map[string]RunnerSpec `yaml:"runners" json:"runners"`
	// GitHub is the App orbit reaches GitHub as. Without it, no ticket may
	// name a repository and github:* actions are refused at load.
	GitHub *GitHub `yaml:"github,omitempty" json:"github,omitempty"`
	// Build is how orbit builds approved tickets (build.go). Without it no
	// ticket is built.
	Build *Build `yaml:"build,omitempty" json:"build,omitempty"`
	// Sandbox is sandboxd for sandboxed chat agents, when it is not the
	// build section's (SandboxService).
	Sandbox *SandboxService `yaml:"sandbox,omitempty" json:"sandbox,omitempty"`
	// Mail is orbit's mailbox (mail.go). Without it orbit reads no mail and its mails carry no reply address.
	Mail *Mail `yaml:"mail,omitempty" json:"mail,omitempty"`
	// Telegram is a two-way bot channel (telegram.go). Without it orbit does not poll Telegram updates.
	Telegram *Telegram `yaml:"telegram,omitempty" json:"telegram,omitempty"`
	// UsagePoll polls the subscription's usage windows (usage.go). Without
	// it /usage shows only what runs' streams reported.
	UsagePoll *UsagePoll `yaml:"usage_poll,omitempty" json:"usage_poll,omitempty"`
	// DeferLowPriority holds the scheduled runs of priority: low agents
	// while a subscription window is nearly spent (internal/usage).
	DeferLowPriority bool `yaml:"defer_low_priority,omitempty" json:"defer_low_priority,omitempty"`
	// Credentials are the variables a source or an endpoint's headers may send, keyed by name, each only to URLs under its prefix (trust.go). They are the only ${VAR} a database or built-in agent may reference.
	Credentials Credentials `yaml:"credentials,omitempty" json:"credentials,omitempty"`
	// Connectors are MCP servers agents may be given (connectors.go); orbit calls them on the agents' behalf.
	Connectors map[string]Connector `yaml:"connectors,omitempty" json:"connectors,omitempty"`
	// GoogleActions binds typed Gmail and Calendar writes to fixed tools on
	// configured connectors; action parameters cannot choose a server or tool.
	GoogleActions *GoogleActions `yaml:"google_actions,omitempty" json:"google_actions,omitempty"`
	// Expires records when a credential orbit cannot inspect stops working (the Claude Code subscription token lasts a year), keyed by its variable, as YYYY-MM-DD (expires.go). It only feeds orbit_credential_expiry_timestamp_seconds, so an alert fires before the date.
	Expires map[string]string `yaml:"expires,omitempty" json:"expires,omitempty"`
	// WebhookSecrets are the signing secrets inbound webhooks are verified with, keyed by variable, each bound to the agents that may use it (webhook.go).
	WebhookSecrets map[string]WebhookSecret `yaml:"webhook_secrets,omitempty" json:"webhook_secrets,omitempty"`
	// Board tunes GET /board (docs/api.md, The board).
	Board *Board `yaml:"board,omitempty" json:"board,omitempty"`
}

// GoogleActions configures the MCP tools used by typed Google write actions.
type GoogleActions struct {
	GmailDraft     *GoogleActionBinding   `yaml:"gmail_draft,omitempty" json:"gmail_draft,omitempty"`
	CalendarCreate *GoogleCalendarBinding `yaml:"calendar_create,omitempty" json:"calendar_create,omitempty"`
}

// GoogleActionBinding names one configured connector and its write tool.
type GoogleActionBinding struct {
	Connector string `yaml:"connector" json:"connector"`
	Tool      string `yaml:"tool" json:"tool"`
}

// GoogleCalendarBinding also fixes the destination calendar.
type GoogleCalendarBinding struct {
	Connector  string `yaml:"connector" json:"connector"`
	Tool       string `yaml:"tool" json:"tool"`
	CalendarID string `yaml:"calendar_id,omitempty" json:"calendar_id,omitempty"`
}

// Board tunes the board of every open conversation.
type Board struct {
	// Idle is how long a conversation with nothing waiting stays on the
	// board after its last activity (default store.DefaultBoardIdle, 30m).
	Idle Duration `yaml:"idle,omitempty" json:"idle,omitempty"`
}

// BoardIdle is board.idle, or 0 for the default.
func (d Deployment) BoardIdle() time.Duration {
	if d.Board == nil {
		return 0
	}
	return time.Duration(d.Board.Idle)
}

// GitHub configures the one GitHub App orbit acts as (docs/tickets.md). The
// private key is read by the harness from PrivateKeyEnv and never reaches a
// model; every token minted from it is limited to one of Repositories.
type GitHub struct {
	AppID int64 `yaml:"app_id" json:"app_id"`
	// PrivateKeyEnv names the variable holding the App's PEM private key,
	// or base64 of it, so it fits one line of an env file.
	PrivateKeyEnv string `yaml:"private_key_env" json:"private_key_env"`
	// APIURL overrides https://api.github.com (tests, GitHub Enterprise).
	APIURL string `yaml:"api_url,omitempty" json:"api_url,omitempty"`
	// Repositories ("owner/name") tickets and github:merge may target.
	Repositories []string `yaml:"repositories" json:"repositories"`
	// ReadRepositories ("owner/name") are the repositories an agent's
	// repos may name: orbit mirrors them and mints contents:read tokens
	// (and pull request, check and status reads for the repository tools)
	// for them. Reading a repository never permits a ticket or a merge
	// there; that is Repositories alone.
	ReadRepositories []string `yaml:"read_repositories,omitempty" json:"read_repositories,omitempty"`
	// CommentRepositories ("owner/name") are where github:comment may post: a token of issues:write alone, minted per comment. Commenting never permits a push, a ticket or a merge.
	CommentRepositories []string `yaml:"comment_repositories,omitempty" json:"comment_repositories,omitempty"`
}

// AllowsRepo reports whether repo is one orbit may act on.
func (d Deployment) AllowsRepo(repo string) bool {
	return d.GitHub != nil && slices.Contains(d.GitHub.Repositories, repo)
}

// Location is the deployment's Timezone, parsed once when the configuration was validated; UTC before that.
func (d Deployment) Location() *time.Location {
	if d.loc == nil {
		return time.UTC
	}
	return d.loc
}

// RunURL is a run's page under PublicURL, the link notifications and search results carry; "" when no public URL is configured, since a relative link is useless in a notification.
func (d Deployment) RunURL(runID string) string {
	if d.PublicURL == "" {
		return ""
	}
	return strings.TrimRight(d.PublicURL, "/") + "/ui/runs/" + url.PathEscape(runID)
}

// AllowsComment reports whether github:comment may post on repo.
func (d Deployment) AllowsComment(repo string) bool {
	return d.GitHub != nil && slices.Contains(d.GitHub.CommentRepositories, repo)
}

// AllowsRead reports whether repo is one an agent may read.
func (d Deployment) AllowsRead(repo string) bool {
	return d.GitHub != nil && slices.Contains(d.GitHub.ReadRepositories, repo)
}

// Defaults apply to every agent that does not override them.
type Defaults struct {
	Runner   string   `yaml:"runner" json:"runner"`
	Timeout  Duration `yaml:"timeout" json:"timeout"`
	Channels []string `yaml:"channels" json:"channels"`
	// MemoryMaxBytes bounds the memory document an agent may write back.
	MemoryMaxBytes int `yaml:"memory_max_bytes" json:"memory_max_bytes"`
	// DecisionStall is how long a delegated task's decision may wait for a
	// person before its batch counts as stalled and its parent is woken.
	DecisionStall Duration `yaml:"decision_stall" json:"decision_stall"`
}

// Channel is a notification destination. Kind selects the adapter; the
// remaining fields are that adapter's settings. Secrets are named by
// environment variable (`*_env`), never written here.
type Channel struct {
	Kind string `yaml:"kind" json:"kind"`
	// ntfy
	URL      string `yaml:"url,omitempty" json:"url,omitempty"`
	Topic    string `yaml:"topic,omitempty" json:"topic,omitempty"`
	TopicEnv string `yaml:"topic_env,omitempty" json:"topic_env,omitempty"`
	TokenEnv string `yaml:"token_env,omitempty" json:"token_env,omitempty"`
	// smtp
	Host        string   `yaml:"host,omitempty" json:"host,omitempty"`
	Port        int      `yaml:"port,omitempty" json:"port,omitempty"`
	UsernameEnv string   `yaml:"username_env,omitempty" json:"username_env,omitempty"`
	PasswordEnv string   `yaml:"password_env,omitempty" json:"password_env,omitempty"`
	From        string   `yaml:"from,omitempty" json:"from,omitempty"`
	To          []string `yaml:"to,omitempty" json:"to,omitempty"`
	// webhook
	Headers map[string]string `yaml:"headers,omitempty" json:"headers,omitempty"`
	// telegram
	ChatID string `yaml:"chat_id,omitempty" json:"chat_id,omitempty"`
}

// Endpoint is a named HTTP target a `webhook` action may call. The agent
// chooses only the endpoint name, the body and the values of the path's
// placeholders, each checked against Params; the URL, method and credentials
// stay in configuration, so a model cannot point a write at anything new.
type Endpoint struct {
	URL     string            `yaml:"url" json:"url"`
	Method  string            `yaml:"method" json:"method"`
	Headers map[string]string `yaml:"headers" json:"headers"`
	// Description tells the model what the endpoint does.
	Description string `yaml:"description,omitempty" json:"description,omitempty"`
	// Path is appended to URL; each {name} in it is filled from the action's path params, declared in Params (endpoint.go).
	Path   string                   `yaml:"path,omitempty" json:"path,omitempty"`
	Params map[string]EndpointParam `yaml:"params,omitempty" json:"params,omitempty"`
	// BodySchema is a JSON Schema the action's body must satisfy; without one, any object.
	BodySchema map[string]any `yaml:"body_schema,omitempty" json:"body_schema,omitempty"`
}

// RunnerSpec configures one model backend.
type RunnerSpec struct {
	Kind  string `yaml:"kind" json:"kind"`
	Model string `yaml:"model" json:"model"`
	// MaxTurns and MaxBudgetUSD are passed to runners that support them.
	MaxTurns     int     `yaml:"max_turns,omitempty" json:"max_turns,omitempty"`
	MaxBudgetUSD float64 `yaml:"max_budget_usd,omitempty" json:"max_budget_usd,omitempty"`
	// Binary overrides the CLI path for CLI runners.
	Binary string `yaml:"binary,omitempty" json:"binary,omitempty"`
	// APIKeyEnv names the key's environment variable for API runners.
	APIKeyEnv string `yaml:"api_key_env,omitempty" json:"api_key_env,omitempty"`
	MaxTokens int    `yaml:"max_tokens,omitempty" json:"max_tokens,omitempty"`
}

// Agent is one agents/<name>.yaml: a standing responsibility.
type Agent struct {
	Name        string `yaml:"name" json:"name"`
	Description string `yaml:"description" json:"description"`
	// Charter is the standing instruction every run starts from.
	Charter string `yaml:"charter" json:"charter"`
	// Starters are prompts a client offers for a new conversation with the agent (at most MaxStarters, each MaxStarterLen bytes).
	Starters []string `yaml:"starters,omitempty" json:"starters,omitempty"`
	Enabled  *bool    `yaml:"enabled,omitempty" json:"enabled,omitempty"`
	Timezone string   `yaml:"timezone,omitempty" json:"timezone,omitempty"`
	Runner   string   `yaml:"runner,omitempty" json:"runner,omitempty"`
	Timeout  Duration `yaml:"timeout,omitempty" json:"timeout,omitempty"`
	// Budget limits API-equivalent cost and runner-started runs in a local
	// calendar period. Only unattended scheduled runs are stopped by it.
	Budget   *AgentBudget `yaml:"budget,omitempty" json:"budget,omitempty"`
	Triggers []Trigger    `yaml:"triggers" json:"triggers"`
	Sources  []Source     `yaml:"sources,omitempty" json:"sources,omitempty"`
	// Transforms run over sources before the model sees them; each output
	// is an input of its own (internal/transforms).
	Transforms []Transform `yaml:"transforms,omitempty" json:"transforms,omitempty"`
	// Gates end a scheduled run without a backend call when code finds
	// nothing to judge; the run is skipped, naming the gate.
	Gates []Gate   `yaml:"gates,omitempty" json:"gates,omitempty"`
	Tools []string `yaml:"tools,omitempty" json:"tools,omitempty"`
	// Web limits WebFetch to named domains on orbit's host (web.go).
	Web     *WebSpec   `yaml:"web,omitempty" json:"web,omitempty"`
	Actions ActionSet  `yaml:"actions,omitempty" json:"actions,omitempty"`
	Notify  NotifySpec `yaml:"notify,omitempty" json:"notify,omitempty"`
	// MessagesTrigger starts a run when a person sends the agent a message.
	MessagesTrigger bool `yaml:"wake_on_message,omitempty" json:"wake_on_message,omitempty"`
	// Session keeps the backend's conversation between runs (claude-code
	// only). Without it every run starts a fresh session and sees the
	// recent thread as text in its prompt.
	Session *SessionSpec `yaml:"session,omitempty" json:"session,omitempty"`
	// Verify names deterministic checks of a run's output. "figures": every
	// money amount in the summary and report must be in the inputs or in a
	// result of orbit's query or calc tool.
	Verify []string `yaml:"verify,omitempty" json:"verify,omitempty"`
	// Topics are the shared memory topics (finance, homelab) whose entries
	// every run of this agent is shown, beside the profile's and its own.
	// "*" (AllTopics) is every topic there is. The API can attach more
	// (docs/api.md, Memory entries).
	Topics []string `yaml:"topics,omitempty" json:"topics,omitempty"`
	// Skills are skills from orbit's library (docs/api.md, Skills) this agent runs with; the API can attach more.
	Skills []string `yaml:"skills,omitempty" json:"skills,omitempty"`
	// Hooks are Claude Code hooks for this agent's runs, beside orbit's own (docs/agents/configuration.md, Hooks).
	Hooks []Hook `yaml:"hooks,omitempty" json:"hooks,omitempty"`
	// Learn is how orbit learns from this agent's conversations once they go quiet (engine/learn.go). Absent is on, with the defaults.
	Learn *LearnSpec `yaml:"learn,omitempty" json:"learn,omitempty"`
	// Needs names the capabilities a host must have to run this agent
	// ("docker-network:hetzner", "sure", "browser", "sandbox"). A worker
	// claims a run only when its ORBIT_CAPABILITIES cover every one; an
	// agent with none runs anywhere (docs/ARCHITECTURE.md, Worker fleet).
	Needs []string `yaml:"needs,omitempty" json:"needs,omitempty"`
	// Priority is normal (the default) or low: a low agent's scheduled runs
	// wait while the subscription is nearly spent, when the deployment sets
	// defer_low_priority.
	Priority string `yaml:"priority,omitempty" json:"priority,omitempty"`
	// Chat makes the agent a conversation, as a Claude Code session is
	// (claude-code only): its reply is the session's final message, not a
	// report schema, and memory, questions and actions are orbit's tools it
	// may call (docs/ARCHITECTURE.md, Chat agents).
	Chat bool `yaml:"chat,omitempty" json:"chat,omitempty"`
	// Sandbox runs a chat agent's CLI in a sandboxd container per
	// conversation, over a workspace kept between messages, as one of
	// SandboxProfiles: "workstation" (every Claude Code tool, broad egress,
	// a home kept in the workspace) or "build" (the tools it names, the
	// build egress allowlist). Needs sandboxd in orbit.yaml.
	Sandbox string `yaml:"sandbox,omitempty" json:"sandbox,omitempty"`
	// Permissions is how a chat session asks before a tool runs, and how far a conversation may raise it (permissions.go). Absent is the defaults: full in a sandbox, locked otherwise.
	Permissions *Permissions `yaml:"permissions,omitempty" json:"permissions,omitempty"`
	// Delegates names the agents this one may hand tasks to with the
	// delegate action ("*" is every configured agent, itself included).
	// The delegate rule still decides whether it may (docs/ARCHITECTURE.md,
	// Delegation).
	Delegates []string `yaml:"delegates,omitempty" json:"delegates,omitempty"`
	// DelegateTemplates names active templates this one may instantiate as
	// task workers with the delegate action ("*" is every active template).
	DelegateTemplates []string `yaml:"delegate_templates,omitempty" json:"delegate_templates,omitempty"`
	// Connectors names orbit.yaml's connectors whose tools this agent's runs may call (claude-code only); each tool's rule is in actions as mcp:<connector>:<tool> (connectors.go).
	Connectors []string `yaml:"connectors,omitempty" json:"connectors,omitempty"`
	// Repos are git repositories every run reads: orbit fetches each into
	// its mirror and gives the run the tree at Ref, without .git, under
	// inputs/repos/<name>, with read-only history and GitHub tools
	// (internal/repos). Each must be in github.read_repositories.
	Repos []RepoRef `yaml:"repos,omitempty" json:"repos,omitempty"`

	// delegateTargets is Delegates resolved against the configuration,
	// sorted; set by Validate.
	delegateTargets []string
	// delegateTemplateTargets is DelegateTemplates, sorted; active template
	// names are resolved when the delegate action is offered.
	delegateTemplateTargets []string
	// loc is Timezone parsed; set by Validate, which rejects a zone that does not parse.
	loc *time.Location

	// SHA is the hash of the agent's canonical JSON, set by Load.
	SHA string `yaml:"-" json:"-"`
	// Origin is where the agent is defined: OriginFile, OriginDatabase or
	// OriginBuiltin. Only database agents are changed through the API.
	Origin string `yaml:"-" json:"-"`
	// Version is a database agent's definition version (agent_definitions).
	Version int `yaml:"-" json:"-"`
}

// AgentBudget is the optional per-agent spend and run limit.
type AgentBudget struct {
	Period     string  `yaml:"period" json:"period"`
	MaxCostUSD float64 `yaml:"max_cost_usd,omitempty" json:"max_cost_usd,omitempty"`
	MaxRuns    int     `yaml:"max_runs,omitempty" json:"max_runs,omitempty"`
}

// RepoRef is one repository an agent reads, at a branch, tag or commit.
type RepoRef struct {
	Repo string `yaml:"repo" json:"repo"`
	// Ref is a branch, a tag or a full commit sha; empty is the default
	// branch.
	Ref string `yaml:"ref,omitempty" json:"ref,omitempty"`
}

// Name is the directory the repository's tree is given under
// (inputs/repos/<name>): the repository's name without its owner.
func (r RepoRef) Name() string {
	_, name, _ := strings.Cut(r.Repo, "/")
	return name
}

// RepoRefRE is a ref an agent may name: a branch or tag in git's own
// spelling without anything git would read as an option or a revision
// expression, or a full commit sha.
var RepoRefRE = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9._/-]{0,199}$`)

// NeedsGitHub is the capability of a host holding the GitHub App's key: an
// agent with repos must need it, so a worker without the key never claims
// its runs.
const NeedsGitHub = "github"

// Where an agent is defined.
const (
	OriginFile     = "file"
	OriginDatabase = "database"
	OriginBuiltin  = "builtin"
)

// SessionSpec is an agent's opt-in to continuing its Claude Code session.
type SessionSpec struct {
	// Resume continues the previous run's session when that run succeeded
	// under the same spec within Idle; otherwise the run starts fresh.
	Resume bool `yaml:"resume" json:"resume"`
	// Idle is the resume window, measured from the previous run's end.
	Idle Duration `yaml:"idle,omitempty" json:"idle,omitempty"`
	// Live keeps one Claude Code process open for this long after a run a
	// person's message started, and feeds the next runs into it instead of
	// starting a process per run. Zero means no live process.
	Live Duration `yaml:"live,omitempty" json:"live,omitempty"`
	// Stall bounds time without CLI output while no tool call is running.
	Stall Duration `yaml:"stall,omitempty" json:"stall,omitempty"`
}

// Hook is one Claude Code hook of an agent: at Event (and, for a tool event, a tool Matcher matches), orbit's hook binary does one thing: Say tells the model something (as context; at Stop it sends the model back once with it), Deny refuses the tool with a reason (PreToolUse), or Run runs a command with the event on stdin, in the run's workdir, with an empty environment but PATH, under Timeout.
type Hook struct {
	Event   string   `yaml:"event" json:"event"`
	Matcher string   `yaml:"matcher,omitempty" json:"matcher,omitempty"`
	Say     string   `yaml:"say,omitempty" json:"say,omitempty"`
	Deny    string   `yaml:"deny,omitempty" json:"deny,omitempty"`
	Run     []string `yaml:"run,omitempty" json:"run,omitempty"`
	Timeout Duration `yaml:"timeout,omitempty" json:"timeout,omitempty"`
}

// HookEvents are the Claude Code events an agent's hooks may name; ToolHookEvents take a matcher. SessionStart fires once per CLI process, so in a live process it does not fire for every run.
var (
	HookEvents     = []string{"PreToolUse", "PostToolUse", "UserPromptSubmit", "SessionStart", "Stop"}
	ToolHookEvents = []string{"PreToolUse", "PostToolUse"}
)

// Bounds on an agent's hooks: a hook's text rides in the model's context, and a command holds up the turn it fires in.
const (
	MaxHooks           = 20
	MaxHookText        = 2 << 10
	DefaultHookTimeout = 10 * time.Second
	MaxHookTimeout     = time.Minute
)

// Bounds on an agent's starters: a handful of buttons in a new chat, each a sentence or two.
const (
	MaxStarters   = 6
	MaxStarterLen = 500
)

// LearnSpec configures an agent's learning runs.
type LearnSpec struct {
	// Enabled false turns learning off for the agent.
	Enabled *bool `yaml:"enabled,omitempty" json:"enabled,omitempty"`
	// Idle is how long a conversation must be quiet before it is learned from.
	Idle Duration `yaml:"idle,omitempty" json:"idle,omitempty"`
	// Runner runs learning runs instead of the agent's own (a cheaper model).
	Runner string `yaml:"runner,omitempty" json:"runner,omitempty"`
}

// DefaultLearnIdle is the quiet a conversation needs before it is learned from: long enough that the person has moved on, short enough that what they said is kept the same day.
const DefaultLearnIdle = 30 * time.Minute

// MinLearnIdle and MaxLearnIdle bound learn.idle: under five minutes a learning run would interrupt a conversation still going (and each one is a backend call); past a day, what was said waits too long to be kept.
const (
	MinLearnIdle = 5 * time.Minute
	MaxLearnIdle = 24 * time.Hour
)

// DefaultSessionIdle is the resume window when a spec names none: a
// working day's conversation, short enough that a stale session does not
// outlive the person's train of thought.
const DefaultSessionIdle = 6 * time.Hour

// MaxSessionIdle bounds the resume window. A resumed session re-reads its
// whole transcript, and Claude Code's prompt cache lasts at most an hour,
// so a resume after a long gap pays for every earlier run again; past a
// day a fresh session with the thread in its prompt is cheaper and as good.
const MaxSessionIdle = 24 * time.Hour

// MaxSessionLive bounds how long an idle live process is kept. Each one is
// a resident CLI process; an hour covers a conversation with pauses, and
// resume picks up after it closes.
const MaxSessionLive = time.Hour

// DefaultSessionStall is session.stall when a spec names none.
const DefaultSessionStall = 10 * time.Minute

// MinSessionStall and MaxSessionStall bound session.stall.
const (
	MinSessionStall = time.Minute
	MaxSessionStall = 24 * time.Hour
)

// Trigger is one way an agent wakes: a cron Schedule, an RRule (RFC
// 5545, internal/rrule) anchored at Start, or a Webhook (webhook.go).
// Exactly one is set.
type Trigger struct {
	Schedule string `yaml:"schedule" json:"schedule"`
	// RRule is a recurrence rule ("FREQ=WEEKLY;INTERVAL=2;BYDAY=TU"), for
	// what cron cannot say: every other week, the last Friday, a series
	// that ends.
	RRule string `yaml:"rrule,omitempty" json:"rrule,omitempty"`
	// Start anchors an RRule: its first period, and the time of day it
	// does not name, as local time in the agent's timezone
	// ("2026-11-02T09:00").
	Start string `yaml:"start,omitempty" json:"start,omitempty"`
	// Prompt is the reason for this wake, appended to the charter.
	Prompt string `yaml:"prompt,omitempty" json:"prompt,omitempty"`
	// Paused stops the trigger firing; PausedUntil, a local time in the agent's timezone as Start is, ends the pause there (fires before it are skipped, never caught up). The schedule:pause and schedule:resume actions write them.
	Paused      bool   `yaml:"paused,omitempty" json:"paused,omitempty"`
	PausedUntil string `yaml:"paused_until,omitempty" json:"paused_until,omitempty"`
	// Webhook names an inbound route, POST /hooks/<agent>/<webhook>: a delivery signed with Secret starts a run.
	Webhook string `yaml:"webhook,omitempty" json:"webhook,omitempty"`
	// Secret is the variable holding the webhook's signing secret, one of orbit.yaml's webhook_secrets bound to this agent.
	Secret string `yaml:"secret,omitempty" json:"secret,omitempty"`
	// MaxBytes bounds a delivery's body; a larger one is refused.
	MaxBytes int64 `yaml:"max_bytes,omitempty" json:"max_bytes,omitempty"`
	// MissedGrace is how late a schedule fire may start; a later one is skipped as missed (MissedGraceFor).
	MissedGrace Duration `yaml:"missed_grace,omitempty" json:"missed_grace,omitempty"`
}

// paused is a schedule with a pause: no fire before until, and none at all when until is zero.
type paused struct {
	Schedule
	until time.Time
}

// Next is the inner schedule's first fire strictly after t that is not before until.
func (p paused) Next(t time.Time) time.Time {
	if p.until.IsZero() {
		return time.Time{}
	}
	if from := p.until.Add(-time.Second); t.Before(from) {
		// Fires are whole seconds (rrule.Rule.Start is truncated, cron has none), so a fire at until itself is after from.
		t = from
	}
	return p.Schedule.Next(t)
}

// ParseLocal reads a local time in loc in the forms Trigger.Start is written in.
func ParseLocal(s string, loc *time.Location) (time.Time, error) {
	var err error
	for _, l := range startLayouts {
		var t time.Time
		if t, err = time.ParseInLocation(l, s, loc); err == nil {
			return t, nil
		}
	}
	return time.Time{}, err
}

// Schedule is when a trigger fires: the first fire strictly after a time,
// or the zero time when it never fires again. A cron schedule and an
// RRULE are both one.
type Schedule interface {
	Next(time.Time) time.Time
}

// startLayouts are the forms Trigger.Start is written in.
var startLayouts = []string{"2006-01-02T15:04:05", "2006-01-02T15:04", "2006-01-02 15:04"}

// TriggerSchedule is a trigger's schedule, read in loc (the agent's
// timezone), its pause applied.
func TriggerSchedule(t Trigger, loc *time.Location) (Schedule, error) {
	sched, err := triggerSchedule(t, loc)
	if err != nil {
		return nil, err
	}
	switch {
	case t.PausedUntil != "" && !t.Paused:
		return nil, errors.New("paused_until needs paused")
	case t.PausedUntil != "":
		until, err := ParseLocal(t.PausedUntil, loc)
		if err != nil {
			return nil, fmt.Errorf("paused_until %q: want local time as 2026-11-09T00:00", t.PausedUntil)
		}
		return paused{sched, until}, nil
	case t.Paused:
		return paused{Schedule: sched}, nil
	}
	return sched, nil
}

func triggerSchedule(t Trigger, loc *time.Location) (Schedule, error) {
	switch {
	case t.IsWebhook():
		return nil, ErrWebhookTrigger
	case t.Schedule != "" && t.RRule != "":
		return nil, errors.New("schedule and rrule cannot both be set")
	case t.Schedule != "":
		if t.Start != "" {
			return nil, errors.New("start is for an rrule")
		}
		return ParseSchedule(t.Schedule)
	case t.RRule != "":
		if t.Start == "" {
			return nil, errors.New("an rrule needs a start (local time, 2026-11-02T09:00)")
		}
		start, err := ParseLocal(t.Start, loc)
		if err != nil {
			return nil, fmt.Errorf("start %q: want local time as 2026-11-02T09:00", t.Start)
		}
		r, err := rrule.Parse(t.RRule, start)
		if err != nil {
			return nil, fmt.Errorf("rrule: %w", err)
		}
		return r, nil
	}
	return nil, errors.New("schedule or rrule is required")
}

// Source is data the harness fetches before a run and hands to the agent as
// a file. The harness holds the credentials; the model only sees the data.
type Source struct {
	Name string `yaml:"name" json:"name"`
	Kind string `yaml:"kind" json:"kind"`
	// http
	URL      string            `yaml:"url,omitempty" json:"url,omitempty"`
	Headers  map[string]string `yaml:"headers,omitempty" json:"headers,omitempty"`
	Query    map[string]string `yaml:"query,omitempty" json:"query,omitempty"`
	Paginate *Paginate         `yaml:"paginate,omitempty" json:"paginate,omitempty"`
	// prometheus (PromQL) and loki (LogQL)
	Expr string `yaml:"expr,omitempty" json:"expr,omitempty"`
	// file
	Path string `yaml:"path,omitempty" json:"path,omitempty"`
	// github: Repo is one of github.read_repositories, Resource one of GitHubResources; Ref is the commit checks reads, State the issues or pulls listed.
	Repo     string `yaml:"repo,omitempty" json:"repo,omitempty"`
	Resource string `yaml:"resource,omitempty" json:"resource,omitempty"`
	Ref      string `yaml:"ref,omitempty" json:"ref,omitempty"`
	State    string `yaml:"state,omitempty" json:"state,omitempty"`
	// Limit bounds a github list's items or a loki query's lines.
	Limit int `yaml:"limit,omitempty" json:"limit,omitempty"`
	// Since is how far back a loki query reads, ending now.
	Since Duration `yaml:"since,omitempty" json:"since,omitempty"`
	// lake: endpoint, bucket, prefix, region and credentials for a read-only S3-compatible lake.
	Bucket          string `yaml:"bucket,omitempty" json:"bucket,omitempty"`
	Prefix          string `yaml:"prefix,omitempty" json:"prefix,omitempty"`
	Region          string `yaml:"region,omitempty" json:"region,omitempty"`
	AccessKeyID     string `yaml:"access_key_id,omitempty" json:"access_key_id,omitempty"`
	SecretAccessKey string `yaml:"secret_access_key,omitempty" json:"secret_access_key,omitempty"`
	SessionToken    string `yaml:"session_token,omitempty" json:"session_token,omitempty"`
	MaxObjects      int    `yaml:"max_objects,omitempty" json:"max_objects,omitempty"`
	// MaxBytes bounds one source's fetched size.
	MaxBytes int64 `yaml:"max_bytes,omitempty" json:"max_bytes,omitempty"`
	// ByTrigger overrides query values for runs of one trigger: a person's
	// message may want a wider window than the schedule's review.
	ByTrigger map[string]SourceOverride `yaml:"by_trigger,omitempty" json:"by_trigger,omitempty"`
}

// SourceOverride is what a source changes for runs of one trigger.
type SourceOverride struct {
	// Query values replace the source's own by key, and add keys it lacks.
	Query map[string]string `yaml:"query" json:"query"`
}

// SourceTriggers are the run triggers a source may override for: the
// agent's own runs that fetch sources (a build fetches none, and a
// learning pass reads conversations, not sources).
var SourceTriggers = []string{"schedule", "manual", "message", "continue", "task", "delegation", "webhook"}

// QueryFor is the source's query for a run of trigger: its own values with
// that trigger's override over them.
func (s Source) QueryFor(trigger string) map[string]string {
	o, ok := s.ByTrigger[trigger]
	if !ok {
		return s.Query
	}
	q := make(map[string]string, len(s.Query)+len(o.Query))
	maps.Copy(q, s.Query)
	maps.Copy(q, o.Query)
	return q
}

// Transform is code run over one source's bytes before the run: its output
// is the input named Name. A source a transform reads is withheld from the
// model (stored under sources/, not inputs/) unless one of its transforms
// keeps it.
type Transform struct {
	Name   string         `yaml:"name" json:"name"`
	Source string         `yaml:"source" json:"source"`
	Kind   string         `yaml:"kind" json:"kind"`
	Params map[string]any `yaml:"params,omitempty" json:"params,omitempty"`
	// KeepSource hands the source to the model as well.
	KeepSource bool `yaml:"keep_source,omitempty" json:"keep_source,omitempty"`
}

// Gate is a check, in code, that ends a scheduled run as skipped before
// any backend call. Kinds: unchanged (every input the model would see is
// byte for byte what the agent's newest succeeded run saw, under the same
// spec), items_above (Input, a source or transform, has at most N records
// at Items: nothing to review) and precheck (Command, a shell command the
// person wrote, exits 1: nothing to do; 0 runs, anything else fails).
type Gate struct {
	Kind  string `yaml:"kind" json:"kind"`
	Input string `yaml:"input,omitempty" json:"input,omitempty"`
	Items string `yaml:"items,omitempty" json:"items,omitempty"`
	N     int    `yaml:"n,omitempty" json:"n,omitempty"`
	// Command is a precheck's shell command (sh -c), run in the run's
	// workdir with the inputs fetched; Timeout bounds it.
	Command string   `yaml:"command,omitempty" json:"command,omitempty"`
	Timeout Duration `yaml:"timeout,omitempty" json:"timeout,omitempty"`
}

// GateKinds lists every gate kind.
var GateKinds = []string{"unchanged", "items_above", "precheck"}

// Precheck bounds: the default and the longest a precheck may run. It runs
// before every scheduled run of its agent, on the executor, so it is a
// quick test, not a job.
const (
	DefaultPrecheckTimeout = 30 * time.Second
	MaxPrecheckTimeout     = 2 * time.Minute
)

// Withheld reports whether the model is not shown source: a transform reads
// it and none keeps it.
func (a *Agent) Withheld(source string) bool {
	read := false
	for _, t := range a.Transforms {
		if t.Source == source {
			if t.KeepSource {
				return false
			}
			read = true
		}
	}
	return read
}

// Paginate describes page-numbered pagination: the harness requests
// PageParam=1..N and stops when TotalPagesPath reports N, or when ItemsPath
// returns an empty page. Pages are merged into one JSON array of items.
type Paginate struct {
	PageParam      string `yaml:"page_param" json:"page_param"`
	ItemsPath      string `yaml:"items_path" json:"items_path"`
	TotalPagesPath string `yaml:"total_pages_path,omitempty" json:"total_pages_path,omitempty"`
	MaxPages       int    `yaml:"max_pages,omitempty" json:"max_pages,omitempty"`
}

// ActionSet maps an action type ("webhook:<endpoint>", "notify") to a rule.
type ActionSet map[string]Rule

// NotifySpec says where an agent's report goes.
type NotifySpec struct {
	Channels []string `yaml:"channels,omitempty" json:"channels,omitempty"`
	// When is "always" (every run), "agent" (only when the agent asks), or
	// "never". Failures and pending approvals always notify.
	When string `yaml:"when,omitempty" json:"when,omitempty"`
}

// Duration is a time.Duration that reads "15m" from YAML.
type Duration time.Duration

// UnmarshalYAML parses a Go duration string.
func (d *Duration) UnmarshalYAML(n *yaml.Node) error {
	var s string
	if err := n.Decode(&s); err != nil {
		return err
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("line %d: %w", n.Line, err)
	}
	*d = Duration(v)
	return nil
}

// MarshalJSON writes the duration as a string, so stored specs stay readable.
func (d Duration) MarshalJSON() ([]byte, error) {
	return json.Marshal(time.Duration(d).String())
}

// UnmarshalJSON reads what MarshalJSON writes.
func (d *Duration) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return err
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return err
	}
	*d = Duration(v)
	return nil
}

// Verifies reports whether the agent's spec names a check.
func (a *Agent) Verifies(check string) bool { return slices.Contains(a.Verify, check) }

// StallAfter is the agent's session.stall, or DefaultSessionStall.
func (a *Agent) StallAfter() time.Duration {
	if a.Session != nil && a.Session.Stall > 0 {
		return time.Duration(a.Session.Stall)
	}
	return DefaultSessionStall
}

// IsEnabled reports whether the agent's triggers fire. Manual runs work
// either way.
func (a *Agent) IsEnabled() bool { return a.Enabled == nil || *a.Enabled }

// RuleFor returns the rule for an action type, deny if unnamed. The task
// types (task:message, task:cancel, task:retry) manage tasks the agent delegated, so
// unnamed they follow the delegate rule.
func (a *Agent) RuleFor(actionType string) Rule {
	if r, ok := a.Actions[actionType]; ok {
		return r
	}
	if strings.HasPrefix(actionType, "task:") {
		if r, ok := a.Actions["delegate"]; ok {
			return r
		}
	}
	return RuleDeny
}

// Location is the agent's Timezone, parsed once when the configuration was validated. An agent that was never validated (built in code, not loaded) reads as UTC.
func (a *Agent) Location() *time.Location {
	if a.loc == nil {
		return time.UTC
	}
	return a.loc
}

// SetTimezone sets Timezone and the Location it parses to, refusing a zone the system's tz database does not have.
func (a *Agent) SetTimezone(name string) error {
	loc, err := time.LoadLocation(name)
	if err != nil {
		return err
	}
	a.Timezone, a.loc = name, loc
	return nil
}

// DelegateTargets is the agents this one may delegate to, sorted: Delegates
// with "*" resolved against the configuration it was validated in.
func (a *Agent) DelegateTargets() []string { return a.delegateTargets }

// DelegateTemplateTargets returns the templates this agent may instantiate,
// sorted. "*" is expanded against the active library by the engine.
func (a *Agent) DelegateTemplateTargets() []string { return a.delegateTemplateTargets }

// ReservedAgentNames are the names an agent may not take: the portfolio
// serves an agent at /orbit/<name>, beside these static segments of its own
// (personal-portfolio app/orbit), plus orbit-admin, the authentik
// application. A new top-level segment in the UI is added here first, or an
// existing agent of that name would become unreachable.
var ReservedAgentNames = map[string]bool{
	"admin": true, "api": true, "c": true, "new": true, "components": true, "lib": true,
	"orbit-admin": true,
}

// AgentNamePattern is an agent's name (and a source's, a transform's).
const AgentNamePattern = `^[a-z][a-z0-9-]{0,62}$`

var (
	nameRE = regexp.MustCompile(AgentNamePattern)
	// CapabilityRE is a capability's shape: a name, optionally qualified
	// (docker-network:hetzner).
	CapabilityRE = regexp.MustCompile(`^[a-z][a-z0-9-]{0,62}(:[a-z0-9][a-z0-9._-]{0,62})?$`)
	envRefRE     = regexp.MustCompile(`\$\{([A-Z_][A-Z0-9_]*)\}`)
	// RepoRE is GitHub's owner/name shape.
	RepoRE    = regexp.MustCompile(`^[A-Za-z0-9-]+/[A-Za-z0-9._-]+$`)
	cronParse = cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow | cron.Descriptor)

	// Kinds every adapter registry must cover. Kept here so validation does
	// not import the adapters (which import spec).
	channelKinds = []string{"ntfy", "smtp", "webhook", "telegram", "log"}
	sourceKinds  = []string{"http", "prometheus", "loki", "github", "lake", "file", "webhook"}
	runnerKinds  = []string{"claude-code", "codex", "anthropic"}
	notifyWhen   = []string{"", "always", "agent", "never"}
	verifyChecks = []string{"figures"}
)

// ParseSchedule parses a five-field cron expression or a descriptor.
func ParseSchedule(expr string) (cron.Schedule, error) { return cronParse.Parse(expr) }

// Load reads orbit.yaml and every agents/*.yaml under dir, then validates the
// whole configuration. An invalid file fails the load: a deployment never
// runs with part of its configuration silently skipped.
func Load(dir string) (*Config, error) { return load(dir, nil, false) }

// Definition is an agent defined in the database (agent_definitions): its
// newest version, which LoadAll merges with the files.
type Definition struct {
	Name    string
	Version int
	Enabled bool
	// Spec is the agent as JSON, the same fields as an agents/*.yaml file.
	Spec json.RawMessage
}

// LoadAll is the configuration orbit runs: the files under dir, the
// database's agent definitions, and the built-in agents (builtin.go). A
// database agent named like a file agent is a configuration error; a
// built-in agent yields to a file or database agent of its name, so a
// deployment can replace it.
func LoadAll(dir string, defs []Definition) (*Config, error) { return load(dir, defs, true) }

// DecodeAgent reads one agent's JSON as strictly as a file is read: an
// unknown field is an error.
func DecodeAgent(b []byte) (*Agent, error) {
	a := &Agent{}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(a); err != nil {
		return nil, err
	}
	if dec.More() {
		return nil, errors.New("more than one JSON value")
	}
	return a, nil
}

func load(dir string, defs []Definition, all bool) (*Config, error) {
	cfg := &Config{Agents: map[string]*Agent{}}
	if err := decodeFile(filepath.Join(dir, "orbit.yaml"), &cfg.Deployment); err != nil {
		return nil, err
	}
	files, err := filepath.Glob(filepath.Join(dir, "agents", "*.yaml"))
	if err != nil {
		return nil, err
	}
	sort.Strings(files)
	for _, f := range files {
		a := &Agent{}
		if err := decodeFile(f, a); err != nil {
			return nil, err
		}
		if want := strings.TrimSuffix(filepath.Base(f), ".yaml"); a.Name != want {
			return nil, fmt.Errorf("%s: name %q must match the file name %q", f, a.Name, want)
		}
		if _, dup := cfg.Agents[a.Name]; dup {
			return nil, fmt.Errorf("%s: duplicate agent %q", f, a.Name)
		}
		a.Origin = OriginFile
		cfg.Agents[a.Name] = a
	}
	for _, d := range defs {
		a, err := DecodeAgent(d.Spec)
		if err != nil {
			return nil, fmt.Errorf("database agent %q version %d: %w", d.Name, d.Version, err)
		}
		if a.Name != d.Name {
			return nil, fmt.Errorf("database agent %q version %d: its spec names %q", d.Name, d.Version, a.Name)
		}
		if _, dup := cfg.Agents[a.Name]; dup {
			return nil, fmt.Errorf("agent %q is defined twice: in agents/%s.yaml and in the database", a.Name, a.Name)
		}
		enabled := d.Enabled
		a.Enabled, a.Origin, a.Version = &enabled, OriginDatabase, d.Version
		cfg.Agents[a.Name] = a
	}
	if all {
		for _, a := range Builtins(cfg.Deployment) {
			if _, taken := cfg.Agents[a.Name]; !taken {
				cfg.Agents[a.Name] = a
			}
		}
	}
	cfg.applyDefaults()
	cfg.applyBuildDefaults()
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	for _, a := range cfg.Agents {
		b, err := json.Marshal(a)
		if err != nil {
			return nil, err
		}
		sum := sha256.Sum256(b)
		a.SHA = hex.EncodeToString(sum[:])
	}
	return cfg, nil
}

func decodeFile(path string, into any) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	dec := yaml.NewDecoder(f)
	dec.KnownFields(true)
	if err := dec.Decode(into); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	return nil
}

func (c *Config) applyDefaults() {
	d := &c.Deployment
	if d.Timezone == "" {
		d.Timezone = "UTC"
	}
	if d.Defaults.Timeout == 0 {
		// Long enough for a multi-turn agent reading a few files; a run that
		// needs more names its own.
		d.Defaults.Timeout = Duration(15 * time.Minute)
	}
	c.applyMailDefaults()
	if d.Defaults.DecisionStall == 0 {
		// A person answers within a day, or the parent should hear about it.
		d.Defaults.DecisionStall = Duration(24 * time.Hour)
	}
	c.applyUsageDefaults()
	if d.Defaults.MemoryMaxBytes == 0 {
		// About 4k tokens: memory rides in every prompt, so it must stay a
		// small fraction of the context the run needs for its inputs.
		d.Defaults.MemoryMaxBytes = 16 << 10
	}
	for _, a := range c.Agents {
		if a.Timezone == "" {
			a.Timezone = d.Timezone
		}
		if a.Runner == "" {
			a.Runner = d.Defaults.Runner
		}
		if a.Timeout == 0 {
			a.Timeout = d.Defaults.Timeout
		}
		if len(a.Notify.Channels) == 0 {
			a.Notify.Channels = d.Defaults.Channels
		}
		if a.Notify.When == "" {
			a.Notify.When = "always"
		}
		if a.Session != nil && a.Session.Resume && a.Session.Idle == 0 {
			a.Session.Idle = Duration(DefaultSessionIdle)
		}
		for i := range a.Sources {
			if a.Sources[i].MaxBytes == 0 {
				// A month of transactions from Sure is about 300 KB; 8 MB is
				// headroom for a year without letting one source fill the disk.
				a.Sources[i].MaxBytes = 8 << 20
			}
		}
	}
}

// Validate checks every cross-reference: runners, channels, endpoints,
// schedules, timezones and action types.
func (c *Config) Validate() error {
	var errs []error
	if loc, err := time.LoadLocation(c.Deployment.Timezone); err != nil {
		errs = append(errs, fmt.Errorf("orbit.yaml timezone: %w", err))
	} else {
		c.Deployment.loc = loc
	}
	d := c.Deployment
	for name, ch := range d.Channels {
		if !nameRE.MatchString(name) {
			errs = append(errs, fmt.Errorf("channel %q: invalid name", name))
		}
		if !slices.Contains(channelKinds, ch.Kind) {
			errs = append(errs, fmt.Errorf("channel %q: unknown kind %q (want one of %v)", name, ch.Kind, channelKinds))
		}
		switch ch.Kind {
		case "ntfy":
			if ch.URL == "" || (ch.Topic == "" && ch.TopicEnv == "") {
				errs = append(errs, fmt.Errorf("channel %q: ntfy needs url and topic or topic_env", name))
			}
		case "smtp":
			if ch.Host == "" || ch.Port == 0 || ch.From == "" || len(ch.To) == 0 {
				errs = append(errs, fmt.Errorf("channel %q: smtp needs host, port, from and to", name))
			}
		case "webhook":
			if ch.URL == "" {
				errs = append(errs, fmt.Errorf("channel %q: webhook needs url", name))
			}
		case "telegram":
			if ch.TokenEnv == "" || ch.ChatID == "" {
				errs = append(errs, fmt.Errorf("channel %q: telegram needs token_env and chat_id", name))
			} else if id, err := strconv.ParseInt(ch.ChatID, 10, 64); err != nil || id == 0 {
				errs = append(errs, fmt.Errorf("channel %q: telegram chat_id must be a non-zero integer", name))
			}
		}
	}
	for _, name := range slices.Sorted(maps.Keys(d.Endpoints)) {
		errs = append(errs, c.validateEndpoint(name, d.Endpoints[name])...)
	}
	for name, r := range d.Runners {
		if !nameRE.MatchString(name) {
			errs = append(errs, fmt.Errorf("runner %q: invalid name", name))
		}
		if !slices.Contains(runnerKinds, r.Kind) {
			errs = append(errs, fmt.Errorf("runner %q: unknown kind %q (want one of %v)", name, r.Kind, runnerKinds))
		}
		if r.Kind == "anthropic" && r.APIKeyEnv == "" {
			errs = append(errs, fmt.Errorf("runner %q: anthropic needs api_key_env", name))
		}
	}
	if g := d.GitHub; g != nil {
		if g.AppID <= 0 {
			errs = append(errs, fmt.Errorf("github: app_id is required"))
		}
		if g.PrivateKeyEnv == "" {
			errs = append(errs, fmt.Errorf("github: private_key_env is required"))
		}
		if len(g.Repositories) == 0 {
			errs = append(errs, fmt.Errorf("github: repositories must list at least one owner/name"))
		}
		for _, r := range g.Repositories {
			if !RepoRE.MatchString(r) {
				errs = append(errs, fmt.Errorf("github: repository %q is not owner/name", r))
			}
		}
		for _, r := range g.ReadRepositories {
			if !RepoRE.MatchString(r) || strings.Contains(r, "..") {
				errs = append(errs, fmt.Errorf("github: read_repositories: %q is not owner/name", r))
			}
		}
		for _, r := range g.CommentRepositories {
			if !RepoRE.MatchString(r) || strings.Contains(r, "..") {
				errs = append(errs, fmt.Errorf("github: comment_repositories: %q is not owner/name", r))
			}
		}
	}
	if d.Board != nil && d.Board.Idle < 0 {
		errs = append(errs, fmt.Errorf("board.idle must not be negative"))
	}
	for _, ch := range d.Defaults.Channels {
		if _, ok := d.Channels[ch]; !ok {
			errs = append(errs, fmt.Errorf("defaults.channels: unknown channel %q", ch))
		}
	}
	names := make([]string, 0, len(c.Agents))
	for n := range c.Agents {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		errs = append(errs, c.validateAgent(c.Agents[n])...)
	}
	errs = append(errs, c.validateCredentials()...)
	errs = append(errs, c.validateConnectors()...)
	errs = append(errs, c.validateGoogleActions()...)
	errs = append(errs, c.validateExpires()...)
	errs = append(errs, c.validateWebhookSecrets()...)
	errs = append(errs, c.validateBuild()...)
	if sb := c.Deployment.Sandbox; sb != nil && !strings.HasPrefix(sb.Sandboxd, "unix:/") && !strings.Contains(sb.Sandboxd, ":") {
		errs = append(errs, fmt.Errorf("sandbox: sandboxd %q is neither host:port nor unix:/path", sb.Sandboxd))
	}
	errs = append(errs, c.validateMail()...)
	errs = append(errs, c.validateTelegram()...)
	errs = append(errs, c.validateUsage()...)
	return errors.Join(errs...)
}

func (c *Config) validateAgent(a *Agent) []error {
	var errs []error
	at := func(format string, args ...any) {
		errs = append(errs, fmt.Errorf("agent %q: "+format, append([]any{a.Name}, args...)...))
	}
	if !nameRE.MatchString(a.Name) {
		at("name must match %s", nameRE)
	}
	if ReservedAgentNames[a.Name] {
		at("name is reserved: the UI routes /orbit/%s to its own page, so an agent there could never be opened", a.Name)
	}
	if strings.TrimSpace(a.Charter) == "" {
		at("charter is required")
	}
	if err := a.SetTimezone(a.Timezone); err != nil {
		at("timezone: %v", err)
	}
	if b := a.Budget; b != nil {
		if b.Period != "day" && b.Period != "week" && b.Period != "month" {
			at("budget.period must be day, week or month")
		}
		if b.MaxCostUSD < 0 || math.IsNaN(b.MaxCostUSD) || math.IsInf(b.MaxCostUSD, 0) {
			at("budget.max_cost_usd must be finite and non-negative")
		}
		if b.MaxRuns < 0 {
			at("budget.max_runs must be non-negative")
		}
		if b.MaxCostUSD == 0 && b.MaxRuns == 0 {
			at("budget needs max_cost_usd or max_runs")
		}
	}
	if r, ok := c.Deployment.Runners[a.Runner]; !ok {
		at("unknown runner %q", a.Runner)
	} else if len(a.Tools) > 0 && r.Kind != "claude-code" {
		// codex and anthropic have no per-tool grant to map the list onto, so every run would fail; refuse the spec instead.
		at("tools: runner %q is %s, which takes no tools (only a claude-code runner does)", a.Runner, r.Kind)
	}
	for i, t := range a.Triggers {
		if t.IsWebhook() {
			errs = append(errs, c.validateWebhookTrigger(a, i, t)...)
			continue
		}
		if t.Secret != "" || t.MaxBytes != 0 {
			at("trigger %d: secret and max_bytes are for a webhook trigger", i)
		}
		if g := time.Duration(t.MissedGrace); g != 0 && (g < MinMissedGrace || g > MaxMissedGrace) {
			at("trigger %d: missed_grace must be between %s and %s", i, MinMissedGrace, MaxMissedGrace)
		}
		if _, err := TriggerSchedule(t, a.Location()); err != nil {
			at("trigger %d: %v", i, err)
		}
	}
	seen := map[string]bool{}
	for _, s := range a.Sources {
		if !nameRE.MatchString(s.Name) {
			at("source %q: invalid name", s.Name)
		}
		if seen[s.Name] {
			at("source %q: duplicate", s.Name)
		}
		seen[s.Name] = true
		if !slices.Contains(sourceKinds, s.Kind) {
			at("source %q: unknown kind %q (want one of %v)", s.Name, s.Kind, sourceKinds)
		}
		if s.Kind != "http" && len(s.ByTrigger) > 0 {
			at("source %q: by_trigger is only for http sources", s.Name)
		}
		switch s.Kind {
		case "http":
			if s.URL == "" {
				at("source %q: url is required", s.Name)
			}
			if p := s.Paginate; p != nil && (p.PageParam == "" || p.ItemsPath == "") {
				at("source %q: paginate needs page_param and items_path", s.Name)
			}
			for _, tr := range slices.Sorted(maps.Keys(s.ByTrigger)) {
				switch o := s.ByTrigger[tr]; {
				case !slices.Contains(SourceTriggers, tr):
					at("source %q: by_trigger: unknown trigger %q (want one of %v)", s.Name, tr, SourceTriggers)
				case len(o.Query) == 0:
					at("source %q: by_trigger.%s: query is required", s.Name, tr)
				case s.Paginate != nil && hasKey(o.Query, s.Paginate.PageParam):
					at("source %q: by_trigger.%s: query may not set the page parameter %q", s.Name, tr, s.Paginate.PageParam)
				}
			}
		}
		switch s.Kind {
		case "prometheus":
			if s.URL == "" || s.Expr == "" {
				at("source %q: prometheus needs url and expr", s.Name)
			}
		case "loki":
			if s.URL == "" || s.Expr == "" {
				at("source %q: loki needs url and expr", s.Name)
			}
			if s.Since < 0 || time.Duration(s.Since) > MaxLokiSince {
				at("source %q: since is at most %s", s.Name, MaxLokiSince)
			}
			if s.Limit < 0 || s.Limit > MaxLokiLimit {
				at("source %q: limit is at most %d lines", s.Name, MaxLokiLimit)
			}
		case "github":
			errs = append(errs, c.validateGitHubSource(a, s)...)
		case "lake":
			errs = append(errs, c.validateLakeSource(a, s)...)
		case "file":
			if s.Path == "" {
				at("source %q: path is required", s.Name)
			}
		case "webhook":
			if s.URL != "" || s.Expr != "" || s.Path != "" || len(s.Headers) > 0 || len(s.Query) > 0 || s.Paginate != nil {
				at("source %q: a webhook source is the delivery that started the run, and takes no url, expr, path, headers, query or paginate", s.Name)
			}
			if !a.hasWebhook() {
				at("source %q: a webhook source needs a webhook trigger", s.Name)
			}
		}
		if s.Kind != "github" && (s.Repo != "" || s.Resource != "" || s.Ref != "" || s.State != "") {
			at("source %q: repo, resource, ref and state are for a github source", s.Name)
		}
		if s.Kind != "github" && s.Kind != "loki" && s.Limit != 0 {
			at("source %q: limit is for a github or loki source", s.Name)
		}
		if s.Kind != "loki" && s.Since != 0 {
			at("source %q: since is for a loki source", s.Name)
		}
		if s.Kind != "lake" && (s.Bucket != "" || s.Prefix != "" || s.Region != "" || s.AccessKeyID != "" || s.SecretAccessKey != "" || s.SessionToken != "" || s.MaxObjects != 0) {
			at("source %q: bucket, prefix, region, S3 credentials and max_objects are for a lake source", s.Name)
		}
	}
	for _, t := range a.Transforms {
		if !nameRE.MatchString(t.Name) {
			at("transform %q: invalid name", t.Name)
		}
		if seen[t.Name] {
			at("transform %q: name is taken by another source or transform", t.Name)
		}
		seen[t.Name] = true
		if !slices.ContainsFunc(a.Sources, func(s Source) bool { return s.Name == t.Source }) {
			at("transform %q: unknown source %q", t.Name, t.Source)
		}
		params, err := json.Marshal(t.Params)
		if err == nil {
			err = transforms.Validate(t.Kind, params)
		}
		if err != nil {
			at("transform %q: %v", t.Name, err)
		}
	}
	for i, g := range a.Gates {
		switch g.Kind {
		case "unchanged":
			if g.Input != "" || g.Items != "" || g.N != 0 || g.Command != "" || g.Timeout != 0 {
				at("gate %d: unchanged takes no input, items, n, command or timeout", i)
			}
		case "precheck":
			if a.Restricted() {
				// The API and agent:define write database agents; a
				// command on orbit's host is for reviewed files only.
				at("gate %d: a precheck runs a command on orbit's host, so only an agent in the configuration files may have one", i)
			}
			if strings.TrimSpace(g.Command) == "" {
				at("gate %d: precheck needs a command", i)
			}
			if g.Input != "" || g.Items != "" || g.N != 0 {
				at("gate %d: precheck takes a command and a timeout only", i)
			}
			if g.Timeout < 0 || time.Duration(g.Timeout) > MaxPrecheckTimeout {
				at("gate %d: timeout is at most %s", i, MaxPrecheckTimeout)
			}
		case "items_above":
			if g.Command != "" || g.Timeout != 0 {
				at("gate %d: items_above takes no command or timeout", i)
			}
			if !seen[g.Input] {
				at("gate %d: input %q is not a source or transform of this agent", i, g.Input)
			}
			if g.N < 0 {
				at("gate %d: n must be at least 0", i)
			}
		default:
			at("gate %d: unknown kind %q (want one of %v)", i, g.Kind, GateKinds)
		}
	}
	for typ, rule := range a.Actions {
		if !slices.Contains([]Rule{RuleAuto, RuleApprove, RuleDeny}, rule) {
			at("action %q: rule must be auto, approve or deny", typ)
		}
		switch {
		case typ == "notify", typ == "continue", typ == "agent:define", typ == "agent:save", typ == "skill:write",
			typ == "schedule:add", typ == "schedule:pause", typ == "schedule:resume", typ == "schedule:remove", typ == "run:now", typ == "template:create":
		case typ == "delegate":
			if len(a.Delegates) == 0 && len(a.DelegateTemplates) == 0 {
				at("action %q: needs delegates or delegate_templates, the targets this one may hand tasks to", typ)
			}
		case typ == "task:message", typ == "task:cancel", typ == "task:retry":
			if len(a.Delegates) == 0 {
				at("action %q: needs delegates, the agents this one may hand tasks to", typ)
			}
		case strings.HasPrefix(typ, "webhook:"):
			if _, ok := c.Deployment.Endpoints[strings.TrimPrefix(typ, "webhook:")]; !ok {
				at("action %q: unknown endpoint", typ)
			}
		case typ == "github:merge", typ == "github:clone", typ == "github:pr":
			if c.Deployment.GitHub == nil {
				at("action %q: needs a github block in orbit.yaml", typ)
			}
		case typ == "github:comment":
			if c.Deployment.GitHub == nil || len(c.Deployment.GitHub.CommentRepositories) == 0 {
				at("action %q: needs github.comment_repositories in orbit.yaml", typ)
			}
		case typ == "gmail:draft":
			if c.Deployment.GoogleActions == nil || c.Deployment.GoogleActions.GmailDraft == nil {
				at("action %q: needs google_actions.gmail_draft in orbit.yaml", typ)
			}
		case typ == "calendar:create":
			if c.Deployment.GoogleActions == nil || c.Deployment.GoogleActions.CalendarCreate == nil {
				at("action %q: needs google_actions.calendar_create in orbit.yaml", typ)
			}
		case strings.HasPrefix(typ, ConnectorRulePrefix):
			// validateAgentConnectors
		case strings.HasPrefix(typ, ConnectorActionPrefix):
			at("action %q: a connector tool's rule is named mcp:<connector>:<tool>", typ)
		default:
			at("action %q: unknown action type (want notify, continue, agent:define, agent:save, skill:write, template:create, gmail:draft, calendar:create, schedule:add, schedule:pause, schedule:resume, schedule:remove, run:now, delegate, task:message, task:cancel, task:retry, webhook:<endpoint>, github:merge, github:clone, github:pr, github:comment or mcp:<connector>:<tool>)", typ)
		}
	}
	for _, ch := range a.Notify.Channels {
		if _, ok := c.Deployment.Channels[ch]; !ok {
			at("notify: unknown channel %q", ch)
		}
	}
	if !slices.Contains(notifyWhen, a.Notify.When) {
		at("notify.when must be always, agent or never")
	}
	for _, v := range a.Verify {
		if !slices.Contains(verifyChecks, v) {
			at("verify: unknown check %q (want one of %v)", v, verifyChecks)
		}
	}
	if len(a.Hooks) > 0 && c.Deployment.Runners[a.Runner].Kind != "claude-code" {
		at("hooks: need a claude-code runner")
	}
	if len(a.Hooks) > MaxHooks {
		at("hooks: at most %d", MaxHooks)
	}
	if len(a.Starters) > MaxStarters {
		at("starters: at most %d", MaxStarters)
	}
	for i, st := range a.Starters {
		if strings.TrimSpace(st) == "" || len(st) > MaxStarterLen {
			at("starters[%d]: 1 to %d bytes", i, MaxStarterLen)
		}
	}
	for i, h := range a.Hooks {
		if !slices.Contains(HookEvents, h.Event) {
			at("hooks[%d]: event must be one of %v", i, HookEvents)
		}
		if h.Matcher != "" && !slices.Contains(ToolHookEvents, h.Event) {
			at("hooks[%d]: only %v take a matcher", i, ToolHookEvents)
		}
		n := 0
		for _, set := range []bool{h.Say != "", h.Deny != "", len(h.Run) > 0} {
			if set {
				n++
			}
		}
		if n != 1 {
			at("hooks[%d]: give exactly one of say, deny and run", i)
		}
		if h.Deny != "" && h.Event != "PreToolUse" {
			at("hooks[%d]: deny is for PreToolUse", i)
		}
		if len(h.Say) > MaxHookText || len(h.Deny) > MaxHookText {
			at("hooks[%d]: say and deny are at most %d bytes", i, MaxHookText)
		}
		if len(h.Run) > 0 && h.Run[0] == "" {
			at("hooks[%d]: run names a command", i)
		}
		if h.Timeout < 0 || time.Duration(h.Timeout) > MaxHookTimeout || (h.Timeout != 0 && len(h.Run) == 0) {
			at("hooks[%d]: timeout is for run, at most %s", i, MaxHookTimeout)
		}
	}
	a.delegateTargets = nil
	for _, d := range a.Delegates {
		switch {
		case d == "*":
			a.delegateTargets = slices.Sorted(maps.Keys(c.Agents))
		case c.Agents[d] == nil:
			at("delegates: %q is not a configured agent", d)
		case !slices.Contains(a.delegateTargets, d):
			a.delegateTargets = append(a.delegateTargets, d)
		}
	}
	if slices.Contains(a.Delegates, "*") && len(a.Delegates) > 1 {
		at("delegates: \"*\" already names every agent; list nothing beside it")
	}
	slices.Sort(a.delegateTargets)
	a.delegateTemplateTargets = nil
	for _, name := range a.DelegateTemplates {
		if name == "*" {
			if !slices.Contains(a.delegateTemplateTargets, name) {
				a.delegateTemplateTargets = append(a.delegateTemplateTargets, name)
			}
			continue
		}
		if !nameRE.MatchString(name) {
			at("delegate_templates: %q must match %s or be \"*\"", name, nameRE)
			continue
		}
		if !slices.Contains(a.delegateTemplateTargets, name) {
			a.delegateTemplateTargets = append(a.delegateTemplateTargets, name)
		}
	}
	if slices.Contains(a.DelegateTemplates, "*") && len(a.DelegateTemplates) > 1 {
		at("delegate_templates: \"*\" already names every active template; list nothing beside it")
	}
	slices.Sort(a.delegateTemplateTargets)
	seenSkill := map[string]bool{}
	for _, k := range a.Skills {
		if !nameRE.MatchString(k) {
			at("skills: %q must match %s", k, nameRE)
		}
		if seenSkill[k] {
			at("skills: %q is listed twice", k)
		}
		seenSkill[k] = true
	}
	seenTopic := map[string]bool{}
	for _, t := range a.Topics {
		if t == AllTopics {
			if len(a.Topics) > 1 {
				at("topics: %q is every topic, so it stands alone", AllTopics)
			}
			continue
		}
		if !nameRE.MatchString(t) {
			at("topics: %q must match %s", t, nameRE)
		}
		if seenTopic[t] {
			at("topics: %q is listed twice", t)
		}
		seenTopic[t] = true
	}
	if l := a.Learn; l != nil {
		if l.Idle != 0 && (time.Duration(l.Idle) < MinLearnIdle || time.Duration(l.Idle) > MaxLearnIdle) {
			at("learn.idle must be between %s and %s", MinLearnIdle, MaxLearnIdle)
		}
		if l.Runner != "" {
			if _, ok := c.Deployment.Runners[l.Runner]; !ok {
				at("learn.runner: unknown runner %q", l.Runner)
			}
		}
	}
	seenNeed := map[string]bool{}
	for _, n := range a.Needs {
		if !CapabilityRE.MatchString(n) {
			at("needs: %q is not a capability (want name or name:qualifier, lower case)", n)
		}
		if seenNeed[n] {
			at("needs: %q is listed twice", n)
		}
		seenNeed[n] = true
	}
	seenRepo := map[string]bool{}
	for i, r := range a.Repos {
		switch {
		case !RepoRE.MatchString(r.Repo) || strings.Contains(r.Repo, ".."):
			at("repos %d: %q is not owner/name", i, r.Repo)
		case !c.Deployment.AllowsRead(r.Repo):
			at("repos %d: %s is not in github.read_repositories", i, r.Repo)
		}
		if r.Ref != "" && (!RepoRefRE.MatchString(r.Ref) || strings.Contains(r.Ref, "..") || strings.HasSuffix(r.Ref, ".lock") || strings.HasSuffix(r.Ref, "/")) {
			at("repos %d: ref %q is not a branch, tag or commit sha", i, r.Ref)
		}
		if seenRepo[r.Name()] {
			at("repos %d: a second repository named %q (each is given as inputs/repos/<name>)", i, r.Name())
		}
		seenRepo[r.Name()] = true
	}
	if len(a.Repos) > 0 {
		if !slices.Contains(a.Needs, NeedsGitHub) {
			at("repos: needs %q in needs, so only a host holding the GitHub App's key claims its runs", NeedsGitHub)
		}
		if k := c.Deployment.Runners[a.Runner].Kind; k == "anthropic" {
			at("repos: an anthropic runner reads no files; give the agent a claude-code or codex runner")
		}
	}
	if a.Chat && c.Deployment.Runners[a.Runner].Kind != "claude-code" {
		at("chat: needs a claude-code runner (runner %q is %q)", a.Runner, c.Deployment.Runners[a.Runner].Kind)
	}
	if a.Sandbox != "" {
		if !slices.Contains(SandboxProfiles, a.Sandbox) {
			at("sandbox: %q is not a profile (want one of %v)", a.Sandbox, SandboxProfiles)
		}
		if !a.Chat {
			at("sandbox: needs chat: true")
		}
		if addr, _ := c.Deployment.SandboxService(); addr == "" {
			at("sandbox: needs sandboxd in orbit.yaml (a sandbox: or build: section)")
		}
	}
	errs = append(errs, a.validateWeb()...)
	errs = append(errs, a.validatePermissions()...)
	errs = append(errs, c.validateTrust(a)...)
	errs = append(errs, c.validateAgentConnectors(a)...)
	if ss := a.Session; ss != nil {
		rs := c.Deployment.Runners[a.Runner]
		if rs.Kind != "claude-code" {
			at("session: needs a claude-code runner (runner %q is %q)", a.Runner, rs.Kind)
		}
		if !ss.Resume && (ss.Idle != 0 || ss.Live != 0) {
			at("session: idle and live need resume: true")
		}
		if ss.Idle < 0 || time.Duration(ss.Idle) > MaxSessionIdle {
			at("session.idle must be between 0 and %s", MaxSessionIdle)
		}
		if ss.Stall != 0 && (time.Duration(ss.Stall) < MinSessionStall || time.Duration(ss.Stall) > MaxSessionStall) {
			at("session.stall must be between %s and %s", MinSessionStall, MaxSessionStall)
		}
		if ss.Live < 0 || time.Duration(ss.Live) > MaxSessionLive {
			at("session.live must be between 0 and %s", MaxSessionLive)
		}
		if ss.Live > ss.Idle {
			at("session.live must not exceed session.idle: a live process outliving the resume window would continue a session resume would not")
		}
		if ss.Live > 0 && rs.MaxBudgetUSD > 0 {
			// --max-budget-usd counts a process's whole lifetime, every
			// turn fed into it (sdk.d.ts, Options.maxBudgetUsd and
			// SDKResultMessage.total_cost_usd), so it is no per-run bound.
			at("session.live cannot be used with runner %q's max_budget_usd: a live process's budget would cover every run fed into it", a.Runner)
		}
	}
	return errs
}

// ExpandEnv replaces ${VAR} in s from lookup. A reference to an unset
// variable is an error, never an empty string: a blank credential would send
// an unauthenticated request that fails far from its cause.
func ExpandEnv(s string, lookup func(string) (string, bool)) (string, error) {
	var missing []string
	out := envRefRE.ReplaceAllStringFunc(s, func(m string) string {
		name := envRefRE.FindStringSubmatch(m)[1]
		v, ok := lookup(name)
		if !ok {
			missing = append(missing, name)
		}
		return v
	})
	if len(missing) > 0 {
		return "", fmt.Errorf("unset environment variable(s): %s", strings.Join(missing, ", "))
	}
	return out, nil
}

// EnvRefs lists the variables s references, for `orbit validate`.
func EnvRefs(s string) []string {
	var out []string
	for _, m := range envRefRE.FindAllStringSubmatch(s, -1) {
		out = append(out, m[1])
	}
	return out
}

func hasKey(m map[string]string, k string) bool {
	_, ok := m[k]
	return ok
}
