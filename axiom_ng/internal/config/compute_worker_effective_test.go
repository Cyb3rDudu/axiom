package config

import (
	"testing"

	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/deprecate"
)

// TestEffectiveComputeWorkerURLShadowingMatrix — F10 #304: the two
// compute-worker URL rows must tell the resolver's (computeWorkerEnv's)
// truth about WHO fed the effective value — the generalized dual-fed
// mechanism behind the F08 worker-command matrix. The remote-carrier
// pointing var is the highest-traffic member of the family; it stands in
// for all seven dual-fed compute-worker fields.
func TestEffectiveComputeWorkerURLShadowingMatrix(t *testing.T) {
	deprecate.SetSilent(true)
	t.Cleanup(func() { deprecate.SetSilent(false) })
	row := func() (canonical, legacy Entry) {
		for _, e := range Effective(Load()) {
			switch e.Env {
			case "AXIOM_COMPUTE_WORKER_URL":
				canonical = e
			case "AXIOM_PROCESSOR_URL":
				legacy = e
			}
		}
		if canonical.Env == "" || legacy.Env == "" {
			t.Fatal("compute-worker URL rows missing from effective view")
		}
		return
	}

	t.Run("both set: canonical wins, legacy shadowed", func(t *testing.T) {
		t.Setenv("AXIOM_COMPUTE_WORKER_URL", "http://carrier:19542")
		t.Setenv("AXIOM_PROCESSOR_URL", "http://legacy:8012")
		canonical, legacy := row()
		if canonical.Source != "env" || canonical.Value != "http://carrier:19542" {
			t.Fatalf("canonical row = %v/%v, want env + canonical value", canonical.Source, canonical.Value)
		}
		if legacy.Source != "default" {
			t.Fatalf("shadowed legacy row source = %q, want default (it fed nothing)", legacy.Source)
		}
		if legacy.Value != "http://carrier:19542" {
			t.Fatalf("legacy row value = %v, want the effective (canonical) value", legacy.Value)
		}
	})

	t.Run("legacy only: legacy row is env, canonical renders default", func(t *testing.T) {
		t.Setenv("AXIOM_COMPUTE_WORKER_URL", "")
		t.Setenv("AXIOM_PROCESSOR_URL", "http://legacy:8012")
		canonical, legacy := row()
		if legacy.Source != "env" || legacy.Value != "http://legacy:8012" {
			t.Fatalf("legacy row = %v/%v, want env + legacy value", legacy.Source, legacy.Value)
		}
		if canonical.Source != "default" {
			t.Fatalf("canonical row source = %q, want default (its key fed nothing)", canonical.Source)
		}
		if canonical.Value != "http://legacy:8012" {
			t.Fatalf("canonical row value = %v, want the effective (legacy-fed) value", canonical.Value)
		}
	})

	t.Run("none set: both default", func(t *testing.T) {
		t.Setenv("AXIOM_COMPUTE_WORKER_URL", "")
		t.Setenv("AXIOM_PROCESSOR_URL", "")
		canonical, legacy := row()
		if canonical.Source != "default" || legacy.Source != "default" {
			t.Fatalf("sources = %q/%q, want default/default", canonical.Source, legacy.Source)
		}
	})
}
