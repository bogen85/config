// output-tool.go
// JSONL capture + meta for pipe/file, and a clean tcell viewer with match-span highlighting
// and mouse-driven editing (double-click).
//
// Build: go build -o output-tool output-tool.go

package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/gdamore/tcell/v2"
)

type Rule struct {
	ID          string
	Regex       *regexp.Regexp
	FileGroup   int // 1-based capture group index for file path (0 = none)
	LineGroup   int // 1-based capture group index for line number
	ColumnGroup int // 1-based capture group index for column number
}

type Meta struct {
	Version int `json:"version"`
	Source  struct {
		Mode string `json:"mode"` // pipe|file
		Arg  string `json:"arg"`
	} `json:"source"`
	CapturePath    string   `json:"capture_path"`
	Filtered       bool     `json:"filtered"` // true if only-view-matches applied upstream
	LineFormat     string   `json:"line_format"`
	LinesTotal     int      `json:"lines_total"`
	MatchLines     int      `json:"match_lines"`
	MatchesTotal   int      `json:"matches_total"`
	Rules          []string `json:"rules"`
	CreatedUnixSec int64    `json:"created_unix"`
}

type Rec struct {
	N    int    `json:"n"`
	Text string `json:"text"`
	M    bool   `json:"m"`
}

const (
	defaultLauncher = "xfce4-terminal --hide-menubar --hide-scrollbar --hide-toolbar --title='OutputTool' --command"
)

var (
	// CLI
	flagPipe        = flag.Bool("pipe", false, "Read from stdin; stream to stdout, capture JSONL, and (optionally) launch viewer in new terminal")
	flagFile        = flag.String("file", "", "Read from file PATH and view inline")
	flagOnlyView    = flag.Bool("only-view-matches", false, "Viewer shows only matching lines (capture filtered in pipe)")
	flagOnlyOnMatch = flag.Bool("only-on-matches", false, "Do not launch viewer when no matches were seen")
	flagMatchStderr = flag.String("match-stderr", "line", "During --pipe, echo matches to stderr: none|line")
	flagLauncher    = flag.String("launcher", defaultLauncher, "Terminal launcher command prefix to spawn viewer (pipe mode). Example: 'xfce4-terminal -- ... --command'")

	// Internal viewer mode
	flagView        = flag.Bool("view", false, "Internal: run viewer on a capture JSONL file")
	flagCapturePath = flag.String("capture", "", "Internal: capture JSONL path for --view")
	flagMetaPath    = flag.String("meta", "", "Internal: meta.json path for --view")

	// Viewer styling/behavior
	flagViewerTitle = flag.String("viewer-title", "OutputTool Viewer", "Viewer window title")
	flagGutterWidth = flag.Int("gutter-width", 6, "Fixed gutter width for line numbers")
	flagTopBar      = flag.Bool("top-bar", true, "Show top status bar")
	flagBottomBar   = flag.Bool("bottom-bar", true, "Show bottom status bar")
	flagNoAltScreen = flag.Bool("no-alt", false, "Do not use terminal alt screen (debug)")
	flagMouse       = flag.Bool("mouse", false, "Enable mouse tracking (disables terminal text selection)")

	// Editor
	flagEditor         = flag.String("editor", "cudatext", "Editor executable")
	flagEditorArgPrefx = flag.String("editor-arg-prefix", "", "Optional prefix arg placed before target file (e.g. '-n')")
	// Debug
	flagHelpUsage      = flag.Bool("usage", false, "Show usage")
	flagDryRunLauncher = flag.Bool("dry-launch", false, "Pipe-mode: print the launch command and do not spawn")
)

func usage() {
	fmt.Fprintf(os.Stdout, `Usage:
  output-tool --pipe [--only-view-matches] [--only-on-matches] [--match-stderr=none|line] [--launcher="..."]
  output-tool --file=PATH [--only-view-matches]
  output-tool --view --capture=/tmp/ot-XXXX.jsonl --meta=/tmp/ot-XXXX.meta.json   (internal)

Notes:
  - Pipe mode acts like 'cat': streams stdin to stdout in real time, scans matches, writes JSONL capture and meta.
  - match-stderr:
      none  = no stderr output during streaming
      line  = echo matching lines to stderr with original line numbers
  - After streaming: if (--only-on-matches && none), exits quietly. Otherwise spawns terminal with viewer and exits.
  - File mode reads file, builds capture in-memory, and runs tcell viewer inline.

Viewer:
  - ↑/↓ PgUp/PgDn Home/End to navigate; q/Esc to quit.
  - Enter or double-click launches editor if line contains path:line[:col].
  - --mouse enables click/double-click; press 'M' in the viewer to toggle mouse mode.
`)
}

func main() {
	flag.Parse()
	if *flagHelpUsage {
		usage()
		return
	}

	// internal viewer mode
	if *flagView {
		if *flagCapturePath == "" {
			exitErr("missing --capture for --view")
		}
		runViewer(*flagCapturePath, *flagMetaPath)
		return
	}

	// select exactly one of pipe|file (clipboard/primary omitted in this cut)
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

	// Compile rules (extendable)
	rules := defaultRules()

	switch {
	case *flagPipe:
		runPipe(rules, *flagOnlyView, *flagOnlyOnMatch, *flagMatchStderr, *flagLauncher)
	case *flagFile != "":
		runFile(rules, *flagFile, *flagOnlyView)
	}
}

// ---------- Rules ----------

func defaultRules() []Rule {
	rx := regexp.MustCompile(`(?:\.?\.?\/)?([A-Za-z0-9._\/\-]+):(\d+):(\d+)`)
	return []Rule{
		{ID: "path:line:col", Regex: rx, FileGroup: 1, LineGroup: 2, ColumnGroup: 3},
	}
}

func anyMatch(rules []Rule, line string) (bool, int) {
	total := 0
	for _, r := range rules {
		locs := r.Regex.FindAllStringIndex(line, -1)
		if len(locs) > 0 {
			total += len(locs)
		}
	}
	return total > 0, total
}

// For highlighting and editor extraction: find all [start,end] byte spans across rules
func allMatchSpans(rules []Rule, s string) [][2]int {
	var spans [][2]int
	for _, r := range rules {
		locs := r.Regex.FindAllStringIndex(s, -1)
		for _, se := range locs {
			spans = append(spans, [2]int{se[0], se[1]})
		}
	}
	// optional: coalesce overlaps
	if len(spans) > 1 {
		spans = coalesce(spans)
	}
	return spans
}

func coalesce(spans [][2]int) [][2]int {
	// simple insertion sort by start then merge
	for i := 1; i < len(spans); i++ {
		j := i
		for j > 0 && spans[j-1][0] > spans[j][0] {
			spans[j-1], spans[j] = spans[j], spans[j-1]
			j--
		}
	}
	var out [][2]int
	cur := spans[0]
	for i := 1; i < len(spans); i++ {
		s := spans[i]
		if s[0] <= cur[1] { // overlap/touch
			if s[1] > cur[1] {
				cur[1] = s[1]
			}
		} else {
			out = append(out, cur)
			cur = s
		}
	}
	out = append(out, cur)
	return out
}

// ---------- Pipe Mode ----------

func runPipe(rules []Rule, onlyView, onlyOn bool, matchStderr, launcher string) {
	// Create temp paths
	base := fmt.Sprintf("ot-%d", time.Now().UnixNano())
	dir := os.TempDir()
	capturePath := filepath.Join(dir, base+".jsonl")
	metaPath := filepath.Join(dir, base+".meta.json")

	out, err := os.Create(capturePath)
	if err != nil {
		exitErr("open capture: %v", err)
	}
	defer out.Close()

	w := bufio.NewWriterSize(out, 64*1024)
	defer w.Flush()

	in := bufio.NewReaderSize(os.Stdin, 64*1024)
	stdout := bufio.NewWriterSize(os.Stdout, 64*1024)
	stderr := bufio.NewWriterSize(os.Stderr, 64*1024)
	defer stdout.Flush()
	defer stderr.Flush()

	var (
		lineNo       = 0
		linesTotal   = 0
		matchLines   = 0
		matchesTotal = 0
		any          = false
	)

	enc := json.NewEncoder(w)

	for {
		line, err := in.ReadString('\n')
		if errors.Is(err, io.EOF) {
			if len(line) == 0 {
				break
			}
			// handle last line without trailing newline
		} else if err != nil {
			// best-effort: break
			break
		}

		line = strings.TrimRight(line, "\r\n")
		lineNo++
		linesTotal++

		// cat to stdout immediately
		stdout.WriteString(line)
		stdout.WriteByte('\n')

		matched, total := anyMatch(rules, line)
		if matched {
			any = true
			matchLines++
			matchesTotal += total
			if matchStderr == "line" {
				fmt.Fprintf(stderr, "%d: %s\n", lineNo, line)
			}
		}

		// write JSONL record
		rec := Rec{N: lineNo, Text: line, M: matched}
		if onlyView {
			// keep only matches
			if matched {
				_ = enc.Encode(&rec)
			}
		} else {
			// keep everything
			_ = enc.Encode(&rec)
		}
	}

	_ = w.Flush()
	_ = stdout.Flush()
	_ = stderr.Flush()

	// if onlyOn and none matched, quit silently
	if onlyOn && !any {
		_ = os.Remove(capturePath)
		return
	}

	// write meta
	meta := Meta{
		Version:        1,
		CapturePath:    capturePath,
		Filtered:       onlyView,
		LineFormat:     "jsonl",
		LinesTotal:     linesTotal,
		MatchLines:     matchLines,
		MatchesTotal:   matchesTotal,
		CreatedUnixSec: time.Now().Unix(),
	}
	meta.Source.Mode = "pipe"
	meta.Source.Arg = ""
	for _, r := range rules {
		meta.Rules = append(meta.Rules, r.ID)
	}
	if err := atomicWriteJSON(metaPath, &meta, 0644); err != nil {
		exitErr("write meta: %v", err)
	}

	// Launch viewer in NEW terminal, then exit parent
	if err := spawnTerminalViewer(launcher, capturePath, metaPath); err != nil {
		exitErr("launch viewer: %v", err)
	}
}

func atomicWriteJSON(path string, v any, perm os.FileMode) error {
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, perm)
	if err != nil {
		return err
	}
	enc := json.NewEncoder(f)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		f.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func spawnTerminalViewer(launcher, capture, meta string) error {
	self, err := os.Executable()
	if err != nil {
		return err
	}
	// Build the command string to run inside the terminal
	var inner bytes.Buffer
	inner.WriteString(shellQuote(self))
	inner.WriteString(" --view ")
	inner.WriteString("--capture=" + shellQuote(capture) + " ")
	if meta != "" {
		inner.WriteString("--meta=" + shellQuote(meta) + " ")
	}
	if *flagOnlyView {
		inner.WriteString("--only-view-matches ")
	}
	if *flagViewerTitle != "" {
		inner.WriteString("--viewer-title=" + shellQuote(*flagViewerTitle) + " ")
	}
	if *flagMouse {
		inner.WriteString("--mouse ")
	}

	// launcher is a prefix, e.g.:
	//   xfce4-terminal --hide-menubar ... --command
	parts := splitLauncher(launcher)
	if len(parts) == 0 {
		return fmt.Errorf("invalid launcher")
	}
	args := parts[1:]
	args = append(args, inner.String())

	if *flagDryRunLauncher {
		fmt.Fprintf(os.Stderr, "DRY LAUNCH: %s %s\n", parts[0], strings.Join(args, " "))
		return nil
	}

	cmd := exec.Command(parts[0], args...)
	return cmd.Start()
}

func splitLauncher(s string) []string {
	// simple split respecting single/double quotes
	var out []string
	var cur bytes.Buffer
	quote := byte(0)
	for i := 0; i < len(s); i++ {
		c := s[i]
		if quote == 0 {
			if c == '\'' || c == '"' {
				quote = c
				continue
			}
			if c == ' ' || c == '\t' {
				if cur.Len() > 0 {
					out = append(out, cur.String())
					cur.Reset()
				}
				continue
			}
			cur.WriteByte(c)
		} else {
			if c == quote {
				quote = 0
				continue
			}
			cur.WriteByte(c)
		}
	}
	if cur.Len() > 0 {
		out = append(out, cur.String())
	}
	return out
}

func shellQuote(s string) string {
	if s == "" {
		return "''"
	}
	if !strings.ContainsAny(s, " \t\n'\"\\$&;|*?<>`()[]{}") {
		return s
	}
	// single-quote safe form
	return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'"
}

// ---------- File Mode ----------

func runFile(rules []Rule, path string, onlyView bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		exitErr("read file: %v", err)
	}

	// Build capture in-memory (JSONL buffer)
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)

	sc := bufio.NewScanner(bytes.NewReader(data))
	sc.Buffer(make([]byte, 0, 64*1024), 10*1024*1024)
	lineNo := 0
	for sc.Scan() {
		lineNo++
		line := sc.Text()
		matched, _ := anyMatch(rules, line)
		if onlyView {
			if matched {
				_ = enc.Encode(Rec{N: lineNo, Text: line, M: matched})
			}
		} else {
			_ = enc.Encode(Rec{N: lineNo, Text: line, M: matched})
		}
	}
	if err := sc.Err(); err != nil {
		exitErr("scan file: %v", err)
	}

	// Run viewer inline from buffer
	runViewerFromReader(bytes.NewReader(buf.Bytes()), "")
}

// ---------- Viewer ----------

type viewerRec struct {
	N    int
	Text string
	M    bool
}

func runViewer(capturePath, metaPath string) {
	// Load capture JSONL
	f, err := os.Open(capturePath)
	if err != nil {
		exitErr("open capture: %v", err)
	}
	defer f.Close()
	runViewerFromReader(f, metaPath)
}

func runViewerFromReader(r io.Reader, metaPath string) {
	var recs []viewerRec
	dec := json.NewDecoder(r)
	for {
		var rec Rec
		if err := dec.Decode(&rec); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			exitErr("decode capture: %v", err)
		}
		recs = append(recs, viewerRec{N: rec.N, Text: rec.Text, M: rec.M})
	}

	// Recompile rules here for highlighting & editor extraction
	rules := defaultRules()

	// tcell screen
	screen, err := tcell.NewScreen()
	if err != nil {
		exitErr("tcell screen: %v", err)
	}
	if err := screen.Init(); err != nil {
		exitErr("tcell init: %v", err)
	}
	defer screen.Fini()

	if *flagMouse {
		screen.EnableMouse()
	} else {
		screen.DisableMouse()
	}

	// styles
	normalStyle := tcell.StyleDefault.Foreground(tcell.ColorWhite).Background(tcell.ColorBlack)
	matchStyle := tcell.StyleDefault.Foreground(tcell.ColorBlack).Background(tcell.ColorGreen) // spans
	cursorStyle := tcell.StyleDefault.Foreground(tcell.ColorBlack).Background(tcell.ColorYellow)
	cursorMatchStyle := tcell.StyleDefault.Foreground(tcell.ColorBlack).Background(tcell.ColorBlue)
	gutterStyle := tcell.StyleDefault.Foreground(tcell.ColorGray).Background(tcell.ColorBlack)
	gutterCursor := tcell.StyleDefault.Foreground(tcell.ColorWhite).Background(tcell.ColorBlue)
	topStyle := tcell.StyleDefault.Foreground(tcell.ColorBlack).Background(tcell.ColorGreen)
	botStyle := tcell.StyleDefault.Foreground(tcell.ColorBlack).Background(tcell.ColorYellow)

	gw := *flagGutterWidth
	if gw < 3 {
		gw = 3
	}

	cur := 0
	top := 0

	// mouse double-click detection
	var lastClick time.Time
	lastClickLine := -1
	doubleClickMax := 300 * time.Millisecond

	for {
		w, h := screen.Size()
		bodyTop := 0
		bodyBottom := h
		if *flagTopBar {
			bodyTop = 1
		}
		if *flagBottomBar {
			bodyBottom = bodyBottom - 1
		}
		rows := bodyBottom - bodyTop
		if rows < 1 {
			rows = 1
		}

		// clamp scroll
		if cur < 0 {
			cur = 0
		}
		if cur >= len(recs) {
			cur = len(recs) - 1
		}
		if cur < 0 {
			cur = 0
		}
		if cur < top {
			top = cur
		}
		if cur >= top+rows {
			top = cur - rows + 1
		}
		if top < 0 {
			top = 0
		}
		if top > int(math.Max(0, float64(len(recs)-rows))) {
			top = int(math.Max(0, float64(len(recs)-rows)))
		}

		screen.Clear()

		// top bar
		if *flagTopBar {
			s := fmt.Sprintf(" %s | lines:%d  pos:%d/%d  (mouse:%v) ",
				*flagViewerTitle, len(recs), cur+1, len(recs), *flagMouse)
			drawLine(screen, 0, 0, w, s, topStyle)
		}

		// body
		for row := 0; row < rows; row++ {
			idx := top + row
			if idx >= len(recs) {
				break
			}
			y := bodyTop + row
			rec := recs[idx]

			// gutter
			ln := fmt.Sprintf("%*d", gw-2, rec.N)
			gs := gutterStyle
			if idx == cur {
				gs = gutterCursor
			}
			drawText(screen, 0, y, ln, gs)
			drawText(screen, gw-2, y, ": ", gs)

			// content with span highlighting
			spans := allMatchSpans(rules, rec.Text)
			// build byte->rune index mapping
			byteToRuneIndex := make([]int, 0, len(rec.Text)+1)
			runePos := 0
			for i := 0; i < len(rec.Text); {
				byteToRuneIndex = append(byteToRuneIndex, runePos)
				_, size := utf8.DecodeRuneInString(rec.Text[i:])
				i += size
				runePos++
			}
			byteToRuneIndex = append(byteToRuneIndex, runePos) // sentinel

			// paint content char-by-char
			maxw := w - gw
			rx := gw
			if maxw < 0 {
				maxw = 0
			}
			// convert spans (byte) -> rune spans
			runeSpans := make([][2]int, 0, len(spans))
			for _, se := range spans {
				startRune := byteIndexToRuneIndex(byteToRuneIndex, se[0])
				endRune := byteIndexToRuneIndex(byteToRuneIndex, se[1])
				if endRune < startRune {
					endRune = startRune
				}
				runeSpans = append(runeSpans, [2]int{startRune, endRune})
			}

			// iterate runes with an index
			runeIdx := 0
			for _, r := range rec.Text {
				if rx >= w {
					break
				}
				style := normalStyle
				if idx == cur {
					style = cursorStyle
				}
				if insideAnySpan(runeIdx, runeSpans) {
					if idx == cur {
						style = cursorMatchStyle
					} else {
						style = matchStyle
					}
				}
				screen.SetContent(rx, y, r, nil, style)
				rx++
				runeIdx++
			}
			// clear rest of line slice
			for ; rx < w; rx++ {
				screen.SetContent(rx, y, ' ', nil, normalStyle)
			}
		}

		// bottom bar
		if *flagBottomBar {
			status := " ↑/↓ PgUp/PgDn Home/End  Enter=edit  M=toggle-mouse  q/Esc=quit "
			drawLine(screen, 0, h-1, w, status, botStyle)
		}

		screen.Show()

		// events
		ev := screen.PollEvent()
		switch e := ev.(type) {
		case *tcell.EventResize:
			screen.Sync()

		case *tcell.EventMouse:
			if !*flagMouse {
				break
			}
			//x,
			_, y := e.Position()
			btn := e.Buttons()
			// map y to index
			bodyTop := 0
			if *flagTopBar {
				bodyTop = 1
			}
			if y < bodyTop {
				break
			}
			idx := top + (y - bodyTop)
			if idx < 0 || idx >= len(recs) {
				break
			}
			if btn&tcell.Button1 != 0 {
				// left click: move cursor
				cur = idx
				// detect double-click
				now := time.Now()
				if lastClickLine == cur && now.Sub(lastClick) <= doubleClickMax {
					// double click -> edit
					launchEditorForLine(recs[cur].Text, rules)
				}
				lastClick = now
				lastClickLine = cur
			}

		case *tcell.EventKey:
			switch e.Key() {
			case tcell.KeyEsc:
				return
			case tcell.KeyEnter:
				if cur >= 0 && cur < len(recs) {
					launchEditorForLine(recs[cur].Text, rules)
				}
			case tcell.KeyRune:
				switch e.Rune() {
				case 'q', 'Q':
					return
				case 'M', 'm':
					*flagMouse = !*flagMouse
					if *flagMouse {
						screen.EnableMouse()
					} else {
						screen.DisableMouse()
					}
				}
			case tcell.KeyUp:
				cur--
			case tcell.KeyDown:
				cur++
			case tcell.KeyHome:
				cur = 0
			case tcell.KeyEnd:
				cur = len(recs) - 1
			case tcell.KeyPgUp:
				_, h := screen.Size()
				bodyTop := 0
				bodyBottom := h
				if *flagTopBar {
					bodyTop = 1
				}
				if *flagBottomBar {
					bodyBottom = bodyBottom - 1
				}
				rows := bodyBottom - bodyTop
				cur -= rows
			case tcell.KeyPgDn:
				_, h := screen.Size()
				bodyTop := 0
				bodyBottom := h
				if *flagTopBar {
					bodyTop = 1
				}
				if *flagBottomBar {
					bodyBottom = bodyBottom - 1
				}
				rows := bodyBottom - bodyTop
				cur += rows
			}
			if cur < 0 {
				cur = 0
			}
			if cur >= len(recs) {
				cur = len(recs) - 1
			}
		}
	}
}

func byteIndexToRuneIndex(mapping []int, byteIndex int) int {
	// mapping[i] is rune index at byte offset i. We built it dense.
	if byteIndex < 0 {
		return 0
	}
	if byteIndex >= len(mapping) {
		return mapping[len(mapping)-1]
	}
	return mapping[byteIndex]
}

func insideAnySpan(pos int, spans [][2]int) bool {
	for _, s := range spans {
		if pos >= s[0] && pos < s[1] {
			return true
		}
	}
	return false
}

func drawText(s tcell.Screen, x, y int, text string, st tcell.Style) {
	w, _ := s.Size()
	if y < 0 || x >= w {
		return
	}
	rx := x
	for _, r := range text {
		if rx >= w {
			break
		}
		s.SetContent(rx, y, r, nil, st)
		rx++
	}
}

func drawLine(s tcell.Screen, x, y, w int, text string, st tcell.Style) {
	for i := 0; i < w; i++ {
		s.SetContent(x+i, y, ' ', nil, st)
	}
	drawText(s, x, y, truncateTo(text, w), st)
}

func truncateTo(s string, max int) string {
	if max <= 0 {
		return ""
	}
	var b strings.Builder
	b.Grow(max)
	count := 0
	for _, r := range s {
		if count >= max {
			break
		}
		b.WriteRune(r)
		count++
	}
	return b.String()
}

// ---------- Editor launching ----------

func launchEditorForLine(line string, rules []Rule) {
	// Find first rule match and extract file/line/col
	for _, r := range rules {
		idxs := r.Regex.FindStringSubmatchIndex(line)
		if idxs == nil {
			continue
		}
		// helper to get group substring
		getGroup := func(g int) (string, bool) {
			if g <= 0 {
				return "", false
			}
			i := 2 * g
			if i+1 >= len(idxs) || idxs[i] < 0 || idxs[i+1] < 0 {
				return "", false
			}
			return line[idxs[i]:idxs[i+1]], true
		}

		var file string
		var ok bool
		if file, ok = getGroup(r.FileGroup); !ok {
			continue
		}
		var lineNum, colNum int
		if s, ok := getGroup(r.LineGroup); ok {
			lineNum, _ = strconv.Atoi(s)
		}
		if s, ok := getGroup(r.ColumnGroup); ok {
			colNum, _ = strconv.Atoi(s)
		}

		target := file
		if lineNum > 0 && colNum > 0 {
			target = fmt.Sprintf("%s@%d@%d", file, lineNum, colNum)
		} else if lineNum > 0 {
			target = fmt.Sprintf("%s@%d", file, lineNum)
		}

		args := []string{}
		if *flagEditorArgPrefx != "" {
			args = append(args, *flagEditorArgPrefx)
		}
		args = append(args, target)

		cmd := exec.Command(*flagEditor, args...)
		_ = cmd.Start() // don't wait
		return
	}

	// Fallback: no path match — write temp JSON with the line and open it
	tmp := filepath.Join(os.TempDir(), fmt.Sprintf("ot-line-%d.json", time.Now().UnixNano()))
	obj := map[string]any{"line": line}
	b, _ := json.MarshalIndent(obj, "", "  ")
	_ = os.WriteFile(tmp, b, 0644)

	args := []string{}
	if *flagEditorArgPrefx != "" {
		args = append(args, *flagEditorArgPrefx)
	}
	args = append(args, tmp)
	_ = exec.Command(*flagEditor, args...).Start()
}

// ---------- Utils ----------

func exitErr(fmtStr string, a ...any) {
	fmt.Fprintf(os.Stderr, fmtStr+"\n", a...)
	os.Exit(1)
}

func init() {
	if runtime.GOOS == "windows" {
		*flagLauncher = ""
	}
}
