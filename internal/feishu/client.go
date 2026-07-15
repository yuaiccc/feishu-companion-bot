package feishu

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
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
	MsgType     string
	Content     string
	ImageKey    string
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
	// lastSent tracks the most recent message_id the bot sent per chat (or
	// receive_id), so the bot can recall its own wrong reply.
	lastSent        map[string]string
	lastSentMu      sync.Mutex
	ocrMu           sync.Mutex
	ocrBlockedUntil time.Time
}

func NewClient(appID, appSecret, botOpenID string) *Client {
	return &Client{
		appID:     appID,
		appSecret: appSecret,
		botOpenID: botOpenID,
		httpCli:   &http.Client{Timeout: 30 * time.Second},
		lastSent:  make(map[string]string),
	}
}

// SetOwnerOpenID sets the owner's OpenID for identity判断.
func (c *Client) SetOwnerOpenID(openID string) {
	c.ownerOpenID = openID
}

// LabelSender maps a sender open_id to a readable name for conversation
// context, so the LLM sees "三哥: ..." / "小弟: ..." instead of opaque open_ids.
// Unknown users stay "对方"; only targetOpenID is labeled as the configured
// target, preventing the bot from mistaking owner/other users for the target.
func (c *Client) LabelSender(openid, ownerName, botName, targetName string) string {
	switch openid {
	case c.ownerOpenID:
		return ownerName
	case c.botOpenID:
		if botName != "" {
			return botName
		}
		return "小弟"
	case c.targetOpenID:
		if targetName != "" {
			return targetName
		}
	}
	return "对方"
}

// SetTargetOpenID sets the target's OpenID.
func (c *Client) SetTargetOpenID(openID string) {
	c.targetOpenID = openID
}

// HealthCheck performs a real authenticated Feishu request. When chatID is
// set, it also verifies that the bot can access the configured chat.
func (c *Client) HealthCheck(ctx context.Context, chatID string) error {
	if strings.TrimSpace(chatID) == "" {
		return c.refreshToken(ctx)
	}
	_, err := c.do(ctx, "GET", fmt.Sprintf("/im/v1/chats/%s", url.PathEscape(chatID)), nil)
	return err
}

func (c *Client) refreshToken(ctx context.Context) error {
	// Feishu tenant_access_token expires at 2h. Refresh a bit early (110m) so a
	// request doesn't slip through in the expiry window with an about-to-expire
	// token (which Feishu rejects with 99991663).
	if c.token != "" && time.Since(c.tokenTime) < 110*time.Minute {
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
	var marshaled []byte
	if body != nil {
		marshaled, _ = json.Marshal(body)
	}
	// Retry once on 99991663 (token rejected as invalid/expired): force a
	// refresh and re-send. Covers the 2h expiry edge and external invalidation.
	for attempt := 0; attempt < 2; attempt++ {
		if err := c.refreshToken(ctx); err != nil {
			return nil, err
		}
		var reqBody io.Reader
		if marshaled != nil {
			reqBody = bytes.NewReader(marshaled)
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
		data, err := io.ReadAll(resp.Body)
		resp.Body.Close()
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
		if result.Code == 99991663 && attempt == 0 {
			c.token = ""
			c.tokenTime = time.Time{}
			continue
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
	return nil, fmt.Errorf("api 99991663: token still invalid after retry")
}

// ---- Messaging APIs ----

func (c *Client) SendText(ctx context.Context, text string, receiveID string) (string, error) {
	return c.sendMsg(ctx, receiveID, "text", map[string]string{"text": text})
}

func (c *Client) ReplyText(ctx context.Context, text string, messageID string) (string, error) {
	data, err := c.do(ctx, "POST", fmt.Sprintf("/im/v1/messages/%s/reply", messageID),
		map[string]interface{}{
			"msg_type": "text",
			"content":  jsonMarshal(map[string]string{"text": text}),
		})
	if err != nil {
		return "", err
	}
	var resp struct {
		MessageID string `json:"message_id"`
	}
	json.Unmarshal(data, &resp)
	return resp.MessageID, nil
}

// RecallMessage recalls (撤回) a message the bot sent. Feishu allows recalling
// a message within a time window after sending; used so the bot can take back a
// reply it sent in error.
func (c *Client) RecallMessage(ctx context.Context, messageID string) error {
	_, err := c.do(ctx, "DELETE", fmt.Sprintf("/im/v1/messages/%s", messageID), nil)
	return err
}

// NoteSent records a sent message_id for a chat (or receive_id), so the bot can
// recall its last reply. Public so ReplyText callers (which reply by message_id
// and don't go through sendMsg) can record the chat mapping themselves.
func (c *Client) NoteSent(chatID, messageID string) {
	if chatID == "" || messageID == "" {
		return
	}
	c.lastSentMu.Lock()
	defer c.lastSentMu.Unlock()
	c.lastSent[chatID] = messageID
}

// RecallLastSent recalls the bot's most recent message in the given chat.
// Returns nil if there's nothing recorded to recall.
func (c *Client) RecallLastSent(ctx context.Context, chatID string) error {
	c.lastSentMu.Lock()
	messageID := c.lastSent[chatID]
	c.lastSentMu.Unlock()
	if messageID == "" {
		return fmt.Errorf("no recent bot message to recall in this chat")
	}
	return c.RecallMessage(ctx, messageID)
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

func (c *Client) DownloadImage(ctx context.Context, imageKey string) ([]byte, error) {
	if imageKey == "" {
		return nil, fmt.Errorf("empty image key")
	}
	if err := c.refreshToken(ctx); err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, "GET", "https://open.feishu.cn/open-apis/im/v1/images/"+url.PathEscape(imageKey), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	resp, err := c.httpCli.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		snippet := string(data)
		if len(snippet) > 800 {
			snippet = snippet[:800]
		}
		return nil, fmt.Errorf("download image http %d: %s", resp.StatusCode, snippet)
	}
	if strings.Contains(resp.Header.Get("Content-Type"), "application/json") {
		var result struct {
			Code int    `json:"code"`
			Msg  string `json:"msg"`
		}
		if json.Unmarshal(data, &result) == nil && result.Code != 0 {
			return nil, fmt.Errorf("download image api %d: %s", result.Code, result.Msg)
		}
	}
	return data, nil
}

func (c *Client) RecognizeImageText(ctx context.Context, image []byte, cooldown time.Duration) ([]string, error) {
	if len(image) == 0 {
		return nil, fmt.Errorf("empty image")
	}
	c.ocrMu.Lock()
	if time.Now().Before(c.ocrBlockedUntil) {
		until := c.ocrBlockedUntil
		c.ocrMu.Unlock()
		return nil, fmt.Errorf("feishu ocr cooldown until %s", until.Format(time.RFC3339))
	}
	c.ocrMu.Unlock()

	body := map[string]string{"image": base64.StdEncoding.EncodeToString(image)}
	data, err := c.doOCR(ctx, body)
	if err != nil {
		if IsRateLimitError(err) && cooldown > 0 {
			c.ocrMu.Lock()
			c.ocrBlockedUntil = time.Now().Add(cooldown)
			c.ocrMu.Unlock()
		}
		return nil, err
	}
	return extractOCRTexts(data), nil
}

func (c *Client) doOCR(ctx context.Context, body interface{}) (json.RawMessage, error) {
	marshaled, _ := json.Marshal(body)
	if err := c.refreshToken(ctx); err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, "POST", "https://open.feishu.cn/open-apis/optical_char_recognition/v1/image/basic_recognize", bytes.NewReader(marshaled))
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
		return nil, fmt.Errorf("parse ocr response: %s", string(data))
	}
	if result.Code != 0 {
		snippet := string(data)
		if len(snippet) > 800 {
			snippet = snippet[:800]
		}
		return nil, fmt.Errorf("ocr api %d: %s (resp: %s)", result.Code, result.Msg, snippet)
	}
	return result.Data, nil
}

func IsRateLimitError(err error) bool {
	return err != nil && (strings.Contains(err.Error(), "99991400") || strings.Contains(err.Error(), "frequency limit"))
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
	c.NoteSent(receiveID, result.MessageID)
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
		limit = 50
	}
	var out []Message
	pageToken := ""
	for len(out) < limit {
		pageSize := limit - len(out)
		if pageSize > 50 {
			pageSize = 50 // Feishu caps page_size at 50
		}
		path := fmt.Sprintf("/im/v1/messages?container_id_type=chat&container_id=%s&sort_type=ByCreateTimeDesc&page_size=%d", chatID, pageSize)
		if pageToken != "" {
			path += "&page_token=" + url.QueryEscape(pageToken)
		}
		respData, err := c.do(ctx, "GET", path, nil)
		if err != nil {
			if len(out) > 0 {
				return out, nil // return what we have if a later page fails
			}
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
					ID         string `json:"id"`
					IDType     string `json:"id_type"`
					SenderType string `json:"sender_type"`
				} `json:"sender"`
			} `json:"items"`
			HasMore   bool   `json:"has_more"`
			PageToken string `json:"page_token"`
		}
		if err := json.Unmarshal(respData, &result); err != nil {
			if len(out) > 0 {
				return out, nil
			}
			return nil, err
		}
		for _, item := range result.Items {
			if len(out) >= limit {
				break
			}
			// Recalled messages carry no original text, but "someone sent then
			// recalled a message" is itself context — keep a placeholder instead
			// of dropping the turn entirely.
			var content string
			if strings.Contains(item.Body.Content, "This message was recalled") || strings.Contains(item.Body.Content, "消息已被撤回") {
				content = "（消息已撤回）"
			} else {
				content = ExtractText(item.MsgType, item.Body.Content)
				if content == "" && item.MsgType == "image" {
					content = "发来了一张图片"
				}
				if content == "" {
					continue
				}
				content = CleanHistoryText(content)
				if content == "" {
					continue
				}
			}
			// Feishu identifies the bot's own messages as sender_type=app with
			// sender.id = the app_id; map those onto the bot's open_id so callers
			// can label them as the bot (小弟) instead of an unknown sender.
			senderID := item.Sender.ID
			if item.Sender.SenderType == "app" {
				senderID = c.botOpenID
			}
			out = append(out, Message{
				MessageID: item.MessageID,
				Time:      FormatTime(item.CreateTime),
				MsgType:   item.MsgType,
				Content:   content,
				ImageKey:  ExtractImageKey(item.MsgType, item.Body.Content),
				Sender:    senderID,
				IsOwner:   senderID == c.ownerOpenID,
				ChatID:    chatID,
			})
		}
		if !result.HasMore || result.PageToken == "" {
			break
		}
		pageToken = result.PageToken
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
		if msgType == "image" {
			content = "发来了一张图片"
		} else {
			content = "只叫了你一声"
		}
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
		MsgType:     msgType,
		Content:     content,
		ImageKey:    ExtractImageKey(msgType, derefStr(em.Content)),
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

func ExtractImageKey(msgType, content string) string {
	if msgType != "image" || content == "" {
		return ""
	}
	var m map[string]interface{}
	if err := json.Unmarshal([]byte(content), &m); err != nil {
		return ""
	}
	for _, key := range []string{"image_key", "imageKey", "key"} {
		if v, ok := m[key].(string); ok {
			return v
		}
	}
	if image, ok := m["image"].(map[string]interface{}); ok {
		for _, key := range []string{"image_key", "imageKey", "key"} {
			if v, ok := image[key].(string); ok {
				return v
			}
		}
	}
	return ""
}

func extractOCRTexts(data json.RawMessage) []string {
	var root interface{}
	if err := json.Unmarshal(data, &root); err != nil {
		return nil
	}
	var texts []string
	var walk func(interface{})
	walk = func(v interface{}) {
		switch x := v.(type) {
		case map[string]interface{}:
			for _, key := range []string{"text", "content", "words"} {
				if s, ok := x[key].(string); ok && strings.TrimSpace(s) != "" {
					texts = append(texts, strings.TrimSpace(s))
				}
			}
			for _, child := range x {
				walk(child)
			}
		case []interface{}:
			for _, child := range x {
				walk(child)
			}
		}
	}
	walk(root)
	if len(texts) == 0 {
		return nil
	}
	seen := make(map[string]bool, len(texts))
	uniq := make([]string, 0, len(texts))
	for _, text := range texts {
		if !seen[text] {
			seen[text] = true
			uniq = append(uniq, text)
		}
	}
	return uniq
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
