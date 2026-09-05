// quarantine — custody CLI (#253): copies an ORIGINAL into the quarantine
// root via the same repair.Quarantine path the invoker uses (custody
// fail-closed). Operator tool for manual custody moves and the fixer IT.
//
// Usage: quarantine <root> <zotero-key> <source-path>
package main

import (
	"fmt"
	"os"

	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/repair"
)

func main() {
	if len(os.Args) != 4 {
		fmt.Fprintln(os.Stderr, "usage: quarantine <root> <zotero-key> <source-path>")
		os.Exit(2)
	}
	dst, err := repair.Quarantine(os.Args[1], os.Args[2], os.Args[3])
	if err != nil {
		fmt.Fprintln(os.Stderr, "quarantine:", err)
		os.Exit(1)
	}
	fmt.Println(dst)
}
