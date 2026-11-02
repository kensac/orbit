package engine

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/kensac/orbit/internal/spec"
	"github.com/kensac/orbit/internal/store"
)

// AlwaysAllow is the change "approve and always allow" makes beside approving act: the next version of act's agent with act's type under auto (for a connector call, its tool's mcp:<connector>:<tool> rule), through DefineAgent (revertible, with by as its author and no run, as a change made through the API). With dryRun it is validated and nothing is stored. An agent in the configuration files is ErrReadOnly; a type already auto is a conflict, since what holds such an action is not its rule (an agent:define that widens the agent always waits for a person).
func (e *Engine) AlwaysAllow(ctx context.Context, act *store.Action, by string, dryRun bool) (*Defined, error) {
	a, version, err := e.definedSpec(ctx, act.Agent)
	if err != nil {
		return nil, err
	}
	// A connector call's rule is named by its tool, mcp:<connector>:<tool>, not by its action type.
	key := act.Type
	if conn, tool, ok := spec.ParseConnectorAction(act.Type); ok {
		key = spec.ConnectorRuleKey(conn, tool)
	}
	if a.RuleFor(key) == spec.RuleAuto {
		return nil, fmt.Errorf("%s is already auto for %s; this action waits for another reason (a change that widens an agent always does): %w", act.Type, act.Agent, store.ErrConflict)
	}
	if a.Actions == nil {
		a.Actions = spec.ActionSet{}
	}
	a.Actions[key] = spec.RuleAuto
	b, err := json.Marshal(a)
	if err != nil {
		return nil, err
	}
	if by == "operator" {
		by = ""
	}
	return e.DefineAgent(ctx, DefineRequest{Name: a.Name, Spec: b, Author: by, Upsert: true, Base: version, DryRun: dryRun})
}
