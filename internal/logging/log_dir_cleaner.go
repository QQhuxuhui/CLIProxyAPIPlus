package logging

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	log "github.com/sirupsen/logrus"
)

const logDirCleanerInterval = time.Minute

// orphanTempFileMinAge is the minimum age a temp file must reach before it is treated as
// orphaned. Younger temp files may still belong to in-flight requests.
const orphanTempFileMinAge = time.Hour

var logDirCleanerCancel context.CancelFunc

func configureLogDirCleanerLocked(logDir string, maxTotalSizeMB int, protectedPath string) {
	stopLogDirCleanerLocked()

	if maxTotalSizeMB <= 0 {
		return
	}

	maxBytes := int64(maxTotalSizeMB) * 1024 * 1024
	if maxBytes <= 0 {
		return
	}

	dir := strings.TrimSpace(logDir)
	if dir == "" {
		return
	}

	ctx, cancel := context.WithCancel(context.Background())
	logDirCleanerCancel = cancel
	go runLogDirCleaner(ctx, filepath.Clean(dir), maxBytes, strings.TrimSpace(protectedPath))
}

func stopLogDirCleanerLocked() {
	if logDirCleanerCancel == nil {
		return
	}
	logDirCleanerCancel()
	logDirCleanerCancel = nil
}

func runLogDirCleaner(ctx context.Context, logDir string, maxBytes int64, protectedPath string) {
	ticker := time.NewTicker(logDirCleanerInterval)
	defer ticker.Stop()

	cleanOnce := func() {
		deleted, errClean := enforceLogDirSizeLimit(logDir, maxBytes, protectedPath)
		if errClean != nil {
			log.WithError(errClean).Warn("logging: failed to enforce log directory size limit")
			return
		}
		if deleted > 0 {
			log.Debugf("logging: removed %d old log file(s) to enforce log directory size limit", deleted)
		}
	}

	cleanOnce()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			cleanOnce()
		}
	}
}

func enforceLogDirSizeLimit(logDir string, maxBytes int64, protectedPath string) (int, error) {
	if maxBytes <= 0 {
		return 0, nil
	}

	dir := strings.TrimSpace(logDir)
	if dir == "" {
		return 0, nil
	}
	dir = filepath.Clean(dir)

	entries, errRead := os.ReadDir(dir)
	if errRead != nil {
		if os.IsNotExist(errRead) {
			return 0, nil
		}
		return 0, errRead
	}

	protected := strings.TrimSpace(protectedPath)
	if protected != "" {
		protected = filepath.Clean(protected)
	}

	type logFile struct {
		path    string
		size    int64
		modTime time.Time
	}

	var (
		files []logFile
		total int64
	)
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if !isLogFileName(name) {
			continue
		}
		info, errInfo := entry.Info()
		if errInfo != nil {
			continue
		}
		if !info.Mode().IsRegular() {
			continue
		}
		if isReclaimableTempFileName(name) && time.Since(info.ModTime()) < orphanTempFileMinAge {
			continue
		}
		path := filepath.Join(dir, name)
		files = append(files, logFile{
			path:    path,
			size:    info.Size(),
			modTime: info.ModTime(),
		})
		total += info.Size()
	}

	if total <= maxBytes {
		return 0, nil
	}

	sort.Slice(files, func(i, j int) bool {
		return files[i].modTime.Before(files[j].modTime)
	})

	deleted := 0
	for _, file := range files {
		if total <= maxBytes {
			break
		}
		if protected != "" && filepath.Clean(file.path) == protected {
			continue
		}
		if errRemove := os.Remove(file.path); errRemove != nil {
			log.WithError(errRemove).Warnf("logging: failed to remove old log file: %s", filepath.Base(file.path))
			continue
		}
		total -= file.size
		deleted++
	}

	return deleted, nil
}

func isLogFileName(name string) bool {
	trimmed := strings.TrimSpace(name)
	if trimmed == "" {
		return false
	}
	lower := strings.ToLower(trimmed)
	if strings.HasSuffix(lower, ".log") || strings.HasSuffix(lower, ".log.gz") {
		return true
	}
	return isReclaimableTempFileName(lower)
}

// isReclaimableTempFileName reports whether name is a request/response body temp file that
// the request logger leaves in the logs directory when a process dies mid-request.
func isReclaimableTempFileName(name string) bool {
	lower := strings.ToLower(strings.TrimSpace(name))
	if !strings.HasSuffix(lower, ".tmp") {
		return false
	}
	return strings.HasPrefix(lower, "request-body-") || strings.HasPrefix(lower, "response-body-")
}

// sweepOrphanTempFiles removes temp files in the logs directory that are older than minAge.
// Temp files younger than minAge are kept because they may belong to in-flight requests.
//
// Parameters:
//   - logDir: The logs directory to sweep
//   - minAge: The minimum age a temp file must reach before it is removed
//
// Returns:
//   - int: The number of removed temp files
//   - error: An error if the directory cannot be read, nil otherwise
func sweepOrphanTempFiles(logDir string, minAge time.Duration) (int, error) {
	dir := strings.TrimSpace(logDir)
	if dir == "" {
		return 0, nil
	}
	dir = filepath.Clean(dir)

	entries, errRead := os.ReadDir(dir)
	if errRead != nil {
		if os.IsNotExist(errRead) {
			return 0, nil
		}
		return 0, errRead
	}

	deleted := 0
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if !isReclaimableTempFileName(name) {
			continue
		}
		info, errInfo := entry.Info()
		if errInfo != nil {
			continue
		}
		if !info.Mode().IsRegular() {
			continue
		}
		if time.Since(info.ModTime()) < minAge {
			continue
		}
		if errRemove := os.Remove(filepath.Join(dir, name)); errRemove != nil {
			log.WithError(errRemove).Warnf("logging: failed to remove orphan temp file: %s", name)
			continue
		}
		deleted++
	}

	return deleted, nil
}
