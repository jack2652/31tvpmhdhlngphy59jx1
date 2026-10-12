package store

import (
	"path/filepath"
	"testing"
)

func TestSizeCleanupUsesPythonAvailableDiskSpaceForVacuumGate(t *testing.T) {
	directory := t.TempDir()
	const pythonFreeBytes = int64(7 * 4096)

	got := availableBytes(7, 4096)
	if got != pythonFreeBytes {
		t.Fatalf("availableBytes=%d, want shutil.disk_usage free=%d (f_bavail * f_frsize)", got, pythonFreeBytes)
	}

	databaseBytes := int64(float64(got)/sizeCleanupVacuumRatio) + 1
	if !vacuumSpaceInsufficient(databaseBytes, got) {
		t.Fatalf("VACUUM gate allowed databaseBytes=%d with availableFreeBytes=%d", databaseBytes, got)
	}

	repository, err := Open(filepath.Join(directory, "available-space.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer repository.Close()
	availableMB, err := repository.AvailableDiskSpaceMB()
	if err != nil {
		t.Fatal(err)
	}
	if availableMB <= 0 {
		t.Fatalf("AvailableDiskSpaceMB()=%d, want positive Python available disk space", availableMB)
	}
}
