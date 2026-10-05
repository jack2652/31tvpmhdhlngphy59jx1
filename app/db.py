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
from zoneinfo import ZoneInfo

from app.runtime import low_memory_enabled
from app.db_constants import (ANALYSIS_JOB_STALE_SECONDS, NO_FLOOR, ORPHANED_ANALYSIS_MESSAGE, SIZE_CLEANUP_PROTECT_HOURS, SIZE_CLEANUP_TARGET_RATIO, SIZE_CLEANUP_TIME_BUDGET_SECONDS)
from app.db_cleanup import DatabaseCleanupMixin
from app.db_options import DatabaseOptionsMixin


logger = logging.getLogger(__name__)

# 体积清理时保留的历史批次时长：未平仓量回退（_open_interest_fallback）只依赖这段时间内的旧批次。
# 体积清理目标水位相对上限的比例，留出余量，避免每次写入都触发一轮清理。
# 体积清理的时间上限（秒）：跑在后台线程里，避免长时间占住数据库连接。
# 表示「不设保护期」的哨兵时间戳，比任何写入时间都新，可复用同一套 SQL。
# 后台分析停在 running 超过这个时间，下一轮可以重新领取。进程还活着时由接口侧的看门狗先标记失败。
MARKET_TIMEZONE = ZoneInfo("America/New_York")
MAX_PUSH_REFRESH_BACKOFF_SECONDS = 300



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


class Database(DatabaseOptionsMixin, DatabaseCleanupMixin):
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
                    previous_close REAL,
                    fair_value REAL,
                    fair_value_low REAL,
                    fair_value_high REAL,
                    fair_value_buy_low REAL,
                    fair_value_buy_high REAL,
                    fair_value_source TEXT,
                    fair_value_model TEXT,
                    fair_value_forward_eps REAL,
                    fair_value_forward_eps_source TEXT,
                    fair_value_safety_margin REAL,
                    fair_value_confidence TEXT
                    ,fair_value_confidence_score REAL
                    ,fair_value_interest_coverage REAL
                    ,fair_value_regime TEXT
                    ,fair_value_regime_signals_json TEXT
                    ,fair_value_model_under_regime TEXT
                    ,fair_value_defensive_json TEXT
                    ,fair_value_optimistic_json TEXT
                    ,fair_value_normalized_eps_source TEXT
                    ,fair_value_quarterly_momentum_json TEXT
                    ,fair_value_historical_valuation_percentiles_json TEXT
                    ,fair_value_shareholder_total_return_yield REAL
                    ,fair_value_market_cap_data_quality TEXT
                    ,fair_value_data_quality_score REAL
                    ,fair_value_owner_earnings_maintenance_ratio REAL
                    ,fair_value_owner_earnings_ratio_source TEXT
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
                CREATE TABLE IF NOT EXISTS push_events (
                    id INTEGER PRIMARY KEY AUTOINCREMENT,
                    symbol TEXT NOT NULL,
                    kind TEXT NOT NULL,
                    expiration TEXT,
                    created_at TEXT NOT NULL
                );
                CREATE INDEX IF NOT EXISTS idx_push_events_id ON push_events(id);
                CREATE TABLE IF NOT EXISTS push_refresh_state (
                    symbol TEXT NOT NULL,
                    expiration TEXT NOT NULL DEFAULT '',
                    started_at TEXT NOT NULL,
                    retry_after TEXT,
                    retry_delay_seconds INTEGER NOT NULL DEFAULT 0,
                    last_interval_seconds INTEGER NOT NULL DEFAULT 0,
                    lease_until TEXT,
                    PRIMARY KEY(symbol, expiration)
                );
                CREATE INDEX IF NOT EXISTS idx_api_analysis_cache_time
                    ON api_analysis_cache(created_at ASC);
                """
            )
            push_event_columns = {row[1] for row in connection.execute("PRAGMA table_info(push_events)")}
            if "expiration" not in push_event_columns:
                connection.execute("ALTER TABLE push_events ADD COLUMN expiration TEXT")
            push_refresh_columns = {row[1] for row in connection.execute("PRAGMA table_info(push_refresh_state)")}
            if "retry_after" not in push_refresh_columns:
                connection.execute("ALTER TABLE push_refresh_state ADD COLUMN retry_after TEXT")
            if "retry_delay_seconds" not in push_refresh_columns:
                connection.execute("ALTER TABLE push_refresh_state ADD COLUMN retry_delay_seconds INTEGER NOT NULL DEFAULT 0")
            if "last_interval_seconds" not in push_refresh_columns:
                connection.execute("ALTER TABLE push_refresh_state ADD COLUMN last_interval_seconds INTEGER NOT NULL DEFAULT 0")
            if "lease_until" not in push_refresh_columns:
                connection.execute("ALTER TABLE push_refresh_state ADD COLUMN lease_until TEXT")
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
            if "fair_value" not in quote_columns:
                connection.execute("ALTER TABLE quote_snapshots ADD COLUMN fair_value REAL")
            if "fair_value_low" not in quote_columns:
                connection.execute("ALTER TABLE quote_snapshots ADD COLUMN fair_value_low REAL")
            if "fair_value_high" not in quote_columns:
                connection.execute("ALTER TABLE quote_snapshots ADD COLUMN fair_value_high REAL")
            if "fair_value_buy_low" not in quote_columns:
                connection.execute("ALTER TABLE quote_snapshots ADD COLUMN fair_value_buy_low REAL")
            if "fair_value_buy_high" not in quote_columns:
                connection.execute("ALTER TABLE quote_snapshots ADD COLUMN fair_value_buy_high REAL")
            if "fair_value_source" not in quote_columns:
                connection.execute("ALTER TABLE quote_snapshots ADD COLUMN fair_value_source TEXT")
            if "fair_value_model" not in quote_columns:
                connection.execute("ALTER TABLE quote_snapshots ADD COLUMN fair_value_model TEXT")
            if "fair_value_forward_eps" not in quote_columns:
                connection.execute("ALTER TABLE quote_snapshots ADD COLUMN fair_value_forward_eps REAL")
            if "fair_value_forward_eps_source" not in quote_columns:
                connection.execute("ALTER TABLE quote_snapshots ADD COLUMN fair_value_forward_eps_source TEXT")
            if "fair_value_safety_margin" not in quote_columns:
                connection.execute("ALTER TABLE quote_snapshots ADD COLUMN fair_value_safety_margin REAL")
            if "fair_value_confidence" not in quote_columns:
                connection.execute("ALTER TABLE quote_snapshots ADD COLUMN fair_value_confidence TEXT")
            if "fair_value_confidence_score" not in quote_columns:
                connection.execute("ALTER TABLE quote_snapshots ADD COLUMN fair_value_confidence_score REAL")
            if "fair_value_interest_coverage" not in quote_columns:
                connection.execute("ALTER TABLE quote_snapshots ADD COLUMN fair_value_interest_coverage REAL")
            if "fair_value_regime" not in quote_columns:
                connection.execute("ALTER TABLE quote_snapshots ADD COLUMN fair_value_regime TEXT")
            if "fair_value_regime_signals_json" not in quote_columns:
                connection.execute("ALTER TABLE quote_snapshots ADD COLUMN fair_value_regime_signals_json TEXT")
            if "fair_value_model_under_regime" not in quote_columns:
                connection.execute("ALTER TABLE quote_snapshots ADD COLUMN fair_value_model_under_regime TEXT")
            if "fair_value_defensive_json" not in quote_columns:
                connection.execute("ALTER TABLE quote_snapshots ADD COLUMN fair_value_defensive_json TEXT")
            if "fair_value_optimistic_json" not in quote_columns:
                connection.execute("ALTER TABLE quote_snapshots ADD COLUMN fair_value_optimistic_json TEXT")
            if "fair_value_normalized_eps_source" not in quote_columns:
                connection.execute("ALTER TABLE quote_snapshots ADD COLUMN fair_value_normalized_eps_source TEXT")
            if "fair_value_quarterly_momentum_json" not in quote_columns:
                connection.execute("ALTER TABLE quote_snapshots ADD COLUMN fair_value_quarterly_momentum_json TEXT")
            if "fair_value_historical_valuation_percentiles_json" not in quote_columns:
                connection.execute("ALTER TABLE quote_snapshots ADD COLUMN fair_value_historical_valuation_percentiles_json TEXT")
            if "fair_value_shareholder_total_return_yield" not in quote_columns:
                connection.execute("ALTER TABLE quote_snapshots ADD COLUMN fair_value_shareholder_total_return_yield REAL")
            if "fair_value_market_cap_data_quality" not in quote_columns:
                connection.execute("ALTER TABLE quote_snapshots ADD COLUMN fair_value_market_cap_data_quality TEXT")
            if "fair_value_data_quality_score" not in quote_columns:
                connection.execute("ALTER TABLE quote_snapshots ADD COLUMN fair_value_data_quality_score REAL")
            if "fair_value_owner_earnings_maintenance_ratio" not in quote_columns:
                connection.execute("ALTER TABLE quote_snapshots ADD COLUMN fair_value_owner_earnings_maintenance_ratio REAL")
            if "fair_value_owner_earnings_ratio_source" not in quote_columns:
                connection.execute("ALTER TABLE quote_snapshots ADD COLUMN fair_value_owner_earnings_ratio_source TEXT")
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
            resume_result: dict[str, Any] | None = None
            if row:
                current = dict(row)
                raw_result = current.get("result_json")
                if raw_result:
                    try:
                        parsed_result = json.loads(raw_result)
                        if isinstance(parsed_result, dict):
                            resume_result = parsed_result
                    except (TypeError, ValueError):
                        resume_result = None
                running_age = self._age_seconds(current.get("started_at"))
                finished_age = self._age_seconds(current.get("finished_at"))
                if current["status"] == "running" and running_age is not None and running_age < stale_seconds:
                    return {"job_id": job_id, "symbol": symbol, "horizon_days": horizon_days, "status": "running", "started_at": current.get("started_at"), "claimed": False}
                # 分批 Gamma 任务完成一批后仍可能有下一批；这种中间状态不能被冷却时间挡住。
                has_more = bool((resume_result or {}).get("has_more"))
                if current["status"] in {"completed", "failed"} and finished_age is not None and finished_age < cooldown_seconds and not has_more:
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
            "resume_result": resume_result,
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
            finished = cursor.rowcount > 0
            if finished and status in {"completed", "failed"}:
                symbol = str(job_id).rsplit(":", 1)[0].upper()
                connection.execute(
                    "INSERT INTO push_events(symbol, kind, expiration, created_at) VALUES (?, 'gamma', NULL, ?)",
                    (symbol, iso()),
                )
                connection.execute(
                    "DELETE FROM push_events WHERE id <= (SELECT COALESCE(MAX(id), 0) - 20000 FROM push_events)"
                )
            return finished

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
        entry = self.get_analysis_cache_entry(cache_key)
        return entry[0] if entry else None

    def get_analysis_cache_entry(self, cache_key: str) -> tuple[Any, str] | None:
        """读取共享分析缓存及写入时间，供多 worker 复用稳定结果。"""
        with self.connect() as connection:
            row = connection.execute(
                "SELECT payload, created_at FROM api_analysis_cache WHERE cache_key=?", (cache_key,)
            ).fetchone()
        if not row:
            return None
        try:
            return json.loads(row["payload"]), str(row["created_at"])
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

    def publish_push_event(self, symbol: str, kind: str = "snapshot", expiration: str | None = None) -> int:
        """Append a small cross-worker notification; payloads remain in the normal snapshot tables."""
        normalized = str(symbol).strip().upper()
        if not normalized:
            return 0
        with self.connect() as connection:
            cursor = connection.execute(
                "INSERT INTO push_events(symbol, kind, expiration, created_at) VALUES (?, ?, ?, ?)",
                (normalized, kind, expiration, iso()),
            )
            # Bounded event log supports brief disconnect/reconnect without unbounded growth.
            connection.execute("DELETE FROM push_events WHERE id <= (SELECT COALESCE(MAX(id), 0) - 20000 FROM push_events)")
            return int(cursor.lastrowid or 0)

    def push_events_since(self, event_id: int, symbol: str | None = None, kind: str | None = None, expiration: str | None = None) -> list[dict[str, Any]]:
        with self.connect() as connection:
            clauses = ["id > ?"]
            params: list[Any] = [max(0, int(event_id))]
            if symbol is not None:
                clauses.append("symbol = ?")
                params.append(str(symbol).strip().upper())
            if kind is not None:
                clauses.append("kind = ?")
                params.append(kind)
            if expiration is not None:
                clauses.append("(expiration = ? OR expiration IS NULL)")
                params.append(expiration)
            rows = connection.execute(
                f"SELECT id, symbol, kind, expiration, created_at FROM push_events WHERE {' AND '.join(clauses)} ORDER BY id LIMIT 100",
                params,
            ).fetchall()
        return [dict(row) for row in rows]

    def claim_push_refresh(self, symbol: str, expiration: str | None, interval_seconds: int) -> bool:
        """Atomically reserve one refresh per symbol/expiry across workers, honoring cadence/backoff and an in-flight lease."""
        normalized = str(symbol).strip().upper()
        expiry = str(expiration or "")
        now = utc_now()
        with self.connect() as connection:
            connection.execute("BEGIN IMMEDIATE")
            row = connection.execute(
                "SELECT started_at, retry_after, retry_delay_seconds, last_interval_seconds, lease_until FROM push_refresh_state WHERE symbol=? AND expiration=?",
                (normalized, expiry),
            ).fetchone()
            if row:
                try:
                    lease_until = row["lease_until"]
                    if lease_until:
                        lease_at = datetime.fromisoformat(str(lease_until))
                        if lease_at.tzinfo is None:
                            lease_at = lease_at.replace(tzinfo=timezone.utc)
                        if now < lease_at.astimezone(timezone.utc):
                            return False
                    retry_after = row["retry_after"]
                    if retry_after:
                        retry_at = datetime.fromisoformat(str(retry_after))
                        if retry_at.tzinfo is None:
                            retry_at = retry_at.replace(tzinfo=timezone.utc)
                        if now < retry_at.astimezone(timezone.utc):
                            return False
                    previous = datetime.fromisoformat(str(row["started_at"]))
                    if previous.tzinfo is None:
                        previous = previous.replace(tzinfo=timezone.utc)
                    # A cadence regime change may legitimately shorten the previous interval (e.g. market opens).
                    previous_interval = int(row["last_interval_seconds"] or 0)
                    if previous_interval == max(1, int(interval_seconds)) and (now - previous.astimezone(timezone.utc)).total_seconds() < max(1, interval_seconds - 1):
                        return False
                except ValueError:
                    pass
            connection.execute(
                """INSERT INTO push_refresh_state(symbol, expiration, started_at, retry_after, retry_delay_seconds, last_interval_seconds, lease_until)
                   VALUES (?, ?, ?, NULL, 0, ?, ?)
                   ON CONFLICT(symbol, expiration) DO UPDATE SET started_at=excluded.started_at,
                     retry_after=NULL, last_interval_seconds=excluded.last_interval_seconds, lease_until=excluded.lease_until""",
                (normalized, expiry, now.isoformat(), max(1, int(interval_seconds)), (now + timedelta(seconds=180)).isoformat()),
            )
        return True

    def push_refresh_wait_seconds(self, symbol: str, expiration: str | None, interval_seconds: int) -> float:
        """Return the shared lease/cadence/cooldown delay after another worker's claim succeeds."""
        normalized = str(symbol).strip().upper()
        expiry = str(expiration or "")
        now = utc_now()
        with self.connect() as connection:
            row = connection.execute(
                "SELECT started_at, retry_after, retry_delay_seconds, lease_until FROM push_refresh_state WHERE symbol=? AND expiration=?",
                (normalized, expiry),
            ).fetchone()
        if not row:
            return 0.1
        retry_after = row["retry_after"]
        if retry_after:
            try:
                retry_at = datetime.fromisoformat(str(retry_after))
                if retry_at.tzinfo is None:
                    retry_at = retry_at.replace(tzinfo=timezone.utc)
                retry_wait = (retry_at.astimezone(timezone.utc) - now).total_seconds()
                if retry_wait > 0:
                    return retry_wait
            except ValueError:
                pass
        lease_until = row["lease_until"]
        if lease_until:
            try:
                lease_at = datetime.fromisoformat(str(lease_until))
                if lease_at.tzinfo is None:
                    lease_at = lease_at.replace(tzinfo=timezone.utc)
                lease_wait = (lease_at.astimezone(timezone.utc) - now).total_seconds()
                if lease_wait > 0:
                    # Let the owning worker clear its lease promptly without hammering SQLite.
                    return min(lease_wait, 5.0)
            except ValueError:
                pass
        if int(row["last_interval_seconds"] or 0) != max(1, int(interval_seconds)):
            return 0.1
        try:
            started = datetime.fromisoformat(str(row["started_at"]))
            if started.tzinfo is None:
                started = started.replace(tzinfo=timezone.utc)
            elapsed = (now - started.astimezone(timezone.utc)).total_seconds()
            return max(0.1, max(1, int(interval_seconds) - 1) - elapsed)
        except ValueError:
            return 0.1

    def finish_push_refresh(self, symbol: str, expiration: str | None) -> None:
        normalized = str(symbol).strip().upper()
        expiry = str(expiration or "")
        with self.connect() as connection:
            connection.execute(
                "UPDATE push_refresh_state SET lease_until=NULL, retry_delay_seconds=0 WHERE symbol=? AND expiration=?",
                (normalized, expiry),
            )

    def defer_push_refresh(self, symbol: str, expiration: str | None, delay_seconds: int) -> int:
        """Persist shared retry backoff after an upstream failure so workers don't synchronize retries."""
        normalized = str(symbol).strip().upper()
        expiry = str(expiration or "")
        now = utc_now()
        with self.connect() as connection:
            connection.execute("BEGIN IMMEDIATE")
            row = connection.execute(
                "SELECT retry_delay_seconds FROM push_refresh_state WHERE symbol=? AND expiration=?",
                (normalized, expiry),
            ).fetchone()
            previous = int(row["retry_delay_seconds"] or 0) if row else 0
            delay = min(MAX_PUSH_REFRESH_BACKOFF_SECONDS, max(int(delay_seconds), previous * 2 or int(delay_seconds), 1))
            retry_after = (now + timedelta(seconds=delay)).isoformat()
            connection.execute(
                "UPDATE push_refresh_state SET retry_after=?, retry_delay_seconds=?, lease_until=NULL WHERE symbol=? AND expiration=?",
                (retry_after, delay, normalized, expiry),
            )
        return delay

    def latest_push_event_id(self) -> int:
        with self.connect() as connection:
            row = connection.execute("SELECT COALESCE(MAX(id), 0) AS id FROM push_events").fetchone()
        return int(row["id"] or 0)

    def write_snapshot(self, quote: dict[str, Any], options: Iterable[dict[str, Any]], fetched_at: str) -> int:
        # raw_json 目前没有任何读取方，为避免小磁盘环境被冗余 JSON 撑爆，写入时不再保存原始报文。
        option_rows = list(options)
        self._preserve_cached_fair_value(quote)
        with self.connect() as connection:
            connection.execute(
                """INSERT INTO quote_snapshots
                (symbol, fetched_at, price, change_percent, currency, market_state, provider, sessions_json, today_open, previous_close, fair_value, fair_value_low, fair_value_high, fair_value_buy_low, fair_value_buy_high, fair_value_source, fair_value_model, fair_value_forward_eps, fair_value_forward_eps_source, fair_value_safety_margin, fair_value_confidence, fair_value_confidence_score, fair_value_interest_coverage, fair_value_regime, fair_value_regime_signals_json, fair_value_model_under_regime, fair_value_defensive_json, fair_value_optimistic_json, fair_value_normalized_eps_source, fair_value_quarterly_momentum_json, fair_value_historical_valuation_percentiles_json, fair_value_shareholder_total_return_yield, fair_value_market_cap_data_quality, fair_value_data_quality_score, fair_value_owner_earnings_maintenance_ratio, fair_value_owner_earnings_ratio_source)
                VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)""",
                (
                    quote["symbol"], fetched_at, quote.get("price"), quote.get("change_percent"),
                    quote.get("currency"), quote.get("market_state"), quote.get("provider", "upstream"),
                    json.dumps(quote.get("sessions") or {}, ensure_ascii=True),
                    quote.get("today_open"), quote.get("previous_close"),
                    quote.get("fair_value"), quote.get("fair_value_low"), quote.get("fair_value_high"), quote.get("fair_value_buy_low"), quote.get("fair_value_buy_high"), quote.get("fair_value_source"), quote.get("fair_value_model"), quote.get("fair_value_forward_eps"), quote.get("fair_value_forward_eps_source"), quote.get("fair_value_safety_margin"), quote.get("fair_value_confidence"), quote.get("fair_value_confidence_score"), quote.get("fair_value_interest_coverage"), quote.get("fair_value_regime"), json.dumps(quote.get("fair_value_regime_signals") or {}, ensure_ascii=False), quote.get("fair_value_model_under_regime"), json.dumps(quote.get("fair_value_defensive") or {}, ensure_ascii=False), json.dumps(quote.get("fair_value_optimistic") or {}, ensure_ascii=False), quote.get("fair_value_normalized_eps_source"), json.dumps(quote.get("fair_value_quarterly_momentum") or {}, ensure_ascii=False), json.dumps(quote.get("fair_value_historical_valuation_percentiles") or {}, ensure_ascii=False), quote.get("fair_value_shareholder_total_return_yield"), quote.get("fair_value_market_cap_data_quality"), quote.get("fair_value_data_quality_score"), quote.get("fair_value_owner_earnings_maintenance_ratio"), quote.get("fair_value_owner_earnings_ratio_source"),
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

    def _preserve_cached_fair_value(self, quote: dict[str, Any]) -> None:
        """行情刷新缺少估值时沿用上一份有效结果，避免后台计算窗口覆盖估值卡片。"""
        symbol = str(quote.get("symbol") or "").upper()
        if not symbol:
            return
        cached = self.latest_quote(symbol)
        if not cached or cached.get("fair_value") is None:
            return
        scalar_fields = (
            "fair_value", "fair_value_low", "fair_value_high", "fair_value_buy_low",
            "fair_value_buy_high", "fair_value_source", "fair_value_model",
            "fair_value_forward_eps", "fair_value_forward_eps_source", "fair_value_safety_margin",
            "fair_value_confidence", "fair_value_confidence_score", "fair_value_interest_coverage",
            "fair_value_regime", "fair_value_model_under_regime",
            "fair_value_normalized_eps_source", "fair_value_market_cap_data_quality", "fair_value_data_quality_score",
            "fair_value_owner_earnings_maintenance_ratio", "fair_value_owner_earnings_ratio_source",
            "fair_value_shareholder_total_return_yield",
        )
        for field in scalar_fields:
            if quote.get(field) is None and cached.get(field) is not None:
                quote[field] = cached[field]
        for field, column in (
            ("fair_value_regime_signals", "fair_value_regime_signals_json"),
            ("fair_value_defensive", "fair_value_defensive_json"),
            ("fair_value_optimistic", "fair_value_optimistic_json"),
            ("fair_value_quarterly_momentum", "fair_value_quarterly_momentum_json"),
            ("fair_value_historical_valuation_percentiles", "fair_value_historical_valuation_percentiles_json"),
        ):
            current = quote.get(field)
            if current not in (None, {}, ""):
                continue
            raw = cached.get(column)
            if not raw:
                continue
            try:
                value = json.loads(raw)
            except (TypeError, ValueError):
                continue
            if value not in (None, {}, ""):
                quote[field] = value

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














    def latest_status(self, symbol: str) -> dict[str, Any] | None:
        with self.connect() as connection:
            row = connection.execute(
                "SELECT * FROM refresh_runs WHERE symbol=? ORDER BY started_at DESC LIMIT 1", (symbol,)
            ).fetchone()
        return dict(row) if row else None
