package store

import (
	"os"
	"path/filepath"
	"testing"

	"option-scope-go/internal/model"
)

func TestOpenPreservesReservedCharactersInDatabasePath(t *testing.T) {
	path := filepath.Join(t.TempDir(), "options?account#fragment%name.db")
	repository, err := Open(path)
	if err != nil {
		t.Fatalf("open database with reserved path characters: %v", err)
	}
	price := 123.45
	if err := repository.SaveQuote(model.Quote{
		Symbol: "PATH", Provider: "test", Price: &price, Sessions: map[string]any{},
	}); err != nil {
		_ = repository.Close()
		t.Fatalf("save quote: %v", err)
	}
	if err := repository.Close(); err != nil {
		t.Fatalf("close database: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("database was not created at configured path %q: %v", path, err)
	}

	repository, err = Open(path)
	if err != nil {
		t.Fatalf("reopen database with reserved path characters: %v", err)
	}
	defer repository.Close()
	quote, found, err := repository.LatestQuote("PATH")
	if err != nil || !found || quote.Price == nil || *quote.Price != price {
		t.Fatalf("quote after reopen=%#v found=%v err=%v", quote, found, err)
	}
}

func TestOpenResolvesRelativeDatabasePath(t *testing.T) {
	directory := t.TempDir()
	previousWorkingDir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(directory); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Chdir(previousWorkingDir); err != nil {
			t.Errorf("restore working directory: %v", err)
		}
	})

	repository, err := Open(filepath.Join("nested", "relative.db"))
	if err != nil {
		t.Fatalf("open relative database path: %v", err)
	}
	if err := repository.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(directory, "nested", "relative.db")); err != nil {
		t.Fatalf("database was not created at its relative path: %v", err)
	}
}
