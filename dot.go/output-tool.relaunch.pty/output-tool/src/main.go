package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/BurntSushi/toml"

	"local/capture"
	"local/cleanup"
	"local/config"
	"local/editor"
	"local/execcap"
	"local/launcher"
	"local/rules"
	"local/viewer"
)

var (
	defaultConfig = config.Default(baseExe(os.Args[0]))
	// Config
	flagConfigPath  = flag.String("config", "", "Path to config TOML (use /default to resolve to XDG path)")
	flagNewConfig   = flag.Bool("write-new-config", false, "Write a new config TOML and exit (flags override defaults)")
	flagForceConfig = flag.Bool("force", false, "Allow overwriting config when writing a new one")

	// Modes
	flagPipe = flag.Bool("pipe", false, "Read from stdin; stream to stdout, capture JSONL, and (optionally) launch viewer in a new terminal")
	flagFile = flag.String("file", "", "Read from file PATH and view inline")
	flagExec = flag.Bool("exec", true, "If extra args are present, run them as a command (set --exec=false to forbid)")

	// Pipe behavior
	flagOnlyView    = flag.Bool("only-view-matches", defaultConfig.Behavior.OnlyViewMatches, "Viewer shows only matching lines (capture filtered in pipe)")
	flagOnlyOnMatch = flag.Bool("only-on-matches", defaultConfig.Behavior.OnlyOnMatches, "Do not launch viewer when no matches were seen")
	flagMatchStderr = flag.String("match-stderr", "line", "During --pipe, echo matches to stderr: none|line")

	// Viewer internal
	flagView        = flag.Bool("view", false, "Internal: run viewer on a capture JSONL file")
	flagCapturePath = flag.String("capture", "", "Internal: capture JSONL path for --view")
	flagMetaPath    = flag.String("meta", "", "Internal: meta.json path for --view")

	// Viewer options
	flagViewerTitle = flag.String("viewer-title", "OutputTool Viewer", "Viewer window title")
	flagGutterWidth = flag.Int("gutter-width", defaultConfig.Viewer.GutterWidth, "Fixed gutter width for line numbers")
	flagTopBar      = flag.Bool("top-bar", defaultConfig.Viewer.ShowTopBar, "Show top status bar")
	flagBottomBar   = flag.Bool("bottom-bar", defaultConfig.Viewer.ShowBottomBar, "Show bottom status bar")
	flagErrLines    = flag.Int("err-lines", 5, "Max lines for bottom error/log pane")
	flagNoAlt       = flag.Bool("no-alt", defaultConfig.Viewer.NoAlt, "Do not use terminal alt screen (debug)")
	flagMouse       = flag.Bool("mouse", defaultConfig.Viewer.Mouse, "Enable mouse tracking (disables terminal text selection)")

	// Launcher (pipe -> new terminal)
	flagLauncher  = flag.String("launcher", "xfce4-terminal --hide-menubar --hide-scrollbar --hide-toolbar --title='OutputTool' --command", "Terminal launcher prefix")
	flagDryLaunch = flag.Bool("dry-launch", false, "Pipe-mode: print the launch command and do not spawn")

	// Cleanup behavior
	flagKeepCapture = flag.Bool("keep-capture", defaultConfig.Cleanup.KeepCapture, "Viewer: keep capture/meta files (skip auto-cleanup)")
	flagTTLMinutes  = flag.Int("cleanup-ttl-minutes", defaultConfig.Cleanup.TTLMinutes, "Viewer: sweep temp orphans older than this many minutes on startup")

	// Tmux
	flagTmuxForce = flag.Bool("tmux", false, "Force tmux popup when launching viewer (overrides config)")
	flagTmuxOff   = flag.Bool("no-tmux", false, "Disable tmux popup even if available (overrides config)")

	// Help / utilities
	flagUsage             = flag.Bool("usage", false, "Show usage")
	flagPrintEffectiveCfg = flag.Bool("print-effective-config", false, "Print merged config (defaults -> file -> CLI) as TOML and exit")
	flagWhichConfig       = flag.Bool("which-config", false, "Print the resolved config path and origin, then exit")
	flagDebugLaunch       = flag.Bool("debug-launch", false, "Print tmux/launch decision inputs (implies --dry-launch)")
)

func usage() {
	fmt.Fprintf(os.Stdout, `Usage:
  output-tool --pipe [--only-view-matches] [--only-on-matches] [--match-stderr=none|line] [--launcher="..."] [--mouse]
  output-tool --file=PATH [--only-view-matches] [--mouse]
  output-tool --view --capture=/tmp/ot-XXXX.jsonl --meta=/tmp/ot-XXXX.meta.json   (internal)

Config:
  --config=/default     Use ${XDG_CONFIG_HOME:-~/.config}/user-dev-tooling/output-tool/<bexename>-config.toml
  --output-new-config   Write a new config TOML and exit (respects --config and --force)
  --force               Overwrite config if it exists (when writing)

Notes:
  - Pipe mode acts like 'cat': streams stdin to stdout in real time, scans matches, writes JSONL capture and meta.
  - After streaming: if (--only-on-matches && none), exits quietly. Otherwise spawns terminal with viewer and exits.
  - File mode reads file, builds capture in-memory, and runs tcell viewer inline.
`)
}

func main() {
	flag.Parse()
	if *flagUsage {
		usage()
		return
	}

	// --- Resolve config path
	cfgPath, isDefault, cfgOrigin := config.Resolve(*flagConfigPath, os.Args[0])

	if *flagWhichConfig {
		fmt.Printf("config: path=%s origin=%s\n", config.CleanPath(cfgPath), cfgOrigin)
		return
	}

	// --- Write new config and exit?
	if *flagNewConfig {
		cfg := configFromCurrentFlags(os.Args[0]) // defaults + CLI overrides
		st, err := config.Save(cfgPath, cfg, *flagForceConfig)
		switch st {
		case config.WroteNew:
			fmt.Printf("config: written %s\n", config.CleanPath(cfgPath))
		case config.WroteOverwritten:
			fmt.Printf("config: overwritten %s (forced)\n", config.CleanPath(cfgPath))
		case config.NotWrittenExists:
			fmt.Printf("config: not written (exists) %s\n", config.CleanPath(cfgPath))
		case config.NotWrittenCanceled:
			fmt.Printf("config: not written (canceled) %s\n", config.CleanPath(cfgPath))
		}
		if err != nil {
			fmt.Fprintf(os.Stderr, "config: error: %v\n", err)
			os.Exit(1)
		}
		return
	}

	// --- Load config (if present), apply to flags not set explicitly
	cfg, err := config.Load(cfgPath)
	if err == nil {
		fmt.Printf("config: loaded %s (origin=%s)\n", config.CleanPath(cfgPath), cfgOrigin)
	} else {
		// If path is /default and not found, that's fine; we proceed with compiled defaults.
		// Only print a note if user explicitly pointed at a path that doesn't exist.
		if *flagConfigPath != "" && !isDefault {
			fmt.Printf("config: not found %s (using compiled defaults)\n", config.CleanPath(cfgPath))
		}
		cfg = defaultConfig // fallback for rules
	}

	// Apply config values to flags not set on CLI (works for both: loaded or default)
	applyConfigToFlagsIfNotSet(cfg)

	if *flagPrintEffectiveCfg {
		// Comment header with path/origin + names of CLI-overridden flags
		overridden := []string{}
		flag.Visit(func(f *flag.Flag) { overridden = append(overridden, f.Name) })
		fmt.Printf("# effective config (merged: defaults -> %s -> CLI)\n", config.CleanPath(cfgPath))
		fmt.Printf("# origin: %s\n", cfgOrigin)
		if len(overridden) > 0 {
			fmt.Printf("# overridden flags: %s\n", strings.Join(overridden, ", "))
		} else {
			fmt.Printf("# overridden flags: (none)\n")
		}
		eff := configFromCurrentFlags(os.Args[0])
		enc := toml.NewEncoder(os.Stdout)
		if err := enc.Encode(eff); err != nil {
			fmt.Fprintf(os.Stderr, "print-effective-config: %v\n", err)
			os.Exit(1)
		}
		return
	}

	// --- Viewer internal mode
	if *flagView {
		runViewerWithCleanup(*flagCapturePath, *flagMetaPath, cfg)
		return
	}

	// modes: exactly one of pipe|file|exec
	args := flag.Args()
	execImplied := *flagExec && len(args) > 0

	modes := 0
	if *flagPipe {
		modes++
	}
	if *flagFile != "" {
		modes++
	}
	if execImplied {
		modes++
	}
	if modes != 1 {
		if len(args) > 0 && !*flagExec {
			fmt.Fprintln(os.Stderr, "error: extra arguments present but --exec=false was set")
		}
		usage()
		os.Exit(2)
	}

	// compile rules from cfg
	rs := compileRules(cfg)
	if *flagPipe {
		runPipe(rs, cfg)
		return
	}
	if *flagFile != "" {
		runFile(rs, cfg, *flagFile)
		return
	}
	if execImplied {
		runExec(rs, cfg, args)
		return
	}

}

// ---------- Config <-> Flags glue ----------

func baseExe(args0 string) string { return filepathBase(args0) }
func filepathBase(p string) string {
	i := strings.LastIndexByte(p, '/')
	if i < 0 {
		return p
	}
	return p[i+1:]
}

func configFromCurrentFlags(args0 string) *config.Config {
	cfg := defaultConfig
	// Viewer
	cfg.Viewer.Title = *flagViewerTitle
	cfg.Viewer.GutterWidth = *flagGutterWidth
	cfg.Viewer.ShowTopBar = *flagTopBar
	cfg.Viewer.ShowBottomBar = *flagBottomBar
	cfg.Viewer.Mouse = *flagMouse
	cfg.Viewer.NoAlt = *flagNoAlt

	// Launcher
	cfg.Launcher.TermPrefix = *flagLauncher

	// Behavior
	cfg.Behavior.OnlyViewMatches = *flagOnlyView
	cfg.Behavior.OnlyOnMatches = *flagOnlyOnMatch
	cfg.Behavior.MatchStderr = *flagMatchStderr
	// Cleanup
	cfg.Cleanup.KeepCapture = *flagKeepCapture
	cfg.Cleanup.TTLMinutes = *flagTTLMinutes
	return cfg
}

func applyConfigToFlagsIfNotSet(cfg *config.Config) {
	set := map[string]bool{}
	flag.Visit(func(f *flag.Flag) { set[f.Name] = true })

	// Viewer
	if !set["viewer-title"] {
		*flagViewerTitle = cfg.Viewer.Title
	}
	if !set["gutter-width"] && cfg.Viewer.GutterWidth > 0 {
		*flagGutterWidth = cfg.Viewer.GutterWidth
	}
	if !set["top-bar"] {
		*flagTopBar = cfg.Viewer.ShowTopBar
	}
	if !set["bottom-bar"] {
		*flagBottomBar = cfg.Viewer.ShowBottomBar
	}
	if !set["mouse"] {
		*flagMouse = cfg.Viewer.Mouse
	}
	if !set["no-alt"] {
		*flagNoAlt = cfg.Viewer.NoAlt
	}
	// Launcher
	if !set["launcher"] && cfg.Launcher.TermPrefix != "" {
		*flagLauncher = cfg.Launcher.TermPrefix
	}
	// Behavior
	if !set["only-view-matches"] {
		*flagOnlyView = cfg.Behavior.OnlyViewMatches
	}
	if !set["only-on-matches"] {
		*flagOnlyOnMatch = cfg.Behavior.OnlyOnMatches
	}
	if !set["match-stderr"] && cfg.Behavior.MatchStderr != "" {
		*flagMatchStderr = cfg.Behavior.MatchStderr
	}
	// Cleanup
	if !set["keep-capture"] {
		*flagKeepCapture = cfg.Cleanup.KeepCapture
	}
	if !set["cleanup-ttl-minutes"] && cfg.Cleanup.TTLMinutes > 0 {
		*flagTTLMinutes = cfg.Cleanup.TTLMinutes
	}
}

// compileRules builds []rules.Rule from cfg.Rules
func compileRules(cfg *config.Config) []rules.Rule {
	if cfg == nil || len(cfg.Rules) == 0 {
		return rules.Default()
	}
	out := make([]rules.Rule, 0, len(cfg.Rules))
	for _, r := range cfg.Rules {
		re, err := regexp.Compile(r.Regex)
		if err != nil {
			// invalid regex → skip
			continue
		}
		out = append(out, rules.Rule{
			ID:          r.ID,
			Regex:       re,
			FileGroup:   r.FileGroup,
			LineGroup:   r.LineGroup,
			ColumnGroup: r.ColumnGroup,
		})
	}
	if len(out) == 0 {
		return rules.Default()
	}
	return out
}

func editorConfig(cfg *config.Config) editor.Config {
	return editor.Config{
		File:        cfg.Editor.File,
		FileLine:    cfg.Editor.FileLine,
		FileLineCol: cfg.Editor.FileLineCol,
		PrettyJSON:  cfg.Editor.PrettyJSON,
	}
}

// ---------- Pipe / File / Viewer implementations ----------
func runExec(rs []rules.Rule, cfg *config.Config, cmdArgs []string) {
	// run command & capture
	res, err := execcap.Run(cmdArgs, rs, execcap.Options{
		OnlyViewMatches: *flagOnlyView,
		MatchStderr:     *flagMatchStderr,
	})
	if err != nil {
		fatalf("exec: %v", err)
	}

	// respect only-on-matches
	if *flagOnlyOnMatch && !res.AnyMatch {
		_ = os.Remove(res.CapturePath)
		_ = os.Remove(res.CapturePath + ".meta.json") // in case future changes wrote meta separately
		// Propagate child exit code
		if res.ExitCode != 0 {
			os.Exit(res.ExitCode)
		}
		return // zero code
	}

	run := func() error {
		// meta is already in res.Meta (Temp=false)
		return viewer.RunFromFile(res.CapturePath, &res.Meta, rs, viewerOptions(), viewer.Hooks{
			OnActivate: func(lineText string) ([]string, error) {
				return editor.LaunchForLine(lineText, rs, editorConfig(cfg))
			},
		})
	}
	ccfg := cleanup.Config{KeepCapture: *flagKeepCapture, TTLMinutes: *flagTTLMinutes}
	if err := cleanup.WrapWithSignals(run, &res.Meta, ccfg, res.CapturePath, res.CapturePath+".meta.json"); err != nil {
		fatalf("viewer: %v", err)
	}
}

func runPipe(rs []rules.Rule, cfg *config.Config) {
	// Create temp writer
	wr, err := capture.NewTempWriter("ot-")
	if err != nil {
		fatalf("capture: %v", err)
	}
	defer wr.Close()

	in := bufio.NewReaderSize(os.Stdin, 64*1024)
	out := bufio.NewWriterSize(os.Stdout, 64*1024)
	errw := bufio.NewWriterSize(os.Stderr, 64*1024)
	defer out.Flush()
	defer errw.Flush()

	any := false
	lineNo := 0
	linesTotal := 0
	matchLines := 0
	matchesTotal := 0

	enc := json.NewEncoder(wr.Writer())

	for {
		line, err := in.ReadString('\n')
		if errors.Is(err, io.EOF) {
			if len(line) == 0 {
				break
			}
		} else if err != nil {
			break
		}
		line = strings.TrimRight(line, "\r\n")
		lineNo++
		linesTotal++

		// stream to stdout
		out.WriteString(line)
		out.WriteByte('\n')

		matched, count := rules.AnyMatch(rs, line)
		if matched {
			any = true
			matchLines++
			matchesTotal += count
			if *flagMatchStderr == "line" {
				fmt.Fprintf(errw, "%d: %s\n", lineNo, line)
			}
		}
		rec := capture.Rec{N: lineNo, Text: line, M: matched}
		if *flagOnlyView {
			if matched {
				_ = enc.Encode(&rec)
			}
		} else {
			_ = enc.Encode(&rec)
		}
	}

	// meta
	meta := capture.Meta{
		Version:        1,
		CapturePath:    wr.Path(),
		Filtered:       *flagOnlyView,
		LineFormat:     "jsonl",
		LinesTotal:     linesTotal,
		MatchLines:     matchLines,
		MatchesTotal:   matchesTotal,
		CreatedUnixSec: time.Now().Unix(),
		Temp:           true,
		OwnerPID:       os.Getpid(),
	}
	meta.Source.Mode = "pipe"
	meta.Source.Arg = ""
	for _, r := range rs {
		meta.Rules = append(meta.Rules, r.ID)
	}

	metaPath := wr.Path() + ".meta.json"
	if err := capture.WriteMeta(metaPath, &meta); err != nil {
		fatalf("write meta: %v", err)
	}

	// decide
	if *flagOnlyOnMatch && !any {
		_ = os.Remove(wr.Path())
		_ = os.Remove(metaPath)
		return
	}

	// spawn viewer in terminal
	self, _ := os.Executable()
	lcfg := launcher.Config{
		TermPrefix:    cfg.Launcher.TermPrefix,
		TmuxPrefix:    cfg.Launcher.TmuxPrefix,
		PreferTmux:    cfg.Launcher.PreferTmux,
		ViewerTitle:   *flagViewerTitle,
		OnlyView:      *flagOnlyView,
		Mouse:         *flagMouse,
		KeepCapture:   *flagKeepCapture,
		CleanupTTLMin: *flagTTLMinutes,
		DryRun:        *flagDryLaunch,
		ForceTmux:     *flagTmuxForce,
		NoTmux:        *flagTmuxOff,
		ErrLinesMax:   *flagErrLines,
	}
	if err := launcher.SpawnTerminalViewer(lcfg, self, wr.Path(), metaPath); err != nil {
		fatalf("launch viewer: %v", err)
	}
}

func runFile(rs []rules.Rule, cfg *config.Config, path string) {
	data, err := os.ReadFile(path)
	if err != nil {
		fatalf("read: %v", err)
	}

	// write capture to temp for simplicity (Temp=false so no auto-delete)
	wr, err := capture.NewTempWriter("ot-")
	if err != nil {
		fatalf("capture: %v", err)
	}
	enc := json.NewEncoder(wr.Writer())

	sc := bufio.NewScanner(bytes.NewReader(data))
	sc.Buffer(make([]byte, 0, 64*1024), 10*1024*1024)
	lineNo := 0
	for sc.Scan() {
		lineNo++
		line := sc.Text()
		matched, _ := rules.AnyMatch(rs, line)
		rec := capture.Rec{N: lineNo, Text: line, M: matched}
		if *flagOnlyView {
			if matched {
				_ = enc.Encode(&rec)
			}
		} else {
			_ = enc.Encode(&rec)
		}
	}
	_ = wr.Close()

	meta := capture.Meta{
		Version:        1,
		CapturePath:    wr.Path(),
		Filtered:       *flagOnlyView,
		LineFormat:     "jsonl",
		LinesTotal:     lineNo,
		MatchLines:     0,
		MatchesTotal:   0,
		CreatedUnixSec: time.Now().Unix(),
		Temp:           false,
		OwnerPID:       os.Getpid(),
	}
	meta.Source.Mode = "file"
	meta.Source.Arg = path
	metaPath := wr.Path() + ".meta.json"
	_ = capture.WriteMeta(metaPath, &meta)

	// run viewer inline, with cleanup wrapper (won't delete since Temp=false)
	run := func() error {
		return viewer.RunFromFile(wr.Path(), &meta, rs, viewerOptions(), viewer.Hooks{
			OnActivate: func(lineText string) ([]string, error) {
				return editor.LaunchForLine(lineText, rs, editorConfig(cfg))
			},
		})
	}
	ccfg := cleanup.Config{KeepCapture: *flagKeepCapture, TTLMinutes: *flagTTLMinutes}
	if err := cleanup.WrapWithSignals(run, &meta, ccfg, wr.Path(), metaPath); err != nil {
		fatalf("viewer: %v", err)
	}
}

func viewerOptions() viewer.Options {
	return viewer.Options{
		Title:         *flagViewerTitle,
		GutterWidth:   *flagGutterWidth,
		ShowTopBar:    *flagTopBar,
		ShowBottomBar: *flagBottomBar,
		Mouse:         *flagMouse,
		NoAlt:         *flagNoAlt,
		ErrLinesMax:   *flagErrLines,
	}
}

func runViewerWithCleanup(capturePath, metaPath string, cfg *config.Config) {
	// load meta if present
	var meta capture.Meta
	if metaPath != "" {
		if b, err := os.ReadFile(metaPath); err == nil {
			_ = json.Unmarshal(b, &meta)
		}
	}
	// sweep orphans
	cleanup.SweepOrphans(*flagTTLMinutes)

	// rules from compiled defaults (config already applied above to flags; rules for viewer can be default)
	rs := rules.Default()
	run := func() error {
		return viewer.RunFromFile(capturePath, &meta, rs, viewerOptions(), viewer.Hooks{
			OnActivate: func(lineText string) ([]string, error) {
				return editor.LaunchForLine(lineText, rs, editorConfig(cfg))
			},
		})
	}
	ccfg := cleanup.Config{KeepCapture: *flagKeepCapture, TTLMinutes: *flagTTLMinutes}
	_ = cleanup.WrapWithSignals(run, &meta, ccfg, capturePath, metaPath)
}

func fatalf(f string, a ...any) {
	fmt.Fprintf(os.Stderr, f+"\n", a...)
	os.Exit(1)
}
