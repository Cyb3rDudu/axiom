// errors.go — the typed error envelope of the internal edge and the
// transport→contracterr mapping of the HTTP clients (F11 #305).
//
// The envelope mirrors the public routes' writeContractError shape
// ({"error":{component,class,message[,idempotency_key]}}) so both edges
// speak the same error dialect; the table lives in the package doc.
package bindings

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/Cyb3rDudu/axiom/axiom/internal/contracts/contracterr"
)

// errorEnvelope is the wire form of a typed error (both directions).
type errorEnvelope struct {
	Error struct {
		Component      string `json:"component"`
		Class          string `json:"class"`
		Message        string `json:"message"`
		IdempotencyKey string `json:"idempotency_key,omitempty"`
	} `json:"error"`
}

// statusOfClass is the contracterr→HTTP table (the exact table the
// contracterr package documents for HTTP adapters): every class has
// exactly one status.
func statusOfClass(class contracterr.Class) int {
	switch class {
	case contracterr.ClassNotFound:
		return http.StatusNotFound
	case contracterr.ClassInvalidArgument:
		return http.StatusBadRequest
	case contracterr.ClassConflict:
		return http.StatusConflict
	case contracterr.ClassUnavailable:
		return http.StatusServiceUnavailable
	case contracterr.ClassDeadline:
		return http.StatusGatewayTimeout
	default:
		return http.StatusInternalServerError
	}
}

// writeErr renders err as the envelope (server side of the edge). A
// non-contract error surfaces as Internal — never a bare string, never
// a stack trace. fallback names the serving component when the error
// carries none.
func writeErr(w http.ResponseWriter, fallback contracterr.Component, err error) {
	class, ok := contracterr.ClassOf(err)
	if !ok {
		class = contracterr.ClassInternal
	}
	component := fallback
	var ce *contracterr.Error
	if errors.As(err, &ce) && ce.Component != "" {
		component = ce.Component
	}
	body := errorEnvelope{}
	body.Error.Component = string(component)
	body.Error.Class = string(class)
	body.Error.Message = err.Error()
	var mm *contracterr.IdempotencyMismatch
	if errors.As(err, &mm) {
		body.Error.IdempotencyKey = mm.Key
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(statusOfClass(class))
	_ = json.NewEncoder(w).Encode(body)
}

// writeJSON renders a 2xx JSON body.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// decodeErr maps an HTTP response onto the typed error world (client
// side of the edge). Precedence: the envelope when the body carries one
// (the class comes from the wire, not the status); the status table as
// fallback for non-envelope bodies; Internal as the last resort. The
// returned error never embeds host, port, or URL — the leak sonde's
// guarantee (the raw detail belongs to the log, not the answer).
func decodeErr(component contracterr.Component, op string, resp *http.Response) error {
	var env errorEnvelope
	if body, rerr := io.ReadAll(io.LimitReader(resp.Body, 1<<20)); rerr == nil && len(body) > 0 {
		_ = json.Unmarshal(body, &env)
	}
	if class, ok := classOfName(env.Error.Class); ok && env.Error.Message != "" {
		if class == contracterr.ClassConflict && env.Error.IdempotencyKey != "" {
			return &contracterr.IdempotencyMismatch{Component: componentOf(env.Error.Component, component), Key: env.Error.IdempotencyKey}
		}
		return contracterr.New(componentOf(env.Error.Component, component), class, env.Error.Message)
	}
	switch resp.StatusCode {
	case http.StatusNotFound:
		return contracterr.New(component, contracterr.ClassNotFound, op+": not found")
	case http.StatusBadRequest:
		return contracterr.New(component, contracterr.ClassInvalidArgument, op+": invalid argument")
	case http.StatusConflict:
		return contracterr.New(component, contracterr.ClassConflict, op+": conflict")
	case http.StatusServiceUnavailable:
		return contracterr.New(component, contracterr.ClassUnavailable, op+": backend unavailable")
	case http.StatusGatewayTimeout:
		return contracterr.New(component, contracterr.ClassDeadline, op+": deadline exceeded")
	case http.StatusRequestTimeout:
		return contracterr.New(component, contracterr.ClassDeadline, op+": deadline exceeded")
	default:
		return contracterr.New(component, contracterr.ClassInternal, fmt.Sprintf("%s: internal error (HTTP %d)", op, resp.StatusCode))
	}
}

// componentOf prefers the envelope's component, falling back to the
// client's own when absent.
func componentOf(from string, fallback contracterr.Component) contracterr.Component {
	if from != "" {
		return contracterr.Component(from)
	}
	return fallback
}

// classOfName validates an envelope class against the closed set — a
// garbage body must not mint an unknown class.
func classOfName(name string) (contracterr.Class, bool) {
	switch contracterr.Class(name) {
	case contracterr.ClassNotFound, contracterr.ClassInvalidArgument,
		contracterr.ClassConflict, contracterr.ClassUnavailable,
		contracterr.ClassDeadline, contracterr.ClassInternal:
		return contracterr.Class(name), true
	}
	return "", false
}

// transportErr maps a transport-level failure (no HTTP answer) onto the
// typed world: expired budgets → Deadline (not auto-retryable), every
// other transport failure (refused, reset, EOF, unreachable — a killed
// backend looks exactly like this) → Unavailable (retryable). Caller
// cancellation propagates unwrapped. Messages are fixed strings; raw is
// for the log.
func transportErr(component contracterr.Component, op string, raw error) error {
	if errors.Is(raw, context.Canceled) {
		return raw // the caller gave up — nothing to classify
	}
	if errors.Is(raw, context.DeadlineExceeded) {
		return contracterr.New(component, contracterr.ClassDeadline, op+": deadline exceeded (component budget)")
	}
	return contracterr.New(component, contracterr.ClassUnavailable, op+": backend unavailable (transport)")
}
