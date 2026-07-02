package localapps

import (
	"context"
	"os/exec"
	"strings"
	"time"
)

// Status represents the local machine's active window and idle state.
type Status struct {
	WindowTitle   string
	AppName       string
	IsLocked      bool
	KeyboardIdle  time.Duration
	MouseIdle     time.Duration
	LastActivity  time.Time
}

// Reader reads local application/window status via AppleScript.
type Reader struct{}

func NewReader() *Reader {
	return &Reader{}
}

// GetStatus returns current foreground window and input idle time.
func (r *Reader) GetStatus() (*Status, error) {
	script := `
tell application "System Events"
    set frontApp to front application
    set appName to name of frontApp
end tell

try
    set winTitle to name of front window of front application
on error
    set winTitle to ""
end try

try
    set isLocked to (do shell script "defaults read /var/tmp/com.apple.screensaver.locked 2>/dev/null")
on error
    set isLocked to "0"
end try

return appName & "||" & winTitle & "||" & isLocked
`
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	out, err := exec.CommandContext(ctx, "osascript", "-e", script).Output()
	if err != nil {
		return nil, err
	}

	parts := strings.Split(strings.TrimSpace(string(out)), "||")
	appName := ""
	isLocked := false
	if len(parts) >= 1 {
		appName = strings.TrimSpace(parts[0])
	}
	if len(parts) >= 3 {
		isLocked = strings.TrimSpace(parts[2]) == "1"
	}

	return &Status{
		AppName:  appName,
		IsLocked: isLocked,
	}, nil
}

// InterpretStatus converts raw status into a human-readable one-liner.
func InterpretStatus(s *Status) string {
	if s.IsLocked {
		return "屏幕已锁定"
	}
	if s.AppName == "" {
		return "未知状态"
	}

	switch s.AppName {
	case "Terminal":
		return "正在用 Terminal"
	case "Code":
		return "正在用 VS Code"
	case "Safari", "Google Chrome", "Chromium":
		return "正在用浏览器"
	case "WeChat", "微信":
		return "正在用微信"
	case "Feishu", "Lark":
		return "正在用飞书"
	case "Messages", "iMessage":
		return "正在用 iMessage"
	case "Finder":
		return "正在用 Finder"
	}

	if s.WindowTitle != "" {
		return s.AppName + " - " + s.WindowTitle
	}
	return "正在用 " + s.AppName
}
