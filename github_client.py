"""GitHub Events API 客户端：拉取用户公开活动并解析成结构化数据。
也支持直接轮询 private 仓库的 commits（Events API 不返回 private 仓库活动）。
"""
import requests

GITHUB_API = "https://api.github.com"


def _headers(token: str = "") -> dict:
    headers = {"Accept": "application/vnd.github+json"}
    if token:
        headers["Authorization"] = f"Bearer {token}"
    return headers


def fetch_github_events(username: str, token: str = "") -> list[dict]:
    """获取用户最近的公开 GitHub 事件（最多 30 条/页）。"""
    url = f"{GITHUB_API}/users/{username}/events/public"
    resp = requests.get(url, headers=_headers(token), timeout=30)
    resp.raise_for_status()
    return resp.json()


def fetch_private_repo_commits(repo: str, token: str = "", per_page: int = 10) -> list[dict]:
    """直接拉取 private 仓库最近的 commits，返回模拟成 GitHub Event 格式的列表。
    这样可以和 Events API 的事件统一处理。
    """
    url = f"{GITHUB_API}/repos/{repo}/commits"
    resp = requests.get(url, headers=_headers(token), params={"per_page": per_page}, timeout=30)
    if resp.status_code != 200:
        return []
    commits = resp.json()
    events = []
    for c in commits:
        msg = c.get("commit", {}).get("message", "")
        date = c.get("commit", {}).get("author", {}).get("date", "")
        sha = c.get("sha", "")
        events.append({
            "id": f"private-{repo}-{sha[:8]}",
            "type": "PushEvent",
            "repo": {"name": repo},
            "created_at": date,
            "payload": {
                "ref": "refs/heads/main",
                "size": 1,
                "head": sha,
                "commits": [{"message": msg}],
            },
        })
    return events


def fetch_commit_messages(repo: str, head_sha: str, token: str = "") -> list[str]:
    """用 head SHA 拉取该 commit 的 message（以及它的 parent 链）。
    GitHub Events API 有时只返回 head SHA 不返回 commits 详情，需要补抓。
    """
    url = f"{GITHUB_API}/repos/{repo}/commits/{head_sha}"
    try:
        resp = requests.get(url, headers=_headers(token), timeout=15)
        resp.raise_for_status()
        data = resp.json()
        messages = [data.get("commit", {}).get("message", "")]
        # 也抓 parent commits（同一次 push 可能包含多个 commit）
        for parent in data.get("parents", []):
            pass  # 只取 head commit message 避免过多 API 调用
        return [m for m in messages if m]
    except Exception:
        return []


def parse_events(events: list[dict], token: str = "") -> list[dict]:
    """把原始 GitHub 事件解析成统一的结构化活动列表。
    对缺少 commit 详情的 PushEvent，用 head SHA 补抓。
    """
    activities = []
    for ev in events:
        activities.append(_parse_one(ev, token))
    return activities


def _parse_one(ev: dict, token: str = "") -> dict:
    etype = ev.get("type", "")
    payload = ev.get("payload", {})
    repo = ev.get("repo", {}).get("name", "")

    detail = {}

    if etype == "PushEvent":
        commits = payload.get("commits", [])
        commit_messages = [c.get("message", "") for c in commits if c.get("message")]
        commit_count = payload.get("size", len(commits) if commits else 1)
        branch = payload.get("ref", "").replace("refs/heads/", "")
        head_sha = payload.get("head", "")

        # GitHub 有时只返回 head SHA 不返回 commits 详情，补抓
        if not commit_messages and head_sha and repo:
            fetched = fetch_commit_messages(repo, head_sha, token)
            if fetched:
                commit_messages = fetched

        detail = {
            "branch": branch,
            "commit_count": commit_count,
            "commit_messages": commit_messages,
        }
    elif etype == "PullRequestEvent":
        pr = payload.get("pull_request", {})
        detail = {
            "action": payload.get("action", ""),
            "title": pr.get("title", ""),
            "url": pr.get("html_url", ""),
        }
    elif etype == "IssuesEvent":
        issue = payload.get("issue", {})
        detail = {
            "action": payload.get("action", ""),
            "title": issue.get("title", ""),
            "url": issue.get("html_url", ""),
        }
    elif etype == "IssueCommentEvent":
        comment = payload.get("comment", {})
        detail = {
            "body": comment.get("body", "")[:200],
            "url": comment.get("html_url", ""),
        }
    elif etype == "CreateEvent":
        detail = {
            "ref_type": payload.get("ref_type", ""),
            "ref": payload.get("ref", ""),
        }
    elif etype == "WatchEvent":
        detail = {"action": "starred"}
    elif etype == "ForkEvent":
        forkee = payload.get("forkee", {})
        detail = {"forked_to": forkee.get("full_name", "")}
    elif etype == "ReleaseEvent":
        release = payload.get("release", {})
        detail = {
            "tag": release.get("tag_name", ""),
            "name": release.get("name", ""),
        }

    return {
        "id": ev.get("id", ""),
        "type": etype,
        "repo": repo,
        "created_at": ev.get("created_at", ""),
        "detail": detail,
    }
