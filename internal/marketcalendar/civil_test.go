package marketcalendar

import (
	"testing"
	"time"
)

var civilEpoch = time.Date(1970, 1, 1, 0, 0, 0, 0, time.UTC)

func TestCivilOrdinalsMatchStdlib(t *testing.T) {
	end := time.Date(2035, 12, 31, 0, 0, 0, 0, time.UTC)
	for day := time.Date(1960, 1, 1, 0, 0, 0, 0, time.UTC); !day.After(end); day = day.AddDate(0, 0, 1) {
		year, month, dayOfMonth := day.Date()
		want := int(day.Sub(civilEpoch) / (24 * time.Hour))
		got := DaysFromCivil(year, int(month), dayOfMonth)
		if got != want {
			t.Fatalf("DaysFromCivil(%d,%d,%d)=%d, want %d", year, month, dayOfMonth, got, want)
		}
		if backYear, backMonth, backDay := CivilFromDays(want); backYear != year || backMonth != int(month) || backDay != dayOfMonth {
			t.Fatalf("CivilFromDays(%d)=%d-%d-%d, want %d-%d-%d", want, backYear, backMonth, backDay, year, month, dayOfMonth)
		}
		if formatted := FormatDateOnly(want); formatted != day.Format(time.DateOnly) {
			t.Fatalf("FormatDateOnly(%d)=%q, want %q", want, formatted, day.Format(time.DateOnly))
		}
		if parsed, ok := ParseDateOnly(day.Format(time.DateOnly)); !ok || parsed != want {
			t.Fatalf("ParseDateOnly(%q)=%d,%v, want %d", day.Format(time.DateOnly), parsed, ok, want)
		}
		if weekday := WeekdayOrdinal(want); weekday != int(day.Weekday()) {
			t.Fatalf("WeekdayOrdinal(%d)=%d, want %d", want, weekday, int(day.Weekday()))
		}
	}
}

func TestParseDateOnlyRejectsNonCanonicalDates(t *testing.T) {
	for _, value := range []string{"", "2024-1-02", "2024-01-2", "2024-13-01", "2024-00-01", "2024-02-30", "2023-02-29", "2024/01/02", "2024-01-02T00:00", "abcd-01-02"} {
		if _, ok := ParseDateOnly(value); ok {
			t.Fatalf("ParseDateOnly(%q) accepted a non-canonical date", value)
		}
	}
	if days, ok := ParseDateOnly("2024-02-29"); !ok || FormatDateOnly(days) != "2024-02-29" {
		t.Fatalf("leap day rejected: days=%d ok=%v", days, ok)
	}
}

func TestParseAndFormatDateTime(t *testing.T) {
	for _, value := range []string{"2024-01-02 09:30", "2024-06-30 15:45", "2025-12-31 00:00", "2024-02-29 23:59"} {
		days, minute, ok := ParseDateTime(value)
		if !ok {
			t.Fatalf("ParseDateTime(%q) rejected", value)
		}
		if formatted := FormatDateTime(days, minute); formatted != value {
			t.Fatalf("FormatDateTime round trip=%q, want %q", formatted, value)
		}
	}
	for _, value := range []string{"2024-01-02", "2024-01-02 24:00", "2024-01-02 09:60", "2024-01-02T09:30", "2024-02-30 09:30"} {
		if _, _, ok := ParseDateTime(value); ok {
			t.Fatalf("ParseDateTime(%q) accepted an invalid stamp", value)
		}
	}
}

func TestTradingCalendarOrdinalsMatchTimeBasedHelpers(t *testing.T) {
	location := location()
	end := time.Date(2030, 12, 31, 0, 0, 0, 0, location)
	for day := time.Date(2015, 1, 1, 0, 0, 0, 0, location); !day.After(end); day = day.AddDate(0, 0, 1) {
		year, month, dayOfMonth := day.Date()
		days := DaysFromCivil(year, int(month), dayOfMonth)
		if got, want := IsTradingDayOrdinal(days), calendarDay(day) != calendarClosed; got != want {
			t.Fatalf("IsTradingDayOrdinal(%s)=%v, want %v", day.Format(time.DateOnly), got, want)
		}
		if got, want := RegularCloseMinuteOrdinal(days), RegularCloseMinute(day.Add(12*time.Hour)); got != want {
			t.Fatalf("RegularCloseMinuteOrdinal(%s)=%d, want %d", day.Format(time.DateOnly), got, want)
		}
	}
}

func TestNextTradingDayOrdinalSkipsWeekendsAndHolidays(t *testing.T) {
	friday := DaysFromCivil(2024, 5, 24)
	if next := NextTradingDayOrdinal(friday); FormatDateOnly(next) != "2024-05-28" {
		t.Fatalf("next trading day after 2024-05-24=%s, want 2024-05-28 (Memorial Day)", FormatDateOnly(next))
	}
	day := NextTradingDayOrdinal(DaysFromCivil(2024, 12, 24))
	if FormatDateOnly(day) != "2024-12-26" {
		t.Fatalf("next trading day after 2024-12-24=%s, want 2024-12-26", FormatDateOnly(day))
	}
}
