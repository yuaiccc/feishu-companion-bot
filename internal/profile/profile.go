package profile

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

type Member struct {
	OpenID   string   `json:"open_id"`
	Name     string   `json:"name"`
	Role     string   `json:"role"`
	Relation string   `json:"relation"`
	Aliases  []string `json:"aliases"`
}

type Profile struct {
	ID             string                 `json:"id"`
	Name           string                 `json:"name"`
	BotRole        string                 `json:"bot_role"`
	BotName        string                 `json:"bot_name"`
	OwnerName      string                 `json:"owner_name"`
	TargetName     string                 `json:"target_name"`
	Members        []Member               `json:"members"`
	MemoryKeywords []string               `json:"memory_keywords"`
	Config         map[string]interface{} `json:"config"`
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

func (m Member) DisplayName() string {
	name := strings.TrimSpace(m.Name)
	if name != "" {
		return name
	}
	if len(m.Aliases) > 0 {
		return strings.TrimSpace(m.Aliases[0])
	}
	return "对方"
}

func (p *Profile) MemberByOpenID(openID string) (Member, bool) {
	openID = strings.TrimSpace(openID)
	if openID == "" {
		return Member{}, false
	}
	for _, member := range p.Members {
		if strings.TrimSpace(member.OpenID) == openID {
			return member, true
		}
	}
	return Member{}, false
}

func (p *Profile) MemberRole(openID string) string {
	member, ok := p.MemberByOpenID(openID)
	if !ok {
		return ""
	}
	return strings.ToLower(strings.TrimSpace(member.Role))
}

func (p *Profile) IdentityRoster() string {
	var lines []string
	if owner := p.OwnerDisplay(); owner != "" {
		lines = append(lines, fmt.Sprintf("- owner：%s", owner))
	}
	if target := p.TargetDisplay(); target != "" {
		lines = append(lines, fmt.Sprintf("- target：%s", target))
	}
	for _, member := range p.Members {
		name := member.DisplayName()
		role := strings.TrimSpace(member.Role)
		if name == "" || role == "" {
			continue
		}
		relation := strings.TrimSpace(member.Relation)
		if relation != "" {
			lines = append(lines, fmt.Sprintf("- %s：%s（%s）", role, name, relation))
		} else {
			lines = append(lines, fmt.Sprintf("- %s：%s", role, name))
		}
	}
	if len(lines) == 0 {
		return ""
	}
	return strings.Join(lines, "\n")
}
