package editor

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"time"

	"local/rules"
)

// Config: only templated argv variants (preferred).
// Each slice is a full argv (no shell), with ${...} expansion.
// Available vars in templates and child env: __FILE__, __LINE__, __COLUMN__, PWD,
// plus any existing environment variable (fallback during expansion).

type Config struct {
	// Templated definitions (preferred). Each is a full argv with ${...} expansion.
	// Available vars: __FILE__, __LINE__, __COLUMN__, PWD (plus any environment var).
	File        []string `toml:"file"`          // e.g. ["cudatext", "${__FILE__}"]
	FileLine    []string `toml:"file_line"`     // e.g. ["cudatext", "${__FILE__}@${__LINE__}"]
	FileLineCol []string `toml:"file_line_col"` // e.g. ["cudatext", "${__FILE__}@${__LINE__}@${__COLUMN__}"]
	PrettyJSON  bool     `toml:"pretty_json"`
}

// LaunchForLine builds argv from the configured templates, sets editor vars,
// starts the editor (non-blocking), and returns the argv used.
//
// Behavior:
//   - If the line yields (file,line,col):
//   - prefer FileLineColDef, else FileLineDef, else FileDef
//   - Else (no file/line found): write a temp JSON and use FileDef with __FILE__=that path
func LaunchForLine(line string, rs []rules.Rule, cfg Config) ([]string, error) {
	file, ln, col, ok := rules.ExtractPathLineCol(rs, line)

	// If no (file,line) extracted, write a small JSON payload and use that path as __FILE__.
	if !ok {
		tmp := filepath.Join(os.TempDir(), fmt.Sprintf("ot-line-%d.json", time.Now().UnixNano()))
		if err := writeJSON(tmp, line, cfg.PrettyJSON); err != nil {
			return nil, err
		}
		file, ln, col = tmp, 0, 0
	}

	// Choose a template.
	var tmpl []string
	switch {
	case col > 0 && ln > 0 && len(cfg.FileLineCol) > 0:
		tmpl = cfg.FileLineCol
	case ln > 0 && len(cfg.FileLine) > 0:
		tmpl = cfg.FileLine
	default:
		tmpl = cfg.File
	}
	if len(tmpl) == 0 {
		return nil, errors.New("editor: no template configured (need [editor].file / file_line / file_line_col)")
	}

	// Vars for expansion + child env.
	pwd, _ := os.Getwd()
	vars := map[string]string{
		"PWD":        pwd,
		"__FILE__":   file,
		"__LINE__":   strconv.Itoa(ln),
		"__COLUMN__": strconv.Itoa(col),
	}

	argv := expandArgs(tmpl, vars)
	if len(argv) == 0 || argv[0] == "" {
		return argv, errors.New("editor: empty argv after expansion")
	}

	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Env = withVars(os.Environ(), vars) // inject our vars
	return argv, cmd.Start()
}

func writeJSON(path string, line string, pretty bool) error {
	payload := struct {
		Line string `json:"line"`
	}{
		Line: line,
	}
	var b []byte
	if pretty {
		b, _ = json.MarshalIndent(payload, "", "\t") // switch to 4 spaces if you prefer
	} else {
		b, _ = json.Marshal(payload)
	}
	return os.WriteFile(path, b, 0644)
}

func expandArgs(tmpl []string, vars map[string]string) []string {
	lookup := func(key string) string {
		if v, ok := vars[key]; ok {
			return v
		}
		return os.Getenv(key)
	}
	out := make([]string, 0, len(tmpl))
	for _, a := range tmpl {
		out = append(out, os.Expand(a, lookup))
	}
	return out
}

func withVars(env []string, vars map[string]string) []string {
	out := make([]string, 0, len(env)+len(vars))
	out = append(out, env...)
	for k, v := range vars {
		out = append(out, k+"="+v)
	}
	return out
}
