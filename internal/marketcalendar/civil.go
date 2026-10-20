package marketcalendar

// Civil-day ordinals count days since 1970-01-01 in the proleptic Gregorian
// calendar. History analysis asks the same date questions for every bar, so
// integer dates keep parsing, formatting and calendar walks off the tzdata
// lookup path.

// DaysFromCivil converts a Gregorian date to a day ordinal.
func DaysFromCivil(year, month, day int) int {
	if month <= 2 {
		year--
	}
	era := year / 400
	if year < 0 && year%400 != 0 {
		era--
	}
	yearOfEra := year - era*400
	monthPrime := (month + 9) % 12
	dayOfYear := (153*monthPrime+2)/5 + day - 1
	dayOfEra := yearOfEra*365 + yearOfEra/4 - yearOfEra/100 + dayOfYear
	return era*146097 + dayOfEra - 719468
}

// CivilFromDays converts a day ordinal back to a Gregorian date.
func CivilFromDays(days int) (year, month, day int) {
	z := days + 719468
	era := z / 146097
	if z < 0 && z%146097 != 0 {
		era--
	}
	dayOfEra := z - era*146097
	yearOfEra := (dayOfEra - dayOfEra/1460 + dayOfEra/36524 - dayOfEra/146096) / 365
	year = yearOfEra + era*400
	dayOfYear := dayOfEra - (365*yearOfEra + yearOfEra/4 - yearOfEra/100)
	monthPrime := (5*dayOfYear + 2) / 153
	day = dayOfYear - (153*monthPrime+2)/5 + 1
	month = monthPrime + 3
	if month > 12 {
		month -= 12
		year++
	}
	return year, month, day
}

// WeekdayOrdinal returns the weekday for a day ordinal, with Sunday as 0.
func WeekdayOrdinal(days int) int {
	weekday := (days + 4) % 7
	if weekday < 0 {
		weekday += 7
	}
	return weekday
}

// ParseDateOnly parses a canonical "2006-01-02" string. It accepts exactly the
// format time.DateOnly produces, matching the previous ParseInLocation check.
func ParseDateOnly(value string) (int, bool) {
	if len(value) != 10 || value[4] != '-' || value[7] != '-' {
		return 0, false
	}
	for index := 0; index < len(value); index++ {
		if index == 4 || index == 7 {
			continue
		}
		if value[index] < '0' || value[index] > '9' {
			return 0, false
		}
	}
	year := digit4(value, 0)
	month := digit2(value, 5)
	day := digit2(value, 8)
	if month < 1 || month > 12 || day < 1 || day > daysInMonth(year, month) {
		return 0, false
	}
	return DaysFromCivil(year, month, day), true
}

// ParseDateTime parses a canonical "2006-01-02 15:04" string.
func ParseDateTime(value string) (days, minute int, ok bool) {
	if len(value) != 16 || value[4] != '-' || value[7] != '-' || value[10] != ' ' || value[13] != ':' {
		return 0, 0, false
	}
	for index := 0; index < len(value); index++ {
		if index == 4 || index == 7 || index == 10 || index == 13 {
			continue
		}
		if value[index] < '0' || value[index] > '9' {
			return 0, 0, false
		}
	}
	year := digit4(value, 0)
	month := digit2(value, 5)
	day := digit2(value, 8)
	hour := digit2(value, 11)
	minuteOfHour := digit2(value, 14)
	if month < 1 || month > 12 || day < 1 || day > daysInMonth(year, month) ||
		hour > 23 || minuteOfHour > 59 {
		return 0, 0, false
	}
	return DaysFromCivil(year, month, day), hour*60 + minuteOfHour, true
}

// FormatDateOnly renders a day ordinal as "2006-01-02".
func FormatDateOnly(days int) string {
	year, month, day := CivilFromDays(days)
	var buffer [10]byte
	buffer[4], buffer[7] = '-', '-'
	write4(buffer[:4], year)
	write2(buffer[5:7], month)
	write2(buffer[8:10], day)
	return string(buffer[:])
}

// FormatDateTime renders a day ordinal and minute-of-day as "2006-01-02 15:04".
func FormatDateTime(days, minute int) string {
	year, month, day := CivilFromDays(days)
	var buffer [16]byte
	buffer[4], buffer[7], buffer[10], buffer[13] = '-', '-', ' ', ':'
	write4(buffer[:4], year)
	write2(buffer[5:7], month)
	write2(buffer[8:10], day)
	write2(buffer[11:13], minute/60)
	write2(buffer[14:16], minute%60)
	return string(buffer[:])
}

// IsTradingDayOrdinal reports whether the ordinal is an XNYS trading day.
func IsTradingDayOrdinal(days int) bool {
	return calendarDayOrdinal(days) != calendarClosed
}

// NextTradingDayOrdinal returns the first trading day after the ordinal.
func NextTradingDayOrdinal(days int) int {
	for {
		days++
		if IsTradingDayOrdinal(days) {
			return days
		}
	}
}

// RegularCloseMinuteOrdinal returns the XNYS regular-session close as minutes
// after midnight Eastern for the given day ordinal.
func RegularCloseMinuteOrdinal(days int) int {
	year, month, day := CivilFromDays(days)
	switch {
	case year == 1990 && month == 12 && day == 24,
		year == 1991 && month == 12 && day == 24,
		year == 1992 && month == 11 && day == 27,
		year == 1992 && month == 12 && day == 24:
		return 14 * 60
	}
	if calendarDayOrdinal(days) == calendarEarlyClose {
		return 13 * 60
	}
	return 16 * 60
}

func calendarDayOrdinal(days int) uint8 {
	year, _, _ := CivilFromDays(days)
	dayOfYear := days - DaysFromCivil(year, 1, 1) + 1
	if dayOfYear < 1 || dayOfYear > 366 {
		return calendarClosed
	}
	calendarMu.RLock()
	calendar := calendars[year]
	calendarMu.RUnlock()
	if calendar == nil {
		calendar = buildYearCalendar(year)
		calendarMu.Lock()
		if existing := calendars[year]; existing != nil {
			calendar = existing
		} else {
			if len(calendars) >= cachedCalendarYears {
				for cachedYear := range calendars {
					delete(calendars, cachedYear)
					break
				}
			}
			calendars[year] = calendar
		}
		calendarMu.Unlock()
	}
	return calendar.days[dayOfYear]
}

func digit2(value string, offset int) int {
	return int(value[offset]-'0')*10 + int(value[offset+1]-'0')
}

func digit4(value string, offset int) int {
	return digit2(value, offset)*100 + digit2(value, offset+2)
}

func write2(buffer []byte, value int) {
	buffer[0], buffer[1] = byte('0'+value/10), byte('0'+value%10)
}

func write4(buffer []byte, value int) {
	write2(buffer[:2], value/100)
	write2(buffer[2:4], value%100)
}

func daysInMonth(year, month int) int {
	switch month {
	case 2:
		if year%4 == 0 && (year%100 != 0 || year%400 == 0) {
			return 29
		}
		return 28
	case 4, 6, 9, 11:
		return 30
	default:
		return 31
	}
}
