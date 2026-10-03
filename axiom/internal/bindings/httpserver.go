// httpserver.go — the internal, versioned component edge (F11 #305):
// the /internal/v1/… muxes a component process serves so the api
// process can bind the same F03 contracts over HTTP. Wire form is the
// F03 DTO JSON (the contract goldens freeze it); errors ride the typed
// envelope; the Authenticator hook guards the edge (Noop default).
//
//	GET  /internal/v1/library/sources/{sourceID}
//	POST /internal/v1/library/imports            multipart(request,file)
//	GET  /internal/v1/library/imports/{importID}
//	POST /internal/v1/library/imports/{importID}/confirm   (extended svc)
//	POST /internal/v1/library/imports/{importID}/retry     (extended svc)
//	GET  /internal/v1/library/renditions/{ticket}
//	POST /internal/v1/library/citations
//	POST /internal/v1/store/ingest
//	POST /internal/v1/store/search
//	GET  /internal/v1/store/passages/{chunkID}
//	GET  /internal/v1/store/events                SSE (broker wired)
package bindings

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"time"

	"github.com/Cyb3rDudu/axiom/axiom/internal/contracts/contracterr"
	"github.com/Cyb3rDudu/axiom/axiom/internal/contracts/library"
	"github.com/Cyb3rDudu/axiom/axiom/internal/contracts/store"
	"github.com/Cyb3rDudu/axiom/axiom/internal/events"
	"github.com/go-chi/chi/v5"
)

// Authenticator is the AuthN/AuthZ hook at the internal edge. Real
// implementations arrive post-0.2.0; 0.2.0 ships the Noop.
type Authenticator interface {
	// Authenticate authorizes one internal request; a non-nil error
	// denies it (403 envelope).
	Authenticate(r *http.Request) error
}

// NoopAuthenticator accepts every request — the documented 0.2.0
// default (the internal edge is deployment-private by topology).
func NoopAuthenticator() Authenticator { return noopAuth{} }

type noopAuth struct{}

func (noopAuth) Authenticate(*http.Request) error { return nil }

// extendedLibrary is the F06 public-surface superset the internal edge
// surfaces when the backing service provides it (replay signal,
// confirm/retry — the F03 interface is read/state-only for those).
type extendedLibrary interface {
	StartImportDetailed(ctx context.Context, req library.ImportRequest, content io.Reader) (op library.ImportOperation, replayed bool, err error)
	ConfirmImport(ctx context.Context, importID, decisionID, candidateID string) (library.ImportOperation, error)
	RetryImport(ctx context.Context, importID string) (library.ImportOperation, error)
}

// importMaxBounder exposes the service's content bound (F06 shape).
type importMaxBounder interface {
	MaxImportBytes() int64
}

// defaultInternalImportBound caps an internal multipart upload when the
// service exposes no bound; the edge's bound only polices transport
// framing — the service still enforces its own limit against the
// content part.
const defaultInternalImportBound = 260 << 20

// LibraryInternalRoutes builds the library edge handler serving
// /internal/v1/library/… (mount on its own listener; auth nil = Noop).
func LibraryInternalRoutes(svc library.Library, auth Authenticator) http.Handler {
	if auth == nil {
		auth = NoopAuthenticator()
	}
	r := chi.NewRouter()
	r.Route("/internal/v1/library", func(r chi.Router) {
		r.Use(authGuard(auth, contracterr.ComponentLibrary))
		mountLibraryRoutes(r, svc)
	})
	return r
}

func mountLibraryRoutes(r chi.Router, svc library.Library) {
	ext, hasExt := svc.(extendedLibrary)

	r.Get("/sources/{sourceID}", func(w http.ResponseWriter, req *http.Request) {
		src, err := svc.GetSource(req.Context(), library.SourceRef{SourceID: chi.URLParam(req, "sourceID")})
		if err != nil {
			writeErr(w, contracterr.ComponentLibrary, err)
			return
		}
		writeJSON(w, http.StatusOK, src)
	})

	r.Post("/imports", func(w http.ResponseWriter, req *http.Request) {
		limit := int64(defaultInternalImportBound)
		if mb, ok := svc.(importMaxBounder); ok && mb.MaxImportBytes() > 0 {
			limit = mb.MaxImportBytes() + (4 << 20) // content limit + request-part headroom
		}
		req.Body = http.MaxBytesReader(w, req.Body, limit)
		if err := req.ParseMultipartForm(32 << 20); err != nil {
			writeErr(w, contracterr.ComponentLibrary, contracterr.New(contracterr.ComponentLibrary, contracterr.ClassInvalidArgument, "multipart form: "+err.Error()))
			return
		}
		file, _, err := req.FormFile("file")
		if err != nil {
			writeErr(w, contracterr.ComponentLibrary, contracterr.New(contracterr.ComponentLibrary, contracterr.ClassInvalidArgument, "multipart part 'file' is required"))
			return
		}
		defer file.Close()
		reqRaw := req.FormValue("request")
		if reqRaw == "" {
			writeErr(w, contracterr.ComponentLibrary, contracterr.New(contracterr.ComponentLibrary, contracterr.ClassInvalidArgument, "multipart part 'request' (JSON) is required"))
			return
		}
		var lreq library.ImportRequest
		if err := json.Unmarshal([]byte(reqRaw), &lreq); err != nil {
			writeErr(w, contracterr.ComponentLibrary, contracterr.New(contracterr.ComponentLibrary, contracterr.ClassInvalidArgument, "request part is not valid JSON: "+err.Error()))
			return
		}
		if hasExt {
			// Replay answers 200 with the SAME operation (the F06 public
			// semantics carried across the edge); fresh answers 201.
			op, replayed, err := ext.StartImportDetailed(req.Context(), lreq, file)
			if err != nil {
				writeErr(w, contracterr.ComponentLibrary, err)
				return
			}
			writeJSON(w, statusFor(replayed), op)
			return
		}
		op, err := svc.StartImport(req.Context(), lreq, file)
		if err != nil {
			writeErr(w, contracterr.ComponentLibrary, err)
			return
		}
		writeJSON(w, http.StatusCreated, op)
	})

	r.Get("/imports/{importID}", func(w http.ResponseWriter, req *http.Request) {
		op, err := svc.GetImport(req.Context(), library.ImportRef{ImportID: chi.URLParam(req, "importID")})
		if err != nil {
			writeErr(w, contracterr.ComponentLibrary, err)
			return
		}
		writeJSON(w, http.StatusOK, op)
	})

	// confirm/retry exist only when the backing service offers the F06
	// decision surface (capability-honest 404 — the public unwired
	// pattern).
	r.Post("/imports/{importID}/confirm", func(w http.ResponseWriter, req *http.Request) {
		if !hasExt {
			http.NotFound(w, req)
			return
		}
		var body struct {
			DecisionID  string `json:"decision_id"`
			CandidateID string `json:"candidate_id"`
		}
		if err := json.NewDecoder(io.LimitReader(req.Body, 1<<20)).Decode(&body); err != nil {
			writeErr(w, contracterr.ComponentLibrary, contracterr.New(contracterr.ComponentLibrary, contracterr.ClassInvalidArgument, "confirm body: "+err.Error()))
			return
		}
		op, err := ext.ConfirmImport(req.Context(), chi.URLParam(req, "importID"), body.DecisionID, body.CandidateID)
		if err != nil {
			writeErr(w, contracterr.ComponentLibrary, err)
			return
		}
		writeJSON(w, http.StatusOK, op)
	})

	r.Post("/imports/{importID}/retry", func(w http.ResponseWriter, req *http.Request) {
		if !hasExt {
			http.NotFound(w, req)
			return
		}
		op, err := ext.RetryImport(req.Context(), chi.URLParam(req, "importID"))
		if err != nil {
			writeErr(w, contracterr.ComponentLibrary, err)
			return
		}
		writeJSON(w, http.StatusOK, op)
	})

	r.Get("/renditions/{ticket}", func(w http.ResponseWriter, req *http.Request) {
		rc, err := svc.OpenRendition(req.Context(), library.ContentTicket(chi.URLParam(req, "ticket")))
		if err != nil {
			writeErr(w, contracterr.ComponentLibrary, err)
			return
		}
		defer rc.Close()
		w.Header().Set("Content-Type", "application/octet-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.Copy(w, rc)
	})

	r.Post("/citations", func(w http.ResponseWriter, req *http.Request) {
		var creq library.CitationRequest
		if err := json.NewDecoder(io.LimitReader(req.Body, 1<<20)).Decode(&creq); err != nil {
			writeErr(w, contracterr.ComponentLibrary, contracterr.New(contracterr.ComponentLibrary, contracterr.ClassInvalidArgument, "citation body: "+err.Error()))
			return
		}
		proj, err := svc.ProjectCitation(req.Context(), creq)
		if err != nil {
			writeErr(w, contracterr.ComponentLibrary, err)
			return
		}
		writeJSON(w, http.StatusOK, proj)
	})
}

// statusFor picks fresh (201) vs replay (200) for StartImport answers.
func statusFor(replayed bool) int {
	if replayed {
		return http.StatusOK
	}
	return http.StatusCreated
}

// StoreInternalRoutes builds the store edge handler serving
// /internal/v1/store/…. broker nil = the SSE events route answers 404
// (a store process without the events role has no bus to stream).
func StoreInternalRoutes(svc store.Store, broker *events.Broker, auth Authenticator) http.Handler {
	if auth == nil {
		auth = NoopAuthenticator()
	}
	r := chi.NewRouter()
	r.Route("/internal/v1/store", func(r chi.Router) {
		r.Use(authGuard(auth, contracterr.ComponentStore))
		mountStoreRoutes(r, svc, broker)
	})
	return r
}

func mountStoreRoutes(r chi.Router, svc store.Store, broker *events.Broker) {
	r.Post("/ingest", func(w http.ResponseWriter, req *http.Request) {
		var ireq store.IngestRevisionRequest
		if err := json.NewDecoder(http.MaxBytesReader(w, req.Body, 1<<20)).Decode(&ireq); err != nil {
			writeErr(w, contracterr.ComponentStore, contracterr.New(contracterr.ComponentStore, contracterr.ClassInvalidArgument, "intake body: "+err.Error()))
			return
		}
		job, err := svc.IngestRevision(req.Context(), ireq)
		if err != nil {
			writeErr(w, contracterr.ComponentStore, err)
			return
		}
		writeJSON(w, http.StatusAccepted, job)
	})

	r.Post("/search", func(w http.ResponseWriter, req *http.Request) {
		var sreq store.SearchRequest
		if err := json.NewDecoder(http.MaxBytesReader(w, req.Body, 1<<20)).Decode(&sreq); err != nil {
			writeErr(w, contracterr.ComponentStore, contracterr.New(contracterr.ComponentStore, contracterr.ClassInvalidArgument, "search body: "+err.Error()))
			return
		}
		res, err := svc.Search(req.Context(), sreq)
		if err != nil {
			writeErr(w, contracterr.ComponentStore, err)
			return
		}
		writeJSON(w, http.StatusOK, res)
	})

	r.Get("/passages/{chunkID}", func(w http.ResponseWriter, req *http.Request) {
		p, err := svc.GetPassage(req.Context(), store.PassageRef{ChunkID: chi.URLParam(req, "chunkID")})
		if err != nil {
			writeErr(w, contracterr.ComponentStore, err)
			return
		}
		writeJSON(w, http.StatusOK, p)
	})

	r.Get("/events", func(w http.ResponseWriter, req *http.Request) {
		if broker == nil {
			http.NotFound(w, req)
			return
		}
		serveEventStream(w, req, broker)
	})
}

// authGuard wraps a mux with the Authenticator hook: a denial answers
// 403 with the typed envelope (the class stays Internal — the client's
// documented mapping — while the STATUS names what happened: an auth
// refusal, not a component crash).
func authGuard(auth Authenticator, component contracterr.Component) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			if err := auth.Authenticate(req); err != nil {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusForbidden)
				body := errorEnvelope{}
				body.Error.Component = string(component)
				body.Error.Class = string(contracterr.ClassInternal)
				body.Error.Message = "internal edge authentication failed"
				_ = json.NewEncoder(w).Encode(body)
				return
			}
			next.ServeHTTP(w, req)
		})
	}
}

// eventHeartbeat is the SSE keepalive cadence.
const eventHeartbeat = 15 * time.Second

// serveEventStream streams the broker as SSE (one data frame per event,
// heartbeat comment on the cadence). The api-process bridge consumes
// this to republish onto its own broker (the WS forwarding decision —
// package doc).
func serveEventStream(w http.ResponseWriter, req *http.Request, broker *events.Broker) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	sub := events.NewSubscription()
	broker.Subscribe(sub, 0)
	defer broker.Unsubscribe(sub)
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	done := req.Context().Done()
	// Next blocks until an event or done; pump it through a channel so
	// the heartbeat can fire on an idle stream.
	type arrival struct {
		ev events.Event
		ok bool
	}
	arrivals := make(chan arrival)
	go func() {
		for {
			ev, _, ok := sub.Next(done)
			select {
			case arrivals <- arrival{ev, ok}:
			case <-done:
				return
			}
			if !ok {
				return
			}
		}
	}()
	beat := time.NewTicker(eventHeartbeat)
	defer beat.Stop()
	for {
		select {
		case <-done:
			return
		case <-beat.C:
			if _, err := io.WriteString(w, ": heartbeat\n\n"); err != nil {
				return
			}
			flusher.Flush()
		case a := <-arrivals:
			if !a.ok {
				return
			}
			frame, err := marshalEventFrame(a.ev)
			if err != nil {
				continue // an unserializable event must not kill the stream
			}
			if _, err := io.WriteString(w, frame); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}
