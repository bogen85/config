package launcher

import (
	"fmt"
	"os/exec"
	"strings"

	"local/util"
)

type Config struct {
	LauncherPrefix string
	ViewerTitle    string
	OnlyView       bool
	Mouse          bool
	KeepCapture    bool
	CleanupTTLMin  int
	DryRun         bool
}

func SpawnTerminalViewer(cfg Config, selfExe, capturePath, metaPath string) error {
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

	parts := util.SplitLauncher(cfg.LauncherPrefix)
	if len(parts) == 0 {
		return fmt.Errorf("invalid launcher")
	}
	args := append(parts[1:], inner.String())
	if cfg.DryRun {
		fmt.Printf("DRY LAUNCH: %s %s\n", parts[0], strings.Join(args, " "))
		return nil
	}
	cmd := exec.Command(parts[0], args...)
	return cmd.Start()
}
