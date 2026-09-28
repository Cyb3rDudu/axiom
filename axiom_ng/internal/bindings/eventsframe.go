// eventsframe.go — the SSE wire codec for the internal event stream:
// one JSON frame per bus event ({"type":"JobClaimed","payload":{…}}).
// The typed registry is closed (the bus events are a closed set); an
// unknown frame decodes to nil and the bridge skips it.
package bindings

import (
	"encoding/json"
	"fmt"

	"github.com/Cyb3rDudu/axiom/axiom_ng/internal/events"
)

// eventFrame is the SSE data payload.
type eventFrame struct {
	Type    string          `json:"type"`
	Payload json.RawMessage `json:"payload"`
}

// marshalEventFrame renders one SSE data frame (trailing blank line
// included) for ev.
func marshalEventFrame(ev events.Event) (string, error) {
	var name string
	var payload any
	switch v := ev.(type) {
	case events.JobClaimed:
		name, payload = "JobClaimed", v
	case events.JobStageChanged:
		name, payload = "JobStageChanged", v
	case events.JobCompleted:
		name, payload = "JobCompleted", v
	case events.JobFailed:
		name, payload = "JobFailed", v
	case events.OutboxDrained:
		name, payload = "OutboxDrained", v
	case events.RunnerStateChanged:
		name, payload = "RunnerStateChanged", v
	default:
		return "", fmt.Errorf("unserializable event type %T", ev)
	}
	raw, err := json.Marshal(eventFrame{Type: name, Payload: mustJSON(payload)})
	if err != nil {
		return "", err
	}
	return "data: " + string(raw) + "\n\n", nil
}

// unmarshalEventFrame decodes one SSE data payload back to its typed
// event (nil for unknown types — forward compatibility: a newer store
// process may emit events this bridge does not know).
func unmarshalEventFrame(data []byte) (events.Event, error) {
	var f eventFrame
	if err := json.Unmarshal(data, &f); err != nil {
		return nil, err
	}
	switch f.Type {
	case "JobClaimed":
		var e events.JobClaimed
		return e, json.Unmarshal(f.Payload, &e)
	case "JobStageChanged":
		var e events.JobStageChanged
		return e, json.Unmarshal(f.Payload, &e)
	case "JobCompleted":
		var e events.JobCompleted
		return e, json.Unmarshal(f.Payload, &e)
	case "JobFailed":
		var e events.JobFailed
		return e, json.Unmarshal(f.Payload, &e)
	case "OutboxDrained":
		var e events.OutboxDrained
		return e, json.Unmarshal(f.Payload, &e)
	case "RunnerStateChanged":
		var e events.RunnerStateChanged
		return e, json.Unmarshal(f.Payload, &e)
	}
	return nil, nil
}

func mustJSON(v any) json.RawMessage {
	raw, err := json.Marshal(v)
	if err != nil {
		return json.RawMessage("null")
	}
	return raw
}
