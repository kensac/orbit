package store

import (
	"errors"
	"testing"
)

func TestADelegationsOwnDependenciesAreADAG(t *testing.T) {
	for name, tasks := range map[string][]NewTask{
		"cycle": {{Key: "a", After: []string{"b"}}, {Key: "b", After: []string{"a"}}},
		"self":  {{Key: "a", After: []string{"a"}}},
		"twice": {{Key: "a"}, {Key: "a"}},
		"empty": {},
	} {
		if err := ValidateDelegation(tasks); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: err = %v", name, err)
		}
	}
	// A key the batch does not have names an earlier task; Delegate
	// resolves it.
	ok := []NewTask{{Key: "c", After: []string{"a", "b"}}, {Key: "a", After: []string{"earlier"}}, {Key: "b", After: []string{"a"}}}
	if err := ValidateDelegation(ok); err != nil {
		t.Fatal(err)
	}
}
