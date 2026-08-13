package dispatcher

// Dispatcher wiring for Gate 4 persistence: capability dimension extraction,
// processor identity, and durable-artifact staging. These bridge the negotiated
// processor capabilities to PersistResult's PersistOptions.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/processor"
	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/repo"
)

// capDim returns the int dense-embedding dimension the processor declared in
// /v1/capabilities, or 0 if unknown (validation then skips the dim check).
// Hivemind Gate-3 hint: dimensions is an int — no string fallback. A
// non-int/missing dimension yields 0 (skip), never a silent string coercion.
func (d *Dispatcher) capDim() int {
	if d.caps == nil {
		return 0
	}
	de, ok := d.caps.Models["dense_embedding"].(map[string]any)
	if !ok {
		return 0
	}
	switch v := de["dimensions"].(type) {
	case int:
		return v
	case int64:
		return int(v)
	case float64:
		return int(v)
	default:
		// string or missing — do NOT coerce; validation will not check dims.
		return 0
	}
}

// processorIdentity returns the name/version the processor declared in
// /v1/capabilities, used to stamp the completed job's processor identity.
func (d *Dispatcher) processorIdentity() (string, string) {
	if d.caps == nil {
		return "unknown", "unknown"
	}
	return d.caps.Processor.Name, d.caps.Processor.Version
}

// stageArtifacts fetches, verifies and durably commits every artifact the
// processor result declares (§13). Contract §13 defines result.artifacts as
// durable-only ("Unreferenced temporary files are never durable artifacts"),
// so every declared artifact is fetched, hash+length-verified, and committed.
// For each:
//   - sanitize the ref (reject '/', '..' — §18 path safety)
//   - GET /v1/jobs/{id}/artifacts/{ref}
//   - hash (sha256) and length-check the fetched bytes against the result's
//     declared digest/size (mismatch is a validation failure, terminal)
//   - stage under ArtifactRoot/<jobID>/<ref> then atomic rename on the same
//     filesystem (work-order §7 crash-safe strategy)
//
// Returns the verified ArtifactRecords for PersistResult. A digest/size
// mismatch returns an error so onCompleted makes the job terminal before any
// snapshot row is inserted (§14.4.6).
func (d *Dispatcher) stageArtifacts(ctx context.Context, jobID string, resultBytes []byte) ([]repo.ArtifactRecord, error) {
	var res struct {
		Artifacts []processor.Artifact `json:"artifacts"`
	}
	if err := json.Unmarshal(resultBytes, &res); err != nil {
		return nil, fmt.Errorf("decode result artifacts: %w", err)
	}
	if len(res.Artifacts) == 0 {
		return nil, nil
	}
	if d.cfg.ArtifactRoot == "" {
		return nil, errors.New("result declares artifacts but ArtifactRoot is not configured")
	}
	if err := os.MkdirAll(d.cfg.ArtifactRoot, 0o755); err != nil {
		return nil, fmt.Errorf("create artifact root: %w", err)
	}

	out := make([]repo.ArtifactRecord, 0, len(res.Artifacts))
	for _, a := range res.Artifacts {
		// W2: the ref is processor-controlled and hostile. Sanitize before using
		// it in a path (§18): reject path separators, traversal, empty.
		if !safeArtifactRef(a.Ref) {
			return nil, fmt.Errorf("artifact ref %q is not a safe filename (contains '/', '..', or is empty)", a.Ref)
		}
		bytes, err := d.client.Artifact(ctx, jobID, a.Ref)
		if err != nil {
			return nil, fmt.Errorf("fetch artifact %q: %w", a.Ref, err)
		}
		if int64(len(bytes)) != a.SizeBytes {
			return nil, fmt.Errorf("artifact %q size mismatch: got %d, result declared %d", a.Ref, len(bytes), a.SizeBytes)
		}
		sum := sha256.Sum256(bytes)
		got := hex.EncodeToString(sum[:])
		// Normalize: the processor may declare the sha256 with or without the
		// "sha256:" prefix (contract §10 example shows bare hex). Compare on the
		// bare hex to avoid a false mismatch on identical hashes.
		declared := a.SHA256
		declared = strings.TrimPrefix(declared, "sha256:")
		if got != declared {
			return nil, fmt.Errorf("artifact %q digest mismatch: got sha256:%s, result declared %s", a.Ref, got, a.SHA256)
		}
		finalPath := filepath.Join(d.cfg.ArtifactRoot, jobID, a.Ref)
		if err := os.MkdirAll(filepath.Dir(finalPath), 0o755); err != nil {
			return nil, fmt.Errorf("create artifact dir: %w", err)
		}
		staging := finalPath + ".staging"
		if err := os.WriteFile(staging, bytes, 0o644); err != nil {
			return nil, fmt.Errorf("stage artifact %q: %w", a.Ref, err)
		}
		if err := os.Rename(staging, finalPath); err != nil {
			_ = os.Remove(staging)
			return nil, fmt.Errorf("commit artifact %q: %w", a.Ref, err)
		}
		out = append(out, repo.ArtifactRecord{
			Ref: a.Ref, Kind: a.Kind, MediaType: a.MediaType,
			SHA256: a.SHA256, SizeBytes: a.SizeBytes, Retention: a.Retention,
			StoragePath: finalPath,
		})
	}
	return out, nil
}

// safeArtifactRef returns true iff ref is a bare filename safe for joining
// under ArtifactRoot/<jobID>/ — no path separators, no traversal, non-empty.
func safeArtifactRef(ref string) bool {
	if ref == "" || ref == "." || ref == ".." {
		return false
	}
	if strings.ContainsAny(ref, "/\\") {
		return false
	}
	if strings.Contains(ref, "..") {
		return false
	}
	return true
}
