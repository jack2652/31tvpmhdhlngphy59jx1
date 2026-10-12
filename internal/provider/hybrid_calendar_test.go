package provider

import (
	"testing"
	"time"
)

func TestNYSETradingDaySpecialClosures(t *testing.T) {
	location, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name string
		day  time.Time
	}{
		{"September 11 attacks day one", time.Date(2001, time.September, 11, 0, 0, 0, 0, location)},
		{"September 11 attacks day two", time.Date(2001, time.September, 12, 0, 0, 0, 0, location)},
		{"September 11 attacks day three", time.Date(2001, time.September, 13, 0, 0, 0, 0, location)},
		{"September 11 attacks day four", time.Date(2001, time.September, 14, 0, 0, 0, 0, location)},
		{"Reagan funeral", time.Date(2004, time.June, 11, 0, 0, 0, 0, location)},
		{"Ford funeral", time.Date(2007, time.January, 2, 0, 0, 0, 0, location)},
		{"Hurricane Sandy day one", time.Date(2012, time.October, 29, 0, 0, 0, 0, location)},
		{"Hurricane Sandy day two", time.Date(2012, time.October, 30, 0, 0, 0, 0, location)},
		{"Bush funeral", time.Date(2018, time.December, 5, 0, 0, 0, 0, location)},
		{"Carter funeral", time.Date(2025, time.January, 9, 0, 0, 0, 0, location)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if isNYSETradingDay(tc.day) {
				t.Errorf("isNYSETradingDay(%s) = true, want false", tc.day.Format("2006-01-02"))
			}
			if isNYSERegularSession(tc.day.Add(10 * time.Hour)) {
				t.Errorf("regular session unexpectedly open on %s", tc.day.Format("2006-01-02"))
			}
		})
	}
}

func TestHistoricalFourteenHundredCloseAcrossProviders(t *testing.T) {
	location, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	for _, raw := range []string{"1990-12-24", "1991-12-24", "1992-11-27", "1992-12-24"} {
		day, err := time.ParseInLocation(time.DateOnly, raw, location)
		if err != nil {
			t.Fatal(err)
		}
		for _, test := range []struct {
			name   string
			minute int
			want   string
		}{
			{"before close", 13*60 + 59, "REGULAR"},
			{"at close", 14 * 60, "POST"},
		} {
			at := day.Add(time.Duration(test.minute) * time.Minute)
			t.Run(raw+"/"+test.name, func(t *testing.T) {
				if got := yahooSessionState(at, nil); got != test.want {
					t.Errorf("Yahoo state=%q, want %q", got, test.want)
				}
				if got := cboeMarketState(at); got != test.want {
					t.Errorf("CBOE state=%q, want %q", got, test.want)
				}
			})
		}
	}
}

func TestNYSETradingDayWeekendsAndRegularHolidays(t *testing.T) {
	location, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name string
		day  time.Time
	}{
		{"Saturday", time.Date(2025, time.January, 11, 0, 0, 0, 0, location)},
		{"Sunday", time.Date(2025, time.January, 12, 0, 0, 0, 0, location)},
		{"New Year's Day", time.Date(2025, time.January, 1, 0, 0, 0, 0, location)},
		{"Good Friday", time.Date(2025, time.April, 18, 0, 0, 0, 0, location)},
		{"Christmas", time.Date(2025, time.December, 25, 0, 0, 0, 0, location)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if isNYSETradingDay(tc.day) {
				t.Errorf("isNYSETradingDay(%s) = true, want false", tc.day.Format("2006-01-02"))
			}
		})
	}

	openDay := time.Date(2025, time.January, 10, 0, 0, 0, 0, location)
	if !isNYSETradingDay(openDay) {
		t.Errorf("isNYSETradingDay(%s) = false, want true", openDay.Format("2006-01-02"))
	}
}
