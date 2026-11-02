package store

import (
	"io/fs"
	"strconv"
	"strings"
	"testing"
)

// Two migrations with one version make every orbit start fail ("found duplicate migration version"), and parallel PRs each pass their own CI. This catches it on main and on any branch that already holds both.
func TestMigrationVersionsAreUnique(t *testing.T) {
	entries, err := fs.ReadDir(migrations, "migrations")
	if err != nil {
		t.Fatal(err)
	}
	seen := map[int64]string{}
	for _, e := range entries {
		prefix, _, ok := strings.Cut(e.Name(), "_")
		if !ok {
			t.Fatalf("%s: want <version>_<name>.sql", e.Name())
		}
		v, err := strconv.ParseInt(prefix, 10, 64)
		if err != nil {
			t.Fatalf("%s: version %q is not a number", e.Name(), prefix)
		}
		if other, dup := seen[v]; dup {
			t.Fatalf("migrations %s and %s share version %d: rename the newer one to a UTC timestamp (YYYYMMDDHHMMSS_name.sql)", other, e.Name(), v)
		}
		seen[v] = e.Name()
	}
}
