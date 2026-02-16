package channels

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"github.com/sipeed/picoclaw/pkg/bus"
	"github.com/sipeed/picoclaw/pkg/config"
	"github.com/sipeed/picoclaw/pkg/logger"
)

type OneBotChannel struct {
	*BaseChannel
	config          config.OneBotConfig
	conn            *websocket.Conn
	ctx             context.Context
	cancel          context.CancelFunc
	dedup           map[string]struct{}
	dedupRing       []string
	dedupIdx        int
	groupPending    map[string][]oneBotQueuedMessage
	groupQueueLimit int
	lastGroupReply  map[string]time.Time
	mu              sync.Mutex
	pendingMu       sync.Mutex
	replyMu         sync.Mutex
	writeMu         sync.Mutex
	echoCounter     int64
	randFloat       func() float64
	nowFunc         func() time.Time
}

type oneBotRawEvent struct {
	PostType      string          `json:"post_type"`
	MessageType   string          `json:"message_type"`
	SubType       string          `json:"sub_type"`
	MessageID     json.RawMessage `json:"message_id"`
	UserID        json.RawMessage `json:"user_id"`
	GroupID       json.RawMessage `json:"group_id"`
	RawMessage    string          `json:"raw_message"`
	Message       json.RawMessage `json:"message"`
	Sender        json.RawMessage `json:"sender"`
	SelfID        json.RawMessage `json:"self_id"`
	Time          json.RawMessage `json:"time"`
	MetaEventType string          `json:"meta_event_type"`
	Echo          string          `json:"echo"`
	RetCode       json.RawMessage `json:"retcode"`
	Status        BotStatus       `json:"status"`
}

type BotStatus struct {
	Online bool `json:"online"`
	Good   bool `json:"good"`
}

type oneBotSender struct {
	UserID   json.RawMessage `json:"user_id"`
	Nickname string          `json:"nickname"`
	Card     string          `json:"card"`
}

type oneBotEvent struct {
	PostType       string
	MessageType    string
	SubType        string
	MessageID      string
	UserID         int64
	GroupID        int64
	Content        string
	RawContent     string
	IsBotMentioned bool
	Sender         oneBotSender
	SelfID         int64
	Time           int64
	MetaEventType  string
}

type oneBotAPIRequest struct {
	Action string      `json:"action"`
	Params interface{} `json:"params"`
	Echo   string      `json:"echo,omitempty"`
}

type oneBotSendPrivateMsgParams struct {
	UserID  int64  `json:"user_id"`
	Message string `json:"message"`
}

type oneBotSendGroupMsgParams struct {
	GroupID int64  `json:"group_id"`
	Message string `json:"message"`
}

type oneBotQueuedMessage struct {
	SenderID   string
	SenderName string
	Content    string
}

func NewOneBotChannel(cfg config.OneBotConfig, messageBus *bus.MessageBus) (*OneBotChannel, error) {
	base := NewBaseChannel("onebot", cfg, messageBus, cfg.AllowFrom)

	const dedupSize = 1024
	const defaultGroupQueueLimit = 20
	groupQueueLimit := cfg.GroupContextQueueSize
	if groupQueueLimit <= 0 {
		groupQueueLimit = defaultGroupQueueLimit
	}

	return &OneBotChannel{
		BaseChannel:     base,
		config:          cfg,
		dedup:           make(map[string]struct{}, dedupSize),
		dedupRing:       make([]string, dedupSize),
		dedupIdx:        0,
		groupPending:    make(map[string][]oneBotQueuedMessage),
		groupQueueLimit: groupQueueLimit,
		lastGroupReply:  make(map[string]time.Time),
		randFloat:       rand.Float64,
		nowFunc:         time.Now,
	}, nil
}

func (c *OneBotChannel) Start(ctx context.Context) error {
	if c.config.WSUrl == "" {
		return fmt.Errorf("OneBot ws_url not configured")
	}

	logger.InfoCF("onebot", "Starting OneBot channel", map[string]interface{}{
		"ws_url": c.config.WSUrl,
	})

	c.ctx, c.cancel = context.WithCancel(ctx)

	if err := c.connect(); err != nil {
		logger.WarnCF("onebot", "Initial connection failed, will retry in background", map[string]interface{}{
			"error": err.Error(),
		})
	} else {
		go c.listen()
	}

	if c.config.ReconnectInterval > 0 {
		go c.reconnectLoop()
	} else {
		// If reconnect is disabled but initial connection failed, we cannot recover
		if c.conn == nil {
			return fmt.Errorf("failed to connect to OneBot and reconnect is disabled")
		}
	}

	c.setRunning(true)
	logger.InfoC("onebot", "OneBot channel started successfully")

	return nil
}

func (c *OneBotChannel) connect() error {
	dialer := websocket.DefaultDialer
	dialer.HandshakeTimeout = 10 * time.Second

	header := make(map[string][]string)
	if c.config.AccessToken != "" {
		header["Authorization"] = []string{"Bearer " + c.config.AccessToken}
	}

	conn, _, err := dialer.Dial(c.config.WSUrl, header)
	if err != nil {
		return err
	}

	c.mu.Lock()
	c.conn = conn
	c.mu.Unlock()

	logger.InfoC("onebot", "WebSocket connected")
	return nil
}

func (c *OneBotChannel) reconnectLoop() {
	interval := time.Duration(c.config.ReconnectInterval) * time.Second
	if interval < 5*time.Second {
		interval = 5 * time.Second
	}

	for {
		select {
		case <-c.ctx.Done():
			return
		case <-time.After(interval):
			c.mu.Lock()
			conn := c.conn
			c.mu.Unlock()

			if conn == nil {
				logger.InfoC("onebot", "Attempting to reconnect...")
				if err := c.connect(); err != nil {
					logger.ErrorCF("onebot", "Reconnect failed", map[string]interface{}{
						"error": err.Error(),
					})
				} else {
					go c.listen()
				}
			}
		}
	}
}

func (c *OneBotChannel) Stop(ctx context.Context) error {
	logger.InfoC("onebot", "Stopping OneBot channel")
	c.setRunning(false)

	if c.cancel != nil {
		c.cancel()
	}

	c.mu.Lock()
	if c.conn != nil {
		c.conn.Close()
		c.conn = nil
	}
	c.mu.Unlock()

	return nil
}

func (c *OneBotChannel) Send(ctx context.Context, msg bus.OutboundMessage) error {
	if !c.IsRunning() {
		return fmt.Errorf("OneBot channel not running")
	}

	c.mu.Lock()
	conn := c.conn
	c.mu.Unlock()

	if conn == nil {
		return fmt.Errorf("OneBot WebSocket not connected")
	}

	action, params, err := c.buildSendRequest(msg)
	if err != nil {
		return err
	}

	c.writeMu.Lock()
	c.echoCounter++
	echo := fmt.Sprintf("send_%d", c.echoCounter)
	c.writeMu.Unlock()

	req := oneBotAPIRequest{
		Action: action,
		Params: params,
		Echo:   echo,
	}

	data, err := json.Marshal(req)
	if err != nil {
		return fmt.Errorf("failed to marshal OneBot request: %w", err)
	}

	c.writeMu.Lock()
	err = conn.WriteMessage(websocket.TextMessage, data)
	c.writeMu.Unlock()

	if err != nil {
		logger.ErrorCF("onebot", "Failed to send message", map[string]interface{}{
			"error": err.Error(),
		})
		return err
	}

	if groupID, ok := parseOneBotGroupChatID(msg.ChatID); ok {
		c.markGroupReplied(groupID)
	}

	return nil
}

func (c *OneBotChannel) buildSendRequest(msg bus.OutboundMessage) (string, interface{}, error) {
	chatID := msg.ChatID

	if len(chatID) > 6 && chatID[:6] == "group:" {
		groupID, err := strconv.ParseInt(chatID[6:], 10, 64)
		if err != nil {
			return "", nil, fmt.Errorf("invalid group ID in chatID: %s", chatID)
		}
		return "send_group_msg", oneBotSendGroupMsgParams{
			GroupID: groupID,
			Message: msg.Content,
		}, nil
	}

	if len(chatID) > 8 && chatID[:8] == "private:" {
		userID, err := strconv.ParseInt(chatID[8:], 10, 64)
		if err != nil {
			return "", nil, fmt.Errorf("invalid user ID in chatID: %s", chatID)
		}
		return "send_private_msg", oneBotSendPrivateMsgParams{
			UserID:  userID,
			Message: msg.Content,
		}, nil
	}

	userID, err := strconv.ParseInt(chatID, 10, 64)
	if err != nil {
		return "", nil, fmt.Errorf("invalid chatID for OneBot: %s", chatID)
	}

	return "send_private_msg", oneBotSendPrivateMsgParams{
		UserID:  userID,
		Message: msg.Content,
	}, nil
}

func (c *OneBotChannel) listen() {
	for {
		select {
		case <-c.ctx.Done():
			return
		default:
			c.mu.Lock()
			conn := c.conn
			c.mu.Unlock()

			if conn == nil {
				logger.WarnC("onebot", "WebSocket connection is nil, listener exiting")
				return
			}

			_, message, err := conn.ReadMessage()
			if err != nil {
				logger.ErrorCF("onebot", "WebSocket read error", map[string]interface{}{
					"error": err.Error(),
				})
				c.mu.Lock()
				if c.conn != nil {
					c.conn.Close()
					c.conn = nil
				}
				c.mu.Unlock()
				return
			}

			logger.DebugCF("onebot", "Raw WebSocket message received", map[string]interface{}{
				"length":  len(message),
				"payload": string(message),
			})

			var raw oneBotRawEvent
			if err := json.Unmarshal(message, &raw); err != nil {
				logger.WarnCF("onebot", "Failed to unmarshal raw event", map[string]interface{}{
					"error":   err.Error(),
					"payload": string(message),
				})
				continue
			}

			if raw.Echo != "" || raw.Status.Online || raw.Status.Good {
				logger.DebugCF("onebot", "Received API response, skipping", map[string]interface{}{
					"echo":   raw.Echo,
					"status": raw.Status,
				})
				continue
			}

			logger.DebugCF("onebot", "Parsed raw event", map[string]interface{}{
				"post_type":       raw.PostType,
				"message_type":    raw.MessageType,
				"sub_type":        raw.SubType,
				"meta_event_type": raw.MetaEventType,
			})

			c.handleRawEvent(&raw)
		}
	}
}

func parseJSONInt64(raw json.RawMessage) (int64, error) {
	if len(raw) == 0 {
		return 0, nil
	}

	var n int64
	if err := json.Unmarshal(raw, &n); err == nil {
		return n, nil
	}

	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return strconv.ParseInt(s, 10, 64)
	}
	return 0, fmt.Errorf("cannot parse as int64: %s", string(raw))
}

func parseJSONString(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}

	return string(raw)
}

type parseMessageResult struct {
	Text           string
	IsBotMentioned bool
}

func parseMessageContentEx(raw json.RawMessage, selfID int64) parseMessageResult {
	if len(raw) == 0 {
		return parseMessageResult{}
	}

	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		mentioned := false
		if selfID > 0 {
			cqAt := fmt.Sprintf("[CQ:at,qq=%d]", selfID)
			if strings.Contains(s, cqAt) {
				mentioned = true
				s = strings.ReplaceAll(s, cqAt, "")
				s = strings.TrimSpace(s)
			}
		}
		return parseMessageResult{Text: s, IsBotMentioned: mentioned}
	}

	var segments []map[string]interface{}
	if err := json.Unmarshal(raw, &segments); err == nil {
		var text string
		mentioned := false
		selfIDStr := strconv.FormatInt(selfID, 10)
		for _, seg := range segments {
			segType, _ := seg["type"].(string)
			data, _ := seg["data"].(map[string]interface{})
			switch segType {
			case "text":
				if data != nil {
					if t, ok := data["text"].(string); ok {
						text += t
					}
				}
			case "at":
				if data != nil && selfID > 0 {
					qqVal := fmt.Sprintf("%v", data["qq"])
					if qqVal == selfIDStr || qqVal == "all" {
						mentioned = true
					}
				}
			}
		}
		return parseMessageResult{Text: strings.TrimSpace(text), IsBotMentioned: mentioned}
	}
	return parseMessageResult{}
}

func (c *OneBotChannel) handleRawEvent(raw *oneBotRawEvent) {
	switch raw.PostType {
	case "message":
		evt, err := c.normalizeMessageEvent(raw)
		if err != nil {
			logger.WarnCF("onebot", "Failed to normalize message event", map[string]interface{}{
				"error": err.Error(),
			})
			return
		}
		c.handleMessage(evt)
	case "meta_event":
		c.handleMetaEvent(raw)
	case "notice":
		logger.DebugCF("onebot", "Notice event received", map[string]interface{}{
			"sub_type": raw.SubType,
		})
	case "request":
		logger.DebugCF("onebot", "Request event received", map[string]interface{}{
			"sub_type": raw.SubType,
		})
	case "":
		logger.DebugCF("onebot", "Event with empty post_type (possibly API response)", map[string]interface{}{
			"echo":   raw.Echo,
			"status": raw.Status,
		})
	default:
		logger.DebugCF("onebot", "Unknown post_type", map[string]interface{}{
			"post_type": raw.PostType,
		})
	}
}

func (c *OneBotChannel) normalizeMessageEvent(raw *oneBotRawEvent) (*oneBotEvent, error) {
	userID, err := parseJSONInt64(raw.UserID)
	if err != nil {
		return nil, fmt.Errorf("parse user_id: %w (raw: %s)", err, string(raw.UserID))
	}

	groupID, _ := parseJSONInt64(raw.GroupID)
	selfID, _ := parseJSONInt64(raw.SelfID)
	ts, _ := parseJSONInt64(raw.Time)
	messageID := parseJSONString(raw.MessageID)

	parsed := parseMessageContentEx(raw.Message, selfID)
	isBotMentioned := parsed.IsBotMentioned

	content := raw.RawMessage
	if content == "" {
		content = parsed.Text
	} else if selfID > 0 {
		cqAt := fmt.Sprintf("[CQ:at,qq=%d]", selfID)
		if strings.Contains(content, cqAt) {
			isBotMentioned = true
			content = strings.ReplaceAll(content, cqAt, "")
			content = strings.TrimSpace(content)
		}
	}

	var sender oneBotSender
	if len(raw.Sender) > 0 {
		if err := json.Unmarshal(raw.Sender, &sender); err != nil {
			logger.WarnCF("onebot", "Failed to parse sender", map[string]interface{}{
				"error":  err.Error(),
				"sender": string(raw.Sender),
			})
		}
	}

	logger.DebugCF("onebot", "Normalized message event", map[string]interface{}{
		"message_type": raw.MessageType,
		"user_id":      userID,
		"group_id":     groupID,
		"message_id":   messageID,
		"content_len":  len(content),
		"nickname":     sender.Nickname,
	})

	return &oneBotEvent{
		PostType:       raw.PostType,
		MessageType:    raw.MessageType,
		SubType:        raw.SubType,
		MessageID:      messageID,
		UserID:         userID,
		GroupID:        groupID,
		Content:        content,
		RawContent:     raw.RawMessage,
		IsBotMentioned: isBotMentioned,
		Sender:         sender,
		SelfID:         selfID,
		Time:           ts,
		MetaEventType:  raw.MetaEventType,
	}, nil
}

func (c *OneBotChannel) handleMetaEvent(raw *oneBotRawEvent) {
	switch raw.MetaEventType {
	case "lifecycle":
		logger.InfoCF("onebot", "Lifecycle event", map[string]interface{}{
			"sub_type": raw.SubType,
		})
	case "heartbeat":
		logger.DebugC("onebot", "Heartbeat received")
	default:
		logger.DebugCF("onebot", "Unknown meta_event_type", map[string]interface{}{
			"meta_event_type": raw.MetaEventType,
		})
	}
}

func (c *OneBotChannel) handleMessage(evt *oneBotEvent) {
	if c.isDuplicate(evt.MessageID) {
		logger.DebugCF("onebot", "Duplicate message, skipping", map[string]interface{}{
			"message_id": evt.MessageID,
		})
		return
	}

	content := evt.Content
	if content == "" {
		logger.DebugCF("onebot", "Received empty message, ignoring", map[string]interface{}{
			"message_id": evt.MessageID,
		})
		return
	}

	senderID := strconv.FormatInt(evt.UserID, 10)
	if !c.IsAllowed(senderID) {
		logger.DebugCF("onebot", "Message ignored (sender not allowed)", map[string]interface{}{
			"sender":     senderID,
			"message_id": evt.MessageID,
			"type":       evt.MessageType,
		})
		return
	}

	var chatID string

	metadata := map[string]string{
		"message_id": evt.MessageID,
	}

	switch evt.MessageType {
	case "private":
		chatID = "private:" + senderID
		logger.InfoCF("onebot", "Received private message", map[string]interface{}{
			"sender":     senderID,
			"message_id": evt.MessageID,
			"length":     len(content),
			"content":    truncate(content, 100),
		})

	case "group":
		groupIDStr := strconv.FormatInt(evt.GroupID, 10)
		if !c.isGroupAllowed(groupIDStr) {
			logger.DebugCF("onebot", "Group message ignored (group not allowed)", map[string]interface{}{
				"sender":  senderID,
				"group":   groupIDStr,
				"content": truncate(content, 100),
			})
			return
		}

		senderUserID, _ := parseJSONInt64(evt.Sender.UserID)
		if senderUserID > 0 {
			metadata["sender_user_id"] = strconv.FormatInt(senderUserID, 10)
		}

		senderName := ""
		if evt.Sender.Card != "" {
			senderName = evt.Sender.Card
		} else if evt.Sender.Nickname != "" {
			senderName = evt.Sender.Nickname
		}
		if senderName != "" {
			metadata["sender_name"] = senderName
		}

		triggered, strippedContent := c.checkGroupTrigger(content, evt.IsBotMentioned)
		if !triggered {
			autoTriggered, reason := c.shouldAutoReplyGroup(groupIDStr)
			if autoTriggered {
				triggered = true
				strippedContent = strings.TrimSpace(content)
				metadata["auto_trigger"] = reason
				logger.InfoCF("onebot", "Group message auto-triggered", map[string]interface{}{
					"group":   groupIDStr,
					"sender":  senderID,
					"reason":  reason,
					"content": truncate(content, 100),
				})
			}
		}
		if !triggered {
			queueSize := c.enqueueGroupMessage(groupIDStr, oneBotQueuedMessage{
				SenderID:   senderID,
				SenderName: senderName,
				Content:    content,
			})
			logger.DebugCF("onebot", "Group message ignored (no trigger)", map[string]interface{}{
				"sender":       senderID,
				"group":        groupIDStr,
				"is_mentioned": evt.IsBotMentioned,
				"queued":       queueSize,
				"content":      truncate(content, 100),
			})
			return
		}
		content = strippedContent
		chatID = "group:" + groupIDStr
		metadata["group_id"] = groupIDStr

		queued := c.drainGroupMessages(groupIDStr)
		if len(queued) > 0 {
			content = buildGroupContextContent(queued, content)
			logger.InfoCF("onebot", "Merged queued group context", map[string]interface{}{
				"group":        groupIDStr,
				"queued_count": len(queued),
			})
		}
		if reason, ok := metadata["auto_trigger"]; ok {
			content = buildAutoTriggeredGroupContent(content, reason)
		}

		logger.InfoCF("onebot", "Received group message", map[string]interface{}{
			"sender":       senderID,
			"group":        groupIDStr,
			"message_id":   evt.MessageID,
			"is_mentioned": evt.IsBotMentioned,
			"length":       len(content),
			"content":      truncate(content, 100),
		})

	default:
		logger.WarnCF("onebot", "Unknown message type, cannot route", map[string]interface{}{
			"type":       evt.MessageType,
			"message_id": evt.MessageID,
			"user_id":    evt.UserID,
		})
		return
	}

	if evt.Sender.Nickname != "" {
		metadata["nickname"] = evt.Sender.Nickname
	}

	logger.DebugCF("onebot", "Forwarding message to bus", map[string]interface{}{
		"sender_id": senderID,
		"chat_id":   chatID,
		"content":   truncate(content, 100),
	})

	c.HandleMessage(senderID, chatID, content, []string{}, metadata)
}

func (c *OneBotChannel) isDuplicate(messageID string) bool {
	if messageID == "" || messageID == "0" {
		return false
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	if _, exists := c.dedup[messageID]; exists {
		return true
	}

	if old := c.dedupRing[c.dedupIdx]; old != "" {
		delete(c.dedup, old)
	}
	c.dedupRing[c.dedupIdx] = messageID
	c.dedup[messageID] = struct{}{}
	c.dedupIdx = (c.dedupIdx + 1) % len(c.dedupRing)

	return false
}

func truncate(s string, n int) string {
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	return string(runes[:n]) + "..."
}

func (c *OneBotChannel) checkGroupTrigger(content string, isBotMentioned bool) (triggered bool, strippedContent string) {
	if isBotMentioned {
		return true, strings.TrimSpace(content)
	}

	for _, prefix := range c.config.GroupTriggerPrefix {
		if prefix == "" {
			continue
		}
		if strings.HasPrefix(content, prefix) {
			return true, strings.TrimSpace(strings.TrimPrefix(content, prefix))
		}
	}

	return false, content
}

func (c *OneBotChannel) isGroupAllowed(groupID string) bool {
	if len(c.config.AllowGroups) == 0 {
		return true
	}

	for _, allowed := range c.config.AllowGroups {
		normalized := strings.TrimSpace(strings.TrimPrefix(allowed, "group:"))
		if normalized == groupID {
			return true
		}
	}

	return false
}

func (c *OneBotChannel) shouldAutoReplyGroup(groupID string) (bool, string) {
	maxSilenceSeconds := c.config.GroupForceReplyIntervalSeconds
	if maxSilenceSeconds > 0 {
		now := c.nowFunc()

		c.replyMu.Lock()
		lastReply, exists := c.lastGroupReply[groupID]
		c.replyMu.Unlock()

		if !exists || now.Sub(lastReply) >= time.Duration(maxSilenceSeconds)*time.Second {
			return true, "max_silence"
		}
	}

	probability := c.config.GroupRandomReplyProbability
	if probability <= 0 {
		return false, ""
	}
	if probability > 1 {
		probability = 1
	}

	if c.randFloat() < probability {
		return true, "probability"
	}

	return false, ""
}

func (c *OneBotChannel) enqueueGroupMessage(groupID string, msg oneBotQueuedMessage) int {
	content := strings.TrimSpace(msg.Content)
	if content == "" {
		return 0
	}
	msg.Content = content

	c.pendingMu.Lock()
	defer c.pendingMu.Unlock()

	queue := append(c.groupPending[groupID], msg)
	if len(queue) > c.groupQueueLimit {
		queue = queue[len(queue)-c.groupQueueLimit:]
	}
	c.groupPending[groupID] = queue

	return len(queue)
}

func (c *OneBotChannel) drainGroupMessages(groupID string) []oneBotQueuedMessage {
	c.pendingMu.Lock()
	defer c.pendingMu.Unlock()

	queue := c.groupPending[groupID]
	if len(queue) == 0 {
		return nil
	}

	result := make([]oneBotQueuedMessage, len(queue))
	copy(result, queue)
	delete(c.groupPending, groupID)

	return result
}

func buildGroupContextContent(queued []oneBotQueuedMessage, triggeredContent string) string {
	if len(queued) == 0 {
		return strings.TrimSpace(triggeredContent)
	}

	var b strings.Builder
	b.WriteString("[Group context before trigger]\n")
	for i, msg := range queued {
		label := msg.SenderID
		if msg.SenderName != "" {
			label = fmt.Sprintf("%s(%s)", msg.SenderName, msg.SenderID)
		}
		fmt.Fprintf(&b, "%d. %s: %s\n", i+1, label, msg.Content)
	}

	triggeredContent = strings.TrimSpace(triggeredContent)
	if triggeredContent != "" {
		b.WriteString("\n[Triggered message]\n")
		b.WriteString(triggeredContent)
	}

	return strings.TrimSpace(b.String())
}

func buildAutoTriggeredGroupContent(content, reason string) string {
	content = strings.TrimSpace(content)
	if content == "" {
		return content
	}

	note := "[Auto-triggered notice]\nThe following messages may not be explicitly addressed to the bot (no trigger prefix or @ mention). Consider them as ambient group context."
	if reason != "" {
		note += "\nReason: " + reason
	}

	return note + "\n\n" + content
}

func (c *OneBotChannel) markGroupReplied(groupID string) {
	if groupID == "" {
		return
	}

	c.replyMu.Lock()
	c.lastGroupReply[groupID] = c.nowFunc()
	c.replyMu.Unlock()
}

func parseOneBotGroupChatID(chatID string) (string, bool) {
	if !strings.HasPrefix(chatID, "group:") {
		return "", false
	}
	groupID := strings.TrimSpace(strings.TrimPrefix(chatID, "group:"))
	if groupID == "" {
		return "", false
	}
	return groupID, true
}
