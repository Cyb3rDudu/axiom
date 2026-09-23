// Command axiom is the canonical runtime binary (ADR 0001 §3, F05 #299):
// role-based serving on top of the composition root (#298), plus the
// minimum operational CLI (version, doctor, config get/validate). The
// role surface (`serve all|api|library|store`) is the contract F06–F10
// dock onto — library/store refuse loudly until their components are
// extracted.
package main

import (
	"os"

	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/cli"
)

func main() {
	os.Exit(cli.Run("axiom", os.Args))
}
