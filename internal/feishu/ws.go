package feishu

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

const (
	wsEndpoint   = "https://open.feishu.cn/open-apis/event/v1/websocket/app_websocket"
	wsPingInterval = 25 * time.Second
	wsReadLimit   = 8192
)

// WSClient is a Feishu WebSocket event client.
type WSClient struct {
	appID       string
	appSecret   string
	botOpenID   string
	ownerOpenID string
	targetOpenID string
	handlers    Handlers
	conn        *websocket.Conn
	mu          sync.Mutex
}

// NewWSClient creates a WSClient. ownerOpenID and targetOpenID are used for identity判断.
func NewWSClient(appID, appSecret, botOpenID, ownerOpenID, targetOpenID string, handlers Handlers) *WSClient {
	return &WSClient{
		appID:       appID,
		appSecret:   appSecret,
		botOpenID:   botOpenID,
		ownerOpenID: ownerOpenID,
		targetOpenID: targetOpenID,
		handlers:    handlers,
	}
}

// Start connects to Feishu WebSocket and runs the event loop.
func (c *WSClient) Start(ctx context.Context) error {
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		url, _, err := c.getWSEndpoint(ctx)
		if err != nil {
			log.Printf("[WS] 获取端点失败: %v，3秒后重试", err)
			time.Sleep(3 * time.Second)
			continue
		}

		if err := c.connect(ctx, url); err != nil {
			log.Printf("[WS] 连接失败: %v，5秒后重试", err)
			time.Sleep(5 * time.Second)
			continue
		}

		log.Println("[WS] 连接已建立")
		c.readLoop(ctx)
		c.mu.Lock()
		c.conn = nil
		c.mu.Unlock()
		log.Println("[WS] 连接断开，将重连")
	}
}

func (c *WSClient) getWSEndpoint(ctx context.Context) (string, string, error) {
	body, _ := json.Marshal(map[string]string{
		"app_id":     c.appID,
		"app_secret": c.appSecret,
	})
	req, err := http.NewRequestWithContext(ctx, "POST", wsEndpoint, bytes.NewReader(body))
	if err != nil {
		return "", "", err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", "", err
	}
	var result struct {
		Code    int    `json:"code"`
		Msg     string `json:"msg"`
		URL     string `json:"ws_url"`
		Token   string `json:"token"`
		Timeout int    `json:"timeout"`
	}
	if err := json.Unmarshal(data, &result); err != nil {
		return "", "", fmt.Errorf("parse ws endpoint: %w", err)
	}
	if result.Code != 0 {
		return "", "", fmt.Errorf("ws endpoint: code=%d msg=%s", result.Code, result.Msg)
	}
	return result.URL, result.Token, nil
}

func (c *WSClient) connect(ctx context.Context, url string) error {
	dialer := websocket.Dialer{
		HandshakeTimeout: 10 * time.Second,
		ReadBufferSize:   wsReadLimit,
		WriteBufferSize:  4096,
	}
	conn, _, err := dialer.DialContext(ctx, url, nil)
	if err != nil {
		return err
	}
	c.mu.Lock()
	c.conn = conn
	c.mu.Unlock()
	return nil
}

func (c *WSClient) readLoop(ctx context.Context) {
	c.mu.Lock()
	conn := c.conn
	c.mu.Unlock()

	conn.SetReadLimit(wsReadLimit)
	conn.SetReadDeadline(time.Now().Add(wsPingInterval + 10*time.Second))
	conn.SetPongHandler(func(string) error {
		conn.SetReadDeadline(time.Now().Add(wsPingInterval + 10*time.Second))
		return nil
	})

	done := make(chan struct{})
	go func() {
		ticker := time.NewTicker(wsPingInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				c.mu.Lock()
				if c.conn != nil {
					c.conn.WriteControl(websocket.PingMessage, nil, time.Now().Add(5*time.Second))
				}
				c.mu.Unlock()
			case <-done:
				return
			}
		}
	}()

	defer close(done)

	for {
		select {
		case <-ctx.Done():
			c.mu.Lock()
			if c.conn != nil {
				c.conn.WriteMessage(websocket.CloseMessage,
					websocket.FormatCloseMessage(websocket.CloseGoingAway, ""))
				c.conn.Close()
			}
			c.mu.Unlock()
			return
		default:
		}

		_, data, err := conn.ReadMessage()
		if err != nil {
			if !websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseNormalClosure) {
				log.Printf("[WS] 读取消息错误: %v", err)
			}
			return
		}

		c.handleFrame(data)
	}
}

func (c *WSClient) handleFrame(data []byte) {
	var frame wsEventFrame
	if err := json.Unmarshal(data, &frame); err != nil {
		return
	}
	if frame.Schema == "" {
		return
	}

	eventType := frame.Header.EventType
	if strings.Contains(eventType, "im.message.receive_v1") {
		c.handleMessageEvent(frame.Event)
	} else if strings.Contains(eventType, "card.action.trigger") {
		c.handleCardActionEvent(frame.Event)
	}
}

func (c *WSClient) handleMessageEvent(eventData json.RawMessage) {
	var payload struct {
		Sender struct {
			SenderID struct {
				OpenID string `json:"open_id"`
			} `json:"sender_id"`
			SenderType string `json:"sender_type"`
		} `json:"sender"`
		Message struct {
			MessageID   string `json:"message_id"`
			ChatID      string `json:"chat_id"`
			ChatType    string `json:"chat_type"`
			MessageType string `json:"message_type"`
			CreateTime  string `json:"create_time"`
			Content     string `json:"content"`
			Mentions    []struct {
				MentionType string `json:"mentioned_type"`
				MentionID   struct {
					OpenID string `json:"open_id"`
				} `json:"mention_id"`
			} `json:"mentions"`
		} `json:"message"`
	}
	if err := json.Unmarshal(eventData, &payload); err != nil {
		log.Printf("[WS] 解析消息事件失败: %v", err)
		return
	}

	content := ExtractText(payload.Message.MessageType, payload.Message.Content)
	content = TrimMention(content)
	if content == "" {
		content = "只叫了你一声"
	}

	isMentioned := payload.Message.ChatType != "group"
	if payload.Message.ChatType == "group" {
		for _, m := range payload.Message.Mentions {
			if m.MentionType == "bot" || m.MentionID.OpenID == c.botOpenID {
				isMentioned = true
				break
			}
		}
	}

	msg := Message{
		MessageID:   payload.Message.MessageID,
		Time:        FormatTime(payload.Message.CreateTime),
		Content:     content,
		Sender:      payload.Sender.SenderID.OpenID,
		IsOwner:     payload.Sender.SenderID.OpenID == c.ownerOpenID,
		ChatID:      payload.Message.ChatID,
		ChatType:    payload.Message.ChatType,
		IsMentioned: isMentioned,
	}

	if payload.Message.ChatType == "group" && !isMentioned {
		if c.handlers.OnPassiveMsg != nil {
			c.handlers.OnPassiveMsg(msg)
		}
		return
	}

	if c.handlers.OnMessage != nil {
		c.handlers.OnMessage(msg)
	}
}

func (c *WSClient) handleCardActionEvent(eventData json.RawMessage) {
	var payload struct {
		Action struct {
			Tag       string                 `json:"tag"`
			ElementID string                 `json:"element_id"`
			Value     map[string]interface{} `json:"value"`
		} `json:"action"`
		Context struct {
			OpenMessageID string `json:"open_message_id"`
			OpenChatID    string `json:"open_chat_id"`
		} `json:"context"`
		Operator struct {
			OperatorID struct {
				OpenID string `json:"open_id"`
			} `json:"operator_id"`
		} `json:"operator"`
		Token string `json:"token"`
	}
	if err := json.Unmarshal(eventData, &payload); err != nil {
		log.Printf("[WS] 解析卡片事件失败: %v", err)
		return
	}

	action := CardAction{
		Action:      getMapString(payload.Action.Value, "action"),
		ActionValue: payload.Action.Value,
		OperatorID:  payload.Operator.OperatorID.OpenID,
		MessageID:   payload.Context.OpenMessageID,
		ChatID:      payload.Context.OpenChatID,
		Token:       payload.Token,
	}

	if c.handlers.OnCardAction != nil {
		c.handlers.OnCardAction(action)
	}
}

func getMapString(m map[string]interface{}, key string) string {
	if v, ok := m[key].(string); ok {
		return v
	}
	return ""
}

type wsEventFrame struct {
	Schema  string          `json:"schema"`
	Header  wsFrameHeader   `json:"header"`
	Event   json.RawMessage `json:"event"`
}

type wsFrameHeader struct {
	EventID    string `json:"event_id"`
	EventType  string `json:"event_type"`
	CreateTime string `json:"create_time"`
	Token      string `json:"token"`
	AppID      string `json:"app_id"`
	TenantKey  string `json:"tenant_key"`
}
