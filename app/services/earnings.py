"""财报日期窗口判断。

只根据已经缓存的日期判断下一财报是否落在未来若干个交易日内。
窗口内外在请求时计算，不写入缓存，也不改变任何评分。
"""

from __future__ import annotations

from datetime import date
from typing import Any

from app.services.market_calendar import upcoming_sessions


EARNINGS_WINDOW_SESSIONS = 5


def summarize_earnings(
    dates: list[str] | None,
    today: date,
    sessions: int = EARNINGS_WINDOW_SESSIONS,
) -> dict[str, Any]:
    """判断下一财报日相对未来交易日窗口的位置。

    没有未来日期时返回 unknown。日历日只要落在窗口起止之间就算窗口内，
    即使当天休市（例如感恩节当天公布，仍算在相邻交易日窗口里）。
    """
    window = upcoming_sessions(today, sessions)
    start = window[0]
    end = window[-1]
    upcoming: list[date] = []
    for raw in dates or []:
        try:
            parsed = date.fromisoformat(str(raw)[:10])
        except ValueError:
            continue
        if parsed >= today:
            upcoming.append(parsed)
    detail = "没有可用的未来财报日期，评分不因财报调整"
    summary: dict[str, Any] = {
        "status": "unknown",
        "date": None,
        "window_start": start.isoformat(),
        "window_end": end.isoformat(),
        "sessions": sessions,
        "detail": detail,
    }
    if not upcoming:
        return summary
    next_date = min(upcoming)
    inside = start <= next_date <= end
    relation = "落在未来交易日窗口内" if inside else "不在未来交易日窗口内"
    summary["status"] = "inside" if inside else "outside"
    summary["date"] = next_date.isoformat()
    summary["detail"] = (
        f"下一财报 {next_date.isoformat()} {relation}"
        f"（{start.isoformat()} 至 {end.isoformat()}，共 {sessions} 个交易日）。"
        "只作提示，不改变评分。"
    )
    return summary
