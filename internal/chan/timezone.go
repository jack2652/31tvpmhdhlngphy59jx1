package chananalysis

import (
	"sync"
	"time"
	_ "time/tzdata"
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
