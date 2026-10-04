// library.go — the Library component's table contract for the bundle
// (DM04 #313): the full data set in dependency-ordered form. The list
// mirrors deploy/postgres/roles.sql section 3 (the ownership contract);
// the migration LEDGER is deliberately absent — migration state travels
// structurally in the manifest's source block, not as row data.
package databundle

// TableSpec is one Library-owned table: its STABLE KEY (component
// truth — every table's identity key, also used for ordering and
// idempotent lookup; validated against the column catalog at export)
// and engine coverage (the SQLite file carries the library_* namespace
// only, per F12: legacy mirror tables have no SQLite home — the import
// skips them with a counted warning, never silently).
type TableSpec struct {
	Name string
	Keys []string
	// SQLite: carried into the library.sqlite target
	SQLite bool
	// Vocab carries the component's STATUS vocabularies (enum-typed
	// columns get theirs from the engine's pg_enum catalog at export;
	// these cover the text-typed status/kind/mode columns whose only
	// other teeth are CHECK constraints or app discipline). The
	// manifest carries them; import validates every row against them —
	// unknown enum/status is a loud abort, never a silent default.
	Vocab map[string][]string
}

// The status vocabularies (component truth; mirrors the schema CHECKs
// and the F03 ImportStatus contract).
var (
	importStatusVocab = []string{"received", "inspecting", "resolving_metadata", "awaiting_confirmation",
		"ensuring_collections", "creating_record", "uploading_rendition", "verifying",
		"committed", "retryable_failed", "terminal_failed"}
	stepVocab           = []string{"inspecting", "resolving_metadata", "ensuring_collections", "creating_record", "uploading_rendition", "verifying"}
	stepStateVocab      = []string{"in_progress", "done"}
	eventKindVocab      = []string{"state_entered", "decision_offered", "decision_resolved", "provider_write", "provenance", "retry", "terminal"}
	modeVocab           = []string{"included", "excluded"}
	identifierKindVocab = []string{"doi", "isbn"}
	anchorKindVocab     = []string{"record", "rendition", "collection"}
	originVocab         = []string{"import", "sync", "heal"}
	auditOperationVocab = []string{"ensure_record", "ensure_rendition", "ensure_membership", "create_collection"}
	auditOutcomeVocab   = []string{"created", "reused", "changed", "adopted", "removed"}
)

// LibraryTables is THE dependency order for the library component.
// Order violations surface as FK failures at import time (targets
// enforce their constraints — the import never disables them).
var LibraryTables = []TableSpec{
	// Legacy mirror (Zotero strangler + selections + repair track)
	{"zotero_sources", []string{"id"}, false, nil},
	{"zotero_items", []string{"id"}, false, nil},
	{"zotero_collections", []string{"id"}, false, nil},
	{"zotero_item_collections", []string{"item_id", "collection_id"}, false, nil},
	{"zotero_documents", []string{"id"}, false, nil},
	{"zotero_attachments", []string{"id"}, false, nil},
	{"zotero_selections", []string{"document_id"}, false, map[string][]string{"mode": modeVocab}},
	{"zotero_collection_selections", []string{"collection_key"}, false, map[string][]string{"mode": modeVocab}},
	{"repair_cases", []string{"id"}, false, nil}, // status: PG enum repair_status — vocabulary from pg_enum at export
	{"zotero_write_audit", []string{"id"}, false, nil},
	// F06/F07 acquisition + identity + bridge
	{"library_imports", []string{"import_id"}, true, map[string][]string{"status": importStatusVocab}},
	{"library_import_events", []string{"import_id", "seq"}, true, map[string][]string{"kind": eventKindVocab}},
	{"library_import_steps", []string{"import_id", "step"}, true, map[string][]string{"step": stepVocab, "state": stepStateVocab}},
	{"library_external_identifiers", []string{"kind", "normalized_value"}, true, map[string][]string{"kind": identifierKindVocab}},
	{"library_metadata_provenance", []string{"id"}, true, nil},
	{"library_source_revisions", []string{"source_id", "record_id", "rendition_id", "revision_id"}, true, map[string][]string{"origin": originVocab}},
	{"library_write_audit", []string{"id"}, true, map[string][]string{"operation": auditOperationVocab, "outcome": auditOutcomeVocab}},
	{"library_provider_anchors", []string{"scope", "kind", "anchor"}, true, map[string][]string{"kind": anchorKindVocab}},
	{"library_writer_leases", []string{"scope"}, true, nil},
}

// librarySpec returns the spec for a carried table (nil when unknown).
func librarySpec(name string) *TableSpec {
	for i := range LibraryTables {
		if LibraryTables[i].Name == name {
			return &LibraryTables[i]
		}
	}
	return nil
}

// sqliteCarried reports whether the table lands in a library.sqlite.
func sqliteCarried(name string) bool {
	s := librarySpec(name)
	return s != nil && s.SQLite
}
