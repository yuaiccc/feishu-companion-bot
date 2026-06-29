"""本地应用检测模块：读取当前打开的应用列表（只读，不操作）。
用 AppleScript 获取前台应用和窗口列表。
"""
import subprocess
import plistlib


def get_running_apps() -> list[str]:
    """获取当前正在运行的可见应用列表（不含系统进程）。"""
    script = """
    tell application "System Events"
        set visibleApps to {}
        repeat with proc in (every process whose background only is false)
            set end of visibleApps to name of proc
        end repeat
        return visibleApps
    end tell
    """
    try:
        result = subprocess.run(
            ["osascript", "-e", script],
            capture_output=True, text=True, timeout=10,
        )
        if result.returncode != 0:
            return []
        # osascript 返回逗号分隔的列表
        apps = [a.strip() for a in result.stdout.strip().split(",") if a.strip()]
        return apps
    except Exception:
        return []


def get_frontmost_app() -> str:
    """获取当前最前台的 应用名称。"""
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


def get_app_summary() -> str:
    """获取当前应用状态的中文摘要，用于告诉舒舒三哥在干什么。"""
    front = get_frontmost_app()
    running = get_running_apps()

    if not running:
        return ""

    # 过滤掉 Finder 等系统应用，保留有意思的
    interesting = [a for a in running if a not in (
        "Finder", "Dock", "SystemUIServer", "ControlCenter",
        "Spotlight", "WindowManager", "CoreServicesUIAgent",
    )]

    summary_parts = []
    if front:
        summary_parts.append(f"正在用 {front}")
    if interesting:
        # 去重
        unique = list(dict.fromkeys(interesting))
        summary_parts.append(f"打开了: {', '.join(unique)}")

    return "，".join(summary_parts)
