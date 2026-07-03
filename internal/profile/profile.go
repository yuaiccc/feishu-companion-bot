package profile

import (
	"encoding/json"
	"os"
	"path/filepath"
)

type Profile struct {
	ID          string                 `json:"id"`
	Name        string                 `json:"name"`
	BotRole     string                 `json:"bot_role"`
	BotName     string                 `json:"bot_name"`
	OwnerName   string                 `json:"owner_name"`
	TargetName  string                 `json:"target_name"`
	MemoryKeywords []string            `json:"memory_keywords"`
	Config      map[string]interface{} `json:"config"`
}

func Load(profileID string, profilesDir string) (*Profile, error) {
	path := filepath.Join(profilesDir, profileID+".json")
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var p Profile
	if err := json.Unmarshal(data, &p); err != nil {
		return nil, err
	}
	return &p, nil
}

func (p *Profile) BotRoleText() string {
	if p.BotRole != "" {
		return p.BotRole
	}
	return "飞书陪伴机器人小弟"
}

// OwnerDisplay returns the owner's display name, falling back to a generic
// "老板" so callers never emit an empty owner name in user-facing text.
func (p *Profile) OwnerDisplay() string {
	if p.OwnerName != "" {
		return p.OwnerName
	}
	return "老板"
}

// TargetDisplay returns the target's display name, or "" when no intimate
// target is configured. Callers use the empty result to gate target-specific
// behavior (intimate emojis, target-aware phrasing).
func (p *Profile) TargetDisplay() string {
	return p.TargetName
}

func (p *Profile) TargetAddressingHint() string {
	return ""
}
