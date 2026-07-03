package feishu

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/larksuite/oapi-sdk-go/v3/event/dispatcher"
	"github.com/larksuite/oapi-sdk-go/v3/event/dispatcher/callback"
	larkim "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"
	larkws "github.com/larksuite/oapi-sdk-go/v3/ws"
)

var mentionRegex = regexp.MustCompile(`@_user_\d+`)

// ---- Types ----

type Message struct {
	MessageID   string
	Time        string
	Content     string
	Sender      string
	IsOwner     bool
	ChatID      string
	ChatType    string
	IsMentioned bool
}

type Handlers struct {
	OnMessage    func(msg Message)
	OnPassiveMsg func(msg Message)
	OnCardAction func(action CardAction) string
}

type CardAction struct {
	Action      string
	ActionValue map[string]interface{}
	OperatorID  string
	MessageID   string
	ChatID      string
	Token       string
}

// ---- Client ----

type Client struct {
	appID        string
	appSecret    string
	botOpenID    string
	ownerOpenID  string
	targetOpenID string
	httpCli      *http.Client
	token        string
	tokenTime    time.Time
}

func NewClient(appID, appSecret, botOpenID string) *Client {
	return &Client{
		appID:     appID,
		appSecret: appSecret,
		botOpenID: botOpenID,
		httpCli:   &http.Client{Timeout: 30 * time.Second},
	}
}

// SetOwnerOpenID sets the owner's OpenID for identity判断.
func (c *Client) SetOwnerOpenID(openID string) {
	c.ownerOpenID = openID
}

// SetTargetOpenID sets the target's OpenID.
func (c *Client) SetTargetOpenID(openID string) {
	c.targetOpenID = openID
}

func (c *Client) refreshToken(ctx context.Context) error {
	if c.token != "" && time.Since(c.tokenTime) < 2*time.Hour {
		return nil
	}
	body, _ := json.Marshal(map[string]string{"app_id": c.appID, "app_secret": c.appSecret})
	req, err := http.NewRequestWithContext(ctx, "POST",
		"https://open.feishu.cn/open-apis/auth/v3/tenant_access_token/internal", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.httpCli.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("token api: %d", resp.StatusCode)
	}
	var result struct {
		Code              int    `json:"code"`
		Msg               string `json:"msg"`
		TenantAccessToken string `json:"tenant_access_token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return err
	}
	if result.Code != 0 {
		return fmt.Errorf("token: code=%d msg=%s", result.Code, result.Msg)
	}
	c.token = result.TenantAccessToken
	c.tokenTime = time.Now()
	return nil
}

func (c *Client) do(ctx context.Context, method, path string, body interface{}) (json.RawMessage, error) {
	if err := c.refreshToken(ctx); err != nil {
		return nil, err
	}
	var reqBody io.Reader
	if body != nil {
		data, _ := json.Marshal(body)
		reqBody = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method,
		"https://open.feishu.cn/open-apis"+path, reqBody)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.httpCli.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	var result struct {
		Code int             `json:"code"`
		Msg  string          `json:"msg"`
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(data, &result); err != nil {
		return nil, fmt.Errorf("parse response: %v", data)
	}
	if result.Code != 0 {
		snippet := string(data)
		if len(snippet) > 800 {
			snippet = snippet[:800]
		}
		return nil, fmt.Errorf("api %d: %s (resp: %s)", result.Code, result.Msg, snippet)
	}
	return result.Data, nil
}

// ---- Messaging APIs ----

func (c *Client) SendText(ctx context.Context, text string, receiveID string) (string, error) {
	return c.sendMsg(ctx, receiveID, "text", map[string]string{"text": text})
}

func (c *Client) ReplyText(ctx context.Context, text string, messageID string) error {
	_, err := c.do(ctx, "POST", fmt.Sprintf("/im/v1/messages/%s/reply", messageID),
		map[string]interface{}{
			"msg_type": "text",
			"content":  jsonMarshal(map[string]string{"text": text}),
		})
	return err
}

func (c *Client) UploadImage(ctx context.Context, filePath string) (string, error) {
	if err := c.refreshToken(ctx); err != nil {
		return "", err
	}
	file, err := os.Open(filePath)
	if err != nil {
		return "", err
	}
	defer file.Close()

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	if err := writer.WriteField("image_type", "message"); err != nil {
		return "", err
	}
	part, err := writer.CreateFormFile("image", filepath.Base(filePath))
	if err != nil {
		return "", err
	}
	if _, err := io.Copy(part, file); err != nil {
		return "", err
	}
	if err := writer.Close(); err != nil {
		return "", err
	}

	req, err := http.NewRequestWithContext(ctx, "POST", "https://open.feishu.cn/open-apis/im/v1/images", &body)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	resp, err := c.httpCli.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	var result struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
		Data struct {
			ImageKey string `json:"image_key"`
		} `json:"data"`
	}
	if err := json.Unmarshal(data, &result); err != nil {
		return "", fmt.Errorf("parse image upload response: %s", string(data))
	}
	if result.Code != 0 {
		snippet := string(data)
		if len(snippet) > 800 {
			snippet = snippet[:800]
		}
		return "", fmt.Errorf("upload image api %d: %s (resp: %s)", result.Code, result.Msg, snippet)
	}
	return result.Data.ImageKey, nil
}

func (c *Client) ReplyImage(ctx context.Context, filePath string, messageID string) error {
	imageKey, err := c.UploadImage(ctx, filePath)
	if err != nil {
		return err
	}
	_, err = c.do(ctx, "POST", fmt.Sprintf("/im/v1/messages/%s/reply", messageID),
		map[string]interface{}{
			"msg_type": "image",
			"content":  jsonMarshal(map[string]string{"image_key": imageKey}),
		})
	return err
}

func (c *Client) SendImage(ctx context.Context, filePath string, receiveID string) (string, error) {
	imageKey, err := c.UploadImage(ctx, filePath)
	if err != nil {
		return "", err
	}
	return c.sendMsg(ctx, receiveID, "image", map[string]string{"image_key": imageKey})
}

func (c *Client) UpdateTextMessage(ctx context.Context, messageID string, text string) error {
	_, err := c.do(ctx, "PATCH", fmt.Sprintf("/im/v1/messages/%s", messageID),
		map[string]interface{}{
			"msg_type": "text",
			"content":  jsonMarshal(map[string]string{"text": text}),
		})
	return err
}

func (c *Client) SendCard(ctx context.Context, card map[string]interface{}, receiveID string) (string, error) {
	return c.sendMsg(ctx, receiveID, "interactive", card)
}

// SendCardToOpenID sends a card to a user's open_id (for private messages).
func (c *Client) SendCardToOpenID(ctx context.Context, card map[string]interface{}, openID string) (string, error) {
	return c.sendMsgToIDType(ctx, openID, "interactive", card, "open_id")
}

func (c *Client) ReplyCard(ctx context.Context, card map[string]interface{}, messageID string) error {
	_, err := c.do(ctx, "POST", fmt.Sprintf("/im/v1/messages/%s/reply", messageID),
		map[string]interface{}{
			"msg_type": "interactive",
			"content":  jsonMarshal(card),
		})
	return err
}

func (c *Client) sendMsg(ctx context.Context, receiveID, msgType string, content interface{}) (string, error) {
	return c.sendMsgToIDType(ctx, receiveID, msgType, content, "chat_id")
}

func (c *Client) sendMsgToIDType(ctx context.Context, receiveID, msgType string, content interface{}, idType string) (string, error) {
	respData, err := c.do(ctx, "POST", fmt.Sprintf("/im/v1/messages?receive_id_type=%s", idType),
		map[string]interface{}{
			"receive_id": receiveID,
			"msg_type":   msgType,
			"content":    jsonMarshal(content),
		})
	if err != nil {
		return "", err
	}
	var result struct {
		MessageID string `json:"message_id"`
	}
	json.Unmarshal(respData, &result)
	return result.MessageID, nil
}

// ---- Reactions ----

func (c *Client) AddReaction(ctx context.Context, messageID string, emojiType string) (string, error) {
	respData, err := c.do(ctx, "POST", fmt.Sprintf("/im/v1/messages/%s/reactions", messageID),
		map[string]interface{}{"reaction_type": map[string]string{"emoji_type": emojiType}})
	if err != nil {
		return "", err
	}
	var result struct {
		ReactionID string `json:"reaction_id"`
	}
	json.Unmarshal(respData, &result)
	return result.ReactionID, nil
}

func (c *Client) DeleteReaction(ctx context.Context, messageID, reactionID string) error {
	_, err := c.do(ctx, "DELETE", fmt.Sprintf("/im/v1/messages/%s/reactions/%s", messageID, reactionID), nil)
	return err
}

// ---- List messages ----

func (c *Client) ListMessages(ctx context.Context, chatID string, limit int) ([]Message, error) {
	if limit <= 0 {
		limit = 20
	}
	if limit > 50 {
		limit = 50
	}
	respData, err := c.do(ctx, "GET",
		fmt.Sprintf("/im/v1/messages?container_id_type=chat&container_id=%s&sort_type=ByCreateTimeDesc&page_size=%d", chatID, limit),
		nil)
	if err != nil {
		return nil, err
	}
	var result struct {
		Items []struct {
			MessageID  string `json:"message_id"`
			CreateTime string `json:"create_time"`
			MsgType    string `json:"msg_type"`
			Body       struct {
				Content string `json:"content"`
			} `json:"body"`
			Sender struct {
				SenderID struct {
					OpenID string `json:"open_id"`
				} `json:"sender_id"`
				SenderType string `json:"sender_type"`
			} `json:"sender"`
		} `json:"items"`
	}
	if err := json.Unmarshal(respData, &result); err != nil {
		return nil, err
	}

	var out []Message
	for _, item := range result.Items {
		if item.Sender.SenderType != "user" {
			continue
		}
		content := ExtractText(item.MsgType, item.Body.Content)
		if content == "" {
			continue
		}
		content = CleanHistoryText(content)
		if content == "" {
			continue
		}
		senderID := item.Sender.SenderID.OpenID
		out = append(out, Message{
			MessageID: item.MessageID,
			Time:      FormatTime(item.CreateTime),
			Content:   content,
			Sender:    senderID,
			IsOwner:   senderID == c.ownerOpenID,
			ChatID:    chatID,
		})
	}
	return out, nil
}

// ---- WebSocket ----

// StartListening starts the Feishu WebSocket long connection.
// StartListening connects to Feishu via the official long-connection (WebSocket)
// SDK and dispatches message / card-action events to the registered handlers.
// The SDK handles the protobuf framing, pings, and reconnection; we only adapt
// its typed events into the bot's Message / CardAction shapes.
func (c *Client) StartListening(ctx context.Context, handlers Handlers) error {
	d := dispatcher.NewEventDispatcher("", "")

	d.OnP2MessageReceiveV1(func(ctx context.Context, event *larkim.P2MessageReceiveV1) error {
		msg, ok := adaptMessageEvent(event, c)
		if !ok {
			return nil
		}
		// In groups the bot only acts when @-mentioned; otherwise the message
		// is passive context (used for activity tracking).
		if msg.ChatType == "group" && !msg.IsMentioned {
			if handlers.OnPassiveMsg != nil {
				handlers.OnPassiveMsg(msg)
			}
			return nil
		}
		if handlers.OnMessage != nil {
			handlers.OnMessage(msg)
		}
		return nil
	})

	d.OnP2CardActionTrigger(func(ctx context.Context, event *callback.CardActionTriggerEvent) (*callback.CardActionTriggerResponse, error) {
		if handlers.OnCardAction != nil {
			handlers.OnCardAction(adaptCardActionEvent(event))
		}
		return &callback.CardActionTriggerResponse{}, nil
	})

	wsCli := larkws.NewClient(c.appID, c.appSecret, larkws.WithEventHandler(d))
	return wsCli.Start(ctx)
}

// adaptMessageEvent converts an SDK message event into the bot's Message.
func adaptMessageEvent(event *larkim.P2MessageReceiveV1, c *Client) (Message, bool) {
	if event == nil || event.Event == nil || event.Event.Message == nil || event.Event.Sender == nil {
		return Message{}, false
	}
	em := event.Event.Message
	es := event.Event.Sender

	msgType := derefStr(em.MessageType)
	content := TrimMention(ExtractText(msgType, derefStr(em.Content)))
	if content == "" {
		content = "只叫了你一声"
	}

	chatType := derefStr(em.ChatType)
	senderOpenID := ""
	if es.SenderId != nil {
		senderOpenID = derefStr(es.SenderId.OpenId)
	}

	isMentioned := chatType != "group"
	if chatType == "group" {
		for _, m := range em.Mentions {
			if m != nil && m.Id != nil && derefStr(m.Id.OpenId) == c.botOpenID {
				isMentioned = true
				break
			}
		}
	}

	return Message{
		MessageID:   derefStr(em.MessageId),
		Time:        FormatTime(derefStr(em.CreateTime)),
		Content:     content,
		Sender:      senderOpenID,
		IsOwner:     senderOpenID == c.ownerOpenID,
		ChatID:      derefStr(em.ChatId),
		ChatType:    chatType,
		IsMentioned: isMentioned,
	}, true
}

// adaptCardActionEvent converts an SDK card-action event into the bot's CardAction.
func adaptCardActionEvent(event *callback.CardActionTriggerEvent) CardAction {
	if event == nil || event.Event == nil {
		return CardAction{}
	}
	req := event.Event
	action := CardAction{Token: req.Token}
	if req.Action != nil {
		action.Action = getMapString(req.Action.Value, "action")
		action.ActionValue = req.Action.Value
	}
	if req.Operator != nil {
		action.OperatorID = req.Operator.OpenID
	}
	if req.Context != nil {
		action.MessageID = req.Context.OpenMessageID
		action.ChatID = req.Context.OpenChatID
	}
	return action
}

func derefStr(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func getMapString(m map[string]interface{}, key string) string {
	if v, ok := m[key].(string); ok {
		return v
	}
	return ""
}

// ---- Helpers ----

func jsonMarshal(v interface{}) string {
	data, _ := json.Marshal(v)
	return string(data)
}

func joinStrings(parts []string, sep string) string {
	switch len(parts) {
	case 0:
		return ""
	case 1:
		return parts[0]
	}
	n := (len(parts) - 1) * len(sep)
	for _, s := range parts {
		n += len(s)
	}
	buf := make([]byte, 0, n)
	buf = append(buf, parts[0]...)
	for _, s := range parts[1:] {
		buf = append(buf, sep...)
		buf = append(buf, s...)
	}
	return string(buf)
}

// ExtractText extracts plain text from a Feishu message body.
func ExtractText(msgType, content string) string {
	if content == "" {
		return ""
	}
	switch msgType {
	case "text":
		var m map[string]string
		if err := json.Unmarshal([]byte(content), &m); err == nil {
			return m["text"]
		}
		return content
	case "post":
		var m map[string]interface{}
		if err := json.Unmarshal([]byte(content), &m); err != nil {
			return ""
		}
		var texts []string
		for _, v := range m {
			if vm, ok := v.(map[string]interface{}); ok {
				if title, ok := vm["title"].(string); ok && title != "" {
					texts = append(texts, title)
				}
				if arr, ok := vm["content"].([]interface{}); ok {
					for _, para := range arr {
						if paraArr, ok := para.([]interface{}); ok {
							for _, elem := range paraArr {
								if em, ok := elem.(map[string]interface{}); ok && em["tag"] == "text" {
									if t, ok := em["text"].(string); ok {
										texts = append(texts, t)
									}
								}
							}
						}
					}
				}
			}
		}
		return joinStrings(texts, " ")
	default:
		return ""
	}
}

// CleanHistoryText removes @ mentions from message content.
func CleanHistoryText(content string) string {
	content = strings.TrimSpace(content)
	if content == "" {
		return ""
	}
	return strings.TrimSpace(mentionRegex.ReplaceAllString(content, ""))
}

// FormatTime formats a Feishu timestamp (ms) into "01-02 15:04".
func FormatTime(createTime string) string {
	if createTime == "" {
		return ""
	}
	ts, err := strconv.ParseInt(createTime, 10, 64)
	if err != nil {
		return createTime
	}
	if ts > 1e12 {
		ts /= 1000
	}
	return time.Unix(ts, 0).In(time.FixedZone("CST", 8*3600)).Format("01-02 15:04")
}

// TrimMention strips @mention tokens from content.
func TrimMention(content string) string {
	content = strings.TrimSpace(content)
	return strings.TrimSpace(mentionRegex.ReplaceAllString(content, ""))
}
