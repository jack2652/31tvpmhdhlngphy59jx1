"""SQLite 初始化、写入和查询。"""

from __future__ import annotations

import json
import logging
import shutil
import sqlite3
import time
from datetime import datetime, timedelta, timezone
from pathlib import Path
from typing import Any, Iterable


logger = logging.getLogger(__name__)

# 体积清理时保留的历史批次时长：未平仓量回退（_open_interest_fallback）只依赖这段时间内的旧批次。
SIZE_CLEANUP_PROTECT_HOURS = 24
# 体积清理目标水位相对上限的比例，留出余量，避免每次写入都触发一轮清理。
SIZE_CLEANUP_TARGET_RATIO = 0.8
# 体积清理的时间上限（秒）：跑在后台线程里，避免长时间占住数据库连接。
SIZE_CLEANUP_TIME_BUDGET_SECONDS = 120
# 表示「不设保护期」的哨兵时间戳，比任何写入时间都新，可复用同一套 SQL。
NO_FLOOR = "9999-12-31T23:59:59+00:00"


def utc_now() -> datetime:
    return datetime.now(timezone.utc)


def iso(value: datetime | None = None) -> str:
    return (value or utc_now()).isoformat()


def parse_sessions(raw: str | None) -> dict[str, Any]:
    """解析 quote_snapshots.sessions_json：历史快照缺字段或数据损坏时返回空字典。"""
    if not raw:
        return {}
    try:
        value = json.loads(raw)
    except ValueError:
        return {}
    return value if isinstance(value, dict) else {}


class Database:
    def __init__(self, path: Path | str):
        self.path = Path(path)
        self.path.parent.mkdir(parents=True, exist_ok=True)
        self.initialize()

    def connect(self) -> sqlite3.Connection:
        connection = sqlite3.connect(self.path, timeout=30)
        connection.row_factory = sqlite3.Row
        connection.execute("PRAGMA busy_timeout=30000")
        connection.execute("PRAGMA foreign_keys=ON")
        return connection

    def initialize(self) -> None:
        with self.connect() as connection:
            connection.execute("PRAGMA journal_mode=WAL")
            connection.executescript(
                """
                CREATE TABLE IF NOT EXISTS quote_snapshots (
                    id INTEGER PRIMARY KEY AUTOINCREMENT,
                    symbol TEXT NOT NULL,
                    fetched_at TEXT NOT NULL,
                    price REAL,
                    change_percent REAL,
                    currency TEXT,
                    market_state TEXT,
                    provider TEXT NOT NULL,
                    raw_json TEXT,
                    sessions_json TEXT
                );
                CREATE INDEX IF NOT EXISTS idx_quote_symbol_time
                    ON quote_snapshots(symbol, fetched_at DESC);

                CREATE TABLE IF NOT EXISTS option_snapshots (
                    id INTEGER PRIMARY KEY AUTOINCREMENT,
                    symbol TEXT NOT NULL,
                    expiration TEXT NOT NULL,
                    fetched_at TEXT NOT NULL,
                    contract_symbol TEXT NOT NULL,
                    contract_type TEXT NOT NULL,
                    strike REAL,
                    last_price REAL,
                    bid REAL,
                    ask REAL,
                    volume INTEGER,
                    open_interest INTEGER,
                    implied_volatility REAL,
                    gamma REAL,
                    in_the_money INTEGER,
                    change_percent REAL,
                    provider TEXT NOT NULL,
                    raw_json TEXT,
                    UNIQUE(symbol, expiration, fetched_at, contract_symbol)
                );
                CREATE INDEX IF NOT EXISTS idx_option_lookup
                    ON option_snapshots(symbol, expiration, fetched_at DESC);
                CREATE INDEX IF NOT EXISTS idx_option_symbol_time
                    ON option_snapshots(symbol, fetched_at DESC);

                CREATE TABLE IF NOT EXISTS refresh_runs (
                    id INTEGER PRIMARY KEY AUTOINCREMENT,
                    symbol TEXT NOT NULL,
                    started_at TEXT NOT NULL,
                    finished_at TEXT,
                    status TEXT NOT NULL,
                    rows_written INTEGER NOT NULL DEFAULT 0,
                    error_message TEXT
                );
                CREATE INDEX IF NOT EXISTS idx_refresh_symbol_time
                    ON refresh_runs(symbol, started_at DESC);

                CREATE TABLE IF NOT EXISTS price_history (
                    symbol TEXT NOT NULL,
                    bar_date TEXT NOT NULL,
                    open REAL,
                    high REAL,
                    low REAL,
                    close REAL,
                    volume REAL,
                    fetched_at TEXT NOT NULL,
                    PRIMARY KEY (symbol, bar_date)
                );

                CREATE TABLE IF NOT EXISTS price_extremes (
                    symbol TEXT PRIMARY KEY,
                    payload TEXT NOT NULL,
                    fetched_at TEXT NOT NULL
                );
                """
            )
            columns = {row[1] for row in connection.execute("PRAGMA table_info(option_snapshots)")}
            if "gamma" not in columns:
                connection.execute("ALTER TABLE option_snapshots ADD COLUMN gamma REAL")
            quote_columns = {row[1] for row in connection.execute("PRAGMA table_info(quote_snapshots)")}
            if "sessions_json" not in quote_columns:
                connection.execute("ALTER TABLE quote_snapshots ADD COLUMN sessions_json TEXT")

    def start_run(self, symbol: str) -> int:
        with self.connect() as connection:
            cursor = connection.execute(
                "INSERT INTO refresh_runs(symbol, started_at, status) VALUES (?, ?, ?)",
                (symbol, iso(), "running"),
            )
            return int(cursor.lastrowid)

    def finish_run(self, run_id: int, status: str, rows_written: int = 0, error_message: str | None = None) -> None:
        with self.connect() as connection:
            connection.execute(
                "UPDATE refresh_runs SET finished_at=?, status=?, rows_written=?, error_message=? WHERE id=?",
                (iso(), status, rows_written, error_message, run_id),
            )

    def write_snapshot(self, quote: dict[str, Any], options: Iterable[dict[str, Any]], fetched_at: str) -> int:
        # raw_json 目前没有任何读取方，为避免小磁盘环境被冗余 JSON 撑爆，写入时不再保存原始报文。
        option_rows = list(options)
        with self.connect() as connection:
            connection.execute(
                """INSERT INTO quote_snapshots
                (symbol, fetched_at, price, change_percent, currency, market_state, provider, sessions_json)
                VALUES (?, ?, ?, ?, ?, ?, ?, ?)""",
                (
                    quote["symbol"], fetched_at, quote.get("price"), quote.get("change_percent"),
                    quote.get("currency"), quote.get("market_state"), quote.get("provider", "yfinance"),
                    json.dumps(quote.get("sessions") or {}, ensure_ascii=True),
                ),
            )
            connection.executemany(
                """INSERT OR IGNORE INTO option_snapshots
                (symbol, expiration, fetched_at, contract_symbol, contract_type, strike, last_price,
                 bid, ask, volume, open_interest, implied_volatility, gamma, in_the_money, change_percent, provider)
                VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)""",
                [
                    (
                        row["symbol"], row["expiration"], fetched_at, row["contract_symbol"], row["contract_type"],
                        row.get("strike"), row.get("last_price"), row.get("bid"), row.get("ask"), row.get("volume"),
                        row.get("open_interest"), row.get("implied_volatility"), row.get("gamma"), int(bool(row.get("in_the_money"))),
                        row.get("change_percent"), row.get("provider", "yfinance"),
                    )
                    for row in option_rows
                ],
            )
        return len(option_rows)

    def latest_quote(self, symbol: str) -> dict[str, Any] | None:
        with self.connect() as connection:
            row = connection.execute(
                "SELECT * FROM quote_snapshots WHERE symbol=? ORDER BY fetched_at DESC LIMIT 1", (symbol,)
            ).fetchone()
        return dict(row) if row else None

    def latest_sessions(self, symbol: str, max_age_seconds: int = 86400) -> dict[str, Any]:
        """最近一次有效的盘前/盘后/夜盘数据，供最新快照缺少时段字段时回退。

        数据源限流、或旧版本进程仍在写入时，最新一行可能没有时段数据；
        限定回看窗口避免把很久以前的时段价格当成当前值展示。
        """
        cutoff = iso(utc_now() - timedelta(seconds=max_age_seconds))
        with self.connect() as connection:
            row = connection.execute(
                """SELECT sessions_json FROM quote_snapshots
                WHERE symbol=? AND fetched_at >= ? AND sessions_json IS NOT NULL AND sessions_json NOT IN ('', '{}')
                ORDER BY fetched_at DESC LIMIT 1""",
                (symbol, cutoff),
            ).fetchone()
        return parse_sessions(row["sessions_json"]) if row else {}

    def write_history(self, symbol: str, bars: Iterable[dict[str, Any]], fetched_at: str) -> int:
        """写入日线历史行情（同一标的、同一天覆盖更新）。"""
        rows = list(bars)
        with self.connect() as connection:
            connection.executemany(
                """INSERT OR REPLACE INTO price_history
                (symbol, bar_date, open, high, low, close, volume, fetched_at)
                VALUES (?, ?, ?, ?, ?, ?, ?, ?)""",
                [
                    (
                        symbol, bar["date"], bar.get("open"), bar.get("high"), bar.get("low"),
                        bar.get("close"), bar.get("volume"), fetched_at,
                    )
                    for bar in rows
                ],
            )
        return len(rows)

    def latest_history(self, symbol: str) -> dict[str, Any] | None:
        """返回本地缓存的日线序列（按日期升序）与最后一次抓取时间。"""
        with self.connect() as connection:
            rows = connection.execute(
                """SELECT bar_date, open, high, low, close, volume, fetched_at
                   FROM price_history WHERE symbol=? ORDER BY bar_date""",
                (symbol,),
            ).fetchall()
        if not rows:
            return None
        bars = [
            {
                "date": str(row["bar_date"]), "open": row["open"], "high": row["high"],
                "low": row["low"], "close": row["close"], "volume": row["volume"],
            }
            for row in rows
        ]
        return {"bars": bars, "fetched_at": max(str(row["fetched_at"]) for row in rows)}


    def write_extremes(self, symbol: str, payload: dict[str, Any], fetched_at: str) -> None:
        """写入日线极值缓存（52 周与历史高低点），同一标的覆盖更新。"""
        with self.connect() as connection:
            connection.execute(
                "INSERT OR REPLACE INTO price_extremes(symbol, payload, fetched_at) VALUES (?, ?, ?)",
                (symbol, json.dumps(payload, ensure_ascii=False), fetched_at),
            )

    def latest_extremes(self, symbol: str) -> dict[str, Any] | None:
        """返回本地缓存的日线极值与抓取时间；缓存损坏时按无缓存处理。"""
        with self.connect() as connection:
            row = connection.execute(
                "SELECT payload, fetched_at FROM price_extremes WHERE symbol=?", (symbol,)
            ).fetchone()
        if not row:
            return None
        try:
            payload = json.loads(row["payload"])
        except (TypeError, ValueError):
            return None
        if not isinstance(payload, dict):
            return None
        return {"extremes": payload, "fetched_at": str(row["fetched_at"])}

    def latest_expirations(self, symbol: str) -> list[str]:
        with self.connect() as connection:
            rows = connection.execute(
                "SELECT DISTINCT expiration FROM option_snapshots WHERE symbol=? ORDER BY expiration", (symbol,)
            ).fetchall()
        return [str(row[0]) for row in rows]

    def latest_chain(self, symbol: str, expiration: str) -> dict[str, Any]:
        with self.connect() as connection:
            timestamp_row = connection.execute(
                "SELECT MAX(fetched_at) FROM option_snapshots WHERE symbol=? AND expiration=?",
                (symbol, expiration),
            ).fetchone()
            fetched_at = timestamp_row[0] if timestamp_row else None
            if not fetched_at:
                return {"fetched_at": None, "data": [], "oi_fallback": {"restored": 0, "as_of": None}}
            rows = connection.execute(
                """SELECT contract_symbol, contract_type, strike, last_price, bid, ask, volume,
                   open_interest, implied_volatility, gamma, in_the_money, change_percent
                   FROM option_snapshots WHERE symbol=? AND expiration=? AND fetched_at=?
                   ORDER BY strike, contract_type""",
                (symbol, expiration, fetched_at),
            ).fetchall()
            fallback = self._open_interest_fallback(connection, symbol, expiration, expiration)
        data = [{**dict(row), "expiration": expiration} for row in rows]
        return {"fetched_at": fetched_at, "data": data, "oi_fallback": self._apply_open_interest_fallback(data, fallback)}

    @staticmethod
    def _open_interest_fallback(connection: sqlite3.Connection, symbol: str, start: str, end: str) -> dict[tuple[str, str], tuple[int, str]]:
        """读取每个合约最近一次非零的未平仓量，供盘前空值兜底使用。"""
        rows = connection.execute(
            """SELECT expiration, contract_symbol, open_interest, MAX(fetched_at) AS fetched_at
                 FROM option_snapshots
                WHERE symbol=? AND expiration BETWEEN ? AND ? AND COALESCE(open_interest, 0) > 0
                GROUP BY expiration, contract_symbol""",
            (symbol, start, end),
        ).fetchall()
        # SQLite 在 GROUP BY 中搭配 MAX() 时，裸列取自最大值所在的那一行，因此这里拿到的是最近一次有效的未平仓量。
        return {(str(row["expiration"]), str(row["contract_symbol"])): (int(row["open_interest"]), str(row["fetched_at"])) for row in rows}

    @staticmethod
    def _apply_open_interest_fallback(rows: list[dict[str, Any]], fallback: dict[tuple[str, str], tuple[int, str]]) -> dict[str, Any]:
        """把为 0 的未平仓量替换成同一合约最近一次有效值，并返回回溯统计。"""
        restored = 0
        as_of: str | None = None
        for row in rows:
            if row.get("open_interest"):
                continue
            entry = fallback.get((str(row.get("expiration")), str(row.get("contract_symbol"))))
            if entry is None:
                continue
            row["open_interest"] = entry[0]
            restored += 1
            if as_of is None or entry[1] > as_of:
                as_of = entry[1]
        return {"restored": restored, "as_of": as_of}

    def latest_chains(self, symbol: str, horizon_days: int = 45) -> dict[str, Any]:
        """返回近期期限的最新期权链，用于跨到期日 Gamma 分析。"""
        start = utc_now().date().isoformat()
        end = (utc_now().date() + timedelta(days=horizon_days)).isoformat()
        with self.connect() as connection:
            rows = connection.execute(
                """SELECT contract_symbol, expiration, contract_type, strike, last_price, bid, ask,
                          volume, open_interest, implied_volatility, gamma, in_the_money, change_percent,
                          fetched_at
                   FROM option_snapshots AS current
                  WHERE symbol=? AND expiration BETWEEN ? AND ?
                    AND fetched_at=(
                        SELECT MAX(latest.fetched_at) FROM option_snapshots AS latest
                         WHERE latest.symbol=current.symbol
                           AND latest.expiration=current.expiration
                    )
                  ORDER BY expiration, strike, contract_type""",
                (symbol, start, end),
            ).fetchall()
            fallback = self._open_interest_fallback(connection, symbol, start, end)
        data = [dict(row) for row in rows]
        expirations = sorted({str(row["expiration"]) for row in data})
        fetched_values = [row["fetched_at"] for row in data if row.get("fetched_at")]
        oi_fallback = self._apply_open_interest_fallback(data, fallback)
        for row in data:
            row.pop("fetched_at", None)
        return {
            "fetched_at": max(fetched_values) if fetched_values else None,
            "data": data,
            "expirations": expirations,
            "horizon_days": horizon_days,
            "oi_fallback": oi_fallback,
        }

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
        """
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

    def latest_status(self, symbol: str) -> dict[str, Any] | None:
        with self.connect() as connection:
            row = connection.execute(
                "SELECT * FROM refresh_runs WHERE symbol=? ORDER BY started_at DESC LIMIT 1", (symbol,)
            ).fetchone()
        return dict(row) if row else None
