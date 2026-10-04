// dm07_helpers_test.go — DM07 #316 unit coverage for the composition
// helpers that need no database: sameDatabase (the shared-database
// identity behind the legacy mirror Mits-Schrieb lane). The pool-backed
// paths (checkPoolRole against real roles, the denial matrix) live in
// the role-matrix drill IT.
package composition

import "testing"

func TestSameDatabase(t *testing.T) {
	cases := []struct {
		name string
		a, b string
		want bool
	}{
		{"identical strings", "postgresql://u:p@h:5432/db", "postgresql://u:p@h:5432/db", true},
		{"same db, different credentials (the split-credentials interim)", "postgresql://axiom_library:pw@h:5432/db", "postgresql://axiom_store:pw@h:5432/db", true},
		{"same db, keyword/value spelling", "postgresql://axiom_library:pw@h:5432/db", "host=h port=5432 dbname=db user=axiom_store", true},
		{"default port on one side", "postgresql://u@h/db", "postgresql://u@h:5432/db", true},
		{"different database", "postgresql://u@h/lib", "postgresql://u@h/store", false},
		{"different host", "postgresql://u@h1/db", "postgresql://u@h2/db", false},
		{"empty never shared", "", "postgresql://u@h/db", false},
		{"unparseable compares unequal (db.Open is the loud authority)", "not a dsn", "postgresql://u@h/db", false},
	}
	for _, tc := range cases {
		if got := sameDatabase(tc.a, tc.b); got != tc.want {
			t.Errorf("%s: sameDatabase(%q, %q) = %v, want %v", tc.name, tc.a, tc.b, got, tc.want)
		}
	}
}
