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

func (p *Profile) TargetAddressingHint() string {
	return ""
}
