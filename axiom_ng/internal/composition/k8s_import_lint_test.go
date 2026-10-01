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
	"strconv"
	"strings"
	"testing"
)

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

// isK8sImportLine reports whether one source line is a quoted k8s.io
// import — the sonde's detector, shared by the tree scan and the teeth
// test so a rotted detector cannot stay green.
func isK8sImportLine(line string) bool {
	// any quoted k8s.io path on the line — covers aliased imports too
	return strings.Contains(strings.TrimSpace(line), "\"k8s.io/")
}

// TestK8sImportSondeHasTeeth — the red path: a planted k8s.io import line
// must be detected (the sonde's own detector, on an in-memory string).
func TestK8sImportSondeHasTeeth(t *testing.T) {
	planted := "package probe\n\nimport (\n\t\"k8s.io/client-go/kubernetes\"\n)\n\nvar _ = kubernetes.TODO\n"
	for _, line := range strings.Split(planted, "\n") {
		if isK8sImportLine(line) {
			return // caught
		}
	}
	t.Fatal("sonde detector does not catch a planted k8s.io import")
}
