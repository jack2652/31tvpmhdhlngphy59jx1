"""并发控制原语：请求合并和上游请求限流。"""

from __future__ import annotations

from collections import OrderedDict
from contextlib import contextmanager
from dataclasses import dataclass
from threading import BoundedSemaphore, Event, Lock
from typing import Callable, Generic, Iterator, TypeVar


Key = TypeVar("Key")
Value = TypeVar("Value")


class UpstreamBusyError(RuntimeError):
    """等待上游请求槽位超时。"""


@dataclass
class _Flight(Generic[Value]):
    event: Event
    value: Value | None = None
    error: BaseException | None = None


class SingleFlight(Generic[Key, Value]):
    """同一个 key 同时只执行一次函数，其余调用者等待同一结果。"""

    def __init__(self):
        self._lock = Lock()
        self._flights: dict[Key, _Flight[Value]] = {}

    def do(self, key: Key, callback: Callable[[], Value]) -> Value:
        with self._lock:
            flight = self._flights.get(key)
            owner = flight is None
            if owner:
                flight = _Flight(Event())
                self._flights[key] = flight

        if not owner:
            flight.event.wait()
            if flight.error is not None:
                raise flight.error
            return flight.value  # type: ignore[return-value]

        try:
            value = callback()
        except BaseException as exc:
            with self._lock:
                flight.error = exc
                self._flights.pop(key, None)
                flight.event.set()
            raise
        with self._lock:
            flight.value = value
            self._flights.pop(key, None)
            flight.event.set()
        return value


class SingleFlightCache(Generic[Key, Value]):
    """有界 LRU 缓存加 single-flight，避免缓存失效瞬间的并发穿透。"""

    def __init__(self, maxsize: int = 32):
        self.maxsize = max(1, maxsize)
        self._cache: OrderedDict[Key, Value] = OrderedDict()
        self._cache_lock = Lock()
        self._flight: SingleFlight[Key, Value] = SingleFlight()

    def get_or_compute(self, key: Key, callback: Callable[[], Value]) -> Value:
        with self._cache_lock:
            cached = self._cache.pop(key, None)
            if cached is not None:
                self._cache[key] = cached
                return cached

        def compute_and_store() -> Value:
            value = callback()
            with self._cache_lock:
                self._cache.pop(key, None)
                self._cache[key] = value
                while len(self._cache) > self.maxsize:
                    self._cache.popitem(last=False)
            return value

        return self._flight.do(key, compute_and_store)


class UpstreamGate:
    """限制单进程同时访问行情源的数量，避免请求突发击穿供应商限流。"""

    def __init__(self, limit: int = 6, wait_seconds: float = 20.0):
        self.limit = max(1, int(limit))
        self.wait_seconds = max(0.1, float(wait_seconds))
        self._semaphore = BoundedSemaphore(self.limit)

    @contextmanager
    def slot(self) -> Iterator[None]:
        if not self._semaphore.acquire(timeout=self.wait_seconds):
            raise UpstreamBusyError(f"上游请求并发已满，等待 {self.wait_seconds:g} 秒后放弃")
        try:
            yield
        finally:
            self._semaphore.release()
