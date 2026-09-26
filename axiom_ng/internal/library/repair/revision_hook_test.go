// revision_hook_test.go — F06 (#300) witness for the heal-point
// source-revision Mits-Schrieb: Apply fires RevisionHook exactly
// once after a successful heal, with the NEW attachment key and the
// sha256 of the healed artifact. The hook lives on the shared ApplyCase
// seam, so this one witness covers BOTH routes (verdict auto-apply and
// manual custody — they run the identical sequence). Hermetic: fake
// write server, no DB.
package repair

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"

	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/zoteroprovider"
)

func TestApplyFiresRevisionHookAfterHeal(t *testing.T) {
	root := t.TempDir()
	orig := filepath.Join(root, "orig.pdf")
	if err := os.WriteFile(orig, []byte("original"), 0o644); err != nil {
		t.Fatal(err)
	}
	fw := newFakeWrite("BROKEN1")
	srv := fw.server(t)
	wc := zoteroprovider.NewWriteClient(srv.URL, "srv", "key")

	var calls [][2]string
	healed := []byte("healed bytes")
	rec := &ManualRecord{AttachmentKey: "BROKEN1", DocumentKey: "P1",
		Reason: "revision hook test", OriginalPath: orig, ContentType: "application/pdf", CreatedAt: ManualNow()}
	res, err := Apply(context.Background(), &ManualDeps{Write: wc, Root: root, Record: rec, RunID: ManualRunID("BROKEN1")}, root, ApplyCase{
		CaseID: "hook-BROKEN1", AttachmentKey: "BROKEN1", DocumentKey: "P1",
		Title: "T", Year: 2020, SrcPath: orig, ContentType: "application/pdf",
		RevisionHook: func(newAttachmentKey, contentHash string) {
			calls = append(calls, [2]string{newAttachmentKey, contentHash})
		},
	}, healed)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(healed)
	want := [2]string{res.NewAttachmentKey, hex.EncodeToString(sum[:])}
	if len(calls) != 1 || calls[0] != want {
		t.Fatalf("revision hook must fire exactly once with (new key, content hash): got %v, want %v", calls, want)
	}
}
