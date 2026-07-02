"""记忆维护入口。"""
import sys
from pathlib import Path

ROOT = Path(__file__).resolve().parents[2]
if str(ROOT) not in sys.path:
    sys.path.insert(0, str(ROOT))

from feishu_companion.app import run_mem_clean_mode


def main() -> None:
    run_mem_clean_mode(dry_run=True)


if __name__ == "__main__":
    main()
