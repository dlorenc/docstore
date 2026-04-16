// Package events provides the eventing system for DocStore.
// Events are emitted after successful mutations and delivered via SSE streaming
// or webhooks using the CloudEvents 1.0 envelope format.
package events

import (
	"encoding/json"
	"time"

	"github.com/google/uuid"
)

// Event is implemented by every domain event type.
type Event interface {
	// Type returns the CloudEvents type, e.g. "com.docstore.commit.created".
	Type() string
	// Source returns the CloudEvents source URI, e.g. "/repos/acme/myrepo".
	Source() string
	// Data returns the event payload, which is marshalled as the CloudEvents data field.
	Data() any
}

// Envelope is the CloudEvents 1.0 JSON envelope.
type Envelope struct {
	SpecVersion     string    `json:"specversion"`
	Type            string    `json:"type"`
	Source          string    `json:"source"`
	ID              string    `json:"id"`
	Time            time.Time `json:"time"`
	DataContentType string    `json:"datacontenttype"`
	Data            any       `json:"data"`
}

// ToCloudEvent serializes an Event into a CloudEvents 1.0 JSON byte slice.
func ToCloudEvent(e Event) ([]byte, error) {
	env := Envelope{
		SpecVersion:     "1.0",
		Type:            e.Type(),
		Source:          e.Source(),
		ID:              uuid.New().String(),
		Time:            time.Now().UTC(),
		DataContentType: "application/json",
		Data:            e.Data(),
	}
	return json.Marshal(env)
}
