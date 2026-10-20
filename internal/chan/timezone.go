package chananalysis

import (
	"sync"
	"time"
	_ "time/tzdata"

	"option-scope-go/internal/marketcalendar"
)

var (
	newYorkLocationOnce  sync.Once
	newYorkLocationValue *time.Location
)

// newYorkLocation loads the exchange timezone once. Analysis invokes date and
// session helpers many times per bar, so repeatedly loading tzdata dominates
// both allocation and runtime in the append path.
func newYorkLocation() *time.Location {
	newYorkLocationOnce.Do(func() {
		newYorkLocationValue, _ = time.LoadLocation("America/New_York")
		if newYorkLocationValue == nil {
			newYorkLocationValue = time.UTC
		}
	})
	return newYorkLocationValue
}

// civilDayMinute converts an instant to its Eastern calendar day ordinal and
// minute of day once, so per-bar comparisons never repeat tzdata lookups.
func civilDayMinute(value time.Time) (days, minute int) {
	local := value.In(newYorkLocation())
	return marketcalendar.DaysFromCivil(local.Year(), int(local.Month()), local.Day()), local.Hour()*60 + local.Minute()
}

// dayTime materializes one exchange-local instant. Callers keep bar stamps as
// integer day/minute pairs and only pay for a time.Time where the result model
// requires one, such as CapturedAt.
func dayTime(days, minute int) time.Time {
	year, month, day := marketcalendar.CivilFromDays(days)
	return time.Date(year, time.Month(month), day, minute/60, minute%60, 0, 0, newYorkLocation())
}
