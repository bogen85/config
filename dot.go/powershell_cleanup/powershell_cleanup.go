package main

import (
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"syscall"
	"time"
)

// SpawnHiddenCleanup launches a hidden, detached PowerShell that tries to
// Remove-Item the given absolute directory path with retries. It does NOT wait.
func SpawnHiddenCleanup(initialDelay, retryDelay time.Duration, maxTries int, absDir string) error {
	if runtime.GOOS != "windows" {
		return fmt.Errorf("SpawnHiddenCleanup is Windows-only")
	}
	if maxTries < 1 {
		maxTries = 1
	}

	// Convert durations to milliseconds for PowerShell
	initMs := int(initialDelay / time.Millisecond)
	retryMs := int(retryDelay / time.Millisecond)

	// PowerShell single-quoted strings escape by doubling single quotes
	psPath := strings.ReplaceAll(absDir, "'", "''")

	tpl := `& {
  $initial=%d; $delay=%d; $max=%d; $p='%s';
  Start-Sleep -Milliseconds $initial;
  for ($i=0; $i -lt $max; $i++) {
    try {
      if (Test-Path -LiteralPath $p) {
        Remove-Item -LiteralPath $p -Recurse -Force -ErrorAction Stop
      }
      if (-not (Test-Path -LiteralPath $p)) { break }
    } catch { }
    if ($i -lt ($max-1)) { Start-Sleep -Milliseconds $delay }
  }
} *> $null`

	script := fmt.Sprintf(tpl, initMs, retryMs, maxTries, psPath)

	// Build the powershell command: hidden, non-interactive, no profile
	cmd := exec.Command("powershell.exe",
		"-NoProfile",
		"-NonInteractive",
		"-WindowStyle", "Hidden",
		"-Command", script,
	)

	// Fully detach + hide window so no console appears and we don't wait
	cmd.SysProcAttr = &syscall.SysProcAttr{
		HideWindow:    true,
		CreationFlags: syscall.CREATE_NEW_PROCESS_GROUP | syscall.DETACHED_PROCESS,
	}

	// Don’t inherit any standard handles from parent
	// (DETACHED_PROCESS prevents a console window anyway; this is extra belt+suspenders)
	null, _ := os.Open(os.DevNull)
	defer null.Close()
	cmd.Stdin = null
	cmd.Stdout = null
	cmd.Stderr = null

	// Start (do NOT Wait) — fire-and-forget
	return cmd.Start()
}
