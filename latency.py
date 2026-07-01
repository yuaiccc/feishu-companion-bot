"""Small local latency traces for bot calls."""
from __future__ import annotations

import time
from contextlib import contextmanager


class LatencyTrace:
    """Record phase timings without sending data to an external observability service."""

    def __init__(self, name: str):
        self.name = name
        self.started_at = time.perf_counter()
        self.durations: dict[str, float] = {}
        self.marks: dict[str, float] = {}

    @contextmanager
    def span(self, key: str):
        started = time.perf_counter()
        try:
            yield
        finally:
            self.durations[key] = self.durations.get(key, 0.0) + time.perf_counter() - started

    def mark(self, key: str) -> None:
        self.marks[key] = time.perf_counter() - self.started_at

    def log(self) -> None:
        total = time.perf_counter() - self.started_at
        pieces = [f"total={_ms(total)}"]
        pieces.extend(f"{key}={_ms(value)}" for key, value in self.durations.items())
        pieces.extend(f"{key}_at={_ms(value)}" for key, value in self.marks.items())
        print(f"  [延迟] {self.name}: " + " ".join(pieces), flush=True)


def _ms(seconds: float) -> str:
    return f"{seconds * 1000:.0f}ms"
