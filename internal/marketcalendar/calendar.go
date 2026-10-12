package marketcalendar

import (
	"sync"
	"time"
	_ "time/tzdata"
)

var (
	newYorkOnce sync.Once
	newYork     *time.Location
)

func location() *time.Location {
	newYorkOnce.Do(func() {
		newYork, _ = time.LoadLocation("America/New_York")
		if newYork == nil {
			newYork = time.UTC
		}
	})
	return newYork
}

func localDate(value time.Time) time.Time {
	local := value.In(location())
	return time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, location())
}

func IsTradingDay(value time.Time) bool {
	day := localDate(value)
	if day.Weekday() == time.Saturday || day.Weekday() == time.Sunday {
		return false
	}
	return !isHoliday(day)
}

func IsRegularSession(value time.Time) bool {
	local := value.In(location())
	if !IsTradingDay(local) {
		return false
	}
	minute := local.Hour()*60 + local.Minute()
	return minute >= 9*60+30 && minute < RegularCloseMinute(local)
}

// RegularCloseMinute returns the XNYS regular-session close as minutes after
// midnight Eastern. A few early closes before 1993 ended at 14:00.
func RegularCloseMinute(value time.Time) int {
	day := localDate(value)
	switch day.Format(time.DateOnly) {
	case "1990-12-24", "1991-12-24", "1992-11-27", "1992-12-24":
		return 14 * 60
	}
	if IsEarlyClose(day) {
		return 13 * 60
	}
	return 16 * 60
}

func IsOvernightSession(value time.Time) bool {
	local := value.In(location())
	minute := local.Hour()*60 + local.Minute()
	if minute >= 20*60 {
		return IsTradingDay(local.AddDate(0, 0, 1))
	}
	return minute < 4*60 && IsTradingDay(local)
}

func IsEarlyClose(value time.Time) bool {
	day := localDate(value)
	if !IsTradingDay(day) {
		return false
	}
	year := day.Year()
	thanksgiving := nthWeekday(year, time.November, time.Thursday, 4)
	christmasEve := time.Date(year, time.December, 24, 0, 0, 0, 0, location())
	if day.Equal(thanksgiving.AddDate(0, 0, 1)) || (year >= 1993 && day.Equal(christmasEve)) {
		return true
	}
	if isAdhocEarlyClose(day) {
		return true
	}
	if year >= 1995 && day.Month() == time.July {
		if day.Day() == 3 {
			switch day.Weekday() {
			case time.Monday, time.Tuesday, time.Thursday:
				return true
			case time.Wednesday:
				return year >= 2013
			}
		}
		if year < 2013 && day.Day() == 5 && day.Weekday() == time.Friday {
			return true
		}
	}
	return false
}

func isAdhocEarlyClose(day time.Time) bool {
	switch day.Format(time.DateOnly) {
	case "1997-12-26", "1999-12-31", "2003-12-26":
		return true
	default:
		return false
	}
}

func isHoliday(day time.Time) bool {
	if isSpecialClosure(day) {
		return true
	}
	year := day.Year()
	for holidayYear := year - 1; holidayYear <= year+1; holidayYear++ {
		newYear := time.Date(holidayYear, time.January, 1, 0, 0, 0, 0, location())
		if day.Equal(newYear) || (newYear.Weekday() == time.Sunday && day.Equal(newYear.AddDate(0, 0, 1))) {
			return true
		}
	}
	fixed := []time.Time{
		time.Date(year, time.July, 4, 0, 0, 0, 0, location()),
		time.Date(year, time.December, 25, 0, 0, 0, 0, location()),
	}
	if year >= 2022 {
		fixed = append(fixed, time.Date(year, time.June, 19, 0, 0, 0, 0, location()))
	}
	for _, holiday := range fixed {
		if day.Equal(observed(holiday)) {
			return true
		}
	}
	if (year >= 1998 && day.Equal(nthWeekday(year, time.January, time.Monday, 3))) ||
		day.Equal(nthWeekday(year, time.February, time.Monday, 3)) ||
		day.Equal(lastWeekday(year, time.May, time.Monday)) ||
		day.Equal(nthWeekday(year, time.September, time.Monday, 1)) ||
		day.Equal(nthWeekday(year, time.November, time.Thursday, 4)) {
		return true
	}
	return day.Equal(easterSunday(year).AddDate(0, 0, -2))
}

func isSpecialClosure(day time.Time) bool {
	switch day.Format(time.DateOnly) {
	case "1929-11-01", "1929-11-29",
		"1933-03-06", "1933-03-07", "1933-03-08", "1933-03-09", "1933-03-10", "1933-03-13", "1933-03-14",
		"1945-08-15", "1945-08-16", "1945-12-24",
		"1956-12-24", "1958-12-26",
		"1963-11-25",
		"1968-02-12", "1968-04-09", "1968-06-12", "1968-06-19", "1968-06-26",
		"1968-07-05", "1968-07-10", "1968-07-17", "1968-07-24", "1968-07-31",
		"1968-08-07", "1968-08-14", "1968-08-21", "1968-08-28",
		"1968-09-11", "1968-09-18", "1968-09-25", "1968-10-02", "1968-10-09", "1968-10-16", "1968-10-23", "1968-10-30",
		"1968-11-11", "1968-11-20", "1968-12-04", "1968-12-11", "1968-12-18",
		"1969-02-10", "1969-03-31", "1969-07-21",
		"1972-11-07", "1972-12-28", "1973-01-25", "1976-11-02",
		"1977-07-14", "1980-11-04", "1985-09-27", "1994-04-27",
		"2001-09-11", "2001-09-12", "2001-09-13", "2001-09-14",
		"2004-06-11", "2007-01-02", "2012-10-29", "2012-10-30",
		"2018-12-05", "2025-01-09":
		return true
	default:
		return false
	}
}

func observed(day time.Time) time.Time {
	switch day.Weekday() {
	case time.Saturday:
		return day.AddDate(0, 0, -1)
	case time.Sunday:
		return day.AddDate(0, 0, 1)
	default:
		return day
	}
}

func nthWeekday(year int, month time.Month, weekday time.Weekday, nth int) time.Time {
	first := time.Date(year, month, 1, 0, 0, 0, 0, location())
	delta := (int(weekday) - int(first.Weekday()) + 7) % 7
	return first.AddDate(0, 0, delta+7*(nth-1))
}

func lastWeekday(year int, month time.Month, weekday time.Weekday) time.Time {
	last := time.Date(year, month+1, 0, 0, 0, 0, 0, location())
	delta := (int(last.Weekday()) - int(weekday) + 7) % 7
	return last.AddDate(0, 0, -delta)
}

func easterSunday(year int) time.Time {
	// Gregorian computus (Meeus/Jones/Butcher).
	a, b, c := year%19, year/100, year%100
	d, e := b/4, b%4
	f := (b + 8) / 25
	g := (b - f + 1) / 3
	h := (19*a + b - d - g + 15) % 30
	i, k := c/4, c%4
	l := (32 + 2*e + 2*i - h - k) % 7
	m := (a + 11*h + 22*l) / 451
	month := (h + l - 7*m + 114) / 31
	day := (h+l-7*m+114)%31 + 1
	return time.Date(year, time.Month(month), day, 0, 0, 0, 0, location())
}
