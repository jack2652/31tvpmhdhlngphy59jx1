"""Database concern mixin extracted from the legacy Database facade."""
from __future__ import annotations
import logging
import sqlite3
import shutil
import time
from datetime import timedelta
from pathlib import Path
from typing import Any
from datetime import datetime, timezone


def utc_now() -> datetime:
    return datetime.now(timezone.utc)


def iso(value: datetime | None = None) -> str:
    return (value or utc_now()).isoformat()
from app.db_constants import (
    ANALYSIS_JOB_STALE_SECONDS, NO_FLOOR, ORPHANED_ANALYSIS_MESSAGE,
    SIZE_CLEANUP_PROTECT_HOURS, SIZE_CLEANUP_TARGET_RATIO, SIZE_CLEANUP_TIME_BUDGET_SECONDS,
)
logger = logging.getLogger("app.db")

class DatabaseCleanupMixin:
    def cleanup(self, retention_days: int, batch_size: int = 5000) -> dict[str, int]:
        cutoff = iso(utc_now() - timedelta(days=retention_days))
        deleted = {"quotes": 0, "options": 0, "runs": 0, "history": 0, "extremes": 0}
        with self.connect() as connection:
            for table, key in (
                ("quote_snapshots", "quotes"),
                ("option_snapshots", "options"),
                ("price_history", "history"),
                ("price_extremes", "extremes"),
            ):
                while True:
                    cursor = connection.execute(
                        # price_history / price_extremes 没有自增 id 列，统一按 rowid 分批删除。
                        f"DELETE FROM {table} WHERE rowid IN (SELECT rowid FROM {table} WHERE fetched_at < ? LIMIT ?)",
                        (cutoff, batch_size),
                    )
                    deleted[key] += cursor.rowcount
                    if cursor.rowcount < batch_size:
                        break
            while True:
                cursor = connection.execute(
                    "DELETE FROM refresh_runs WHERE id IN "
                    "(SELECT id FROM refresh_runs WHERE started_at < ? LIMIT ?)",
                    (cutoff, batch_size),
                )
                deleted["runs"] += cursor.rowcount
                if cursor.rowcount < batch_size:
                    break
            # 清理掉已没有对应快照的索引行，避免过期到期日继续出现在选择框中。
            connection.execute(
                """DELETE FROM option_latest_batches AS latest
                    WHERE NOT EXISTS (
                        SELECT 1 FROM option_snapshots AS current
                         WHERE current.symbol=latest.symbol
                           AND current.expiration=latest.expiration
                           AND current.fetched_at=latest.fetched_at
                    )"""
            )
        return deleted


    def database_size_bytes(self) -> int:
        """返回 SQLite 实际占用的磁盘字节数，包含 WAL/SHM 附属文件。"""
        total = 0
        for suffix in ("", "-wal", "-shm"):
            candidate = Path(f"{self.path}{suffix}")
            if candidate.exists():
                total += candidate.stat().st_size
        return total


    def live_size_bytes(self) -> int:
        """返回有效数据量（去掉空洞后的页数 × 页大小），等价于 VACUUM 之后的文件大小。

        删除行不会立刻缩小 SQLite 文件，清理阶梯必须按有效数据量判断是否已降到目标水位，
        否则会误判为「还没降下来」而继续删更深一层的数据。
        """
        with self.connect() as connection:
            page_count = connection.execute("PRAGMA page_count").fetchone()[0]
            free_pages = connection.execute("PRAGMA freelist_count").fetchone()[0]
            page_size = connection.execute("PRAGMA page_size").fetchone()[0]
        return (page_count - free_pages) * page_size


    def prune_legacy_raw_json(self, batch_size: int = 5000, time_budget_seconds: int = SIZE_CLEANUP_TIME_BUDGET_SECONDS) -> int:
        """清空旧版本写入的冗余报文列并回收文件体积，返回清空的行数。

        新版本已不再写入 raw_json，也没有任何读取方，因此启动时清一次即可（全表扫描只做一次）。
        """
        cleared = self._clear_raw_json(batch_size, time.monotonic() + time_budget_seconds)
        if cleared:
            self._vacuum_if_possible()
        return cleared


    def cleanup_by_size(
        self,
        max_bytes: int,
        batch_size: int = 5000,
        time_budget_seconds: int = SIZE_CLEANUP_TIME_BUDGET_SECONDS,
    ) -> dict[str, Any]:
        """按数据库体积上限清理历史数据，返回本次清理的统计。

        max_bytes <= 0 表示不限制体积，直接返回。清理按阶梯执行，每一步都先看有效数据量是否
        已经降到上限的 SIZE_CLEANUP_TARGET_RATIO，够用就停下：
        1. 清空 raw_json 冗余报文（该列只写不读，纯占空间）并删除过期刷新记录；
        2. 保护期之外的旧批次按「天」保留一批；
        3. 仍未达标时保护期内按「小时」保留一批；
        4. 继续超限才退到每个分组只保留最新一批；
        5. 磁盘空间允许时执行 VACUUM 回收文件体积，空间不足则跳过并告警。
        """
        before = self.database_size_bytes()
        result: dict[str, Any] = {
            "limit_bytes": max_bytes,
            "before_bytes": before,
            "after_bytes": before,
            "live_bytes": before,
            "raw_json_cleared": 0,
            "superseded_options": 0,
            "superseded_quotes": 0,
            "stale_runs": 0,
            "vacuumed": False,
        }
        if max_bytes <= 0 or before <= max_bytes:
            return result

        target = int(max_bytes * SIZE_CLEANUP_TARGET_RATIO)
        deadline = time.monotonic() + time_budget_seconds
        protect_after = iso(utc_now() - timedelta(hours=SIZE_CLEANUP_PROTECT_HOURS))

        # 有效数据量已经达标时说明只是文件还没回收（上一轮 VACUUM 可能因磁盘空间不足被跳过），
        # 此时不再删除业务数据，只等回收文件体积。
        if self.live_size_bytes() > target:
            # 冗余报文列只写不读，超限时直接清空，能省下大头且不丢任何被读取的数据。
            result["raw_json_cleared"] += self._clear_raw_json(batch_size, deadline)
            result["stale_runs"] += self._delete_stale_runs(protect_after, batch_size, deadline)
            # 三步阶梯，每步都比上一步删得狠，只有上一步没降到目标水位才继续：
            # 1. 保护期之外的旧批次按「天」保留一批（保留跨日的未平仓量样本）；
            # 2. 保护期之内按「小时」保留一批（盘前回退只需要近期有效样本）；
            # 3. 每个分组只保留最新一批。
            for floor, bucket_chars in ((protect_after, 10), (protect_after, 13), (NO_FLOOR, None)):
                if self.live_size_bytes() <= target or time.monotonic() >= deadline:
                    break
                result["superseded_options"] += self._thin_superseded(
                    "option_snapshots", ("symbol", "expiration"), floor, bucket_chars, batch_size, deadline
                )
                result["superseded_quotes"] += self._thin_superseded(
                    "quote_snapshots", ("symbol",), floor, bucket_chars, batch_size, deadline
                )

        changed = (
            result["raw_json_cleared"]
            + result["superseded_options"]
            + result["superseded_quotes"]
            + result["stale_runs"]
        )
        # 文件体积超限但有效数据已达标时也要回收，否则文件会一直挂在上限之上。
        if changed or before > max_bytes:
            result["vacuumed"] = self._vacuum_if_possible()
        result["after_bytes"] = self.database_size_bytes()
        result["live_bytes"] = self.live_size_bytes()

        if result["after_bytes"] > max_bytes:
            logger.warning(
                "数据库体积仍超过上限：%.1f MB / %.1f MB，已删除冗余快照 %s 行",
                result["after_bytes"] / 1048576,
                max_bytes / 1048576,
                result["superseded_options"] + result["superseded_quotes"],
            )
        else:
            logger.info(
                "数据库体积清理完成：%.1f MB → %.1f MB（上限 %.1f MB，VACUUM=%s）",
                before / 1048576,
                result["after_bytes"] / 1048576,
                max_bytes / 1048576,
                result["vacuumed"],
            )
        return result


    def _clear_raw_json(self, batch_size: int, deadline: float) -> int:
        """清空 raw_json 冗余报文，不删除任何业务字段（该列当前没有任何读取方）。"""
        cleared = 0
        with self.connect() as connection:
            for table in ("option_snapshots", "quote_snapshots"):
                while time.monotonic() < deadline:
                    cursor = connection.execute(
                        f"UPDATE {table} SET raw_json=NULL WHERE rowid IN "
                        f"(SELECT rowid FROM {table} WHERE raw_json IS NOT NULL LIMIT ?)",
                        (batch_size,),
                    )
                    cleared += cursor.rowcount
                    # 分批提交，避免一次性事务把 WAL 撑大（小磁盘环境尤其关键）。
                    connection.commit()
                    if cursor.rowcount < batch_size:
                        break
        return cleared


    def _thin_superseded(
        self,
        table: str,
        keys: tuple[str, ...],
        floor: str,
        bucket_chars: int | None,
        batch_size: int,
        deadline: float,
    ) -> int:
        """删除已被新批次覆盖的历史行，只保留每个分组的最新一批。

        bucket_chars 非空时额外保留「时间桶代表行」：按 fetched_at 的前 N 个字符分桶
        （10 表示按天、13 表示按小时），每个桶保留该分组的最新一行，用来支撑未平仓量回退；
        桶表只收录 floor 之后的记录，因此 floor 取 NO_FLOOR 时退化为每组只留最新一批。
        """
        keys_sql = ", ".join(keys)
        join_sql = " AND ".join(f"k.{key}=o.{key}" for key in keys)
        deleted = 0
        with self.connect() as connection:
            connection.execute("DROP TABLE IF EXISTS temp.keep_group")
            connection.execute(
                f"CREATE TEMP TABLE keep_group AS "
                f"SELECT {keys_sql}, MAX(fetched_at) AS newest FROM {table} GROUP BY {keys_sql}"
            )
            extra = ""
            if bucket_chars:
                bucket_join = " AND ".join(f"b.{key}=o.{key}" for key in keys)
                connection.execute("DROP TABLE IF EXISTS temp.keep_bucket")
                connection.execute(
                    f"CREATE TEMP TABLE keep_bucket AS "
                    f"SELECT {keys_sql}, substr(fetched_at, 1, {bucket_chars}) AS bucket, MAX(fetched_at) AS newest "
                    f"FROM {table} WHERE fetched_at >= ? GROUP BY {keys_sql}, bucket",
                    (floor,),
                )
                extra = (
                    f" AND NOT EXISTS (SELECT 1 FROM keep_bucket b WHERE {bucket_join} "
                    f"AND b.bucket=substr(o.fetched_at, 1, {bucket_chars}) AND b.newest=o.fetched_at)"
                )
            while time.monotonic() < deadline:
                cursor = connection.execute(
                    f"DELETE FROM {table} WHERE rowid IN ("
                    f"SELECT o.rowid FROM {table} o JOIN keep_group k ON {join_sql} "
                    f"WHERE o.fetched_at < k.newest AND o.fetched_at < ?{extra} "
                    f"ORDER BY o.fetched_at ASC, o.rowid ASC LIMIT ?)",
                    (floor, batch_size),
                )
                deleted += cursor.rowcount
                connection.commit()
                if cursor.rowcount < batch_size:
                    break
        return deleted


    def _delete_stale_runs(self, floor: str, batch_size: int, deadline: float) -> int:
        """删除过期的刷新记录，页面只读取每个标的最新一条。"""
        deleted = 0
        with self.connect() as connection:
            while time.monotonic() < deadline:
                cursor = connection.execute(
                    "DELETE FROM refresh_runs WHERE id IN "
                    "(SELECT id FROM refresh_runs WHERE started_at < ? LIMIT ?)",
                    (floor, batch_size),
                )
                deleted += cursor.rowcount
                connection.commit()
                if cursor.rowcount < batch_size:
                    break
        return deleted


    def _vacuum_if_possible(self) -> bool:
        """回收磁盘文件。VACUUM 需要与库体积相当的临时空间，空间不足时跳过并记录告警。

        WAL 模式下删除只会在 WAL 里留下可用页，主库文件要等 checkpoint 才会真正缩小，
        因此这里先做一次 checkpoint，VACUUM 之后再 checkpoint 一次把体积落盘。
        低内存机器通常也是小磁盘，VACUUM 的整库复制可能直接把容器写满或打爆内存。
        """
        if self.low_memory:
            self._passive_checkpoint()
            logger.info("低内存保护：跳过 VACUUM，仅做 PASSIVE checkpoint")
            return False
        size = self.path.stat().st_size if self.path.exists() else 0
        free = shutil.disk_usage(self.path.parent).free
        if free < size * 1.2:
            logger.warning(
                "跳过 VACUUM：磁盘剩余空间不足（需要约 %.1f MB，剩余 %.1f MB）",
                size * 1.2 / 1048576,
                free / 1048576,
            )
            return False
        # VACUUM 不能跑在事务里，因此单独建立自动提交连接。
        connection = sqlite3.connect(self.path, timeout=30, isolation_level=None)
        try:
            connection.execute("PRAGMA busy_timeout=30000")
            connection.execute("PRAGMA wal_checkpoint(TRUNCATE)")
            connection.execute("VACUUM")
            connection.execute("PRAGMA wal_checkpoint(TRUNCATE)")
        finally:
            connection.close()
        return True
