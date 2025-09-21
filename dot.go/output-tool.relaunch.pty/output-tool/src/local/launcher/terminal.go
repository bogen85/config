package launcher

import (
	"fmt"
	"os/exec"
	"strings"

	"local/tmux"
	"local/util"
)

type Config struct {
	LauncherPrefix string // graphical terminal prefix
	TmuxPrefix     string // tmux popup prefix
	PreferTmux     bool   // prefer tmux when available

	ViewerTitle   string
	OnlyView      bool
	Mouse         bool
	KeepCapture   bool
	CleanupTTLMin int
	DryRun        bool

	ForceTmux bool // CLI override: force tmux
	NoTmux    bool // CLI override: disable tmux
}

func SpawnTerminalViewer(cfg Config, selfExe, capturePath, metaPath string) error {
	// Build inner command once
	var inner strings.Builder
	inner.WriteString(util.ShellQuote(selfExe))
	inner.WriteString(" --view ")
	inner.WriteString("--capture=" + util.ShellQuote(capturePath) + " ")
	if metaPath != "" {
		inner.WriteString("--meta=" + util.ShellQuote(metaPath) + " ")
	}
	if cfg.OnlyView {
		inner.WriteString("--only-view-matches ")
	}
	if cfg.ViewerTitle != "" {
		inner.WriteString("--viewer-title=" + util.ShellQuote(cfg.ViewerTitle) + " ")
	}
	if cfg.Mouse {
		inner.WriteString("--mouse ")
	}
	if cfg.KeepCapture {
		inner.WriteString("--keep-capture ")
	}
	if cfg.CleanupTTLMin != 90 {
		inner.WriteString(fmt.Sprintf("--cleanup-ttl-minutes=%d ", cfg.CleanupTTLMin))
	}

	innerCmd := inner.String()

	// Decide tmux vs graphical
	useTmux := false
	if !cfg.NoTmux {
		if cfg.ForceTmux {
			useTmux = true
		} else if tmux.InTmux() && (cfg.PreferTmux || cfg.TmuxPrefix != "") && tmux.SupportsPopups() {
			useTmux = true
		}
	}

	if useTmux && cfg.TmuxPrefix != "" {
		// tmux popup: pass the whole inner command as a single argument
		// Example default: "tmux display-popup -E -w 100% -h 100% --"
		parts := util.SplitLauncher(cfg.TmuxPrefix)
		if len(parts) == 0 {
			return fmt.Errorf("invalid tmux_prefix")
		}
		// If tmux prefix ends with "--", we can append the program/args directly.
		// Otherwise, pass a single string for -E (exec) to run without a shell.
		args := append(parts[1:], innerCmd)
		if cfg.DryRun {
			fmt.Printf("DRY LAUNCH (tmux): %s %s\n", parts[0], strings.Join(args, " "))
			return nil
		}
		cmd := exec.Command(parts[0], args...)
		return cmd.Start()
	}

	// Fallback to graphical terminal
	parts := util.SplitLauncher(cfg.LauncherPrefix)
	if len(parts) == 0 {
		return fmt.Errorf("invalid launcher prefix")
	}
	args := append(parts[1:], innerCmd)
	if cfg.DryRun {
		fmt.Printf("DRY LAUNCH: %s %s\n", parts[0], strings.Join(args, " "))
		return nil
	}
	cmd := exec.Command(parts[0], args...)
	return cmd.Start()
}
