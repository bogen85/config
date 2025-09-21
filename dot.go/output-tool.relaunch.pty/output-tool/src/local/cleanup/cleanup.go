package cleanup

import (
	"encoding/json"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"local/capture"
)

type Config struct {
	KeepCapture bool
	TTLMinutes  int
}

// WrapWithSignals runs run() and ensures cleanup of temp artifacts on exit/signals unless KeepCapture.
func WrapWithSignals(run func() error, meta *capture.Meta, cfg Config, capturePath, metaPath string) error {
	sigc := make(chan os.Signal, 4)
	signal.Notify(sigc, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	done := make(chan struct{})
	var runErr error
	go func() { runErr = run(); close(done) }()

	select {
	case <-sigc:
		// proceed to cleanup
	case <-done:
		// finished normally
	}

	if !cfg.KeepCapture && shouldCleanup(meta, capturePath, metaPath) {
		_ = os.Remove(capturePath)
		if metaPath != "" {
			_ = os.Remove(metaPath)
		}
	}

	// wait a moment for UI to unwind if signal case
	select {
	case <-done:
	case <-time.After(100 * time.Millisecond):
	}
	return runErr
}

func shouldCleanup(meta *capture.Meta, capturePath, metaPath string) bool {
	if meta == nil || !meta.Temp {
		return false
	}
	tmp := os.TempDir()
	okCap := strings.HasPrefix(capturePath, tmp+string(os.PathSeparator))
	okMeta := (metaPath == "") || strings.HasPrefix(metaPath, tmp+string(os.PathSeparator))
	return okCap && okMeta
}

func SweepOrphans(ttlMinutes int) {
	if ttlMinutes <= 0 {
		return
	}
	dir := os.TempDir()
	ents, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	cutoff := time.Now().Add(-time.Duration(ttlMinutes) * time.Minute)
	for _, e := range ents {
		name := e.Name()
		if !strings.HasPrefix(name, "ot-") || !strings.HasSuffix(name, ".meta.json") {
			continue
		}
		metaPath := filepath.Join(dir, name)
		info, err := e.Info()
		if err != nil || info.ModTime().After(cutoff) {
			continue
		}
		b, err := os.ReadFile(metaPath)
		if err != nil {
			continue
		}
		var m capture.Meta
		if json.Unmarshal(b, &m) != nil {
			continue
		}
		if !m.Temp {
			continue
		}
		_ = os.Remove(metaPath)
		if m.CapturePath != "" && strings.HasPrefix(m.CapturePath, dir+string(os.PathSeparator)) {
			_ = os.Remove(m.CapturePath)
		}
	}
}
