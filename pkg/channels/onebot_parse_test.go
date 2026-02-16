package channels

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestParseMessageContentEx_AppendsRawMessageWhenUnknownSegmentPresent(t *testing.T) {
	raw := json.RawMessage(`[
		{"type":"text","data":{"text":"hello"}},
		{"type":"face","data":{"id":"66"}}
	]`)
	rawMessage := "hello[CQ:face,id=66]"

	result := parseMessageContentEx(raw, rawMessage, 0)

	if !strings.Contains(result.Text, "hello") {
		t.Fatalf("expected result text to keep known text segment, got: %q", result.Text)
	}
	found := false
	for _, seg := range result.Segments {
		if seg.Type == "raw_message" && seg.Raw == rawMessage {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected raw_message segment appended, got: %+v", result.Segments)
	}
}

func TestParseMessageContentEx_DoesNotAppendRawMessageForKnownSegments(t *testing.T) {
	raw := json.RawMessage(`[
		{"type":"text","data":{"text":"hello"}},
		{"type":"at","data":{"qq":"123"}}
	]`)
	rawMessage := "hello[CQ:at,qq=123]"

	result := parseMessageContentEx(raw, rawMessage, 123)

	if result.Text != "hello" {
		t.Fatalf("text = %q, want %q", result.Text, "hello")
	}
	if !result.IsBotMentioned {
		t.Fatal("expected bot mention to be detected")
	}
}

func TestParseMessageContentEx_UsesRawMessageWhenUnknownSegmentWithoutText(t *testing.T) {
	raw := json.RawMessage(`[
		{"type":"dice","data":{"id":"42"}}
	]`)
	rawMessage := "[CQ:dice,id=42]"

	result := parseMessageContentEx(raw, rawMessage, 0)

	if result.Text != "" {
		t.Fatalf("text = %q, want empty", result.Text)
	}
	if len(result.Segments) != 2 {
		t.Fatalf("segment count = %d, want 2", len(result.Segments))
	}
	if result.Segments[1].Type != "raw_message" || result.Segments[1].Raw != rawMessage {
		t.Fatalf("raw message fallback segment mismatch: %+v", result.Segments[1])
	}
}

func TestParseMessageContentEx_ParsesImageAndReplySegments(t *testing.T) {
	raw := json.RawMessage(`[
		{"type":"text","data":{"text":"看图"}},
		{"type":"image","data":{"file":"abc.jpg","url":"https://example.com/a.jpg","path":"/tmp/a.jpg"}},
		{"type":"reply","data":{"id":"9001"}}
	]`)

	result := parseMessageContentEx(raw, "", 0)

	if len(result.Segments) != 3 {
		t.Fatalf("segment count = %d, want %d", len(result.Segments), 3)
	}
	if result.Segments[1].Type != "image" {
		t.Fatalf("segment[1].type = %q, want %q", result.Segments[1].Type, "image")
	}
	if result.Segments[1].ImagePath != "/tmp/a.jpg" {
		t.Fatalf("segment[1].image_path = %q, want %q", result.Segments[1].ImagePath, "/tmp/a.jpg")
	}
	if result.Segments[2].Type != "reply" || result.Segments[2].ReplyID != "9001" {
		t.Fatalf("reply segment parsed incorrectly: %+v", result.Segments[2])
	}
}

func TestParseMessageContentEx_ParsesCQImageAndReply(t *testing.T) {
	raw := json.RawMessage(`"你好[CQ:image,file=abc.jpg,path=/tmp/a.jpg][CQ:reply,id=77]"`)

	result := parseMessageContentEx(raw, "", 0)

	if len(result.Segments) != 3 {
		t.Fatalf("segment count = %d, want %d", len(result.Segments), 3)
	}
	if result.Segments[1].Type != "image" || result.Segments[2].Type != "reply" {
		t.Fatalf("unexpected segment types: %+v", result.Segments)
	}
	if result.Segments[1].ImagePath != "/tmp/a.jpg" {
		t.Fatalf("image path = %q, want %q", result.Segments[1].ImagePath, "/tmp/a.jpg")
	}
	if result.Segments[2].ReplyID != "77" {
		t.Fatalf("reply id = %q, want %q", result.Segments[2].ReplyID, "77")
	}
}
