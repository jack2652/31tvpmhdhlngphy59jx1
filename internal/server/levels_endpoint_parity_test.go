package server

import (
	"testing"
	"time"

	"option-scope-go/internal/model"
)

func TestLevelsOptionsTimestampUsesEmptySelectedBatchMarker(t *testing.T) {
	profileFetchedAt := time.Date(2026, time.January, 1, 12, 0, 0, 0, time.UTC)
	selectedFetchedAt := profileFetchedAt.Add(time.Hour)
	profile := map[string]any{
		"fetched_at":  profileFetchedAt,
		"expirations": []string{"2099-01-15"},
	}
	selected := model.ChainResponse{
		Expiration: "2099-01-16",
		FetchedAt:  &selectedFetchedAt,
	}

	got := levelsOptionsTimestamp(profile, selected, selected.Expiration)
	gotAt, ok := got.(*time.Time)
	if !ok || !gotAt.Equal(selectedFetchedAt) {
		t.Fatalf("fetched_at=%#v, want selected empty-batch timestamp %s", got, selectedFetchedAt)
	}
}
