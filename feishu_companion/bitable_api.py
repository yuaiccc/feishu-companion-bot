"""飞书多维表格模块：创建并维护 GitHub 活动记录表。
- 首次运行自动创建多维表格应用和数据表
- app_token / table_id 持久化到 bitable_state.json，后续复用
- 每次 check_github 有新活动时，批量写入记录
"""
import json
import os
import requests
from datetime import datetime, timezone, timedelta

import lark_oapi as lark
from lark_oapi.api.bitable.v1 import (
    CreateAppRequest, ReqApp,
    CreateAppTableRequest, CreateAppTableRequestBody, AppTable,
    CreateAppTableFieldRequest, AppTableField, AppTableFieldDescription,
    BatchCreateAppTableRecordRequest, BatchCreateAppTableRecordRequestBody,
    AppTableRecord,
)

from feishu_companion.config import FEISHU_APP_ID, FEISHU_APP_SECRET, MEMORY_DIR

_OPEN_API_BASE = "https://open.feishu.cn/open-apis"
_STATE_FILE = str(MEMORY_DIR / "bitable_state.json")
_SHANGHAI = timezone(timedelta(hours=8))

# Bitable 字段类型常量
TYPE_TEXT = 1       # 多行文本
TYPE_NUMBER = 2     # 数字
TYPE_DATETIME = 5   # 日期时间

# 表格字段定义: (字段名, 类型)
_FIELDS = [
    ("时间", TYPE_DATETIME),
    ("仓库", TYPE_TEXT),
    ("项目介绍", TYPE_TEXT),
    ("活动类型", TYPE_TEXT),
    ("活动详情", TYPE_TEXT),
    ("分支", TYPE_TEXT),
]


def _get_token() -> str:
    """获取 tenant_access_token。"""
    resp = requests.post(
        f"{_OPEN_API_BASE}/auth/v3/tenant_access_token/internal",
        json={"app_id": FEISHU_APP_ID, "app_secret": FEISHU_APP_SECRET},
        timeout=30,
    )
    return resp.json()["tenant_access_token"]


def _get_client() -> lark.Client:
    return (
        lark.Client.builder()
        .app_id(FEISHU_APP_ID)
        .app_secret(FEISHU_APP_SECRET)
        .build()
    )


def _load_state() -> dict:
    if os.path.exists(_STATE_FILE):
        try:
            with open(_STATE_FILE, "r") as f:
                return json.load(f)
        except (json.JSONDecodeError, IOError):
            pass
    return {}


def _save_state(state: dict):
    os.makedirs(os.path.dirname(_STATE_FILE), exist_ok=True)
    with open(_STATE_FILE, "w") as f:
        json.dump(state, f, ensure_ascii=False, indent=2)


def ensure_bitable() -> tuple[str, str]:
    """确保多维表格已创建，返回 (app_token, table_id)。
    首次调用会创建应用+表+字段，后续从 state 文件读取。
    """
    state = _load_state()
    app_token = state.get("app_token", "")
    table_id = state.get("table_id", "")

    if app_token and table_id:
        return app_token, table_id

    client = _get_client()

    # 1. 创建多维表格应用
    if not app_token:
        print("  [bitable] 创建多维表格应用...", flush=True)
        req = (
            CreateAppRequest.builder()
            .request_body(ReqApp.builder().name("GitHub活动记录").time_zone("Asia/Shanghai").build())
            .build()
        )
        resp = client.bitable.v1.app.create(req)
        if not resp.success():
            raise RuntimeError(f"创建多维表格失败: code={resp.code} msg={resp.msg}")
        app_token = resp.data.app.app_token
        url = resp.data.app.url
        print(f"  [bitable] 应用创建成功: {url}", flush=True)
        state["app_token"] = app_token
        state["app_url"] = url
        _save_state(state)

    # 2. 创建数据表（带字段）
    if not table_id:
        print("  [bitable] 创建数据表...", flush=True)
        table = AppTable.builder().name("活动记录").build()
        body = CreateAppTableRequestBody.builder().table(table).build()
        req = (
            CreateAppTableRequest.builder()
            .app_token(app_token)
            .request_body(body)
            .build()
        )
        resp = client.bitable.v1.app_table.create(req)
        if not resp.success():
            raise RuntimeError(f"创建数据表失败: code={resp.code} msg={resp.msg}")
        table_id = resp.data.table_id
        print(f"  [bitable] 数据表创建成功: table_id={table_id}", flush=True)
        state["table_id"] = table_id
        _save_state(state)

        # 3. 创建字段（第一个字段是主列，创建表时自动生成一个"文本"主列，我们改它的名字）
        # 先创建其余字段
        for i, (name, ftype) in enumerate(_FIELDS):
            if i == 0:
                # 主列：用更新字段的方式改名（或者直接用默认的）
                # 默认主列已经是文本类型，我们跳过，后面用搜索来更新
                continue
            try:
                field = (
                    AppTableField.builder()
                    .field_name(name)
                    .type(ftype)
                    .build()
                )
                req = (
                    CreateAppTableFieldRequest.builder()
                    .app_token(app_token)
                    .table_id(table_id)
                    .request_body(field)
                    .build()
                )
                resp = client.bitable.v1.app_table_field.create(req)
                if resp.success():
                    print(f"  [bitable] 字段创建: {name}", flush=True)
                else:
                    print(f"  [bitable] 字段创建失败 {name}: {resp.msg}", flush=True)
            except Exception as e:
                print(f"  [bitable] 字段创建异常 {name}: {e}", flush=True)

        # 改主列名（第一个字段默认叫"文本"，改成"时间"）
        # 用 HTTP API 直接改
        try:
            token = _get_token()
            # 先列出字段拿到主列 field_id
            resp = requests.get(
                f"{_OPEN_API_BASE}/bitable/v1/apps/{app_token}/tables/{table_id}/fields",
                headers={"Authorization": f"Bearer {token}"},
                timeout=30,
            )
            fields = resp.json().get("data", {}).get("items", [])
            if fields:
                primary_field_id = fields[0]["field_id"]
                # 更新主列名
                requests.put(
                    f"{_OPEN_API_BASE}/bitable/v1/apps/{app_token}/tables/{table_id}/fields/{primary_field_id}",
                    headers={"Authorization": f"Bearer {token}", "Content-Type": "application/json"},
                    json={"field_name": _FIELDS[0][0], "type": _FIELDS[0][1]},
                    timeout=30,
                )
                print(f"  [bitable] 主列改名: {_FIELDS[0][0]}", flush=True)
        except Exception as e:
            print(f"  [bitable] 改主列名失败: {e}", flush=True)

    return app_token, table_id


def _to_timestamp(iso_str: str) -> int:
    """ISO 时间转毫秒时间戳（Bitable 日期字段需要）。"""
    try:
        dt = datetime.fromisoformat(iso_str.replace("Z", "+00:00"))
        return int(dt.timestamp() * 1000)
    except Exception:
        return int(datetime.now().timestamp() * 1000)


def add_records(activities: list[dict]) -> int:
    """把活动列表批量写入多维表格。返回写入的记录数。"""
    if not activities:
        return 0

    try:
        app_token, table_id = ensure_bitable()
    except Exception as e:
        print(f"  [bitable] 获取表格失败: {e}", flush=True)
        return 0

    # 构建记录
    records = []
    for a in activities:
        detail = a.get("detail", {})
        atype = a.get("type", "")
        repo = a.get("repo", "")

        # 活动详情
        if atype == "PushEvent":
            msgs = detail.get("commit_messages", [])
            count = detail.get("commit_count", len(msgs) if msgs else 1)
            brief = "; ".join(m.strip().split("\n")[0][:40] for m in msgs) if msgs else ""
            activity_detail = f"提交 {count} 次" + (f": {brief}" if brief else "")
        elif atype == "WatchEvent":
            activity_detail = "Star 收藏"
        elif atype == "PullRequestEvent":
            activity_detail = f"{detail.get('action', '')} PR: {detail.get('title', '')}"
        elif atype == "CreateEvent":
            activity_detail = f"创建 {detail.get('ref_type', '')} {detail.get('ref', '')}"
        else:
            activity_detail = atype

        record = AppTableRecord.builder().fields({
            "时间": _to_timestamp(a.get("created_at", "")),
            "仓库": repo.split("/")[-1] if "/" in repo else repo,
            "项目介绍": _get_repo_desc(repo),
            "活动类型": atype,
            "活动详情": activity_detail,
            "分支": detail.get("branch", ""),
        }).build()
        records.append(record)

    if not records:
        return 0

    client = _get_client()
    body = BatchCreateAppTableRecordRequestBody.builder().records(records).build()
    req = (
        BatchCreateAppTableRecordRequest.builder()
        .app_token(app_token)
        .table_id(table_id)
        .request_body(body)
        .build()
    )
    resp = client.bitable.v1.app_table_record.batch_create(req)
    if resp.success():
        print(f"  [bitable] 写入 {len(records)} 条记录", flush=True)
        return len(records)
    else:
        print(f"  [bitable] 写入失败: code={resp.code} msg={resp.msg}", flush=True)
        return 0


# ---- Repo 描述缓存 ----
_repo_desc_cache: dict[str, str] = {}


def _get_repo_desc(repo: str) -> str:
    """获取仓库描述，带内存缓存。"""
    if not repo:
        return ""
    if repo in _repo_desc_cache:
        return _repo_desc_cache[repo]

    from feishu_companion.config import GITHUB_TOKEN
    try:
        resp = requests.get(
            f"https://api.github.com/repos/{repo}",
            headers={"Authorization": f"token {GITHUB_TOKEN}"},
            timeout=10,
        )
        desc = resp.json().get("description", "") or ""
        lang = resp.json().get("language", "") or ""
        result = f"{desc}"
        if lang:
            result = f"[{lang}] {desc}" if desc else f"[{lang}]"
        _repo_desc_cache[repo] = result
        return result
    except Exception:
        _repo_desc_cache[repo] = ""
        return ""


def get_repo_desc_cached(repo: str) -> str:
    """公开接口：获取仓库描述（带缓存）。供 notifier.py 调用。"""
    return _get_repo_desc(repo)
