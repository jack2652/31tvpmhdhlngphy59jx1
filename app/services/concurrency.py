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


class HeavyWorkBusyError(RuntimeError):
    """已有会占用大量内存的刷新或计算，本次不再叠加。"""


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


class HeavyWorkGate:
    """限制同时进行的重内存操作。

    期权链、全量日线和 Gamma 窗口叠在一起时，256MB 的机器会直接被杀掉。
    槽位在归还内存之后才释放，避免下一次刷新叠在尚未还给系统的碎片上。
    """

    def __init__(self, limit: int = 1):
        self.limit = max(1, int(limit))
        self._semaphore = BoundedSemaphore(self.limit)

    def acquire(self, timeout: float = 0.0) -> bool:
        return self._semaphore.acquire(timeout=max(0.0, float(timeout)))

    def release(self) -> None:
        # 先回收，再放行下一个重任务。busy() 不能走这里，否则探测本身会触发回收。
        from app.runtime import low_memory_enabled, release_memory

        if low_memory_enabled():
            release_memory()
        self._semaphore.release()

    def busy(self) -> bool:
        if not self._semaphore.acquire(blocking=False):
            return True
        self._semaphore.release()
        return False


_heavy_gate: HeavyWorkGate | None = None
_heavy_gate_lock = Lock()


def get_heavy_gate() -> HeavyWorkGate:
    """进程内共享的重任务闸门。低内存时只允许一个，其余环境保留有限并行。"""
    global _heavy_gate
    with _heavy_gate_lock:
        if _heavy_gate is None:
            from app.runtime import low_memory_enabled

            _heavy_gate = HeavyWorkGate(1 if low_memory_enabled() else 4)
        return _heavy_gate


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
