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
	"strings"
	"time"

	"local/capture"
	"local/cleanup"
	"local/editor"
	"local/launcher"
	"local/rules"
	"local/viewer"
)

var (
	// Modes
	flagPipe = flag.Bool("pipe", false, "Read from stdin; stream to stdout, capture JSONL, and (optionally) launch viewer in a new terminal")
	flagFile = flag.String("file", "", "Read from file PATH and view inline")

	// Pipe behavior
	flagOnlyView    = flag.Bool("only-view-matches", false, "Viewer shows only matching lines (capture filtered in pipe)")
	flagOnlyOnMatch = flag.Bool("only-on-matches", false, "Do not launch viewer when no matches were seen")
	flagMatchStderr = flag.String("match-stderr", "line", "During --pipe, echo matches to stderr: none|line")

	// Viewer internal
	flagView        = flag.Bool("view", false, "Internal: run viewer on a capture JSONL file")
	flagCapturePath = flag.String("capture", "", "Internal: capture JSONL path for --view")
	flagMetaPath    = flag.String("meta", "", "Internal: meta.json path for --view")

	// Viewer options
	flagViewerTitle = flag.String("viewer-title", "OutputTool Viewer", "Viewer window title")
	flagGutterWidth = flag.Int("gutter-width", 6, "Fixed gutter width for line numbers")
	flagTopBar      = flag.Bool("top-bar", true, "Show top status bar")
	flagBottomBar   = flag.Bool("bottom-bar", true, "Show bottom status bar")
	flagNoAlt       = flag.Bool("no-alt", false, "Do not use terminal alt screen (debug)")
	flagMouse       = flag.Bool("mouse", false, "Enable mouse tracking (disables terminal text selection)")

	// Editor
	flagEditorExe   = flag.String("editor", "cudatext", "Editor executable")
	flagEditorPrefx = flag.String("editor-arg-prefix", "", "Optional prefix arg placed before target file (e.g. '-n')")

	// Launcher (pipe -> new terminal)
	flagLauncher  = flag.String("launcher", "xfce4-terminal --hide-menubar --hide-scrollbar --hide-toolbar --title='OutputTool' --command", "Terminal launcher prefix")
	flagDryLaunch = flag.Bool("dry-launch", false, "Pipe-mode: print the launch command and do not spawn")

	// Cleanup behavior
	flagKeepCapture = flag.Bool("keep-capture", false, "Viewer: keep capture/meta files (skip auto-cleanup)")
	flagTTLMinutes  = flag.Int("cleanup-ttl-minutes", 90, "Viewer: sweep temp orphans older than this many minutes on startup")

	// Help
	flagUsage = flag.Bool("usage", false, "Show usage")
)

func usage() {
	fmt.Fprintf(os.Stdout, `Usage:
  ot --pipe [--only-view-matches] [--only-on-matches] [--match-stderr=none|line] [--launcher="..."] [--mouse]
  ot --file=PATH [--only-view-matches] [--mouse]
  ot --view --capture=/tmp/ot-XXXX.jsonl --meta=/tmp/ot-XXXX.meta.json   (internal)

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

	// viewer internal
	if *flagView {
		runViewerWithCleanup(*flagCapturePath, *flagMetaPath)
		return
	}

	// modes: exactly one of pipe|file
	modes := 0
	if *flagPipe {
		modes++
	}
	if *flagFile != "" {
		modes++
	}
	if modes != 1 {
		usage()
		os.Exit(2)
	}

	rs := rules.Default()

	if *flagPipe {
		runPipe(rs)
		return
	}
	if *flagFile != "" {
		runFile(rs, *flagFile)
		return
	}
}

func runPipe(rs []rules.Rule) {
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
		LauncherPrefix: *flagLauncher,
		ViewerTitle:    *flagViewerTitle,
		OnlyView:       *flagOnlyView,
		Mouse:          *flagMouse,
		KeepCapture:    *flagKeepCapture,
		CleanupTTLMin:  *flagTTLMinutes,
		DryRun:         *flagDryLaunch,
	}
	if err := launcher.SpawnTerminalViewer(lcfg, self, wr.Path(), metaPath); err != nil {
		fatalf("launch viewer: %v", err)
	}
}

func runFile(rs []rules.Rule, path string) {
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
	vopts := viewer.Options{
		Title:         *flagViewerTitle,
		GutterWidth:   *flagGutterWidth,
		ShowTopBar:    *flagTopBar,
		ShowBottomBar: *flagBottomBar,
		Mouse:         *flagMouse,
		NoAlt:         *flagNoAlt,
	}
	eh := editor.Config{EditorExe: *flagEditorExe, EditorArgPrefix: *flagEditorPrefx}
	run := func() error {
		return viewer.RunFromFile(wr.Path(), &meta, rs, vopts, viewer.Hooks{
			OnActivate: func(lineText string) { editor.LaunchForLine(lineText, rs, eh) },
		})
	}
	ccfg := cleanup.Config{KeepCapture: *flagKeepCapture, TTLMinutes: *flagTTLMinutes}
	if err := cleanup.WrapWithSignals(run, &meta, ccfg, wr.Path(), metaPath); err != nil {
		fatalf("viewer: %v", err)
	}
}

func runViewerWithCleanup(capturePath, metaPath string) {
	// load meta if present
	var meta capture.Meta
	if metaPath != "" {
		if b, err := os.ReadFile(metaPath); err == nil {
			_ = json.Unmarshal(b, &meta)
		}
	}
	// sweep orphans
	cleanup.SweepOrphans(*flagTTLMinutes)

	rs := rules.Default()
	vopts := viewer.Options{
		Title:         *flagViewerTitle,
		GutterWidth:   *flagGutterWidth,
		ShowTopBar:    *flagTopBar,
		ShowBottomBar: *flagBottomBar,
		Mouse:         *flagMouse,
		NoAlt:         *flagNoAlt,
	}
	eh := editor.Config{EditorExe: *flagEditorExe, EditorArgPrefix: *flagEditorPrefx}

	run := func() error {
		return viewer.RunFromFile(capturePath, &meta, rs, vopts, viewer.Hooks{
			OnActivate: func(lineText string) { editor.LaunchForLine(lineText, rs, eh) },
		})
	}
	ccfg := cleanup.Config{KeepCapture: *flagKeepCapture, TTLMinutes: *flagTTLMinutes}
	_ = cleanup.WrapWithSignals(run, &meta, ccfg, capturePath, metaPath)
}

func fatalf(f string, a ...any) {
	fmt.Fprintf(os.Stderr, f+"\n", a...)
	os.Exit(1)
}
