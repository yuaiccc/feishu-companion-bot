"""本地应用检测模块：读取当前前台活跃应用（只读，不操作）。
重点看前台在用什么，而不是列出一堆后台打开的。
"""
import re
import subprocess


def get_frontmost_app() -> str:
    """获取当前最前台的应用名称。"""
    script = """
    tell application "System Events"
        set frontApp to name of first process whose frontmost is true
        return frontApp
    end tell
    """
    try:
        result = subprocess.run(
            ["osascript", "-e", script],
            capture_output=True, text=True, timeout=5,
        )
        if result.returncode == 0:
            return result.stdout.strip()
    except Exception:
        pass
    return ""


def get_frontmost_window_title() -> str:
    """获取当前前台应用的窗口标题（能看出具体在做什么）。"""
    script = """
    tell application "System Events"
        set frontProc to first process whose frontmost is true
        try
            set winTitle to title of front window of frontProc
            return winTitle
        on error
            return ""
        end try
    end tell
    """
    try:
        result = subprocess.run(
            ["osascript", "-e", script],
            capture_output=True, text=True, timeout=5,
        )
        if result.returncode == 0:
            return result.stdout.strip()
    except Exception:
        pass
    return ""


def get_recent_apps(n: int = 3) -> list[str]:
    """获取最近活跃的 n 个应用（按 AXRecentWindows 或窗口顺序）。
    AppleScript 无法直接获取"最近使用顺序"，这里用前台 + 可见窗口顶部的几个。
    """
    script = f"""
    tell application "System Events"
        set visibleProcs to {{}}
        repeat with proc in (every process whose background only is false and frontmost is false)
            try
                if (count of windows of proc) > 0 then
                    set end of visibleProcs to name of proc
                end if
            end try
        end repeat
        return visibleProcs
    end tell
    """
    try:
        result = subprocess.run(
            ["osascript", "-e", script],
            capture_output=True, text=True, timeout=10,
        )
        if result.returncode != 0:
            return []
        apps = [a.strip() for a in result.stdout.strip().split(",") if a.strip()]
        # 去掉系统应用
        system_apps = {"Finder", "Dock", "SystemUIServer", "ControlCenter",
                       "Spotlight", "WindowManager", "CoreServicesUIAgent"}
        apps = [a for a in apps if a not in system_apps]
        return apps[:n]
    except Exception:
        return []


def get_app_summary() -> str:
    """获取当前前台应用状态摘要，重点突出在做什么。
    返回格式: "正在用 Terminal（标题: xxx），旁边还开着 Claude, Feishu"
    """
    front = get_frontmost_app()
    win_title = get_frontmost_window_title()
    recent = get_recent_apps(3)

    if not front:
        return ""

    parts = [f"正在用 {front}"]
    if win_title:
        parts.append(f"（{win_title[:50]}）")
    if recent:
        parts.append(f"旁边还开着: {', '.join(recent)}")

    return "，".join(parts)


def get_idle_seconds() -> float | None:
    """Return keyboard/mouse idle seconds from IOHIDSystem."""
    try:
        result = subprocess.run(
            ["ioreg", "-c", "IOHIDSystem"],
            capture_output=True, text=True, timeout=5,
        )
        if result.returncode != 0:
            return None
        match = re.search(r'"HIDIdleTime"\s*=\s*(\d+)', result.stdout)
        if not match:
            return None
        return int(match.group(1)) / 1_000_000_000
    except Exception:
        return None


def is_screen_locked() -> bool | None:
    """Return whether the current console session is screen-locked."""
    try:
        result = subprocess.run(
            ["/usr/sbin/scutil"],
            input="show State:/Users/ConsoleUser\n",
            capture_output=True, text=True, timeout=5,
        )
        if result.returncode != 0:
            return None
        if "CGSSessionScreenIsLocked : TRUE" in result.stdout:
            return True
        if "CGSSessionScreenIsLocked : FALSE" in result.stdout:
            return False
        return False
    except Exception:
        return None


def get_presence_summary() -> str:
    """Summarize whether 三哥 is likely at the computer.

    This is an inference from local-only signals, not a certainty.
    """
    locked = is_screen_locked()
    idle = get_idle_seconds()

    if locked is True:
        return "电脑当前锁屏，三哥大概率不在电脑前"
    if idle is None:
        return "暂时看不到键鼠空闲时间，只能根据前台窗口粗略判断"

    idle_min = idle / 60
    if idle < 90:
        return f"键鼠刚刚有活动（空闲约 {int(idle)} 秒），三哥大概率在电脑前"
    if idle < 600:
        return f"键鼠有一会儿没动了（空闲约 {idle_min:.1f} 分钟），三哥可能在旁边或短暂离开"
    if idle < 1800:
        return f"键鼠较久没动（空闲约 {int(idle_min)} 分钟），三哥可能离开电脑了"
    return f"键鼠很久没动（空闲约 {int(idle_min)} 分钟），三哥大概率不在电脑前"


def get_local_status_summary() -> str:
    """Combine app/window state with presence inference."""
    app_summary = get_app_summary()
    presence = get_presence_summary()
    if app_summary and presence:
        return f"{presence}；{app_summary}"
    return app_summary or presence
