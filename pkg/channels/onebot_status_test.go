package channels

import (
	"encoding/json"
	"testing"
)

func TestOneBotRawEvent_StatusStringUnmarshal(t *testing.T) {
	payload := []byte(`{"echo":"send_1","status":"ok","retcode":0}`)

	var raw oneBotRawEvent
	if err := json.Unmarshal(payload, &raw); err != nil {
		t.Fatalf("unmarshal failed: %v", err)
	}
	if raw.Status.Text != "ok" {
		t.Fatalf("status.text = %q, want %q", raw.Status.Text, "ok")
	}
}

func TestOneBotRawEvent_StatusObjectUnmarshal(t *testing.T) {
	payload := []byte(`{"post_type":"meta_event","status":{"online":true,"good":true}}`)

	var raw oneBotRawEvent
	if err := json.Unmarshal(payload, &raw); err != nil {
		t.Fatalf("unmarshal failed: %v", err)
	}
	if !raw.Status.Online || !raw.Status.Good {
		t.Fatalf("status object not parsed correctly: %+v", raw.Status)
	}
}
