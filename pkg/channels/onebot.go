package channels

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"math/rand"
	"mime"
	"net/http"
	neturl "net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"github.com/sipeed/picoclaw/pkg/bus"
	"github.com/sipeed/picoclaw/pkg/config"
	"github.com/sipeed/picoclaw/pkg/logger"
	"github.com/sipeed/picoclaw/pkg/utils"
)

type OneBotChannel struct {
	*BaseChannel
	config          config.OneBotConfig
	providers       config.ProvidersConfig
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
	apiWaitMu       sync.Mutex
	echoCounter     int64
	randFloat       func() float64
	nowFunc         func() time.Time
	downloadFile    func(url, filename string) string
	httpClient      *http.Client
	apiWaiters      map[string]chan oneBotAPIResponse
	mediaDir        string
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
	Text   string
}

func (s *BotStatus) UnmarshalJSON(data []byte) error {
	trimmed := strings.TrimSpace(string(data))
	if trimmed == "" || trimmed == "null" {
		*s = BotStatus{}
		return nil
	}

	if len(trimmed) > 0 && trimmed[0] == '"' {
		var text string
		if err := json.Unmarshal(data, &text); err != nil {
			return err
		}
		*s = BotStatus{Text: strings.TrimSpace(text)}
		return nil
	}

	var obj struct {
		Online bool `json:"online"`
		Good   bool `json:"good"`
	}
	if err := json.Unmarshal(data, &obj); err != nil {
		return err
	}
	*s = BotStatus{
		Online: obj.Online,
		Good:   obj.Good,
	}
	return nil
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
	Segments       []oneBotMessageSegment
	MediaPaths     []string
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
	MessageID  string
	SenderID   string
	SenderName string
	Time       int64
	Content    string
	Segments   []oneBotMessageSegment
	MediaPaths []string
}

type oneBotMessageSegment struct {
	Type         string
	Text         string
	AtQQ         string
	IsSelf       bool
	ImageURL     string
	ImageFile    string
	ImagePath    string
	ImageSummary string
	ReplyID      string
	ReplyText    string
	ReplySender  string
	ReplyName    string
	ReplyTime    int64
	Raw          string
}

type oneBotAPIResponse struct {
	Status  string          `json:"status"`
	RetCode json.RawMessage `json:"retcode"`
	Data    json.RawMessage `json:"data"`
	Message string          `json:"message"`
	Wording string          `json:"wording"`
	Echo    string          `json:"echo"`
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
		httpClient:      &http.Client{Timeout: 30 * time.Second},
		apiWaiters:      make(map[string]chan oneBotAPIResponse),
		mediaDir:        filepath.Join("tmp", "imgs"),
	}, nil
}

func (c *OneBotChannel) SetProvidersConfig(providers config.ProvidersConfig) {
	c.providers = providers
}

func (c *OneBotChannel) SetWorkspacePath(workspacePath string) {
	workspacePath = strings.TrimSpace(workspacePath)
	if workspacePath == "" {
		return
	}
	c.mediaDir = filepath.Join(workspacePath, "tmp", "imgs")
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

	echo := c.nextEcho("send")

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

func (c *OneBotChannel) nextEcho(prefix string) string {
	c.writeMu.Lock()
	c.echoCounter++
	echo := fmt.Sprintf("%s_%d", prefix, c.echoCounter)
	c.writeMu.Unlock()
	return echo
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

func (c *OneBotChannel) callOneBotAPI(action string, params interface{}, timeout time.Duration) (*oneBotAPIResponse, error) {
	c.mu.Lock()
	conn := c.conn
	c.mu.Unlock()
	if conn == nil {
		return nil, fmt.Errorf("OneBot WebSocket not connected")
	}

	if timeout <= 0 {
		timeout = 8 * time.Second
	}

	echo := c.nextEcho("api")
	waiter := make(chan oneBotAPIResponse, 1)

	c.apiWaitMu.Lock()
	c.apiWaiters[echo] = waiter
	c.apiWaitMu.Unlock()

	defer func() {
		c.apiWaitMu.Lock()
		delete(c.apiWaiters, echo)
		c.apiWaitMu.Unlock()
	}()

	req := oneBotAPIRequest{
		Action: action,
		Params: params,
		Echo:   echo,
	}
	payload, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal OneBot API request: %w", err)
	}

	c.writeMu.Lock()
	err = conn.WriteMessage(websocket.TextMessage, payload)
	c.writeMu.Unlock()
	if err != nil {
		return nil, fmt.Errorf("failed to write OneBot API request: %w", err)
	}

	timer := time.NewTimer(timeout)
	defer timer.Stop()
	var done <-chan struct{}
	if c.ctx != nil {
		done = c.ctx.Done()
	}

	select {
	case resp := <-waiter:
		return &resp, nil
	case <-timer.C:
		return nil, fmt.Errorf("OneBot API request timeout: action=%s", action)
	case <-done:
		return nil, fmt.Errorf("OneBot channel stopped")
	}
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

			if raw.Echo != "" {
				c.dispatchAPIResponse(raw, message)
				logger.DebugCF("onebot", "Received API response, skipping", map[string]interface{}{
					"echo":   raw.Echo,
					"status": raw.Status.Text,
				})
				continue
			}

			logger.DebugCF("onebot", "Parsed raw event", map[string]interface{}{
				"post_type":       raw.PostType,
				"message_type":    raw.MessageType,
				"sub_type":        raw.SubType,
				"meta_event_type": raw.MetaEventType,
			})

			rawCopy := raw
			go c.handleRawEvent(&rawCopy)
		}
	}
}

func (c *OneBotChannel) dispatchAPIResponse(raw oneBotRawEvent, payload []byte) {
	var resp oneBotAPIResponse
	if err := json.Unmarshal(payload, &resp); err != nil {
		resp = oneBotAPIResponse{
			Echo: raw.Echo,
		}
	}

	if resp.Echo == "" {
		resp.Echo = raw.Echo
	}
	if resp.Status == "" {
		resp.Status = raw.Status.Text
	}

	c.apiWaitMu.Lock()
	waiter := c.apiWaiters[resp.Echo]
	c.apiWaitMu.Unlock()
	if waiter == nil {
		return
	}

	select {
	case waiter <- resp:
	default:
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
	HasUnknown     bool
	Segments       []oneBotMessageSegment
	MediaPaths     []string
}

var oneBotCQPattern = regexp.MustCompile(`\[CQ:([a-zA-Z0-9_]+)(?:,([^\]]*))?\]`)

func parseMessageContentEx(raw json.RawMessage, rawMessage string, selfID int64) parseMessageResult {
	if len(raw) == 0 {
		return parseMessageResult{}
	}

	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return parseOneBotCQMessage(s, rawMessage, selfID)
	}

	var segments []map[string]interface{}
	if err := json.Unmarshal(raw, &segments); err == nil {
		var text strings.Builder
		mentioned := false
		unknownSegment := false
		parsedSegments := make([]oneBotMessageSegment, 0, len(segments))
		selfIDStr := strconv.FormatInt(selfID, 10)
		for _, seg := range segments {
			segType, _ := seg["type"].(string)
			data, _ := seg["data"].(map[string]interface{})
			switch segType {
			case "text":
				if data != nil {
					if t, ok := data["text"].(string); ok {
						text.WriteString(t)
						parsedSegments = append(parsedSegments, oneBotMessageSegment{Type: "text", Text: t})
					}
				}
			case "at":
				qqVal := ""
				if data != nil && selfID > 0 {
					qqVal = fmt.Sprintf("%v", data["qq"])
					if qqVal == selfIDStr || qqVal == "all" {
						mentioned = true
					}
				} else if data != nil {
					qqVal = fmt.Sprintf("%v", data["qq"])
				}
				parsedSegments = append(parsedSegments, oneBotMessageSegment{
					Type:   "at",
					AtQQ:   qqVal,
					IsSelf: selfID > 0 && (qqVal == selfIDStr || qqVal == "all"),
				})
			case "image":
				segMsg := oneBotMessageSegment{Type: "image"}
				if data != nil {
					segMsg.ImageURL = oneBotDataString(data["url"])
					segMsg.ImageFile = oneBotDataString(data["file"])
					segMsg.ImagePath = oneBotDataString(data["path"])
				}
				parsedSegments = append(parsedSegments, segMsg)
			case "reply":
				segMsg := oneBotMessageSegment{Type: "reply"}
				if data != nil {
					segMsg.ReplyID = oneBotDataString(data["id"])
				}
				parsedSegments = append(parsedSegments, segMsg)
			default:
				unknownSegment = true
				segRaw := ""
				if segJSON, err := json.Marshal(seg); err == nil {
					segRaw = string(segJSON)
				}
				parsedSegments = append(parsedSegments, oneBotMessageSegment{
					Type: "unknown",
					Raw:  segRaw,
				})
			}
		}

		trimmedText := strings.TrimSpace(text.String())
		trimmedRaw := strings.TrimSpace(rawMessage)
		if unknownSegment && trimmedRaw != "" && trimmedRaw != trimmedText {
			parsedSegments = append(parsedSegments, oneBotMessageSegment{
				Type: "raw_message",
				Raw:  trimmedRaw,
			})
		}

		return parseMessageResult{
			Text:           trimmedText,
			IsBotMentioned: mentioned,
			HasUnknown:     unknownSegment,
			Segments:       parsedSegments,
		}
	}
	trimmedRaw := strings.TrimSpace(rawMessage)
	if trimmedRaw == "" {
		return parseMessageResult{}
	}
	return parseMessageResult{
		Text:     trimmedRaw,
		Segments: []oneBotMessageSegment{{Type: "text", Text: trimmedRaw}},
	}
}

func oneBotDataString(v interface{}) string {
	if v == nil {
		return ""
	}
	switch t := v.(type) {
	case string:
		return strings.TrimSpace(t)
	case float64:
		if t == float64(int64(t)) {
			return strconv.FormatInt(int64(t), 10)
		}
		return strconv.FormatFloat(t, 'f', -1, 64)
	default:
		return strings.TrimSpace(fmt.Sprintf("%v", v))
	}
}

func parseOneBotCQMessage(content string, rawMessage string, selfID int64) parseMessageResult {
	matches := oneBotCQPattern.FindAllStringSubmatchIndex(content, -1)
	if len(matches) == 0 {
		return parseMessageResult{
			Text:     strings.TrimSpace(content),
			Segments: []oneBotMessageSegment{{Type: "text", Text: content}},
		}
	}

	selfIDStr := strconv.FormatInt(selfID, 10)
	segments := make([]oneBotMessageSegment, 0, len(matches)+1)
	var textBuilder strings.Builder
	mentioned := false
	hasUnknown := false
	cursor := 0

	for _, m := range matches {
		if m[0] > cursor {
			textPart := content[cursor:m[0]]
			if textPart != "" {
				segments = append(segments, oneBotMessageSegment{Type: "text", Text: textPart})
				textBuilder.WriteString(textPart)
			}
		}

		segType := content[m[2]:m[3]]
		paramsRaw := ""
		if m[4] >= 0 && m[5] >= 0 {
			paramsRaw = content[m[4]:m[5]]
		}
		segRaw := content[m[0]:m[1]]
		params := parseOneBotCQParams(paramsRaw)

		switch segType {
		case "at":
			qqVal := strings.TrimSpace(params["qq"])
			isSelf := selfID > 0 && (qqVal == selfIDStr || qqVal == "all")
			if isSelf {
				mentioned = true
			}
			segments = append(segments, oneBotMessageSegment{
				Type:   "at",
				AtQQ:   qqVal,
				IsSelf: isSelf,
			})
		case "image":
			segments = append(segments, oneBotMessageSegment{
				Type:      "image",
				ImageURL:  strings.TrimSpace(params["url"]),
				ImageFile: strings.TrimSpace(params["file"]),
				ImagePath: strings.TrimSpace(params["path"]),
			})
		case "reply":
			segments = append(segments, oneBotMessageSegment{
				Type:    "reply",
				ReplyID: strings.TrimSpace(params["id"]),
			})
		default:
			hasUnknown = true
			segments = append(segments, oneBotMessageSegment{
				Type: "unknown",
				Raw:  segRaw,
			})
		}
		cursor = m[1]
	}

	if cursor < len(content) {
		textPart := content[cursor:]
		if textPart != "" {
			segments = append(segments, oneBotMessageSegment{Type: "text", Text: textPart})
			textBuilder.WriteString(textPart)
		}
	}

	trimmedText := strings.TrimSpace(textBuilder.String())
	trimmedRaw := strings.TrimSpace(rawMessage)
	if trimmedRaw == "" {
		trimmedRaw = strings.TrimSpace(content)
	}
	if hasUnknown && trimmedRaw != "" && trimmedRaw != trimmedText {
		segments = append(segments, oneBotMessageSegment{
			Type: "raw_message",
			Raw:  trimmedRaw,
		})
	}

	return parseMessageResult{
		Text:           trimmedText,
		IsBotMentioned: mentioned,
		HasUnknown:     hasUnknown,
		Segments:       segments,
	}
}

func parseOneBotCQParams(params string) map[string]string {
	result := make(map[string]string)
	if params == "" {
		return result
	}

	items := strings.Split(params, ",")
	for _, item := range items {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		parts := strings.SplitN(item, "=", 2)
		if len(parts) != 2 {
			continue
		}
		key := strings.TrimSpace(parts[0])
		value := strings.TrimSpace(parts[1])
		if key == "" {
			continue
		}
		result[key] = value
	}
	return result
}

func (c *OneBotChannel) enrichParsedMessage(parsed parseMessageResult) parseMessageResult {
	if len(parsed.Segments) == 0 {
		return parsed
	}

	mediaPaths := make([]string, 0, len(parsed.Segments))
	for i := range parsed.Segments {
		seg := parsed.Segments[i]
		switch seg.Type {
		case "image":
			if seg.ImagePath != "" {
				seg.ImagePath = c.ensureImageInWorkspace(seg.ImagePath, seg.ImageFile)
			}

			if seg.ImagePath == "" && seg.ImageURL != "" {
				filename := seg.ImageFile
				if filename == "" {
					filename = oneBotFilenameFromURL(seg.ImageURL)
				}
				localPath := c.downloadImageToWorkspace(seg.ImageURL, filename)
				if localPath != "" {
					seg.ImagePath = localPath
				}
			}

			if seg.ImagePath != "" {
				mediaPaths = appendUniqueString(mediaPaths, seg.ImagePath)
				seg.ImageSummary = c.describeImage(seg.ImagePath)
			}
		case "reply":
			if seg.ReplyID != "" {
				replyDetail, err := c.fetchReplyMessage(seg.ReplyID)
				if err != nil {
					logger.WarnCF("onebot", "Failed to fetch reply message", map[string]interface{}{
						"reply_id": seg.ReplyID,
						"error":    err.Error(),
					})
				} else {
					seg.ReplyText = replyDetail.Text
					seg.ReplySender = replyDetail.SenderID
					seg.ReplyName = replyDetail.SenderName
					seg.ReplyTime = replyDetail.Time
				}
			}
		}

		parsed.Segments[i] = seg
	}
	parsed.MediaPaths = mediaPaths
	return parsed
}

func appendUniqueString(items []string, value string) []string {
	value = strings.TrimSpace(value)
	if value == "" {
		return items
	}
	for _, item := range items {
		if item == value {
			return items
		}
	}
	return append(items, value)
}

func oneBotFilenameFromURL(rawURL string) string {
	parsedURL, err := neturl.Parse(rawURL)
	if err != nil {
		return "image"
	}
	base := path.Base(parsedURL.Path)
	base = strings.TrimSpace(base)
	if base == "" || base == "." || base == "/" {
		return "image"
	}
	return base
}

func (c *OneBotChannel) downloadImageToWorkspace(rawURL, filename string) string {
	if c.downloadFile != nil {
		if localPath := c.downloadFile(rawURL, filename); strings.TrimSpace(localPath) != "" {
			return localPath
		}
	}

	return utils.DownloadFile(rawURL, filename, utils.DownloadOptions{
		LoggerPrefix: "onebot",
		LocalDir:     c.mediaDir,
	})
}

func (c *OneBotChannel) ensureImageInWorkspace(imagePath, filename string) string {
	imagePath = strings.TrimSpace(imagePath)
	if imagePath == "" {
		return ""
	}

	if c.mediaDir == "" {
		return imagePath
	}

	absImagePath, err := filepath.Abs(imagePath)
	if err != nil {
		absImagePath = imagePath
	}

	absMediaDir, err := filepath.Abs(c.mediaDir)
	if err != nil {
		absMediaDir = c.mediaDir
	}

	if oneBotPathInDir(absImagePath, absMediaDir) {
		return absImagePath
	}

	if _, err := os.Stat(absImagePath); err != nil {
		return imagePath
	}

	name := strings.TrimSpace(filename)
	if name == "" {
		name = filepath.Base(absImagePath)
	}
	name = utils.SanitizeFilename(name)
	if name == "" || name == "." {
		name = "image"
	}

	if err := os.MkdirAll(absMediaDir, 0700); err != nil {
		logger.WarnCF("onebot", "Failed to ensure image workspace directory", map[string]interface{}{
			"dir":   absMediaDir,
			"error": err.Error(),
		})
		return imagePath
	}

	targetPath := filepath.Join(absMediaDir, oneBotBuildUniqueFilename(name))
	if err := oneBotCopyFile(absImagePath, targetPath); err != nil {
		logger.WarnCF("onebot", "Failed to copy image into workspace", map[string]interface{}{
			"src":   absImagePath,
			"dst":   targetPath,
			"error": err.Error(),
		})
		return imagePath
	}

	return targetPath
}

func oneBotPathInDir(filePath, dir string) bool {
	rel, err := filepath.Rel(dir, filePath)
	if err != nil {
		return false
	}
	return rel == "." || (!strings.HasPrefix(rel, ".."+string(filepath.Separator)) && rel != "..")
}

func oneBotBuildUniqueFilename(base string) string {
	ext := filepath.Ext(base)
	name := strings.TrimSuffix(base, ext)
	if name == "" {
		name = "image"
	}
	return fmt.Sprintf("%d_%s%s", time.Now().UnixNano(), name, ext)
}

func oneBotCopyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()

	if _, err := io.Copy(out, in); err != nil {
		return err
	}
	return out.Sync()
}

func (c *OneBotChannel) describeImage(localPath string) string {
	if cached, ok := oneBotReadImageSummaryCache(localPath); ok {
		return cached
	}

	model := strings.TrimSpace(c.config.ImageCaptionModel)
	if model == "" {
		return ""
	}

	providerName := strings.ToLower(strings.TrimSpace(c.config.ImageCaptionProvider))
	if providerName == "" {
		return ""
	}

	providerCfg, apiBase, ok := c.resolveCaptionProvider(providerName)
	if !ok || strings.TrimSpace(apiBase) == "" {
		return ""
	}

	data, err := os.ReadFile(localPath)
	if err != nil {
		logger.WarnCF("onebot", "Failed to read image for captioning", map[string]interface{}{
			"path":  localPath,
			"error": err.Error(),
		})
		return ""
	}

	ext := path.Ext(localPath)
	mimeType := mime.TypeByExtension(ext)
	if mimeType == "" {
		mimeType = "application/octet-stream"
	}
	dataURL := fmt.Sprintf("data:%s;base64,%s", mimeType, base64.StdEncoding.EncodeToString(data))

	prompt := strings.TrimSpace(c.config.ImageCaptionPrompt)
	if prompt == "" {
		prompt = "请用一句话描述这张图片的主要内容。"
	}

	requestBody := map[string]interface{}{
		"model": model,
		"messages": []map[string]interface{}{
			{
				"role":    "system",
				"content": prompt,
			},
			{
				"role": "user",
				"content": []map[string]interface{}{
					{"type": "text", "text": "请描述这张图片。"},
					{"type": "image_url", "image_url": map[string]string{"url": dataURL}},
				},
			},
		},
	}

	timeout := c.config.ImageCaptionTimeoutSeconds
	if timeout <= 0 {
		timeout = 30
	}

	client := c.httpClient
	if client == nil {
		client = &http.Client{Timeout: time.Duration(timeout) * time.Second}
	} else {
		client = &http.Client{
			Timeout:   time.Duration(timeout) * time.Second,
			Transport: client.Transport,
		}
	}

	bodyJSON, err := json.Marshal(requestBody)
	if err != nil {
		return ""
	}

	url := oneBotJoinURL(apiBase, "/chat/completions")
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(bodyJSON))
	if err != nil {
		return ""
	}
	req.Header.Set("Content-Type", "application/json")
	if strings.TrimSpace(providerCfg.APIKey) != "" {
		req.Header.Set("Authorization", "Bearer "+providerCfg.APIKey)
	}

	resp, err := client.Do(req)
	if err != nil {
		logger.WarnCF("onebot", "Image caption request failed", map[string]interface{}{
			"error": err.Error(),
		})
		return ""
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		logger.WarnCF("onebot", "Image caption response status is not successful", map[string]interface{}{
			"status": resp.StatusCode,
		})
		return ""
	}

	var payload map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		logger.WarnCF("onebot", "Failed to decode image caption response", map[string]interface{}{
			"error": err.Error(),
		})
		return ""
	}

	choices, _ := payload["choices"].([]interface{})
	if len(choices) == 0 {
		return ""
	}
	choice, _ := choices[0].(map[string]interface{})
	msg, _ := choice["message"].(map[string]interface{})
	if msg == nil {
		return ""
	}

	switch content := msg["content"].(type) {
	case string:
		summary := strings.TrimSpace(content)
		oneBotWriteImageSummaryCache(localPath, summary)
		return summary
	case []interface{}:
		var sb strings.Builder
		for _, item := range content {
			part, _ := item.(map[string]interface{})
			if part == nil {
				continue
			}
			if t, _ := part["type"].(string); t == "text" {
				if txt, _ := part["text"].(string); strings.TrimSpace(txt) != "" {
					if sb.Len() > 0 {
						sb.WriteString(" ")
					}
					sb.WriteString(strings.TrimSpace(txt))
				}
			}
		}
		summary := strings.TrimSpace(sb.String())
		oneBotWriteImageSummaryCache(localPath, summary)
		return summary
	default:
		return ""
	}
}

func oneBotReadImageSummaryCache(localPath string) (string, bool) {
	for _, cachePath := range oneBotImageSummaryCachePaths(localPath) {
		if cachePath == "" {
			continue
		}
		data, err := os.ReadFile(cachePath)
		if err != nil {
			continue
		}
		summary := strings.TrimSpace(string(data))
		if summary == "" {
			continue
		}
		return summary, true
	}
	return "", false
}

func oneBotWriteImageSummaryCache(localPath, summary string) {
	summary = strings.TrimSpace(summary)
	if localPath == "" || summary == "" {
		return
	}

	cachePath := localPath + ".txt" // hi-compatible sidecar cache: "<image-file>.txt"
	if err := os.WriteFile(cachePath, []byte(summary), 0644); err != nil {
		logger.WarnCF("onebot", "Failed to write image summary cache", map[string]interface{}{
			"path":  cachePath,
			"error": err.Error(),
		})
	}
}

func oneBotImageSummaryCachePaths(localPath string) []string {
	localPath = strings.TrimSpace(localPath)
	if localPath == "" {
		return nil
	}

	paths := []string{localPath + ".txt"} // hi-compatible primary path

	ext := filepath.Ext(localPath)
	base := filepath.Base(localPath)
	dir := filepath.Dir(localPath)
	if ext != "" {
		altName := strings.TrimSuffix(base, ext) + ".txt"
		altPath := filepath.Join(dir, altName)
		if altPath != paths[0] {
			paths = append(paths, altPath)
		}
	}

	return paths
}

func (c *OneBotChannel) resolveCaptionProvider(name string) (config.ProviderConfig, string, bool) {
	name = strings.ToLower(strings.TrimSpace(name))
	if name == "" {
		return config.ProviderConfig{}, "", false
	}

	var provider config.ProviderConfig
	var defaultBase string
	switch name {
	case "openai", "gpt":
		provider = c.providers.OpenAI
		defaultBase = "https://api.openai.com/v1"
	case "openrouter":
		provider = c.providers.OpenRouter
		defaultBase = "https://openrouter.ai/api/v1"
	case "anthropic", "claude":
		provider = c.providers.Anthropic
		defaultBase = "https://api.anthropic.com/v1"
	case "groq":
		provider = c.providers.Groq
		defaultBase = "https://api.groq.com/openai/v1"
	case "zhipu", "glm":
		provider = c.providers.Zhipu
		defaultBase = "https://open.bigmodel.cn/api/paas/v4"
	case "gemini", "google":
		provider = c.providers.Gemini
		defaultBase = "https://generativelanguage.googleapis.com/v1beta"
	case "nvidia":
		provider = c.providers.Nvidia
		defaultBase = "https://integrate.api.nvidia.com/v1"
	case "moonshot":
		provider = c.providers.Moonshot
		defaultBase = "https://api.moonshot.cn/v1"
	case "deepseek":
		provider = c.providers.DeepSeek
		defaultBase = "https://api.deepseek.com/v1"
	case "shengsuanyun":
		provider = c.providers.ShengSuanYun
		defaultBase = "https://router.shengsuanyun.com/api/v1"
	case "vllm":
		provider = c.providers.VLLM
	case "ollama":
		provider = c.providers.Ollama
		defaultBase = "http://localhost:11434/v1"
	default:
		return config.ProviderConfig{}, "", false
	}

	apiBase := strings.TrimSpace(provider.APIBase)
	if apiBase == "" {
		apiBase = defaultBase
	}
	if apiBase == "" {
		return config.ProviderConfig{}, "", false
	}
	return provider, apiBase, true
}

type oneBotReplyMessage struct {
	Text       string
	SenderID   string
	SenderName string
	Time       int64
}

func (c *OneBotChannel) fetchReplyMessage(replyID string) (*oneBotReplyMessage, error) {
	replyID = strings.TrimSpace(replyID)
	if replyID == "" {
		return nil, fmt.Errorf("empty reply id")
	}

	var messageID interface{} = replyID
	if idNum, err := strconv.ParseInt(replyID, 10, 64); err == nil {
		messageID = idNum
	}

	resp, err := c.callOneBotAPI("get_msg", map[string]interface{}{
		"message_id": messageID,
	}, 8*time.Second)
	if err != nil {
		return nil, err
	}

	status := strings.ToLower(strings.TrimSpace(resp.Status))
	if status != "" && status != "ok" {
		return nil, fmt.Errorf("reply lookup failed: status=%s", status)
	}

	if len(resp.Data) == 0 {
		return nil, fmt.Errorf("reply lookup has empty data")
	}

	var raw struct {
		MessageID  json.RawMessage `json:"message_id"`
		UserID     json.RawMessage `json:"user_id"`
		Time       json.RawMessage `json:"time"`
		RawMessage string          `json:"raw_message"`
		Message    json.RawMessage `json:"message"`
		Sender     json.RawMessage `json:"sender"`
	}
	if err := json.Unmarshal(resp.Data, &raw); err != nil {
		return nil, fmt.Errorf("parse reply payload failed: %w", err)
	}

	parsed := parseMessageContentEx(raw.Message, raw.RawMessage, 0)
	text := strings.TrimSpace(parsed.Text)
	if text == "" {
		text = strings.TrimSpace(raw.RawMessage)
	}

	senderID, _ := parseJSONInt64(raw.UserID)
	senderIDStr := ""
	if senderID > 0 {
		senderIDStr = strconv.FormatInt(senderID, 10)
	}

	senderName := ""
	if len(raw.Sender) > 0 {
		var sender oneBotSender
		if err := json.Unmarshal(raw.Sender, &sender); err == nil {
			if sender.Card != "" {
				senderName = sender.Card
			} else if sender.Nickname != "" {
				senderName = sender.Nickname
			}
			if senderIDStr == "" {
				senderUserID, _ := parseJSONInt64(sender.UserID)
				if senderUserID > 0 {
					senderIDStr = strconv.FormatInt(senderUserID, 10)
				}
			}
		}
	}

	ts, _ := parseJSONInt64(raw.Time)
	return &oneBotReplyMessage{
		Text:       text,
		SenderID:   senderIDStr,
		SenderName: senderName,
		Time:       ts,
	}, nil
}

func oneBotJoinURL(base, pathPart string) string {
	base = strings.TrimRight(strings.TrimSpace(base), "/")
	pathPart = strings.Trim(strings.TrimSpace(pathPart), "/")
	if base == "" {
		return ""
	}
	if pathPart == "" {
		return base
	}
	if strings.HasSuffix(strings.ToLower(base), "/"+strings.ToLower(pathPart)) {
		return base
	}
	return base + "/" + pathPart
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

	parsed := parseMessageContentEx(raw.Message, raw.RawMessage, selfID)
	parsed = c.enrichParsedMessage(parsed)
	isBotMentioned := parsed.IsBotMentioned

	content := strings.TrimSpace(parsed.Text)
	if content == "" {
		content = strings.TrimSpace(raw.RawMessage)
	}
	if content != "" && selfID > 0 {
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
		Segments:       parsed.Segments,
		MediaPaths:     parsed.MediaPaths,
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

	content := strings.TrimSpace(evt.Content)
	if content == "" && len(evt.Segments) == 0 {
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
	mediaPaths := append([]string{}, evt.MediaPaths...)

	metadata := map[string]string{
		"message_id":     evt.MessageID,
		"message_format": "xml",
	}

	senderName := ""
	if evt.Sender.Card != "" {
		senderName = evt.Sender.Card
	} else if evt.Sender.Nickname != "" {
		senderName = evt.Sender.Nickname
	}

	currentMsg := oneBotQueuedMessage{
		MessageID:  evt.MessageID,
		SenderID:   senderID,
		SenderName: senderName,
		Time:       evt.Time,
		Content:    content,
		Segments:   evt.Segments,
		MediaPaths: evt.MediaPaths,
	}
	if replyIDs := extractReplyIDs(evt.Segments); len(replyIDs) > 0 {
		metadata["reply_ids"] = strings.Join(replyIDs, ",")
	}

	switch evt.MessageType {
	case "private":
		chatID = "private:" + senderID
		content = buildOneBotMessageXML(currentMsg, "private")
		c.logDebugXML("private", chatID, "private_received", content)
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
				MessageID:  evt.MessageID,
				SenderID:   senderID,
				SenderName: senderName,
				Time:       evt.Time,
				Content:    content,
				Segments:   evt.Segments,
				MediaPaths: evt.MediaPaths,
			})
			queuedXML := buildOneBotMessageXML(oneBotQueuedMessage{
				MessageID:  evt.MessageID,
				SenderID:   senderID,
				SenderName: senderName,
				Time:       evt.Time,
				Content:    content,
				Segments:   evt.Segments,
				MediaPaths: evt.MediaPaths,
			}, "queued")
			c.logDebugXML("group", "group:"+groupIDStr, "group_queued", queuedXML)
			logger.DebugCF("onebot", "Group message ignored (no trigger)", map[string]interface{}{
				"sender":       senderID,
				"group":        groupIDStr,
				"is_mentioned": evt.IsBotMentioned,
				"queued":       queueSize,
				"content":      truncate(content, 100),
			})
			return
		}
		originalContent := content
		content = strippedContent
		currentMsg.Content = content
		if content != originalContent {
			currentMsg.Segments = overrideTriggeredSegments(currentMsg.Segments, content)
		}
		if len(currentMsg.Segments) == 0 && content != "" {
			currentMsg.Segments = []oneBotMessageSegment{{Type: "text", Text: content}}
		}
		chatID = "group:" + groupIDStr
		metadata["group_id"] = groupIDStr

		queued := c.drainGroupMessages(groupIDStr)
		content, mediaPaths = buildGroupContextContent(queued, currentMsg, metadata["auto_trigger"])
		c.logDebugXML("group", chatID, "group_forward", content)
		if len(queued) > 0 {
			logger.InfoCF("onebot", "Merged queued group context", map[string]interface{}{
				"group":        groupIDStr,
				"queued_count": len(queued),
			})
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

	c.HandleMessage(senderID, chatID, content, mediaPaths, metadata)
}

func (c *OneBotChannel) logDebugXML(messageType, chatID, stage, xmlContent string) {
	if !c.config.Debug {
		return
	}
	xmlContent = strings.TrimSpace(xmlContent)
	if xmlContent == "" {
		return
	}

	logger.InfoCF("onebot", "Debug packaged XML", map[string]interface{}{
		"message_type": messageType,
		"chat_id":      chatID,
		"stage":        stage,
		"xml":          xmlContent,
	})
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
	msg.Content = strings.TrimSpace(msg.Content)
	if msg.Content == "" && len(msg.Segments) == 0 && len(msg.MediaPaths) == 0 {
		return 0
	}

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

func buildGroupContextContent(queued []oneBotQueuedMessage, triggered oneBotQueuedMessage, autoTriggerReason string) (string, []string) {
	var b strings.Builder
	mediaPaths := make([]string, 0, len(queued)+len(triggered.MediaPaths))

	b.WriteString("<group_context>\n")
	b.WriteString("  <messages>\n")
	for _, msg := range queued {
		for _, mediaPath := range msg.MediaPaths {
			mediaPaths = appendUniqueString(mediaPaths, mediaPath)
		}
		b.WriteString(oneBotIndentLines(buildOneBotMessageXML(msg, "queued"), 4))
		b.WriteByte('\n')
	}
	for _, mediaPath := range triggered.MediaPaths {
		mediaPaths = appendUniqueString(mediaPaths, mediaPath)
	}
	b.WriteString(oneBotIndentLines(buildOneBotMessageXML(triggered, "triggered"), 4))
	b.WriteByte('\n')
	b.WriteString("  </messages>\n")
	if autoTriggerReason != "" {
		b.WriteString("  <auto_trigger_notice reason=\"")
		b.WriteString(oneBotXMLEscape(autoTriggerReason))
		b.WriteString("\">这些消息可能并不是直接对机器人说的，请作为群聊环境上下文理解。</auto_trigger_notice>\n")
	}
	b.WriteString("</group_context>")

	return strings.TrimSpace(b.String()), mediaPaths
}

func buildOneBotMessageXML(msg oneBotQueuedMessage, role string) string {
	var b strings.Builder
	b.WriteString("<message")
	if msg.MessageID != "" {
		b.WriteString(" id=\"")
		b.WriteString(oneBotXMLEscape(msg.MessageID))
		b.WriteString("\"")
	}
	if role != "" {
		b.WriteString(" role=\"")
		b.WriteString(oneBotXMLEscape(role))
		b.WriteString("\"")
	}
	if msg.SenderID != "" {
		b.WriteString(" sender_id=\"")
		b.WriteString(oneBotXMLEscape(msg.SenderID))
		b.WriteString("\"")
	}
	if msg.SenderName != "" {
		b.WriteString(" sender_name=\"")
		b.WriteString(oneBotXMLEscape(msg.SenderName))
		b.WriteString("\"")
	}
	if msg.Time > 0 {
		b.WriteString(" time=\"")
		b.WriteString(oneBotXMLEscape(time.Unix(msg.Time, 0).Format("2006-01-02 15:04:05")))
		b.WriteString("\"")
	}
	b.WriteString(">\n")

	if len(msg.Segments) == 0 && msg.Content != "" {
		msg.Segments = []oneBotMessageSegment{{Type: "text", Text: msg.Content}}
	}

	for _, seg := range msg.Segments {
		switch seg.Type {
		case "text":
			text := strings.TrimSpace(seg.Text)
			if text == "" {
				continue
			}
			b.WriteString("  <segment type=\"text\">")
			b.WriteString(oneBotXMLEscape(text))
			b.WriteString("</segment>\n")
		case "at":
			b.WriteString("  <segment type=\"at\"")
			if seg.AtQQ != "" {
				b.WriteString(" qq=\"")
				b.WriteString(oneBotXMLEscape(seg.AtQQ))
				b.WriteString("\"")
			}
			if seg.IsSelf {
				b.WriteString(" mention_self=\"true\"")
			}
			b.WriteString(" />\n")
		case "image":
			b.WriteString("  <segment type=\"image\"")
			if seg.ImageURL != "" {
				b.WriteString(" url=\"")
				b.WriteString(oneBotXMLEscape(seg.ImageURL))
				b.WriteString("\"")
			}
			if seg.ImageFile != "" {
				b.WriteString(" file=\"")
				b.WriteString(oneBotXMLEscape(seg.ImageFile))
				b.WriteString("\"")
			}
			if seg.ImagePath != "" {
				b.WriteString(" path=\"")
				b.WriteString(oneBotXMLEscape(seg.ImagePath))
				b.WriteString("\"")
			}
			b.WriteString(">\n")
			if seg.ImagePath != "" {
				b.WriteString("    <local_path>")
				b.WriteString(oneBotXMLEscape(seg.ImagePath))
				b.WriteString("</local_path>\n")
			}
			if seg.ImageSummary != "" {
				b.WriteString("    <summary>")
				b.WriteString(oneBotXMLEscape(seg.ImageSummary))
				b.WriteString("</summary>\n")
			}
			b.WriteString("  </segment>\n")
		case "reply":
			b.WriteString("  <segment type=\"reply\"")
			if seg.ReplyID != "" {
				b.WriteString(" id=\"")
				b.WriteString(oneBotXMLEscape(seg.ReplyID))
				b.WriteString("\"")
			}
			if seg.ReplyText == "" && seg.ReplySender == "" && seg.ReplyName == "" && seg.ReplyTime <= 0 {
				b.WriteString(" />\n")
				continue
			}
			b.WriteString(">\n")
			b.WriteString("    <quoted")
			if seg.ReplySender != "" {
				b.WriteString(" sender_id=\"")
				b.WriteString(oneBotXMLEscape(seg.ReplySender))
				b.WriteString("\"")
			}
			if seg.ReplyName != "" {
				b.WriteString(" sender_name=\"")
				b.WriteString(oneBotXMLEscape(seg.ReplyName))
				b.WriteString("\"")
			}
			if seg.ReplyTime > 0 {
				b.WriteString(" time=\"")
				b.WriteString(oneBotXMLEscape(time.Unix(seg.ReplyTime, 0).Format("2006-01-02 15:04:05")))
				b.WriteString("\"")
			}
			b.WriteString(">")
			b.WriteString(oneBotXMLEscape(seg.ReplyText))
			b.WriteString("</quoted>\n")
			b.WriteString("  </segment>\n")
		case "raw_message":
			rawText := strings.TrimSpace(seg.Raw)
			if rawText == "" {
				continue
			}
			b.WriteString("  <segment type=\"raw_message\">")
			b.WriteString(oneBotXMLEscape(rawText))
			b.WriteString("</segment>\n")
		default:
			b.WriteString("  <segment type=\"unknown\">")
			b.WriteString(oneBotXMLEscape(strings.TrimSpace(seg.Raw)))
			b.WriteString("</segment>\n")
		}
	}

	b.WriteString("</message>")
	return b.String()
}

func oneBotXMLEscape(s string) string {
	if s == "" {
		return ""
	}
	var b bytes.Buffer
	xml.EscapeText(&b, []byte(s))
	return b.String()
}

func oneBotIndentLines(s string, spaces int) string {
	prefix := strings.Repeat(" ", spaces)
	lines := strings.Split(s, "\n")
	for i, line := range lines {
		lines[i] = prefix + line
	}
	return strings.Join(lines, "\n")
}

func extractReplyIDs(segments []oneBotMessageSegment) []string {
	replyIDs := []string{}
	for _, seg := range segments {
		if seg.Type != "reply" {
			continue
		}
		replyIDs = appendUniqueString(replyIDs, seg.ReplyID)
	}
	return replyIDs
}

func overrideTriggeredSegments(segments []oneBotMessageSegment, strippedText string) []oneBotMessageSegment {
	strippedText = strings.TrimSpace(strippedText)
	if len(segments) == 0 {
		if strippedText == "" {
			return nil
		}
		return []oneBotMessageSegment{{Type: "text", Text: strippedText}}
	}

	result := make([]oneBotMessageSegment, 0, len(segments))
	replacedText := false
	for _, seg := range segments {
		switch seg.Type {
		case "text":
			if replacedText {
				continue
			}
			if strippedText != "" {
				seg.Text = strippedText
				result = append(result, seg)
			}
			replacedText = true
		case "at":
			if !replacedText && seg.IsSelf {
				continue
			}
			result = append(result, seg)
		default:
			result = append(result, seg)
		}
	}

	if !replacedText && strippedText != "" {
		result = append([]oneBotMessageSegment{{Type: "text", Text: strippedText}}, result...)
	}
	return result
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
