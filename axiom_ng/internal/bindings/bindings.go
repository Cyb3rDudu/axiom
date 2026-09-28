// Package bindings is the F11 component-adapter layer (#305): the Local
// and HTTP bindings behind the F03 Library/Store contracts, the internal
// versioned HTTP edge those bindings talk to, and the api-process public
// adapters that keep the public surface one base URL in every topology.
//
// # Binding matrix
//
//	Topology      Library seam             Store seam            public edge
//	serve all     the *library.Service     *store.Service behind one process
//	              bound directly (its      LocalStoreClient
//	              F06 extended surface     (the binding choice is
//	              IS the public surface);  one code shape in
//	              LocalLibraryClient       every topology)
//	              formalizes the same
//	              seam for the parity
//	              suite
//	split         HTTPLibraryClient        HTTPStoreClient       api process only
//	(3 processes) → /internal/v1/          → /internal/v1/
//	  library       library/…                store/
//	  store         on the library           on the store
//	  api           process                  process
//
// The SAME contractsuite (F03 harness, all fixtures) runs against both
// bindings — split-process semantics are a test result, not a claim.
//
// # Port resolution (runtime config, never public contract)
//
// The public client surface stays ONE base URL. Which ports components
// listen on is deployment configuration:
//
//	AXIOM_INTERNAL_LIBRARY_ADDR  host:port the library process serves
//	                             /internal/v1/library/… on ("" = off)
//	AXIOM_INTERNAL_STORE_ADDR    host:port the store process serves
//	                             /internal/v1/store/… on ("" = off)
//	AXIOM_LIBRARY_URL            base URL the api process binds its
//	                             Library surface to ("" = local binding)
//	AXIOM_STORE_URL              base URL the api process binds its
//	                             Store surface to ("" = local binding)
//	AXIOM_COMPONENT_TIMEOUT      per-request budget for non-streaming
//	                             internal calls (default 30s)
//
// # Transport error mapping (HTTP client → contracterr)
//
// The wire carries the typed error envelope (component/class/message,
// idempotency_key on conflicts); the client never string-matches:
//
//	wire / transport                    contract class       retryable
//	──────────────────────────────────  ───────────────────  ────────
//	envelope class (any typed answer)   that class           per class
//	409 + idempotency_key               Conflict as          no
//	                                    *IdempotencyMismatch
//	connection refused/reset/EOF        Unavailable          yes
//	(killed backend)
//	mid-body connection reset           Internal             no
//	(no budget expiry, no cancel)
//	request budget expired              Deadline             no (fresh
//	(incl. mid-body)                                          budget only)
//	non-envelope body                   status-table         per class
//	                                    (404→NotFound …);
//	                                    non-envelope 2xx →   no
//	                                    Internal
//	(context.Canceled is propagated unwrapped — the caller gave up,
//	 whether it cancels the request or the body read)
//
// Messages of transport-mapped errors are fixed generic strings: no
// component host:port, no dial error text — the public answer must not
// leak topology (fault-parity leak sonde). The raw transport error goes
// to the client's logger, never the wire.
//
// Retry policy: the small GET-shaped calls (GetSource, GetImport,
// ProjectCitation, Search, GetPassage) retry ONCE on a retryable
// transport failure (Unavailable). OpenRendition STREAMS — no automatic
// retry (a transport failure surfaces Unavailable and the caller
// decides). The keyed writes (StartImport, IngestRevision) are idempotent
// by contract but go once — a replay needs the caller's explicit
// decision, not a hidden second flight.
//
// # WS/event edge decision: forwarding (relay)
//
// The event bus is process-local (#249). In the split topology the
// dispatcher lives in the store process, so the store's internal edge
// exposes a read-only SSE stream (/internal/v1/store/events) and the api
// process bridges it onto its own broker (BridgeEvents; exponential
// reconnect backoff, capped 30s). The public /api/ws stream and
// /api/runners/live therefore work identically in both topologies.
// Derived frames (RunnerStateChanged) are NOT relayed: both sides run
// the deterministic deriver over the source events, so relaying them
// would double-publish. Stream drops on the edge are silent (no gap
// marker crosses the relay — consistent with the best-effort semantics;
// the public WS keeps its own gap accounting).
//
// # AuthN at the internal edge
//
// The internal mux takes an Authenticator hook; the default is the
// Noop (0.2.0 ships no real auth — the hook is the seam a deployment
// fills). A denying authenticator answers 403 with the error envelope
// (class Internal — terminal for the client).
package bindings
