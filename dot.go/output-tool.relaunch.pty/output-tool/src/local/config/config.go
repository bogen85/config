package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/BurntSushi/toml"

	"local/cleanup"
	"local/editor"
	"local/launcher"
	"local/viewer"
)

// ---------- Types ----------

type Rule struct {
	ID          string `toml:"id"`
	Regex       string `toml:"regex"`
	FileGroup   int    `toml:"file_group"`
	LineGroup   int    `toml:"line_group"`
	ColumnGroup int    `toml:"column_group"`
}

type Behavior struct {
	OnlyViewMatches bool   `toml:"only_view_matches"`
	OnlyOnMatches   bool   `toml:"only_on_matches"`
	MatchStderr     string `toml:"match_stderr"` // none|line
}

type Config struct {
	Rules    []Rule          `toml:"rules"`
	Viewer   viewer.Options  `toml:"viewer"`
	Editor   editor.Config   `toml:"editor"`
	Launcher launcher.Config `toml:"launcher"`
	Behavior Behavior        `toml:"behavior"`
	Cleanup  cleanup.Config  `toml:"cleanup"`
}

// ---------- Defaults ----------

func Default(bexe string) *Config {
	return &Config{
		Rules: []Rule{
			{
				ID:          "path:line:col",
				Regex:       `(?:\.?\.?\/)?([A-Za-z0-9._\/\-]+):(\d+):(\d+)`,
				FileGroup:   1,
				LineGroup:   2,
				ColumnGroup: 3,
			},
		},
		Viewer: viewer.Options{
			Title:         fmt.Sprintf("%s Viewer", bexe),
			GutterWidth:   6,
			ShowTopBar:    true,
			ShowBottomBar: true,
			Mouse:         true,
			NoAlt:         false,
		},
		Editor: editor.Config{
			File:        []string{"cudatext", "${__FILE__}"},
			FileLine:    []string{"cudatext", "${__FILE__}@${__LINE__}"},
			FileLineCol: []string{"cudatext", "${__FILE__}@${__LINE__}@${__COLUMN__}"},
			PrettyJSON:  true,
		},
		Launcher: launcher.Config{
			TermPrefix: "xfce4-terminal --hide-menubar --hide-scrollbar --hide-toolbar --title='OutputTool' --command",
			TmuxPrefix: "tmux new-window --",
			PreferTmux: true,
		},
		Behavior: Behavior{
			OnlyViewMatches: false,
			OnlyOnMatches:   false,
			MatchStderr:     "line",
		},
		Cleanup: cleanup.Config{
			KeepCapture: false,
			TTLMinutes:  5,
		},
	}
}

// ---------- Paths & Resolution ----------

func baseExeName(args0 string) string {
	// no symlink resolution
	return filepath.Base(args0)
}

func xdg(pathEnv, fallback string) string {
	if v := os.Getenv(pathEnv); v != "" {
		return v
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, fallback)
}

func defaultConfigDir() string {
	return filepath.Join(
		xdg("XDG_CONFIG_HOME", ".config"),
		"user-dev-tooling",
		"output-tool",
	)
}

type Origin string

const (
	OriginExplicit      Origin = "explicit"
	OriginDefaultToken  Origin = "default-token"
	OriginProjectLocalA Origin = "project-local-a"
	OriginProjectLocalB Origin = "project-local-b"
	OriginXDGDefault    Origin = "xdg-default"
)

// Resolve returns the chosen path, whether /default token was used, and an origin tag.
func Resolve(cliConfig, args0 string) (path string, isDefaultToken bool, origin Origin) {
	bexe := baseExeName(args0)
	defPath := filepath.Join(defaultConfigDir(), fmt.Sprintf("%s-config.toml", bexe))

	if cliConfig != "" {
		if cliConfig == "/default" {
			return defPath, true, OriginDefaultToken
		}
		return cliConfig, false, OriginExplicit
	}

	// Not specified: lookup order
	// (a) ./output-tool-config.toml if bexename == "output-tool"
	if bexe == "output-tool" {
		a := "./output-tool-config.toml"
		if _, err := os.Stat(a); err == nil {
			return a, false, OriginProjectLocalA
		}
	}
	// (b) ./output-tool-<bexename>-config.toml
	b := fmt.Sprintf("./output-tool-%s-config.toml", bexe)
	if _, err := os.Stat(b); err == nil {
		return b, false, OriginProjectLocalB
	}
	// (c) XDG config dir / <bexename>-config.toml (default)
	return defPath, false, OriginXDGDefault
}

// EnsureDir ensures parent dir for path exists.
func EnsureDir(path string) error {
	dir := filepath.Dir(path)
	return os.MkdirAll(dir, 0o755)
}

// ---------- Load / Save ----------

func Load(path string) (*Config, error) {
	// if file doesn't exist, return (nil, os.ErrNotExist)
	if st, err := os.Stat(path); err != nil || st.IsDir() {
		if err == nil {
			return nil, errors.New("config path is a directory")
		}
		return nil, err
	}
	var cfg Config
	if _, err := toml.DecodeFile(path, &cfg); err != nil {
		return nil, err
	}
	return &cfg, nil
}

type WriteStatus int

const (
	WroteNew WriteStatus = iota
	WroteOverwritten
	NotWrittenExists
	NotWrittenCanceled
)

func (s WriteStatus) String() string {
	switch s {
	case WroteNew:
		return "written"
	case WroteOverwritten:
		return "overwritten"
	case NotWrittenExists:
		return "not written (exists)"
	case NotWrittenCanceled:
		return "not written (canceled)"
	default:
		return "unknown"
	}
}

// Save writes cfg to path. If the file exists and force==false, it returns NotWrittenExists.
func Save(path string, cfg *Config, force bool) (WriteStatus, error) {
	if err := EnsureDir(path); err != nil {
		return NotWrittenExists, err
	}

	if _, err := os.Stat(path); err == nil && !force {
		return NotWrittenExists, nil
	}

	// Write atomically
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return NotWrittenExists, err
	}
	enc := toml.NewEncoder(f)
	if err := enc.Encode(cfg); err != nil {
		f.Close()
		_ = os.Remove(tmp)
		return NotWrittenExists, err
	}
	if err := f.Close(); err != nil {
		return NotWrittenExists, err
	}
	if err := os.Rename(tmp, path); err != nil {
		return NotWrittenExists, err
	}

	if _, err := os.Stat(path); err == nil && force {
		return WroteOverwritten, nil
	}
	return WroteNew, nil
}

// ---------- Helpers to convert between config.Rules and compiled rules ----------

type CompiledRule struct {
	ID          string
	Regex       string
	FileGroup   int
	LineGroup   int
	ColumnGroup int
}

func (c *Config) ToCompiledRules() []CompiledRule {
	out := make([]CompiledRule, 0, len(c.Rules))
	for _, r := range c.Rules {
		out = append(out, CompiledRule{
			ID:          r.ID,
			Regex:       r.Regex,
			FileGroup:   r.FileGroup,
			LineGroup:   r.LineGroup,
			ColumnGroup: r.ColumnGroup,
		})
	}
	return out
}

// CleanPath pretty-prints the path for logs (no changes; here just trim).
func CleanPath(p string) string {
	return strings.TrimSpace(p)
}
