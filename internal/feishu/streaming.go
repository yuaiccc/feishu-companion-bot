package feishu

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
)

// StreamingCardElementID is the fixed element_id of the markdown block that
// receives streamed text. CardKit updates text at the element level, so the
// element must carry a stable id.
const StreamingCardElementID = "reply_text"

// BuildStreamingCardJSON builds a Card 2.0 JSON string with streaming mode
// enabled. The card carries a single markdown element whose content is updated
// incrementally via StreamUpdateCardText. No action buttons: the streaming card
// is reply-only; memory confirmation still goes through the candidate card.
func BuildStreamingCardJSON(initialText string) string {
	card := map[string]interface{}{
		"schema": "2.0",
		"config": map[string]interface{}{
			"streaming_mode": true,
			"update_multi":   true,
			"summary":        map[string]string{"content": "[生成中...]"},
			"streaming_config": map[string]interface{}{
				"print_frequency_ms": map[string]int{"default": 70},
				"print_step":         map[string]int{"default": 1},
				"print_strategy":     "fast",
			},
		},
		"body": map[string]interface{}{
			"elements": []interface{}{
				map[string]interface{}{
					"tag":        "markdown",
					"content":    initialText,
					"element_id": StreamingCardElementID,
				},
			},
		},
	}
	return jsonMarshal(card)
}

// CreateStreamingCard creates a card entity with streaming mode on and returns
// its card_id. The card is not yet visible in any chat until SendCardEntity
// sends it. POST /cardkit/v1/cards.
func (c *Client) CreateStreamingCard(ctx context.Context, cardJSON string) (string, error) {
	data, err := c.do(ctx, "POST", "/cardkit/v1/cards", map[string]interface{}{
		"type": "card_json",
		"data": cardJSON,
	})
	if err != nil {
		return "", err
	}
	var result struct {
		CardID string `json:"card_id"`
	}
	if err := json.Unmarshal(data, &result); err != nil {
		return "", fmt.Errorf("parse card_id: %w", err)
	}
	if result.CardID == "" {
		return "", fmt.Errorf("empty card_id in response: %s", string(data))
	}
	return result.CardID, nil
}

// SendCardEntity sends an already-created card entity to a receive_id (chat_id
// by default) and returns the resulting message_id. The card entity can only be
// sent once. POST /im/v1/messages with msg_type=interactive referencing card_id.
func (c *Client) SendCardEntity(ctx context.Context, cardID, receiveID string) (string, error) {
	return c.sendMsgToIDType(ctx, receiveID, "interactive", map[string]interface{}{
		"type": "card",
		"data": map[string]string{"card_id": cardID},
	}, "chat_id")
}

// StreamUpdateCardText pushes the full accumulated text to the card's markdown
// element. Feishu renders a typewriter effect when the new text extends the old
// prefix; pass the full text each call, not a delta. sequence must start at 1
// and increment per update to preserve ordering.
// PUT /cardkit/v1/cards/:card_id/elements/:element_id/content
func (c *Client) StreamUpdateCardText(ctx context.Context, cardID, elementID, fullText string, sequence int) error {
	_, err := c.do(ctx, "PUT",
		fmt.Sprintf("/cardkit/v1/cards/%s/elements/%s/content", cardID, elementID),
		map[string]interface{}{
			"content":  fullText,
			"uuid":     fmt.Sprintf("stream_%d_%d", time.Now().UnixNano(), sequence),
			"sequence": sequence,
		})
	return err
}

// CloseStreamingCard turns streaming mode off so the card becomes a final,
// immutable message and the chat preview stops showing "[生成中...]".
// sequence must be greater than the last StreamUpdateCardText sequence.
// PATCH /cardkit/v1/cards/:card_id/settings.
func (c *Client) CloseStreamingCard(ctx context.Context, cardID string, sequence int) error {
	_, err := c.do(ctx, "PATCH", fmt.Sprintf("/cardkit/v1/cards/%s/settings", cardID),
		map[string]interface{}{
			"settings": jsonMarshal(map[string]interface{}{"config": map[string]interface{}{"streaming_mode": false}}),
			"uuid":     fmt.Sprintf("close_%d_%d", time.Now().UnixNano(), sequence),
			"sequence": sequence,
		})
	return err
}
