package channels

import (
	"context"
	"encoding/json"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sipeed/picoclaw/pkg/bus"
	"github.com/sipeed/picoclaw/pkg/config"
)

func TestOneBotHandleMessage_GroupAllowedByDefault(t *testing.T) {
	msgBus := bus.NewMessageBus()
	ch, err := NewOneBotChannel(config.OneBotConfig{}, msgBus)
	if err != nil {
		t.Fatalf("NewOneBotChannel() error = %v", err)
	}

	ch.handleMessage(&oneBotEvent{
		MessageType:    "group",
		MessageID:      "m1",
		UserID:         2002,
		GroupID:        1001,
		Content:        "hello",
		IsBotMentioned: true,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	msg, ok := msgBus.ConsumeInbound(ctx)
	if !ok {
		t.Fatal("expected inbound message, got none")
	}
	if msg.ChatID != "group:1001" {
		t.Fatalf("chat_id = %q, want %q", msg.ChatID, "group:1001")
	}
}

func TestOneBotHandleMessage_GroupNotAllowed(t *testing.T) {
	msgBus := bus.NewMessageBus()
	ch, err := NewOneBotChannel(config.OneBotConfig{
		AllowGroups: config.FlexibleStringSlice{"1001"},
	}, msgBus)
	if err != nil {
		t.Fatalf("NewOneBotChannel() error = %v", err)
	}

	ch.handleMessage(&oneBotEvent{
		MessageType:    "group",
		MessageID:      "m2",
		UserID:         2002,
		GroupID:        1002,
		Content:        "hello",
		IsBotMentioned: true,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	if msg, ok := msgBus.ConsumeInbound(ctx); ok {
		t.Fatalf("unexpected inbound message: %+v", msg)
	}
}

func TestOneBotHandleMessage_GroupAllowedWithPrefixFormat(t *testing.T) {
	msgBus := bus.NewMessageBus()
	ch, err := NewOneBotChannel(config.OneBotConfig{
		AllowGroups: config.FlexibleStringSlice{"group:1001"},
	}, msgBus)
	if err != nil {
		t.Fatalf("NewOneBotChannel() error = %v", err)
	}

	ch.handleMessage(&oneBotEvent{
		MessageType:    "group",
		MessageID:      "m3",
		UserID:         2002,
		GroupID:        1001,
		Content:        "hello",
		IsBotMentioned: true,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	if _, ok := msgBus.ConsumeInbound(ctx); !ok {
		t.Fatal("expected inbound message, got none")
	}
}

func TestOneBotHandleRawEvent_GroupNotAllowedSkipsImageDownload(t *testing.T) {
	msgBus := bus.NewMessageBus()
	ch, err := NewOneBotChannel(config.OneBotConfig{
		AllowGroups: config.FlexibleStringSlice{"1001"},
	}, msgBus)
	if err != nil {
		t.Fatalf("NewOneBotChannel() error = %v", err)
	}

	var downloadCalls int32
	ch.downloadFile = func(url, filename string) string {
		atomic.AddInt32(&downloadCalls, 1)
		return ""
	}

	segments := []map[string]interface{}{
		{
			"type": "image",
			"data": map[string]interface{}{
				"url":  "https://example.com/a.jpg",
				"file": "a.jpg",
			},
		},
	}
	messageJSON, err := json.Marshal(segments)
	if err != nil {
		t.Fatalf("marshal message segments failed: %v", err)
	}

	ch.handleRawEvent(&oneBotRawEvent{
		PostType:    "message",
		MessageType: "group",
		MessageID:   json.RawMessage(`"raw-g1"`),
		UserID:      json.RawMessage(`2002`),
		GroupID:     json.RawMessage(`1002`),
		RawMessage:  `[CQ:image,file=a.jpg,url=https://example.com/a.jpg]`,
		Message:     messageJSON,
	})

	if atomic.LoadInt32(&downloadCalls) != 0 {
		t.Fatalf("downloadFile should not be called for non-allowed groups, got %d", downloadCalls)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 80*time.Millisecond)
	defer cancel()
	if msg, ok := msgBus.ConsumeInbound(ctx); ok {
		t.Fatalf("unexpected inbound message: %+v", msg)
	}
}

func TestOneBotHandleMessage_GroupQueueUntilTriggered(t *testing.T) {
	msgBus := bus.NewMessageBus()
	ch, err := NewOneBotChannel(config.OneBotConfig{
		GroupTriggerPrefix:    []string{"/ai"},
		GroupContextQueueSize: 20,
	}, msgBus)
	if err != nil {
		t.Fatalf("NewOneBotChannel() error = %v", err)
	}

	ch.handleMessage(&oneBotEvent{
		MessageType:    "group",
		MessageID:      "q1",
		UserID:         2002,
		GroupID:        1001,
		Content:        "今天有点忙",
		IsBotMentioned: false,
	})

	noMsgCtx, noMsgCancel := context.WithTimeout(context.Background(), 80*time.Millisecond)
	defer noMsgCancel()
	if msg, ok := msgBus.ConsumeInbound(noMsgCtx); ok {
		t.Fatalf("unexpected inbound message before trigger: %+v", msg)
	}

	ch.handleMessage(&oneBotEvent{
		MessageType:    "group",
		MessageID:      "q2",
		UserID:         2002,
		GroupID:        1001,
		Content:        "/ai 帮我总结一下",
		IsBotMentioned: false,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	msg, ok := msgBus.ConsumeInbound(ctx)
	if !ok {
		t.Fatal("expected inbound message on trigger")
	}

	if msg.ChatID != "group:1001" {
		t.Fatalf("chat_id = %q, want %q", msg.ChatID, "group:1001")
	}
	if !strings.Contains(msg.Content, "<group_context>") {
		t.Fatalf("expected xml group context in content, got: %q", msg.Content)
	}
	if !strings.Contains(msg.Content, "role=\"queued\"") || !strings.Contains(msg.Content, "今天有点忙") {
		t.Fatalf("expected queued message in content, got: %q", msg.Content)
	}
	if !strings.Contains(msg.Content, "role=\"triggered\"") || !strings.Contains(msg.Content, "帮我总结一下") {
		t.Fatalf("expected stripped trigger message in content, got: %q", msg.Content)
	}

	// Queue should be drained after the trigger.
	ch.handleMessage(&oneBotEvent{
		MessageType:    "group",
		MessageID:      "q3",
		UserID:         2002,
		GroupID:        1001,
		Content:        "/ai 第二次触发",
		IsBotMentioned: false,
	})

	ctx2, cancel2 := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel2()
	msg2, ok := msgBus.ConsumeInbound(ctx2)
	if !ok {
		t.Fatal("expected second inbound message on trigger")
	}
	if strings.Contains(msg2.Content, "role=\"queued\"") {
		t.Fatalf("expected queue drained after trigger, got: %q", msg2.Content)
	}
}

func TestOneBotHandleMessage_GroupQueueLimit(t *testing.T) {
	msgBus := bus.NewMessageBus()
	ch, err := NewOneBotChannel(config.OneBotConfig{
		GroupTriggerPrefix:    []string{"/ai"},
		GroupContextQueueSize: 2,
	}, msgBus)
	if err != nil {
		t.Fatalf("NewOneBotChannel() error = %v", err)
	}

	ch.handleMessage(&oneBotEvent{MessageType: "group", MessageID: "l1", UserID: 1, GroupID: 1001, Content: "第一条", IsBotMentioned: false})
	ch.handleMessage(&oneBotEvent{MessageType: "group", MessageID: "l2", UserID: 1, GroupID: 1001, Content: "第二条", IsBotMentioned: false})
	ch.handleMessage(&oneBotEvent{MessageType: "group", MessageID: "l3", UserID: 1, GroupID: 1001, Content: "第三条", IsBotMentioned: false})
	ch.handleMessage(&oneBotEvent{MessageType: "group", MessageID: "l4", UserID: 1, GroupID: 1001, Content: "/ai 触发", IsBotMentioned: false})

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	msg, ok := msgBus.ConsumeInbound(ctx)
	if !ok {
		t.Fatal("expected inbound message on trigger")
	}

	if strings.Contains(msg.Content, "第一条") {
		t.Fatalf("expected oldest queued message to be dropped, got: %q", msg.Content)
	}
	if !strings.Contains(msg.Content, "第二条") || !strings.Contains(msg.Content, "第三条") {
		t.Fatalf("expected latest queued messages in content, got: %q", msg.Content)
	}
}

func TestOneBotHandleMessage_GroupAutoReplyProbability(t *testing.T) {
	msgBus := bus.NewMessageBus()
	ch, err := NewOneBotChannel(config.OneBotConfig{
		GroupTriggerPrefix:          []string{"/ai"},
		GroupRandomReplyProbability: 1,
	}, msgBus)
	if err != nil {
		t.Fatalf("NewOneBotChannel() error = %v", err)
	}

	ch.handleMessage(&oneBotEvent{
		MessageType:    "group",
		MessageID:      "p1",
		UserID:         2002,
		GroupID:        1001,
		Content:        "不带触发词的普通消息",
		IsBotMentioned: false,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	msg, ok := msgBus.ConsumeInbound(ctx)
	if !ok {
		t.Fatal("expected inbound message with probability trigger")
	}
	if msg.Metadata["auto_trigger"] != "probability" {
		t.Fatalf("auto_trigger = %q, want %q", msg.Metadata["auto_trigger"], "probability")
	}
	if !strings.Contains(msg.Content, "<auto_trigger_notice") {
		t.Fatalf("expected auto-trigger notice in content, got: %q", msg.Content)
	}
	if strings.Index(msg.Content, "<auto_trigger_notice") < strings.Index(msg.Content, "</messages>") {
		t.Fatalf("expected auto-trigger notice appended at end, got: %q", msg.Content)
	}
	if !strings.Contains(msg.Content, "不带触发词的普通消息") {
		t.Fatalf("content = %q, want original content included", msg.Content)
	}
}

func TestOneBotHandleMessage_GroupForceReplyAfterSilence(t *testing.T) {
	msgBus := bus.NewMessageBus()
	ch, err := NewOneBotChannel(config.OneBotConfig{
		GroupTriggerPrefix:             []string{"/ai"},
		GroupRandomReplyProbability:    0,
		GroupForceReplyIntervalSeconds: 60,
	}, msgBus)
	if err != nil {
		t.Fatalf("NewOneBotChannel() error = %v", err)
	}

	now := time.Unix(1700000000, 0)
	ch.nowFunc = func() time.Time { return now }
	ch.replyMu.Lock()
	ch.lastGroupReply["1001"] = now.Add(-2 * time.Minute)
	ch.replyMu.Unlock()

	ch.handleMessage(&oneBotEvent{
		MessageType:    "group",
		MessageID:      "f1",
		UserID:         2002,
		GroupID:        1001,
		Content:        "超过静默阈值，应强制回复",
		IsBotMentioned: false,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	msg, ok := msgBus.ConsumeInbound(ctx)
	if !ok {
		t.Fatal("expected inbound message with force reply")
	}
	if msg.Metadata["auto_trigger"] != "max_silence" {
		t.Fatalf("auto_trigger = %q, want %q", msg.Metadata["auto_trigger"], "max_silence")
	}
	if !strings.Contains(msg.Content, "<auto_trigger_notice") {
		t.Fatalf("expected auto-trigger notice in content, got: %q", msg.Content)
	}
}

func TestOneBotHandleMessage_GroupProbabilityTriggerMergesQueuedContext(t *testing.T) {
	msgBus := bus.NewMessageBus()
	ch, err := NewOneBotChannel(config.OneBotConfig{
		GroupTriggerPrefix:          []string{"/ai"},
		GroupRandomReplyProbability: 0.5,
	}, msgBus)
	if err != nil {
		t.Fatalf("NewOneBotChannel() error = %v", err)
	}

	rolls := []float64{0.9, 0.1}
	idx := 0
	ch.randFloat = func() float64 {
		if idx >= len(rolls) {
			return 1
		}
		v := rolls[idx]
		idx++
		return v
	}

	ch.handleMessage(&oneBotEvent{
		MessageType:    "group",
		MessageID:      "r1",
		UserID:         2002,
		GroupID:        1001,
		Content:        "先排队的消息",
		IsBotMentioned: false,
	})

	noMsgCtx, noMsgCancel := context.WithTimeout(context.Background(), 80*time.Millisecond)
	defer noMsgCancel()
	if msg, ok := msgBus.ConsumeInbound(noMsgCtx); ok {
		t.Fatalf("unexpected inbound message before probability hit: %+v", msg)
	}

	ch.handleMessage(&oneBotEvent{
		MessageType:    "group",
		MessageID:      "r2",
		UserID:         2002,
		GroupID:        1001,
		Content:        "概率命中的消息",
		IsBotMentioned: false,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	msg, ok := msgBus.ConsumeInbound(ctx)
	if !ok {
		t.Fatal("expected inbound message when probability hits")
	}
	if msg.Metadata["auto_trigger"] != "probability" {
		t.Fatalf("auto_trigger = %q, want %q", msg.Metadata["auto_trigger"], "probability")
	}
	if !strings.Contains(msg.Content, "<auto_trigger_notice") {
		t.Fatalf("expected auto-trigger notice in content, got: %q", msg.Content)
	}
	if !strings.Contains(msg.Content, "先排队的消息") || !strings.Contains(msg.Content, "概率命中的消息") {
		t.Fatalf("expected merged queued context, got: %q", msg.Content)
	}
}

func TestOneBotHandleMessage_GroupAutoReplyCooldownBlocksProbability(t *testing.T) {
	msgBus := bus.NewMessageBus()
	ch, err := NewOneBotChannel(config.OneBotConfig{
		GroupTriggerPrefix:             []string{"/ai"},
		GroupRandomReplyProbability:    1,
		GroupAutoReplyCooldownMessages: 2,
	}, msgBus)
	if err != nil {
		t.Fatalf("NewOneBotChannel() error = %v", err)
	}

	ch.handleMessage(&oneBotEvent{
		MessageType:    "group",
		MessageID:      "cd1",
		UserID:         2002,
		GroupID:        1001,
		Content:        "第一条，触发自动回复",
		IsBotMentioned: false,
	})

	ctx1, cancel1 := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel1()
	first, ok := msgBus.ConsumeInbound(ctx1)
	if !ok {
		t.Fatal("expected first inbound auto-trigger message")
	}
	if first.Metadata["auto_trigger"] != "probability" {
		t.Fatalf("first auto_trigger = %q, want %q", first.Metadata["auto_trigger"], "probability")
	}

	ch.handleMessage(&oneBotEvent{
		MessageType:    "group",
		MessageID:      "cd2",
		UserID:         2002,
		GroupID:        1001,
		Content:        "第二条，冷却中不该触发",
		IsBotMentioned: false,
	})
	ch.handleMessage(&oneBotEvent{
		MessageType:    "group",
		MessageID:      "cd3",
		UserID:         2002,
		GroupID:        1001,
		Content:        "第三条，冷却中不该触发",
		IsBotMentioned: false,
	})

	noMsgCtx, noMsgCancel := context.WithTimeout(context.Background(), 80*time.Millisecond)
	defer noMsgCancel()
	if msg, ok := msgBus.ConsumeInbound(noMsgCtx); ok {
		t.Fatalf("unexpected inbound message during cooldown: %+v", msg)
	}

	ch.handleMessage(&oneBotEvent{
		MessageType:    "group",
		MessageID:      "cd4",
		UserID:         2002,
		GroupID:        1001,
		Content:        "第四条，冷却结束应再次触发",
		IsBotMentioned: false,
	})

	ctx2, cancel2 := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel2()
	fourth, ok := msgBus.ConsumeInbound(ctx2)
	if !ok {
		t.Fatal("expected inbound message after cooldown")
	}
	if fourth.Metadata["auto_trigger"] != "probability" {
		t.Fatalf("fourth auto_trigger = %q, want %q", fourth.Metadata["auto_trigger"], "probability")
	}
	if !strings.Contains(fourth.Content, "第二条，冷却中不该触发") || !strings.Contains(fourth.Content, "第三条，冷却中不该触发") {
		t.Fatalf("expected cooldown-period messages queued into context, got: %q", fourth.Content)
	}
}

func TestOneBotHandleMessage_GroupAutoReplyCooldownStillAllowsTrigger(t *testing.T) {
	msgBus := bus.NewMessageBus()
	ch, err := NewOneBotChannel(config.OneBotConfig{
		GroupTriggerPrefix:             []string{"/ai"},
		GroupRandomReplyProbability:    1,
		GroupAutoReplyCooldownMessages: 3,
	}, msgBus)
	if err != nil {
		t.Fatalf("NewOneBotChannel() error = %v", err)
	}

	ch.handleMessage(&oneBotEvent{
		MessageType:    "group",
		MessageID:      "ct1",
		UserID:         2002,
		GroupID:        1001,
		Content:        "普通消息先触发自动回复",
		IsBotMentioned: false,
	})

	ctx1, cancel1 := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel1()
	if _, ok := msgBus.ConsumeInbound(ctx1); !ok {
		t.Fatal("expected first inbound auto-trigger message")
	}

	ch.handleMessage(&oneBotEvent{
		MessageType:    "group",
		MessageID:      "ct2",
		UserID:         2002,
		GroupID:        1001,
		Content:        "/ai 这是显式触发",
		IsBotMentioned: false,
	})

	ctx2, cancel2 := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel2()
	second, ok := msgBus.ConsumeInbound(ctx2)
	if !ok {
		t.Fatal("expected explicit trigger message during cooldown")
	}
	if second.Metadata["auto_trigger"] != "" {
		t.Fatalf("explicit trigger should not be marked auto_trigger, got: %q", second.Metadata["auto_trigger"])
	}
	if !strings.Contains(second.Content, "这是显式触发") {
		t.Fatalf("expected explicit trigger content kept, got: %q", second.Content)
	}
}

func TestOneBotHandleMessage_GroupReplyWaitCollectsFollowUpMessages(t *testing.T) {
	msgBus := bus.NewMessageBus()
	ch, err := NewOneBotChannel(config.OneBotConfig{
		GroupTriggerPrefix:    []string{"/ai"},
		GroupContextQueueSize: 20,
		GroupReplyWaitSeconds: 1,
	}, msgBus)
	if err != nil {
		t.Fatalf("NewOneBotChannel() error = %v", err)
	}

	ch.handleMessage(&oneBotEvent{
		MessageType:    "group",
		MessageID:      "w1",
		UserID:         2002,
		GroupID:        1001,
		Content:        "触发前上下文",
		IsBotMentioned: false,
	})

	ch.handleMessage(&oneBotEvent{
		MessageType:    "group",
		MessageID:      "w2",
		UserID:         2002,
		GroupID:        1001,
		Content:        "/ai 开始分析",
		IsBotMentioned: false,
	})

	earlyCtx, earlyCancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer earlyCancel()
	if msg, ok := msgBus.ConsumeInbound(earlyCtx); ok {
		t.Fatalf("unexpected inbound message before wait window ends: %+v", msg)
	}

	time.Sleep(300 * time.Millisecond)
	ch.handleMessage(&oneBotEvent{
		MessageType:    "group",
		MessageID:      "w3",
		UserID:         2003,
		GroupID:        1001,
		Content:        "补充说明",
		IsBotMentioned: false,
	})

	stillWaitingCtx, stillWaitingCancel := context.WithTimeout(context.Background(), 700*time.Millisecond)
	defer stillWaitingCancel()
	if msg, ok := msgBus.ConsumeInbound(stillWaitingCtx); ok {
		t.Fatalf("unexpected inbound message before restarted wait window ends: %+v", msg)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 1700*time.Millisecond)
	defer cancel()
	msg, ok := msgBus.ConsumeInbound(ctx)
	if !ok {
		t.Fatal("expected inbound message after wait window")
	}

	if msg.ChatID != "group:1001" {
		t.Fatalf("chat_id = %q, want %q", msg.ChatID, "group:1001")
	}
	if !strings.Contains(msg.Content, "触发前上下文") || !strings.Contains(msg.Content, "开始分析") || !strings.Contains(msg.Content, "补充说明") {
		t.Fatalf("expected merged queued+triggered+follow-up messages, got: %q", msg.Content)
	}
}

func TestOneBotHandleMessage_GroupReplyWaitKeepsOriginalAutoTriggerReason(t *testing.T) {
	msgBus := bus.NewMessageBus()
	ch, err := NewOneBotChannel(config.OneBotConfig{
		GroupTriggerPrefix:          []string{"/ai"},
		GroupRandomReplyProbability: 1,
		GroupReplyWaitSeconds:       1,
	}, msgBus)
	if err != nil {
		t.Fatalf("NewOneBotChannel() error = %v", err)
	}

	ch.handleMessage(&oneBotEvent{
		MessageType:    "group",
		MessageID:      "wr1",
		UserID:         2002,
		GroupID:        1001,
		Content:        "概率触发的第一条",
		IsBotMentioned: false,
	})

	time.Sleep(200 * time.Millisecond)
	ch.handleMessage(&oneBotEvent{
		MessageType:    "group",
		MessageID:      "wr2",
		UserID:         2002,
		GroupID:        1001,
		Content:        "/ai 后续显式触发不应改原因",
		IsBotMentioned: false,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 1700*time.Millisecond)
	defer cancel()
	msg, ok := msgBus.ConsumeInbound(ctx)
	if !ok {
		t.Fatal("expected inbound message after wait window")
	}

	if msg.Metadata["auto_trigger"] != "probability" {
		t.Fatalf("auto_trigger = %q, want %q", msg.Metadata["auto_trigger"], "probability")
	}
	if !strings.Contains(msg.Content, "后续显式触发不应改原因") {
		t.Fatalf("expected follow-up message merged into context, got: %q", msg.Content)
	}
	if !strings.Contains(msg.Content, "<auto_trigger_notice") {
		t.Fatalf("expected auto-trigger notice to keep original reason, got: %q", msg.Content)
	}
}

func TestOneBotHandleMessage_PrivateXMLIncludesImageAndReply(t *testing.T) {
	msgBus := bus.NewMessageBus()
	ch, err := NewOneBotChannel(config.OneBotConfig{}, msgBus)
	if err != nil {
		t.Fatalf("NewOneBotChannel() error = %v", err)
	}

	ch.handleMessage(&oneBotEvent{
		MessageType: "private",
		MessageID:   "img1",
		UserID:      2002,
		Time:        1700000000,
		Content:     "看这个",
		Segments: []oneBotMessageSegment{
			{Type: "text", Text: "看这个"},
			{Type: "image", ImagePath: "/tmp/picoclaw_media/test.jpg"},
			{
				Type:        "reply",
				ReplyID:     "42",
				ReplyText:   "原消息内容",
				ReplySender: "10086",
				ReplyName:   "Alice",
				ReplyTime:   1700000001,
			},
			{Type: "raw_message", Raw: "[CQ:unknown,data=x]"},
		},
		MediaPaths: []string{"/tmp/picoclaw_media/test.jpg"},
	})

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	msg, ok := msgBus.ConsumeInbound(ctx)
	if !ok {
		t.Fatal("expected inbound message")
	}
	if msg.Metadata["reply_ids"] != "42" {
		t.Fatalf("reply_ids = %q, want %q", msg.Metadata["reply_ids"], "42")
	}
	if len(msg.Media) != 1 || msg.Media[0] != "/tmp/picoclaw_media/test.jpg" {
		t.Fatalf("media = %#v, want image path", msg.Media)
	}
	if !strings.Contains(msg.Content, "<segment type=\"image\"") || !strings.Contains(msg.Content, "<local_path><![CDATA[/tmp/picoclaw_media/test.jpg]]></local_path>") {
		t.Fatalf("expected image xml segment in content, got: %q", msg.Content)
	}
	if !strings.Contains(msg.Content, "<segment type=\"reply\" id=\"42\">") || !strings.Contains(msg.Content, "<quoted sender_id=\"10086\" sender_name=\"Alice\"") {
		t.Fatalf("expected expanded reply xml segment in content, got: %q", msg.Content)
	}
	if !strings.Contains(msg.Content, "<segment type=\"raw_message\"><![CDATA[[CQ:unknown,data=x]]]></segment>") {
		t.Fatalf("expected raw_message xml segment in content, got: %q", msg.Content)
	}
}

func TestOneBotHandleMessage_GroupQueueKeepsImageOnlyMessage(t *testing.T) {
	msgBus := bus.NewMessageBus()
	ch, err := NewOneBotChannel(config.OneBotConfig{
		GroupTriggerPrefix:    []string{"/ai"},
		GroupContextQueueSize: 5,
	}, msgBus)
	if err != nil {
		t.Fatalf("NewOneBotChannel() error = %v", err)
	}

	imagePath := "/tmp/picoclaw_media/queued.jpg"
	ch.handleMessage(&oneBotEvent{
		MessageType: "group",
		MessageID:   "imgq1",
		UserID:      2002,
		GroupID:     1001,
		Content:     "",
		Segments: []oneBotMessageSegment{
			{Type: "image", ImagePath: imagePath},
		},
		MediaPaths: []string{imagePath},
	})

	noMsgCtx, noMsgCancel := context.WithTimeout(context.Background(), 80*time.Millisecond)
	defer noMsgCancel()
	if msg, ok := msgBus.ConsumeInbound(noMsgCtx); ok {
		t.Fatalf("unexpected inbound message before trigger: %+v", msg)
	}

	ch.handleMessage(&oneBotEvent{
		MessageType:    "group",
		MessageID:      "imgq2",
		UserID:         2002,
		GroupID:        1001,
		Content:        "/ai 继续",
		IsBotMentioned: false,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	msg, ok := msgBus.ConsumeInbound(ctx)
	if !ok {
		t.Fatal("expected inbound message on trigger")
	}
	if len(msg.Media) != 1 || msg.Media[0] != imagePath {
		t.Fatalf("media = %#v, want queued image path", msg.Media)
	}
	if !strings.Contains(msg.Content, "role=\"queued\"") || !strings.Contains(msg.Content, imagePath) {
		t.Fatalf("expected queued image in xml context, got: %q", msg.Content)
	}
}
