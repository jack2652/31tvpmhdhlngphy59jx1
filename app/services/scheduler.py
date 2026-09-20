"""定时刷新和历史清理任务。"""

from __future__ import annotations

import asyncio
import logging
from uuid import uuid4
from collections.abc import Awaitable, Callable
from typing import Any

from app.config import Settings
from app.services.snapshots import SnapshotService
from app.db import Database

logger = logging.getLogger(__name__)


class Scheduler:
    def __init__(self, settings: Settings, snapshots: SnapshotService, database: Database):
        self.settings = settings
        self.snapshots = snapshots
        self.database = database
        self._task: asyncio.Task[None] | None = None
        self._stop = asyncio.Event()
        self._owner = uuid4().hex
        self._lease_name = "option-snapshot-scheduler"
        self._lease_seconds = max(120, settings.refresh_interval_seconds * 3)

    async def start(self) -> None:
        if self.settings.scheduler_enabled and self._task is None:
            acquired = await asyncio.to_thread(
                self.database.try_acquire_lease,
                self._lease_name,
                self._owner,
                self._lease_seconds,
            )
            if not acquired:
                logger.info("当前 worker 不持有后台调度租约，跳过重复调度")
                return
            self._stop.clear()
            self._task = asyncio.create_task(self._loop(), name="option-snapshot-scheduler")

    async def stop(self) -> None:
        self._stop.set()
        if self._task:
            # 调度协程可能因为历史异常提前结束，这里显式取回结果，避免关闭流程被异常打断
            await asyncio.gather(self._task, return_exceptions=True)
            self._task = None
            await asyncio.to_thread(self.database.release_lease, self._lease_name, self._owner)

    async def _guard(self, step: Callable[[], Awaitable[Any]], description: str) -> None:
        """执行单个调度步骤并吞掉异常。

        调度循环一旦因为清理或刷新抛错而退出，进程还活着、/health 也照常返回，
        但页面数据会一直停在旧快照上（表现为「数据不再刷新」），排查成本极高，
        因此每个步骤单独兜底：只记录日志，循环继续。
        """
        try:
            await step()
        except asyncio.CancelledError:
            raise
        except Exception as exc:  # noqa: BLE001 - 调度必须容错，异常只记录不中断
            logger.warning("%s 失败：%s", description, exc)

    async def _loop(self) -> None:
        await self._guard(self._prune_legacy_raw_json, "启动清理冗余报文")
        await self._guard(self._refresh_once, "定时刷新")
        await self._guard(self._cleanup_by_size, "体积清理")
        elapsed = 0
        while not self._stop.is_set():
            lease_alive = await asyncio.to_thread(
                self.database.try_acquire_lease,
                self._lease_name,
                self._owner,
                self._lease_seconds,
            )
            if not lease_alive:
                logger.warning("后台调度租约已被其他 worker 接管，当前调度退出")
                break
            interval = self.settings.cleanup_interval_seconds if elapsed >= self.settings.cleanup_interval_seconds else self.settings.refresh_interval_seconds
            try:
                await asyncio.wait_for(self._stop.wait(), timeout=interval)
            except asyncio.TimeoutError:
                if elapsed >= self.settings.cleanup_interval_seconds:
                    await self._guard(self._cleanup_expired, "历史数据定时清理")
                    elapsed = 0
                else:
                    await self._guard(self._refresh_once, "定时刷新")
                    elapsed += self.settings.refresh_interval_seconds
                await self._guard(self._cleanup_by_size, "体积清理")

    async def _cleanup_expired(self) -> None:
        """按保留天数删除过期历史数据。"""
        await asyncio.to_thread(self.database.cleanup, self.settings.raw_retention_days)

    async def _refresh_once(self) -> None:
        await asyncio.to_thread(self.snapshots.refresh_default, self.settings.default_symbols)

    async def _prune_legacy_raw_json(self) -> None:
        """启动时清空旧版本写入的冗余报文列，一次回收几十 MB 死数据。"""
        cleared = await asyncio.to_thread(self.database.prune_legacy_raw_json)
        if cleared:
            logger.info("启动清理：已清空 %s 行历史冗余报文（raw_json）并回收文件体积", cleared)

    async def _cleanup_by_size(self) -> None:
        """按体积上限清理数据库。未配置上限时只读取文件大小，开销可忽略。"""
        max_bytes = self.settings.database_max_mb * 1048576
        if max_bytes <= 0:
            return
        await asyncio.to_thread(self.database.cleanup_by_size, max_bytes)
