package provider

import (
	"testing"
	"time"
)

func TestPreviousPostSessionWindow(t *testing.T) {
	newYork, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name               string
		now                time.Time
		wantStart, wantEnd string
	}{
		{
			name:      "pre-market uses the prior regular close",
			now:       time.Date(2026, time.October, 20, 4, 16, 0, 0, newYork),
			wantStart: "2026-10-19 16:00", wantEnd: "2026-10-19 20:00",
		},
		{
			name:      "during post-market the previous session has not closed yet",
			now:       time.Date(2026, time.October, 19, 17, 0, 0, 0, newYork),
			wantStart: "2026-10-16 16:00", wantEnd: "2026-10-16 20:00",
		},
		{
			name:      "early close ends after-hours at 17:00",
			now:       time.Date(2026, time.November, 27, 18, 0, 0, 0, newYork),
			wantStart: "2026-11-27 13:00", wantEnd: "2026-11-27 17:00",
		},
		{
			name:      "weekend uses Friday",
			now:       time.Date(2026, time.October, 24, 12, 0, 0, 0, newYork),
			wantStart: "2026-10-23 16:00", wantEnd: "2026-10-23 20:00",
		},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			start, end, _, ok := previousPostSessionWindow(test.now)
			if !ok {
				t.Fatal("previousPostSessionWindow returned no window")
			}
			if got := start.Format("2006-01-02 15:04"); got != test.wantStart {
				t.Errorf("start = %s, want %s", got, test.wantStart)
			}
			if got := end.Format("2006-01-02 15:04"); got != test.wantEnd {
				t.Errorf("end = %s, want %s", got, test.wantEnd)
			}
		})
	}
}

func TestNextPostSessionEndSkipsTheCurrentWindow(t *testing.T) {
	newYork, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, time.October, 20, 4, 16, 0, 0, newYork)
	if got, want := nextPostSessionEnd(now).Format("2006-01-02 15:04"), "2026-10-20 20:00"; got != want {
		t.Fatalf("nextPostSessionEnd = %s, want %s", got, want)
	}
	afterClose := time.Date(2026, time.October, 20, 20, 1, 0, 0, newYork)
	if got, want := nextPostSessionEnd(afterClose).Format("2006-01-02 15:04"), "2026-10-21 20:00"; got != want {
		t.Fatalf("nextPostSessionEnd after close = %s, want %s", got, want)
	}
}
