// Package spec loads and validates orbit's configuration: the deployment file
// (orbit.yaml: channels, endpoints, defaults) and one file per agent
// (agents/*.yaml). Configuration lives in git next to the deployment; the
// database records the spec each run used, so a run stays explainable after
// its agent's file changes.
package spec

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/robfig/cron/v3"
	"gopkg.in/yaml.v3"
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
}

// AllowsRepo reports whether repo is one orbit may act on.
func (d Deployment) AllowsRepo(repo string) bool {
	return d.GitHub != nil && slices.Contains(d.GitHub.Repositories, repo)
}

// Defaults apply to every agent that does not override them.
type Defaults struct {
	Runner   string   `yaml:"runner" json:"runner"`
	Timeout  Duration `yaml:"timeout" json:"timeout"`
	Channels []string `yaml:"channels" json:"channels"`
	// MemoryMaxBytes bounds the memory document an agent may write back.
	MemoryMaxBytes int `yaml:"memory_max_bytes" json:"memory_max_bytes"`
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
}

// Endpoint is a named HTTP target a `webhook` action may call. The agent
// chooses only the endpoint name and the body; the URL and credentials stay
// in configuration, so a model cannot point a write at anything new.
type Endpoint struct {
	URL     string            `yaml:"url" json:"url"`
	Method  string            `yaml:"method" json:"method"`
	Headers map[string]string `yaml:"headers" json:"headers"`
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
	Charter  string     `yaml:"charter" json:"charter"`
	Enabled  *bool      `yaml:"enabled,omitempty" json:"enabled,omitempty"`
	Timezone string     `yaml:"timezone,omitempty" json:"timezone,omitempty"`
	Runner   string     `yaml:"runner,omitempty" json:"runner,omitempty"`
	Timeout  Duration   `yaml:"timeout,omitempty" json:"timeout,omitempty"`
	Triggers []Trigger  `yaml:"triggers" json:"triggers"`
	Sources  []Source   `yaml:"sources,omitempty" json:"sources,omitempty"`
	Tools    []string   `yaml:"tools,omitempty" json:"tools,omitempty"`
	Actions  ActionSet  `yaml:"actions,omitempty" json:"actions,omitempty"`
	Notify   NotifySpec `yaml:"notify,omitempty" json:"notify,omitempty"`
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
	// The API can attach more (docs/api.md, Memory entries).
	Topics []string `yaml:"topics,omitempty" json:"topics,omitempty"`

	// SHA is the hash of the agent's canonical JSON, set by Load.
	SHA string `yaml:"-" json:"-"`
}

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
}

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

// Trigger is one way an agent wakes. Exactly one of Schedule is set today;
// the struct leaves room for event triggers.
type Trigger struct {
	Schedule string `yaml:"schedule" json:"schedule"`
	// Prompt is the reason for this wake, appended to the charter.
	Prompt string `yaml:"prompt,omitempty" json:"prompt,omitempty"`
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
	// prometheus
	Expr string `yaml:"expr,omitempty" json:"expr,omitempty"`
	// file
	Path string `yaml:"path,omitempty" json:"path,omitempty"`
	// MaxBytes bounds one source's fetched size.
	MaxBytes int64 `yaml:"max_bytes,omitempty" json:"max_bytes,omitempty"`
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

// IsEnabled reports whether the agent's triggers fire. Manual runs work
// either way.
func (a *Agent) IsEnabled() bool { return a.Enabled == nil || *a.Enabled }

// RuleFor returns the rule for an action type, deny if unnamed.
func (a *Agent) RuleFor(actionType string) Rule {
	if r, ok := a.Actions[actionType]; ok {
		return r
	}
	return RuleDeny
}

var (
	nameRE   = regexp.MustCompile(`^[a-z][a-z0-9-]{0,62}$`)
	envRefRE = regexp.MustCompile(`\$\{([A-Z_][A-Z0-9_]*)\}`)
	// RepoRE is GitHub's owner/name shape.
	RepoRE    = regexp.MustCompile(`^[A-Za-z0-9-]+/[A-Za-z0-9._-]+$`)
	cronParse = cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow | cron.Descriptor)

	// Kinds every adapter registry must cover. Kept here so validation does
	// not import the adapters (which import spec).
	channelKinds = []string{"ntfy", "smtp", "webhook", "log"}
	sourceKinds  = []string{"http", "prometheus", "file"}
	runnerKinds  = []string{"claude-code", "codex", "anthropic"}
	notifyWhen   = []string{"", "always", "agent", "never"}
	verifyChecks = []string{"figures"}
)

// ParseSchedule parses a five-field cron expression or a descriptor.
func ParseSchedule(expr string) (cron.Schedule, error) { return cronParse.Parse(expr) }

// Load reads orbit.yaml and every agents/*.yaml under dir, then validates the
// whole configuration. An invalid file fails the load: a deployment never
// runs with part of its configuration silently skipped.
func Load(dir string) (*Config, error) {
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
		cfg.Agents[a.Name] = a
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
	d := c.Deployment
	if _, err := time.LoadLocation(d.Timezone); err != nil {
		errs = append(errs, fmt.Errorf("orbit.yaml timezone: %w", err))
	}
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
		}
	}
	for name, ep := range d.Endpoints {
		if !nameRE.MatchString(name) {
			errs = append(errs, fmt.Errorf("endpoint %q: invalid name", name))
		}
		if ep.URL == "" {
			errs = append(errs, fmt.Errorf("endpoint %q: url is required", name))
		}
		if ep.Method != "" && !slices.Contains([]string{"POST", "PUT", "PATCH"}, ep.Method) {
			errs = append(errs, fmt.Errorf("endpoint %q: method must be POST, PUT or PATCH", name))
		}
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
	errs = append(errs, c.validateBuild()...)
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
	if strings.TrimSpace(a.Charter) == "" {
		at("charter is required")
	}
	if _, err := time.LoadLocation(a.Timezone); err != nil {
		at("timezone: %v", err)
	}
	if _, ok := c.Deployment.Runners[a.Runner]; !ok {
		at("unknown runner %q", a.Runner)
	}
	for i, t := range a.Triggers {
		if t.Schedule == "" {
			at("trigger %d: schedule is required", i)
			continue
		}
		if _, err := ParseSchedule(t.Schedule); err != nil {
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
		switch s.Kind {
		case "http":
			if s.URL == "" {
				at("source %q: url is required", s.Name)
			}
			if p := s.Paginate; p != nil && (p.PageParam == "" || p.ItemsPath == "") {
				at("source %q: paginate needs page_param and items_path", s.Name)
			}
		case "prometheus":
			if s.URL == "" || s.Expr == "" {
				at("source %q: prometheus needs url and expr", s.Name)
			}
		case "file":
			if s.Path == "" {
				at("source %q: path is required", s.Name)
			}
		}
	}
	for typ, rule := range a.Actions {
		if !slices.Contains([]Rule{RuleAuto, RuleApprove, RuleDeny}, rule) {
			at("action %q: rule must be auto, approve or deny", typ)
		}
		switch {
		case typ == "notify", typ == "continue":
		case strings.HasPrefix(typ, "webhook:"):
			if _, ok := c.Deployment.Endpoints[strings.TrimPrefix(typ, "webhook:")]; !ok {
				at("action %q: unknown endpoint", typ)
			}
		case typ == "github:merge":
			if c.Deployment.GitHub == nil {
				at("action %q: needs a github block in orbit.yaml", typ)
			}
		default:
			at("action %q: unknown action type (want notify, continue, webhook:<endpoint> or github:merge)", typ)
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
	seenTopic := map[string]bool{}
	for _, t := range a.Topics {
		if !nameRE.MatchString(t) {
			at("topics: %q must match %s", t, nameRE)
		}
		if seenTopic[t] {
			at("topics: %q is listed twice", t)
		}
		seenTopic[t] = true
	}
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
