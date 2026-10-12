package marketcalendar

import (
	"testing"
	"time"
)

func TestNYSETradingSessions(t *testing.T) {
	newYork, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name      string
		instant   time.Time
		trading   bool
		regular   bool
		overnight bool
	}{
		{"regular session", time.Date(2026, time.October, 7, 10, 0, 0, 0, newYork), true, true, false},
		{"regular close is exclusive", time.Date(2026, time.October, 7, 16, 0, 0, 0, newYork), true, false, false},
		{"Thanksgiving", time.Date(2026, time.November, 26, 10, 0, 0, 0, newYork), false, false, false},
		{"special closure", time.Date(2025, time.January, 9, 10, 0, 0, 0, newYork), false, false, false},
		{"New Year's Day on Saturday leaves Friday open", time.Date(2021, time.December, 31, 10, 0, 0, 0, newYork), true, true, false},
		{"day after Thanksgiving early close", time.Date(2026, time.November, 27, 12, 59, 0, 0, newYork), true, true, false},
		{"early close is exclusive", time.Date(2026, time.November, 27, 13, 0, 0, 0, newYork), true, false, false},
		{"overnight before trading day", time.Date(2026, time.October, 7, 21, 0, 0, 0, newYork), true, false, true},
		{"overnight before holiday", time.Date(2026, time.November, 25, 21, 0, 0, 0, newYork), true, false, false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := IsTradingDay(test.instant); got != test.trading {
				t.Errorf("IsTradingDay() = %v, want %v", got, test.trading)
			}
			if got := IsRegularSession(test.instant); got != test.regular {
				t.Errorf("IsRegularSession() = %v, want %v", got, test.regular)
			}
			if got := IsOvernightSession(test.instant); got != test.overnight {
				t.Errorf("IsOvernightSession() = %v, want %v", got, test.overnight)
			}
		})
	}
}

func TestNYSEEarlyCloseCalendarBoundaries(t *testing.T) {
	newYork, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name    string
		day     time.Time
		trading bool
		early   bool
		at1259  bool
		at1300  bool
	}{
		{"July 3 Wednesday before 2013 is full session", time.Date(1996, time.July, 3, 0, 0, 0, 0, newYork), true, false, true, true},
		{"July 5 Friday before 2013 is early close", time.Date(1996, time.July, 5, 0, 0, 0, 0, newYork), true, true, true, false},
		{"July 3 Thursday is early close", time.Date(2008, time.July, 3, 0, 0, 0, 0, newYork), true, true, true, false},
		{"July 5 outside pre-2013 rule is full session", time.Date(2012, time.July, 5, 0, 0, 0, 0, newYork), true, false, true, true},
		{"July 3 Wednesday from 2013 is early close", time.Date(2013, time.July, 3, 0, 0, 0, 0, newYork), true, true, true, false},
		{"July 3 2020 is observed Independence Day closure", time.Date(2020, time.July, 3, 0, 0, 0, 0, newYork), false, false, false, false},
		{"1997 Boxing Day adhoc early close", time.Date(1997, time.December, 26, 0, 0, 0, 0, newYork), true, true, true, false},
		{"1999 year-end adhoc early close", time.Date(1999, time.December, 31, 0, 0, 0, 0, newYork), true, true, true, false},
		{"2003 Boxing Day adhoc early close", time.Date(2003, time.December, 26, 0, 0, 0, 0, newYork), true, true, true, false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := IsTradingDay(test.day); got != test.trading {
				t.Errorf("IsTradingDay() = %v, want %v", got, test.trading)
			}
			if got := IsEarlyClose(test.day); got != test.early {
				t.Errorf("IsEarlyClose() = %v, want %v", got, test.early)
			}
			at1259 := test.day.Add(12*time.Hour + 59*time.Minute)
			if got := IsRegularSession(at1259); got != test.at1259 {
				t.Errorf("IsRegularSession(12:59) = %v, want %v", got, test.at1259)
			}
			at1300 := test.day.Add(13 * time.Hour)
			if got := IsRegularSession(at1300); got != test.at1300 {
				t.Errorf("IsRegularSession(13:00) = %v, want %v", got, test.at1300)
			}
		})
	}
}

func TestNYSEHistoricalEarlyCloseMinutesMatchExchangeCalendar(t *testing.T) {
	newYork, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	for _, raw := range []string{"1990-12-24", "1991-12-24", "1992-11-27", "1992-12-24"} {
		day, err := time.ParseInLocation(time.DateOnly, raw, newYork)
		if err != nil {
			t.Fatal(err)
		}
		if got := RegularCloseMinute(day); got != 14*60 {
			t.Errorf("RegularCloseMinute(%s)=%d, want 14:00 from XNYS", raw, got)
		}
		if !IsRegularSession(day.Add(13*time.Hour + 59*time.Minute)) {
			t.Errorf("13:59 on %s should remain in the regular session", raw)
		}
		if IsRegularSession(day.Add(14 * time.Hour)) {
			t.Errorf("14:00 on %s should be outside the regular session", raw)
		}
	}
}

func TestNYSEHistoricalClosuresFromExchangeCalendar(t *testing.T) {
	newYork, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	// Dates are the XNYS adhoc_holidays from exchange_calendars 4.13.2,
	// plus presidential election closures in its regular holiday rules.
	closures := []string{
		"1929-11-01", "1929-11-29",
		"1933-03-06", "1933-03-07", "1933-03-08", "1933-03-09", "1933-03-10", "1933-03-13", "1933-03-14",
		"1945-08-15", "1945-08-16", "1945-12-24",
		"1956-12-24", "1958-12-26",
		"1961-05-29",
		"1963-11-25",
		"1968-02-12", "1968-04-09", "1968-06-12", "1968-06-19", "1968-06-26",
		"1968-07-05", "1968-07-10", "1968-07-17", "1968-07-24", "1968-07-31",
		"1968-08-07", "1968-08-14", "1968-08-21", "1968-08-28",
		"1968-09-11", "1968-09-18", "1968-09-25", "1968-10-02", "1968-10-09", "1968-10-16", "1968-10-23", "1968-10-30",
		"1968-11-11", "1968-11-20", "1968-12-04", "1968-12-11", "1968-12-18", "1968-12-25",
		"1969-02-10", "1969-03-31", "1969-07-21",
		"1972-11-07", "1972-12-28", "1973-01-25", "1976-11-02", "1977-07-14", "1980-11-04", "1985-09-27", "1994-04-27",
		"2001-09-11", "2001-09-12", "2001-09-13", "2001-09-14", "2004-06-11", "2007-01-02",
		"2012-10-29", "2012-10-30", "2018-12-05", "2025-01-09",
	}
	for _, value := range closures {
		day, err := time.ParseInLocation(time.DateOnly, value, newYork)
		if err != nil {
			t.Fatalf("parse closure date %q: %v", value, err)
		}
		if IsTradingDay(day) {
			t.Errorf("IsTradingDay(%s) = true, want false", value)
		}
	}
}

func TestNYSEMartinLutherKingHolidayStartYear(t *testing.T) {
	newYork, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		day     string
		trading bool
	}{
		{"1997-01-20", true},
		{"1998-01-19", false},
	} {
		day, err := time.ParseInLocation(time.DateOnly, test.day, newYork)
		if err != nil {
			t.Fatalf("parse date %q: %v", test.day, err)
		}
		if got := IsTradingDay(day); got != test.trading {
			t.Errorf("IsTradingDay(%s) = %v, want %v", test.day, got, test.trading)
		}
	}
}
