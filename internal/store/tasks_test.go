package store

import (
	"errors"
	"testing"
)

func TestADelegationsDependenciesAreADAGOfItsOwnKeys(t *testing.T) {
	for name, tasks := range map[string][]NewTask{
		"cycle":   {{Key: "a", After: []string{"b"}}, {Key: "b", After: []string{"a"}}},
		"self":    {{Key: "a", After: []string{"a"}}},
		"unknown": {{Key: "a", After: []string{"z"}}},
		"twice":   {{Key: "a"}, {Key: "a"}},
		"empty":   {},
	} {
		if err := ValidateDelegation(tasks); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: err = %v", name, err)
		}
	}
	ok := []NewTask{{Key: "c", After: []string{"a", "b"}}, {Key: "a"}, {Key: "b", After: []string{"a"}}}
	if err := ValidateDelegation(ok); err != nil {
		t.Fatal(err)
	}
}
