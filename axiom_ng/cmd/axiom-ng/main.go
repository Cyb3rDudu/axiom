// Command axiom-ng is the compatibility alias entrypoint (ADR 0001 §3,
// F05 #299): the pre-0.2.0 public name, delegating to the same runtime
// surface as `axiom` (internal/cli). Every start records one use of the
// legacy name through the F02 deprecation witness — warned exactly once
// per process, counted on every one, exported in /api/health
// (deprecations) as the data basis for the 0.3.x+ removal decision.
//
// The alias keeps its full legacy surface: the KG mode flags (#244),
// --version, and the bare no-arg full-stack boot. It stays
// lieferfähig through 0.2.x; removal happens earliest in an announced
// major release, never silently.
package main

import (
	"os"

	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/cli"
	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/deprecate"
)

func main() {
	deprecate.Use("axiom-ng")
	os.Exit(cli.Run("axiom-ng", os.Args))
}
