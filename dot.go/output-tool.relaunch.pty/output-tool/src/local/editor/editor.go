package editor

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"local/rules"
)

type Config struct {
	EditorExe       string
	EditorArgPrefix string
}

// LaunchForLine runs the configured editor and returns the argv used.
func LaunchForLine(line string, rs []rules.Rule, cfg Config) ([]string, error) {
	file, ln, col, ok := rules.ExtractPathLineCol(rs, line)
	if ok {
		target := file
		if ln > 0 && col > 0 {
			target = fmt.Sprintf("%s@%d@%d", file, ln, col)
		} else if ln > 0 {
			target = fmt.Sprintf("%s@%d", file, ln)
		}
		args := []string{}
		if cfg.EditorArgPrefix != "" {
			args = append(args, cfg.EditorArgPrefix)
		}
		args = append(args, target)
		return args, exec.Command(cfg.EditorExe, args...).Start()
	}
	// fallback: dump json
	tmp := filepath.Join(os.TempDir(), fmt.Sprintf("ot-line-%d.json", time.Now().UnixNano()))
	b, _ := json.MarshalIndent(map[string]any{"line": line}, "", "  ")
	_ = os.WriteFile(tmp, b, 0644)
	args := []string{}
	if cfg.EditorArgPrefix != "" {
		args = append(args, cfg.EditorArgPrefix)
	}
	args = append(args, tmp)
	return args, exec.Command(cfg.EditorExe, args...).Start()
}
