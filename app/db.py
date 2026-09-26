"""SQLite 初始化、写入和查询。"""

from __future__ import annotations

import json
import logging
import shutil
import sqlite3
import time
from datetime import date, datetime, timedelta, timezone
from pathlib import Path
from typing import Any, Iterable

from app.runtime import low_memory_enabled


logger = logging.getLogger(__name__)

# 体积清理时保留的历史批次时长：未平仓量回退（_open_interest_fallback）只依赖这段时间内的旧批次。
SIZE_CLEANUP_PROTECT_HOURS = 24
# 体积清理目标水位相对上限的比例，留出余量，避免每次写入都触发一轮清理。
SIZE_CLEANUP_TARGET_RATIO = 0.8
# 体积清理的时间上限（秒）：跑在后台线程里，避免长时间占住数据库连接。
SIZE_CLEANUP_TIME_BUDGET_SECONDS = 120
# 表示「不设保护期」的哨兵时间戳，比任何写入时间都新，可复用同一套 SQL。
NO_FLOOR = "9999-12-31T23:59:59+00:00"
# 后台分析停在 running 超过这个时间，下一轮可以重新领取。进程还活着时由接口侧的看门狗先标记失败。
ANALYSIS_JOB_STALE_SECONDS = 180
ORPHANED_ANALYSIS_MESSAGE = "进程重启，后台分析已中断"



def expiration_dates(values: Iterable[Any]) -> list[str]:
    """保留合法的 YYYY-MM-DD，并按原顺序去重。"""
    cleaned: list[str] = []
    seen: set[str] = set()
    for item in values:
        if not isinstance(item, str) or item in seen:
            continue
        try:
            date.fromisoformat(item)
        except ValueError:
            continue
        seen.add(item)
        cleaned.append(item)
    return cleaned


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
        # 启动时定下来，避免每条 SQL 都再读一次 cgroup。
        self.low_memory = low_memory_enabled()
        self.initialize()

    def connect(self) -> sqlite3.Connection:
        connection = sqlite3.connect(self.path, timeout=30)
        connection.row_factory = sqlite3.Row
        connection.execute("PRAGMA busy_timeout=30000")
        connection.execute("PRAGMA foreign_keys=ON")
        if self.low_memory:
            # 页缓存按 KiB 计，-256 是 256KiB。关掉 mmap，临时表落盘，WAL 更早合并。
            connection.execute("PRAGMA cache_size=-256")
            connection.execute("PRAGMA mmap_size=0")
            connection.execute("PRAGMA temp_store=FILE")
            connection.execute("PRAGMA wal_autocheckpoint=200")
        return connection

    def _passive_checkpoint(self) -> None:
        """把 WAL 合并回主库，但不为了缩小文件再复制一整份数据库。"""
        connection = sqlite3.connect(self.path, timeout=30, isolation_level=None)
        try:
            connection.execute("PRAGMA busy_timeout=30000")
            connection.execute("PRAGMA wal_checkpoint(PASSIVE)")
        except sqlite3.Error:
            logger.warning("PASSIVE checkpoint 失败", exc_info=True)
        finally:
            connection.close()

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
                    sessions_json TEXT,
                    today_open REAL,
                    previous_close REAL
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
                CREATE INDEX IF NOT EXISTS idx_option_oi_fallback
                    ON option_snapshots(symbol, expiration, contract_symbol, fetched_at DESC)
                    WHERE open_interest > 0;

                CREATE TABLE IF NOT EXISTS option_latest_batches (
                    symbol TEXT NOT NULL,
                    expiration TEXT NOT NULL,
                    fetched_at TEXT NOT NULL,
                    PRIMARY KEY (symbol, expiration)
                );
                CREATE INDEX IF NOT EXISTS idx_option_latest_expiration
                    ON option_latest_batches(symbol, expiration);

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

                CREATE TABLE IF NOT EXISTS beta_snapshots (
                    symbol TEXT PRIMARY KEY,
                    payload TEXT NOT NULL,
                    fetched_at TEXT NOT NULL
                );

                CREATE TABLE IF NOT EXISTS earnings_snapshots (
                    symbol TEXT PRIMARY KEY,
                    payload TEXT NOT NULL,
                    fetched_at TEXT NOT NULL
                );

                CREATE TABLE IF NOT EXISTS expiration_catalog (
                    symbol TEXT PRIMARY KEY,
                    payload TEXT NOT NULL,
                    fetched_at TEXT NOT NULL
                );

                CREATE TABLE IF NOT EXISTS analysis_refresh_jobs (
                    job_id TEXT PRIMARY KEY,
                    symbol TEXT NOT NULL,
                    horizon_days INTEGER NOT NULL,
                    status TEXT NOT NULL,
                    started_at TEXT NOT NULL,
                    finished_at TEXT,
                    result_json TEXT,
                    error_message TEXT
                );
                CREATE INDEX IF NOT EXISTS idx_analysis_jobs_status
                    ON analysis_refresh_jobs(status, started_at DESC);

                CREATE TABLE IF NOT EXISTS service_leases (
                    name TEXT PRIMARY KEY,
                    owner TEXT NOT NULL,
                    acquired_at TEXT NOT NULL
                );

                CREATE TABLE IF NOT EXISTS api_analysis_cache (
                    cache_key TEXT PRIMARY KEY,
                    payload TEXT NOT NULL,
                    created_at TEXT NOT NULL
                );
                CREATE INDEX IF NOT EXISTS idx_api_analysis_cache_time
                    ON api_analysis_cache(created_at ASC);
                """
            )
            columns = {row[1] for row in connection.execute("PRAGMA table_info(option_snapshots)")}
            if "gamma" not in columns:
                connection.execute("ALTER TABLE option_snapshots ADD COLUMN gamma REAL")
            quote_columns = {row[1] for row in connection.execute("PRAGMA table_info(quote_snapshots)")}
            if "sessions_json" not in quote_columns:
                connection.execute("ALTER TABLE quote_snapshots ADD COLUMN sessions_json TEXT")
            if "today_open" not in quote_columns:
                connection.execute("ALTER TABLE quote_snapshots ADD COLUMN today_open REAL")
            if "previous_close" not in quote_columns:
                connection.execute("ALTER TABLE quote_snapshots ADD COLUMN previous_close REAL")
            # 旧库首次升级时建立每个到期日的最新批次索引，后续写入由 write_snapshot 增量维护。
            if connection.execute("SELECT COUNT(*) FROM option_latest_batches").fetchone()[0] == 0:
                connection.execute(
                    """INSERT INTO option_latest_batches(symbol, expiration, fetched_at)
                       SELECT symbol, expiration, MAX(fetched_at)
                         FROM option_snapshots
                        GROUP BY symbol, expiration"""
                )

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

    @staticmethod
    def _age_seconds(fetched_at: str | None) -> float | None:
        if not fetched_at:
            return None
        try:
            moment = datetime.fromisoformat(fetched_at)
        except ValueError:
            return None
        if moment.tzinfo is None:
            moment = moment.replace(tzinfo=timezone.utc)
        return max((utc_now() - moment.astimezone(timezone.utc)).total_seconds(), 0.0)

    def claim_analysis_job(
        self,
        symbol: str,
        horizon_days: int,
        cooldown_seconds: int = 30,
        stale_seconds: int = ANALYSIS_JOB_STALE_SECONDS,
    ) -> dict[str, Any]:
        """跨进程领取 Gamma 后台任务；同一标的窗口只允许一个 worker 执行。"""
        job_id = f"{symbol}:{horizon_days}"
        now = iso()
        with self.connect() as connection:
            connection.execute("BEGIN IMMEDIATE")
            row = connection.execute(
                "SELECT * FROM analysis_refresh_jobs WHERE job_id=?", (job_id,)
            ).fetchone()
            if row:
                current = dict(row)
                running_age = self._age_seconds(current.get("started_at"))
                finished_age = self._age_seconds(current.get("finished_at"))
                if current["status"] == "running" and running_age is not None and running_age < stale_seconds:
                    return {"job_id": job_id, "symbol": symbol, "horizon_days": horizon_days, "status": "running", "started_at": current.get("started_at"), "claimed": False}
                if current["status"] in {"completed", "failed"} and finished_age is not None and finished_age < cooldown_seconds:
                    return {"job_id": job_id, "symbol": symbol, "horizon_days": horizon_days, "status": current["status"], "finished_at": current.get("finished_at"), "claimed": False}
            connection.execute(
                """INSERT INTO analysis_refresh_jobs
                   (job_id, symbol, horizon_days, status, started_at, finished_at, result_json, error_message)
                   VALUES (?, ?, ?, 'running', ?, NULL, NULL, NULL)
                   ON CONFLICT(job_id) DO UPDATE SET
                       symbol=excluded.symbol,
                       horizon_days=excluded.horizon_days,
                       status='running',
                       started_at=excluded.started_at,
                       finished_at=NULL,
                       result_json=NULL,
                       error_message=NULL""",
                (job_id, symbol, horizon_days, now),
            )
        return {
            "job_id": job_id,
            "symbol": symbol,
            "horizon_days": horizon_days,
            "status": "running",
            "started_at": now,
            "claimed": True,
        }

    def finish_analysis_job(
        self,
        job_id: str,
        status: str,
        result: dict[str, Any] | None,
        error_message: str | None,
        started_at: str,
    ) -> bool:
        """只结束这一轮 running。晚到的线程带旧 started_at 时不能覆盖新领取的任务。"""
        with self.connect() as connection:
            cursor = connection.execute(
                """UPDATE analysis_refresh_jobs
                      SET status=?, finished_at=?, result_json=?, error_message=?
                    WHERE job_id=? AND status='running' AND started_at=?""",
                (
                    status,
                    iso(),
                    json.dumps(result, ensure_ascii=False) if result is not None else None,
                    error_message,
                    job_id,
                    started_at,
                ),
            )
            return cursor.rowcount > 0

    def fail_orphaned_analysis_jobs(self, error_message: str = ORPHANED_ANALYSIS_MESSAGE) -> int:
        """进程重启后，上一轮还停在 running 的分析不会再有线程写回结果。"""
        with self.connect() as connection:
            cursor = connection.execute(
                """UPDATE analysis_refresh_jobs
                      SET status='failed', finished_at=?, error_message=?
                    WHERE status='running'""",
                (iso(), error_message),
            )
            return int(cursor.rowcount or 0)

    def analysis_job(self, symbol: str, horizon_days: int) -> dict[str, Any] | None:
        job_id = f"{symbol}:{horizon_days}"
        with self.connect() as connection:
            row = connection.execute(
                "SELECT * FROM analysis_refresh_jobs WHERE job_id=?", (job_id,)
            ).fetchone()
        if not row:
            return None
        payload = dict(row)
        raw_result = payload.pop("result_json", None)
        if raw_result:
            try:
                payload["result"] = json.loads(raw_result)
            except (TypeError, ValueError):
                payload["result"] = None
        else:
            payload["result"] = None
        return payload

    def try_acquire_lease(self, name: str, owner: str, lease_seconds: int = 120) -> bool:
        """用 SQLite 短事务选出多 worker 下唯一的后台调度者。"""
        now = iso()
        with self.connect() as connection:
            connection.execute("BEGIN IMMEDIATE")
            row = connection.execute(
                "SELECT owner, acquired_at FROM service_leases WHERE name=?", (name,)
            ).fetchone()
            if row and row["owner"] != owner:
                age = self._age_seconds(row["acquired_at"])
                if age is not None and age < lease_seconds:
                    return False
            connection.execute(
                """INSERT INTO service_leases(name, owner, acquired_at) VALUES (?, ?, ?)
                   ON CONFLICT(name) DO UPDATE SET owner=excluded.owner, acquired_at=excluded.acquired_at""",
                (name, owner, now),
            )
        return True

    def release_lease(self, name: str, owner: str) -> None:
        with self.connect() as connection:
            connection.execute("DELETE FROM service_leases WHERE name=? AND owner=?", (name, owner))

    def get_analysis_cache(self, cache_key: str) -> Any | None:
        with self.connect() as connection:
            row = connection.execute(
                "SELECT payload FROM api_analysis_cache WHERE cache_key=?", (cache_key,)
            ).fetchone()
        if not row:
            return None
        try:
            return json.loads(row["payload"])
        except (TypeError, ValueError):
            return None

    def put_analysis_cache(self, cache_key: str, payload: Any, max_entries: int = 128) -> None:
        encoded = json.dumps(payload, ensure_ascii=False, separators=(",", ":"))
        with self.connect() as connection:
            connection.execute(
                """INSERT INTO api_analysis_cache(cache_key, payload, created_at)
                   VALUES (?, ?, ?)
                   ON CONFLICT(cache_key) DO UPDATE SET payload=excluded.payload, created_at=excluded.created_at""",
                (cache_key, encoded, iso()),
            )
            connection.execute(
                """DELETE FROM api_analysis_cache
                    WHERE cache_key IN (
                        SELECT cache_key FROM api_analysis_cache
                         ORDER BY created_at DESC
                         LIMIT -1 OFFSET ?
                    )""",
                (max(1, max_entries),),
            )

    def write_snapshot(self, quote: dict[str, Any], options: Iterable[dict[str, Any]], fetched_at: str) -> int:
        # raw_json 目前没有任何读取方，为避免小磁盘环境被冗余 JSON 撑爆，写入时不再保存原始报文。
        option_rows = list(options)
        with self.connect() as connection:
            connection.execute(
                """INSERT INTO quote_snapshots
                (symbol, fetched_at, price, change_percent, currency, market_state, provider, sessions_json, today_open, previous_close)
                VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)""",
                (
                    quote["symbol"], fetched_at, quote.get("price"), quote.get("change_percent"),
                    quote.get("currency"), quote.get("market_state"), quote.get("provider", "upstream"),
                    json.dumps(quote.get("sessions") or {}, ensure_ascii=True),
                    quote.get("today_open"), quote.get("previous_close"),
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
                        row.get("change_percent"), row.get("provider", "upstream"),
                    )
                    for row in option_rows
                ],
            )
            connection.executemany(
                """INSERT INTO option_latest_batches(symbol, expiration, fetched_at)
                   VALUES (?, ?, ?)
                   ON CONFLICT(symbol, expiration) DO UPDATE SET fetched_at=excluded.fetched_at
                   WHERE excluded.fetched_at >= option_latest_batches.fetched_at""",
                sorted({(row["symbol"], row["expiration"], fetched_at) for row in option_rows}),
            )
        if self.low_memory:
            self._passive_checkpoint()
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

    def write_beta(self, symbol: str, payload: dict[str, Any], fetched_at: str) -> None:
        """写入按标的缓存的 Beta 结果，避免每次趋势面板刷新都请求两年行情。"""
        with self.connect() as connection:
            connection.execute(
                "INSERT OR REPLACE INTO beta_snapshots(symbol, payload, fetched_at) VALUES (?, ?, ?)",
                (symbol, json.dumps(payload, ensure_ascii=False), fetched_at),
            )

    def latest_beta(self, symbol: str) -> dict[str, Any] | None:
        """返回 Beta 缓存；缓存损坏时按无缓存处理。"""
        with self.connect() as connection:
            row = connection.execute(
                "SELECT payload, fetched_at FROM beta_snapshots WHERE symbol=?", (symbol,)
            ).fetchone()
        if not row:
            return None
        try:
            payload = json.loads(row["payload"])
        except (TypeError, ValueError):
            return None
        if not isinstance(payload, dict):
            return None
        return {"beta": payload, "fetched_at": str(row["fetched_at"])}

    def write_earnings(self, symbol: str, payload: dict[str, Any], fetched_at: str) -> None:
        """只缓存财报日期。是否落在交易日窗口内由请求时计算，不写进这份缓存。"""
        with self.connect() as connection:
            connection.execute(
                "INSERT OR REPLACE INTO earnings_snapshots(symbol, payload, fetched_at) VALUES (?, ?, ?)",
                (symbol, json.dumps(payload, ensure_ascii=False), fetched_at),
            )

    def latest_earnings(self, symbol: str) -> dict[str, Any] | None:
        """返回财报日期缓存；缓存损坏或日期不是列表时按无缓存处理。"""
        with self.connect() as connection:
            row = connection.execute(
                "SELECT payload, fetched_at FROM earnings_snapshots WHERE symbol=?", (symbol,)
            ).fetchone()
        if not row:
            return None
        try:
            payload = json.loads(row["payload"])
        except (TypeError, ValueError):
            return None
        if not isinstance(payload, dict) or not isinstance(payload.get("dates"), list):
            return None
        return {"dates": [str(item) for item in payload["dates"]], "fetched_at": str(row["fetched_at"])}

    def write_expiration_catalog(self, symbol: str, expirations: list[str], fetched_at: str) -> None:
        """缓存供应商返回的全部到期日。下拉框不能只依赖已经落库的期权链。"""
        cleaned = expiration_dates(expirations)
        if not cleaned:
            return
        with self.connect() as connection:
            connection.execute(
                "INSERT OR REPLACE INTO expiration_catalog(symbol, payload, fetched_at) VALUES (?, ?, ?)",
                (symbol, json.dumps({"expirations": cleaned}, ensure_ascii=False), fetched_at),
            )

    def latest_expiration_catalog(self, symbol: str) -> list[str]:
        """返回已缓存的全部到期日；没有缓存或内容损坏时返回空列表。"""
        with self.connect() as connection:
            row = connection.execute(
                "SELECT payload FROM expiration_catalog WHERE symbol=?", (symbol,)
            ).fetchone()
        if not row:
            return []
        try:
            payload = json.loads(row["payload"])
        except (TypeError, ValueError):
            return []
        if not isinstance(payload, dict):
            return []
        return expiration_dates(payload.get("expirations") or [])

    def latest_expirations(self, symbol: str) -> list[str]:
        with self.connect() as connection:
            rows = connection.execute(
                "SELECT expiration FROM option_latest_batches WHERE symbol=? ORDER BY expiration", (symbol,)
            ).fetchall()
        return [str(row[0]) for row in rows]

    def latest_chain(self, symbol: str, expiration: str) -> dict[str, Any]:
        with self.connect() as connection:
            timestamp_row = connection.execute(
                "SELECT fetched_at FROM option_latest_batches WHERE symbol=? AND expiration=?",
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
                WHERE symbol=? AND expiration BETWEEN ? AND ? AND open_interest > 0
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
                """SELECT current.contract_symbol, current.expiration, current.contract_type,
                          current.strike, current.last_price, current.bid, current.ask,
                          current.volume, current.open_interest, current.implied_volatility,
                          current.gamma, current.in_the_money, current.change_percent,
                          current.fetched_at
                     FROM option_latest_batches AS latest
                     JOIN option_snapshots AS current
                       ON current.symbol=latest.symbol
                      AND current.expiration=latest.expiration
                      AND latest.fetched_at=current.fetched_at
                    WHERE latest.symbol=? AND latest.expiration BETWEEN ? AND ?
                   ORDER BY current.expiration, current.strike, current.contract_type""",
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

    def latest_status(self, symbol: str) -> dict[str, Any] | None:
        with self.connect() as connection:
            row = connection.execute(
                "SELECT * FROM refresh_runs WHERE symbol=? ORDER BY started_at DESC LIMIT 1", (symbol,)
            ).fetchone()
        return dict(row) if row else None
