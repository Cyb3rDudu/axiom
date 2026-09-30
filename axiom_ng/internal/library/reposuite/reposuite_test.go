// reposuite_test.go — the anti-thinning guard's teeth (F12 #306): a
// suite body that Skips must surface as a violation; a clean run must
// carry zero violations. Pure in-process, engine-free.
package reposuite

import "testing"

func TestSkipGuardProbes(t *testing.T) {
	rec := &skipRecord{}
	t.Run("planted-skip-is-a-violation", func(t *testing.T) {
		g := &guardedT{T: t, rec: rec}
		g.Skip("engine-specific skip planted by the probe")
	})
	if v := rec.violations(); len(v) == 0 {
		t.Fatal("a planted engine-specific skip must be recorded as a violation")
	}
	// SkipNow — the third skip spelling — is recorded as well.
	rec3 := &skipRecord{}
	t.Run("planted-skipnow-is-a-violation", func(t *testing.T) {
		g3 := &guardedT{T: t, rec: rec3}
		g3.SkipNow()
	})
	if v := rec3.violations(); len(v) == 0 {
		t.Fatal("a planted SkipNow must be recorded as a violation")
	}

	rec2 := &skipRecord{}
	if v := rec2.violations(); len(v) != 0 {
		t.Fatalf("clean record must carry zero violations, got %v", v)
	}
}
