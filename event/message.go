package event

import (
	"encoding/json"
	"fmt"
)

type Message struct {
	Event Event `json:"-"`
}

type eventEnvelope struct {
	Type         EventType        `json:"type"`
	EventPayload *json.RawMessage `json:"event"`
}

func (m Message) MarshalJSON() ([]byte, error) {
	var envelope eventEnvelope

	payload, err := json.Marshal(m.Event)
	if err != nil {
		return nil, err
	}

	envelope.Type = m.Event.EventType()
	envelope.EventPayload = (*json.RawMessage)(&payload)

	return json.Marshal(envelope)
}

func (m *Message) UnmarshalJSON(bytes []byte) error {
	var envelope eventEnvelope

	err := json.Unmarshal(bytes, &envelope)
	if err != nil {
		return err
	}

	switch envelope.Type {
	case EventTypeLog:
		event := Log{}
		err = json.Unmarshal(*envelope.EventPayload, &event)
		m.Event = event
	case EventTypeStatus:
		event := Status{}
		err = json.Unmarshal(*envelope.EventPayload, &event)
		m.Event = event
	default:
		return fmt.Errorf("unknown event type: %d", envelope.Type)
	}

	return err
}
