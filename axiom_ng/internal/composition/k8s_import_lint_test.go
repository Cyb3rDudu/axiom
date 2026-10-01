// k8s_import_lint_test.go — the F14 #308 domain-purity sonde for
// Kubernetes: the domain layer (everything under internal/, plus the
// entrypoints) stays free of k8s.io imports. The topology story is that
// the SAME binaries run all-in-one, split, containerized, and on K8s —
// roles per argument, manifests as deploy/ artifacts. If orchestration
// logic ever needs a client, it lives in a dedicated deploy tool, not in
// the domain; this sonde turns an accidental import red the moment it
// lands (the engine-type confinement pattern of F12, applied to K8s).
//
// Also pins the module graph: go.mod/go.sum must not carry k8s.io
// requirements — an unused requirement today is an imported client
// tomorrow.
package composition

import (
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// k8sImportLine: optional alias identifier, whitespace, then a quoted
// k8s.io path — exactly the two spellings an import line can take. The
// shape is ANCHORED so the sonde's own source (which mentions "k8s.io/
// inside string literals) cannot self-flag.
var k8sImportLine = regexp.MustCompile(`^\s*(?:[\w.]+\s+)?"k8s\.io/`)

// isK8sImportLine is the sonde's detector, shared by the tree scan and
// the teeth test so a rotted detector cannot stay green.
func isK8sImportLine(line string) bool { return k8sImportLine.MatchString(line) }

// TestNoKubernetesImportsAnywhere — red on any k8s.io import in any
// non-test or test .go file under the module, and on any k8s.io line in
// the module files.
func TestNoKubernetesImportsAnywhere(t *testing.T) {
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	var violations []string
	err = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			name := d.Name()
			if name == "vendor" || name == "testdata" || strings.HasPrefix(name, ".") {
				return filepath.SkipDir
			}
			return nil
		}
		base := filepath.Base(path)
		if base == "go.mod" || base == "go.sum" {
			body, rerr := os.ReadFile(path)
			if rerr != nil {
				return rerr
			}
			for i, line := range strings.Split(string(body), "\n") {
				if strings.Contains(line, "k8s.io/") {
					violations = append(violations, filepath.Join(root, base)+":"+strconv.Itoa(i+1)+": "+strings.TrimSpace(line))
				}
			}
			return nil
		}
		if !strings.HasSuffix(base, ".go") {
			return nil
		}
		body, rerr := os.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		rel, _ := filepath.Rel(root, path)
		for i, line := range strings.Split(string(body), "\n") {
			if isK8sImportLine(line) {
				violations = append(violations, rel+":"+strconv.Itoa(i+1)+": "+strings.TrimSpace(line))
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(violations) > 0 {
		t.Fatalf("k8s.io import sonde: the domain layer must stay Kubernetes-free (deploy/ ships manifests; orchestration clients would live in a deploy tool):\n%s",
			strings.Join(violations, "\n"))
	}
}

// TestK8sImportSondeHasTeeth — the red path: planted k8s.io import lines
// (plain and aliased) must be detected, and the sonde's own literal
// mentions must NOT be.
func TestK8sImportSondeHasTeeth(t *testing.T) {
	planted := "package probe\n\nimport (\n\t\"k8s.io/client-go/kubernetes\"\n\tclientgo \"k8s.io/apimachinery/pkg/apis/meta/v1\"\n)\n\nvar _ = kubernetes.TODO\n"
	var caught int
	for _, line := range strings.Split(planted, "\n") {
		if isK8sImportLine(line) {
			caught++
		}
	}
	if caught != 2 {
		t.Fatalf("sonde detector: %d planted k8s.io imports caught, want 2 (plain + aliased)", caught)
	}
	self := "return strings.Contains(strings.TrimSpace(line), \"\\\"k8s.io/\")"
	if isK8sImportLine(self) {
		t.Fatal("sonde detector self-flags its own string literal mention")
	}
}
