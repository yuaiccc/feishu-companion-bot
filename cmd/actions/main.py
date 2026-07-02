"""新版 GitHub Actions 兜底入口。"""
import sys
from pathlib import Path

ROOT = Path(__file__).resolve().parents[2]
if str(ROOT) not in sys.path:
    sys.path.insert(0, str(ROOT))

from feishu_companion.actions_app import main


if __name__ == "__main__":
    main()
