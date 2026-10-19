// #379: sync path translation for containerized deployments. Zotero's
// local API reports file:// URIs with host-absolute paths; a container
// sees the same blobs under a different prefix. AXIOM_ZOTERO_PATH_MAP
// (HOST_PREFIX:CONTAINER_PREFIX) rewrites the path AT THE DERIVATION
// POINT (LocalFilePath) so mirror projection, store intake and repair
// invoker all see the same translated path — one layer, nowhere else.
package zoteroprovider

import "testing"

// pairs is the shared #379 test map (host Zotero storage → container state).
func pairs() []PathPair {
	return []PathPair{{From: "/home/dudu/Zotero/storage", To: "/var/lib/axiom/zotero-storage"}}
}

func TestParsePathMap(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want []PathPair // nil = no pairs (unset semantics)
		err  bool
	}{
		{name: "unset is identity", in: "", want: nil},
		{name: "whitespace-only is identity", in: "   ", want: nil},
		{name: "single pair", in: "/home/dudu/Zotero/storage:/var/lib/axiom/zotero-storage",
			want: []PathPair{{From: "/home/dudu/Zotero/storage", To: "/var/lib/axiom/zotero-storage"}}},
		{name: "sides are trimmed", in: " /a/b : /c/d ", want: []PathPair{{From: "/a/b", To: "/c/d"}}},
		{name: "trailing slashes normalize away", in: "/a/b/:/c/d/",
			want: []PathPair{{From: "/a/b", To: "/c/d"}}},
		{name: "comma list keeps order", in: "/a:/x,/b:/y",
			want: []PathPair{{From: "/a", To: "/x"}, {From: "/b", To: "/y"}}},
		{name: "empty entries are slack", in: "/a:/x,,", want: []PathPair{{From: "/a", To: "/x"}}},
		{name: "missing colon", in: "/a/b", err: true},
		{name: "empty host side", in: ":/c/d", err: true},
		{name: "empty container side", in: "/a/b:", err: true},
		{name: "bare colon", in: ":", err: true},
		{name: "second pair malformed fails whole value", in: "/a:/x,broken", err: true},
		{name: "root prefix collapses to empty host side", in: "/:/c", err: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParsePathMap(tc.in)
			if tc.err {
				if err == nil {
					t.Fatalf("ParsePathMap(%q) = %v, want error", tc.in, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParsePathMap(%q) unexpected error: %v", tc.in, err)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("ParsePathMap(%q) = %v, want %v", tc.in, got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("ParsePathMap(%q)[%d] = %+v, want %+v", tc.in, i, got[i], tc.want[i])
				}
			}
		})
	}
}

func TestTranslatePathSegmentBoundary(t *testing.T) {
	m := pairs()
	cases := map[string]string{
		// Boundary cases: the prefix must end at a path segment.
		"/home/dudu/Zotero/storage/AB123/file.pdf": "/var/lib/axiom/zotero-storage/AB123/file.pdf",
		"/home/dudu/Zotero/storage":                "/var/lib/axiom/zotero-storage",
		// storageX is NOT storage — no match at segment granularity.
		"/home/dudu/Zotero/storageX/file.pdf": "/home/dudu/Zotero/storageX/file.pdf",
		"/home/dudu/Zotero/storageX":          "/home/dudu/Zotero/storageX",
		// Shorter / unrelated paths pass through byte-identical.
		"/home/dudu/Other/file.pdf":  "/home/dudu/Other/file.pdf",
		"/home/dudu/Zotero/file.pdf": "/home/dudu/Zotero/file.pdf",
		"relative/path.pdf":          "relative/path.pdf",
	}
	for in, want := range cases {
		if got := TranslatePath(m, in); got != want {
			t.Errorf("TranslatePath(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestTranslatePathFirstMatchWins(t *testing.T) {
	m := []PathPair{
		{From: "/data/zotero", To: "/mnt/z"},
		{From: "/data", To: "/mnt/data"},
	}
	if got := TranslatePath(m, "/data/zotero/x.pdf"); got != "/mnt/z/x.pdf" {
		t.Errorf("first pair must win, got %q", got)
	}
	if got := TranslatePath(m, "/data/other/x.pdf"); got != "/mnt/data/other/x.pdf" {
		t.Errorf("second pair must catch the rest, got %q", got)
	}
}

func TestLocalFilePathWithoutMapIsByteIdentical(t *testing.T) {
	cases := map[string]string{
		"file:///Users/x/Englert%20-%202019.pdf": "/Users/x/Englert - 2019.pdf",
		"file:///home/dudu/Zotero/storage/a.pdf": "/home/dudu/Zotero/storage/a.pdf",
		"/already/native/path.pdf":               "/already/native/path.pdf",
	}
	for in, want := range cases {
		if got := LocalFilePath(in); got != want {
			t.Errorf("LocalFilePath(%q) = %q, want %q (unset env must not translate)", in, got, want)
		}
	}
}

func TestTranslatePathAppliesAfterURIDecode(t *testing.T) {
	m := pairs()
	// Translation applies to the DERIVED path (URI-decoded), so encoded
	// attachment names inside a mapped prefix survive intact — the caller
	// composes LocalFilePath (pure URI→path) with TranslatePath.
	in := LocalFilePath("file:///home/dudu/Zotero/storage/Englert%20-%202019.pdf")
	want := "/var/lib/axiom/zotero-storage/Englert - 2019.pdf"
	if got := TranslatePath(m, in); got != want {
		t.Errorf("TranslatePath(LocalFilePath(...)) = %q, want %q", got, want)
	}
}
