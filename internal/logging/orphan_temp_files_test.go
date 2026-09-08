package logging

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func writeTempFileWithAge(t *testing.T, dir, name string, age time.Duration) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if errWrite := os.WriteFile(path, []byte("payload"), 0o644); errWrite != nil {
		t.Fatalf("write %s: %v", name, errWrite)
	}
	modTime := time.Now().Add(-age)
	if errChtimes := os.Chtimes(path, modTime, modTime); errChtimes != nil {
		t.Fatalf("chtimes %s: %v", name, errChtimes)
	}
	return path
}

func TestIsLogFileNameMatchesReclaimableTempFiles(t *testing.T) {
	tests := []struct {
		name string
		want bool
	}{
		{name: "v1-messages-2026-01-01T000000-abcd.log", want: true},
		{name: "v1-messages-2026-01-01T000000-abcd.log.gz", want: true},
		{name: "request-body-123456.tmp", want: true},
		{name: "response-body-123456.tmp", want: true},
		{name: "api-request-123456.tmp", want: false},
		{name: "config.yaml", want: false},
		{name: "", want: false},
	}
	for i := range tests {
		if got := isLogFileName(tests[i].name); got != tests[i].want {
			t.Fatalf("isLogFileName(%q) = %t, want %t", tests[i].name, got, tests[i].want)
		}
	}
}

func TestSweepOrphanTempFilesRemovesOnlyOldTempFiles(t *testing.T) {
	dir := t.TempDir()

	oldRequest := writeTempFileWithAge(t, dir, "request-body-old.tmp", 2*time.Hour)
	oldResponse := writeTempFileWithAge(t, dir, "response-body-old.tmp", 90*time.Minute)
	unrelatedTemp := writeTempFileWithAge(t, dir, "part-old.tmp", 2*time.Hour)
	newRequest := writeTempFileWithAge(t, dir, "request-body-new.tmp", time.Minute)
	logFile := writeTempFileWithAge(t, dir, "v1-messages-old.log", 5*time.Hour)

	deleted, errSweep := sweepOrphanTempFiles(dir, orphanTempFileMinAge)
	if errSweep != nil {
		t.Fatalf("sweepOrphanTempFiles: %v", errSweep)
	}
	if deleted != 2 {
		t.Fatalf("deleted = %d, want 2", deleted)
	}
	for _, path := range []string{oldRequest, oldResponse} {
		if _, errStat := os.Stat(path); !os.IsNotExist(errStat) {
			t.Fatalf("expected %s to be removed, stat err = %v", filepath.Base(path), errStat)
		}
	}
	for _, path := range []string{newRequest, logFile} {
		if _, errStat := os.Stat(path); errStat != nil {
			t.Fatalf("expected %s to be kept: %v", filepath.Base(path), errStat)
		}
	}
	if _, errStat := os.Stat(unrelatedTemp); errStat != nil {
		t.Fatalf("expected unrelated temp file to be kept: %v", errStat)
	}
}

func TestSweepOrphanTempFilesToleratesMissingDir(t *testing.T) {
	deleted, errSweep := sweepOrphanTempFiles(filepath.Join(t.TempDir(), "missing"), orphanTempFileMinAge)
	if errSweep != nil {
		t.Fatalf("sweepOrphanTempFiles: %v", errSweep)
	}
	if deleted != 0 {
		t.Fatalf("deleted = %d, want 0", deleted)
	}
}

func TestNewFileRequestLoggerSweepsOrphanTempFiles(t *testing.T) {
	dir := t.TempDir()
	orphan := writeTempFileWithAge(t, dir, "request-body-orphan.tmp", 3*time.Hour)
	fresh := writeTempFileWithAge(t, dir, "response-body-fresh.tmp", time.Second)

	NewFileRequestLogger(false, dir, "", 0)

	if _, errStat := os.Stat(orphan); !os.IsNotExist(errStat) {
		t.Fatalf("expected orphan temp file to be swept, stat err = %v", errStat)
	}
	if _, errStat := os.Stat(fresh); errStat != nil {
		t.Fatalf("expected in-flight temp file to be kept: %v", errStat)
	}
}

func TestCleanupOldErrorLogsSweepsOrphanTempFiles(t *testing.T) {
	dir := t.TempDir()
	orphan := writeTempFileWithAge(t, dir, "response-body-orphan.tmp", 3*time.Hour)
	fresh := writeTempFileWithAge(t, dir, "request-body-fresh.tmp", time.Second)

	logger := &FileRequestLogger{logsDir: dir, errorLogsMaxFiles: 0}
	if errCleanup := logger.cleanupOldErrorLogs(); errCleanup != nil {
		t.Fatalf("cleanupOldErrorLogs: %v", errCleanup)
	}

	if _, errStat := os.Stat(orphan); !os.IsNotExist(errStat) {
		t.Fatalf("expected orphan temp file to be removed, stat err = %v", errStat)
	}
	if _, errStat := os.Stat(fresh); errStat != nil {
		t.Fatalf("expected in-flight temp file to be kept: %v", errStat)
	}
}

func TestEnforceLogDirSizeLimitKeepsInFlightTempFiles(t *testing.T) {
	dir := t.TempDir()

	oldTemp := writeTempFileWithAge(t, dir, "request-body-old.tmp", 2*time.Hour)
	newTemp := writeTempFileWithAge(t, dir, "request-body-new.tmp", time.Minute)
	logFile := writeTempFileWithAge(t, dir, "v1-messages.log", 3*time.Hour)

	// maxBytes of 1 forces the cleaner to reclaim everything it considers eligible.
	if _, errEnforce := enforceLogDirSizeLimit(dir, 1, ""); errEnforce != nil {
		t.Fatalf("enforceLogDirSizeLimit: %v", errEnforce)
	}

	if _, errStat := os.Stat(newTemp); errStat != nil {
		t.Fatalf("in-flight temp file must be kept: %v", errStat)
	}
	if _, errStat := os.Stat(oldTemp); !os.IsNotExist(errStat) {
		t.Fatalf("expected old temp file to be reclaimed, stat err = %v", errStat)
	}
	if _, errStat := os.Stat(logFile); !os.IsNotExist(errStat) {
		t.Fatalf("expected old log file to be reclaimed, stat err = %v", errStat)
	}
}
