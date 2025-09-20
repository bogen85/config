package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/BurntSushi/toml"
	"github.com/gdamore/tcell/v2"
)

/* =========================
   Config types & defaults
   ========================= */

type ColorPair struct {
	FG string `toml:"fg"`
	BG string `toml:"bg"`
}

type Rule struct {
	Name  string `toml:"name"`
	Regex string `toml:"regex"`
	FG    string `toml:"fg"`
	BG    string `toml:"bg"`
}

type Config struct {
	Colors struct {
		Normal          ColorPair `toml:"normal"`
		Highlight       ColorPair `toml:"highlight"`
		Gutter          ColorPair `toml:"gutter"`
		GutterHighlight ColorPair `toml:"gutter_highlight"`
		Status          ColorPair `toml:"status"`      // bottom bar
		TopStatus       ColorPair `toml:"top_status"`  // NEW top bar
	} `toml:"colors"`
	Rules []Rule `toml:"rules"`
}

func defaultConfig() Config {
	var c Config
	// Base UI colors
	c.Colors.Normal = ColorPair{FG: "white", BG: "black"}
	c.Colors.Highlight = ColorPair{FG: "black", BG: "white"}
	c.Colors.Gutter = ColorPair{FG: "gray", BG: "black"}
	c.Colors.GutterHighlight = ColorPair{FG: "black", BG: "white"}
	c.Colors.Status = ColorPair{FG: "black", BG: "yellow"}     // bottom bar
	c.Colors.TopStatus = ColorPair{FG: "black", BG: "magenta"} // top bar (new)

	// One default rule: path:line:col (e.g., old/0/file.go:10:2)
	// Note: kept POSIX-y; we can extend for Windows later if you want.
	c.Rules = []Rule{
		{
			Name:  "path:line:col",
			Regex: `(?:\.\.?/)?[A-Za-z0-9._/\-]+:\d+:\d+`,
			FG:    "black",
			BG:    "green",
		},
	}
	return c
}

/* =========================
   Small helpers
   ========================= */

func styleFrom(p ColorPair) tcell.Style {
	return tcell.StyleDefault.Foreground(tcell.GetColor(p.FG)).Background(tcell.GetColor(p.BG))
}

func invertStyle(p ColorPair) tcell.Style {
	// swap fg/bg
	return tcell.StyleDefault.Foreground(tcell.GetColor(p.BG)).Background(tcell.GetColor(p.FG))
}

func digits(n int) int {
	if n <= 0 {
		return 1
	}
	d := 0
	for n > 0 {
		d++
		n /= 10
	}
	return d
}
func clamp(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}
func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

/* =========================
   I/O helpers
   ========================= */

func readLines(path string) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var lines []string
	sc := bufio.NewScanner(f)
	const maxCap = 10 * 1024 * 1024
	buf := make([]byte, 0, 64*1024)
	sc.Buffer(buf, maxCap)
	for sc.Scan() {
		lines = append(lines, sc.Text())
	}
	return lines, sc.Err()
}

func readPrimaryWithXclip() (string, error) {
	_, err := exec.LookPath("xclip")
	if err != nil {
		return "", fmt.Errorf("xclip not found on PATH (install xclip or provide --file)")
	}
	out, err := exec.Command("xclip", "-o", "-selection", "primary").Output()
	if err != nil {
		return "", fmt.Errorf("reading PRIMARY via xclip failed: %w", err)
	}
	// Clear PRIMARY by setting empty input
	cmd := exec.Command("xclip", "-selection", "primary", "-in")
	cmd.Stdin = bytes.NewReader(nil)
	_ = cmd.Run() // best-effort
	return string(out), nil
}

func splitLines(s string) []string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")
	return strings.Split(s, "\n")
}

func defaultConfigPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	base := filepath.Base(exe)
	dir := filepath.Join(home, ".local", "share", "user-dev-tooling", base)
	return filepath.Join(dir, "config.toml"), nil
}

func ensureDir(path string) error {
	return os.MkdirAll(filepath.Dir(path), 0o755)
}

func writeDefaultConfigTo(path string) error {
	cfg := defaultConfig()
	if path == "" {
		return toml.NewEncoder(os.Stdout).Encode(cfg)
	}
	if err := ensureDir(path); err != nil {
		return err
	}
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return toml.NewEncoder(f).Encode(cfg)
}

func loadConfigMaybe(path string) (Config, error) {
	cfg := defaultConfig()
	var tryPath string
	if path == "" || path == "::default::" {
		if dp, err := defaultConfigPath(); err == nil {
			tryPath = dp
		}
	} else {
		tryPath = path
	}
	if tryPath != "" && fileExists(tryPath) {
		if _, err := toml.DecodeFile(tryPath, &cfg); err != nil {
			return cfg, err
		}
	}
	return cfg, nil
}

/* =========================
   Config overwrite dialog
   ========================= */

func confirmOverwriteDialog(target string) bool {
	s, err := tcell.NewScreen()
	if err != nil {
		return false
	}
	if err = s.Init(); err != nil {
		return false
	}
	defer s.Fini()

	def := tcell.StyleDefault
	bg := def.Background(tcell.ColorReset)

	msgLines := []string{
		"Configuration file already exists:",
		target,
		"",
		"Overwrite it?  [y]es / [n]o (Enter = yes, Esc = no)",
	}

	w, h := s.Size()
	boxW := 0
	for _, m := range msgLines {
		if len(m) > boxW {
			boxW = len(m)
		}
	}
	boxW += 4
	boxH := len(msgLines) + 2
	x0 := max(0, (w-boxW)/2)
	y0 := max(0, (h-boxH)/2)

	draw := func() {
		s.Clear()
		w, h = s.Size()
		x0 = max(0, (w-boxW)/2)
		y0 = max(0, (h-boxH)/2)
		for y := 0; y < boxH; y++ {
			for x := 0; x < boxW; x++ {
				ch := ' '
				if y == 0 || y == boxH-1 {
					ch = '─'
				}
				if x == 0 || x == boxW-1 {
					ch = '│'
				}
				if (x == 0 || x == boxW-1) && (y == 0 || y == boxH-1) {
					ch = '┼'
				}
				s.SetContent(x0+x, y0+y, ch, nil, bg)
			}
		}
		for i, m := range msgLines {
			start := x0 + 2
			for j, r := range m {
				s.SetContent(start+j, y0+1+i, r, nil, bg)
			}
		}
		s.Show()
	}

	draw()
	for {
		ev := s.PollEvent()
		switch e := ev.(type) {
		case *tcell.EventResize:
			s.Sync()
			draw()
		case *tcell.EventKey:
			switch e.Key() {
			case tcell.KeyEnter:
				return true
			case tcell.KeyEscape:
				return false
			default:
				switch e.Rune() {
				case 'y', 'Y':
					return true
				case 'n', 'N':
					return false
				}
			}
		}
	}
}

/* =========================
   Regex matching model
   ========================= */

type compiledRule struct {
	re        *regexp.Regexp
	style     tcell.Style
	styleInv  tcell.Style // for selected row (inverted)
	name      string
}

type matchSpan struct {
	start int // byte offsets in the line
	end   int
	rule  int // index into compiledRules
}

type lineInfo struct {
	spans       []matchSpan // sorted by start; later rules can overwrite earlier (see buildSpans)
	matchesText []string    // exact substrings for output
}

func compileRules(cfg Config) []compiledRule {
	var cr []compiledRule
	for i, r := range cfg.Rules {
		if strings.TrimSpace(r.Regex) == "" {
			fmt.Fprintf(os.Stderr, "warning: rule %d has empty regex; skipping\n", i)
			continue
		}
		re, err := regexp.Compile(r.Regex)
		if err != nil {
			fmt.Fprintf(os.Stderr, "warning: rule %d regex compile failed: %v; skipping\n", i, err)
			continue
		}
		cp := ColorPair{FG: r.FG, BG: r.BG}
		cr = append(cr, compiledRule{
			re:       re,
			style:    styleFrom(cp),
			styleInv: invertStyle(cp),
			name:     r.Name,
		})
	}
	return cr
}

// buildLineInfo finds matches for all rules, in config order.
// Overlaps: later rules override earlier visually; for matches text array, include ALL matches.
func buildLineInfo(line string, rules []compiledRule) lineInfo {
	var li lineInfo
	if len(rules) == 0 || len(line) == 0 {
		return li
	}

	// First collect all spans with their rule index
	type rawSpan struct {
		start, end, rule int
	}
	var raw []rawSpan
	for ri, cr := range rules {
		idxs := cr.re.FindAllStringIndex(line, -1)
		for _, p := range idxs {
			raw = append(raw, rawSpan{start: p[0], end: p[1], rule: ri})
			li.matchesText = append(li.matchesText, line[p[0]:p[1]])
		}
	}

	if len(raw) == 0 {
		return li
	}

	// Sort by start, then by rule order (earlier first). We'll resolve overlaps by "last wins".
	// Simple insertion: we'll build a coverage by walking bytes and remembering the last-applied rule.
	// For rendering, we need non-overlapping merged spans with final rule assignment.

	// Build a per-byte "owner" table (cheap for typical short lines; if you want, we can optimize later).
	b := []byte(line)
	owner := make([]int, len(b)) // -1 = none
	for i := range owner {
		owner[i] = -1
	}
	for _, rs := range raw {
		if rs.start < 0 {
			rs.start = 0
		}
		if rs.end > len(b) {
			rs.end = len(b)
		}
		if rs.start >= rs.end {
			continue
		}
		for i := rs.start; i < rs.end; i++ {
			owner[i] = rs.rule // last rule wins
		}
	}

	// Convert owner table to spans
	i := 0
	for i < len(owner) {
		if owner[i] == -1 {
			i++
			continue
		}
		rule := owner[i]
		j := i + 1
		for j < len(owner) && owner[j] == rule {
			j++
		}
		li.spans = append(li.spans, matchSpan{start: i, end: j, rule: rule})
		i = j
	}
	return li
}

type precomputed struct {
	lines       []string
	info        []lineInfo
	matchLines  []int
	totalLines  int
	totalMatches int
}

/* =========================
   UI list with matches
   ========================= */

func runListUIWithRules(lines []string, cfg Config, sourceKind, sourcePath string) (string, []string, int, bool, int, int) {
	if len(lines) == 0 {
		return "", nil, 0, false, 0, 0
	}

	// Compile rules & precompute matches
	cRules := compileRules(cfg)
	info := make([]lineInfo, len(lines))
	matchLines := make([]int, 0, 128)
	totalMatches := 0
	for i, ln := range lines {
		li := buildLineInfo(ln, cRules)
		info[i] = li
		if len(li.spans) > 0 {
			matchLines = append(matchLines, i)
		}
		totalMatches += len(li.matchesText)
	}

	// Screen
	s, err := tcell.NewScreen()
	if err != nil {
		fmt.Fprintln(os.Stderr, "tcell.NewScreen:", err)
		return "", nil, 0, false, len(matchLines), totalMatches
	}
	if err = s.Init(); err != nil {
		fmt.Fprintln(os.Stderr, "screen.Init:", err)
		return "", nil, 0, false, len(matchLines), totalMatches
	}
	defer s.Fini()

	// Styles
	normal := styleFrom(cfg.Colors.Normal)
	highlight := styleFrom(cfg.Colors.Highlight)
	gutterStyle := styleFrom(cfg.Colors.Gutter)
	gutterHLStyle := styleFrom(cfg.Colors.GutterHighlight)
	statusStyle := styleFrom(cfg.Colors.Status)
	topStyle := styleFrom(cfg.Colors.TopStatus)

	// Navigation state
	cursor := 0
	offset := 0
	lnWidth := digits(len(lines))
	gutterWidth := lnWidth + 2
	contentLeft := gutterWidth

	// Helpers
	ensureCursorVisible := func() {
		_, h := s.Size()
		usable := max(0, h-2) // top + bottom bars
		if cursor < offset {
			offset = cursor
		} else if cursor >= offset+usable {
			offset = cursor - usable + 1
		}
		maxOffset := max(0, len(lines)-usable)
		offset = clamp(offset, 0, maxOffset)
	}
	putStr := func(x, y int, style tcell.Style, str string, wlimit int) int {
		col := x
		w := 0
		for _, r := range str {
			if wlimit >= 0 && w >= wlimit {
				break
			}
			s.SetContent(col, y, r, nil, style)
			col++
			w++
		}
		return col - x
	}
	fillRow := func(y int, style tcell.Style) {
		w, _ := s.Size()
		for x := 0; x < w; x++ {
			s.SetContent(x, y, ' ', nil, style)
		}
	}
	// Jump to next/prev line with a match (wrap)
	nextMatch := func() bool {
		if len(matchLines) == 0 {
			return false
		}
		// find first > cursor
		for _, ml := range matchLines {
			if ml > cursor {
				cursor = ml
				return true
			}
		}
		// wrap
		cursor = matchLines[0]
		return true
	}
	prevMatch := func() bool {
		if len(matchLines) == 0 {
			return false
		}
		// find last < cursor
		for i := len(matchLines) - 1; i >= 0; i-- {
			if matchLines[i] < cursor {
				cursor = matchLines[i]
				return true
			}
		}
		// wrap
		cursor = matchLines[len(matchLines)-1]
		return true
	}

	// Draw a single content line with matched spans
	drawContentLine := func(rowY, lineIdx, width int, isSel bool) {
		line := lines[lineIdx]
		rowStyle := normal
		gutStyle := gutterStyle
		if isSel {
			rowStyle = highlight
			gutStyle = gutterHLStyle
		}
		// paint background
		fillRow(rowY, rowStyle)

		// gutter
		numStr := strconv.Itoa(lineIdx + 1)
		pad := lnWidth - len(numStr)
		x := 0
		for j := 0; j < pad; j++ {
			s.SetContent(x, rowY, ' ', nil, gutStyle)
			x++
		}
		x += putStr(x, rowY, gutStyle, numStr, -1)
		x += putStr(x, rowY, gutStyle, ": ", -1)

		// content area
		avail := max(0, width-contentLeft)
		if avail <= 0 {
			return
		}

		// Render: walk runes, track byte offsets; if inside a span, use its style.
		li := info[lineIdx]
		spanIdx := 0
		var curSpan *matchSpan
		if len(li.spans) > 0 {
			curSpan = &li.spans[0]
		}
		bytePos := 0
		col := contentLeft
		for _, r := range line {
			if col >= width {
				break
			}
			rw := 1 // (we'll keep it simple for now; can switch to runewidth later)
			rStart := bytePos
			bytePos += len(string(r)) // safe: Go strings are UTF-8; len of single-rune string = bytes

			// advance span if necessary
			for curSpan != nil && rStart >= curSpan.end && spanIdx+1 < len(li.spans) {
				spanIdx++
				curSpan = &li.spans[spanIdx]
			}
			// determine style
			st := rowStyle
			if curSpan != nil && rStart >= curSpan.start && rStart < curSpan.end {
				rule := cRules[curSpan.rule]
				if isSel {
					st = rule.styleInv
				} else {
					st = rule.style
				}
			}
			// draw rune
			s.SetContent(col, rowY, r, nil, st)
			col += rw
		}
	}

	draw := func() {
		s.Clear()
		w, h := s.Size()
		if h <= 0 {
			s.Show()
			return
		}
		usable := max(0, h-2)

		// Top bar
		fillRow(0, topStyle)
		topMsg := fmt.Sprintf(" match-lines: %d  matches: %d  |  n:next-match  N:prev-match ",
			len(matchLines), totalMatches)
		putStr(0, 0, topStyle, topMsg, -1)

		// Content rows
		for row := 0; row < usable; row++ {
			i := offset + row
			if i >= len(lines) {
				fillRow(1+row, normal)
				continue
			}
			drawContentLine(1+row, i, w, i == cursor)
		}

		// Bottom bar
		statusRow := h - 1
		fillRow(statusRow, statusStyle)
		lineNum := cursor + 1
		charCount := utf8.RuneCountInString(lines[cursor])
		btm := fmt.Sprintf(" %d/%d  |  chars: %d  |  ↑/↓ PgUp/PgDn Home/End  Enter=select  q/Esc=quit ",
			lineNum, len(lines), charCount)
		putStr(0, statusRow, statusStyle, btm, -1)

		s.Show()
	}

	ensureCursorVisible()
	draw()

	for {
		ev := s.PollEvent()
		switch e := ev.(type) {
		case *tcell.EventResize:
			s.Sync()
			ensureCursorVisible()
			draw()
		case *tcell.EventKey:
			switch e.Key() {
			case tcell.KeyEnter:
				// collect outputs
				return lines[cursor], info[cursor].matchesText, cursor + 1, true, len(matchLines), totalMatches
			case tcell.KeyEscape:
				return "", nil, 0, false, len(matchLines), totalMatches
			case tcell.KeyUp:
				if cursor > 0 {
					cursor--
					ensureCursorVisible()
					draw()
				}
			case tcell.KeyDown:
				if cursor < len(lines)-1 {
					cursor++
					ensureCursorVisible()
					draw()
				}
			case tcell.KeyPgUp:
				_, h := s.Size()
				usable := max(0, h-2)
				cursor = clamp(cursor-usable, 0, len(lines)-1)
				ensureCursorVisible()
				draw()
			case tcell.KeyPgDn:
				_, h := s.Size()
				usable := max(0, h-2)
				cursor = clamp(cursor+usable, 0, len(lines)-1)
				ensureCursorVisible()
				draw()
			case tcell.KeyHome:
				cursor = 0
				ensureCursorVisible()
				draw()
			case tcell.KeyEnd:
				cursor = len(lines) - 1
				ensureCursorVisible()
				draw()
			default:
				switch e.Rune() {
				case 'q':
					return "", nil, 0, false, len(matchLines), totalMatches
				case 'k':
					if cursor > 0 {
						cursor--
						ensureCursorVisible()
						draw()
					}
				case 'j':
					if cursor < len(lines)-1 {
						cursor++
						ensureCursorVisible()
						draw()
					}
				case 'n':
					if nextMatch() {
						ensureCursorVisible()
						draw()
					}
				case 'N':
					if prevMatch() {
						ensureCursorVisible()
						draw()
					}
				}
			}
		}
	}
}

/* =========================
   JSON output
   ========================= */

type SourceInfo struct {
	Kind string `json:"kind"`           // "file" or "primary"
	Path string `json:"path,omitempty"` // only when kind == "file"
}

type Output struct {
	Line       string     `json:"line"`
	Matches    []string   `json:"matches"`
	LineNumber int        `json:"line_number"` // 1-based; 0 on cancel
	Source     SourceInfo `json:"source"`
}

/* =========================
   Main
   ========================= */

func main() {
	filePath := flag.String("file", "", "path to a UTF-8 text file (one entry per line)")
	configPath := flag.String("config", "", "path to a config TOML (use ::default:: for per-user default path)")
	outputDefault := flag.Bool("output-default-config", false, "print or write a default config TOML and exit")
	force := flag.Bool("force", false, "allow overwriting existing config when outputting defaults")
	primary := flag.Bool("primary", false, "use PRIMARY selection via xclip as input (mutually exclusive with --file)")
	flag.Parse()

	/* Output default config path (safe write) */
	if *outputDefault {
		if *filePath != "" {
			fmt.Fprintln(os.Stderr, "error: --file cannot be used with --output-default-config")
			os.Exit(2)
		}
		var outPath string
		if *configPath == "::default::" {
			dp, err := defaultConfigPath()
			if err != nil {
				log.Fatalf("cannot resolve default config path: %v", err)
			}
			outPath = dp
		} else {
			outPath = *configPath // empty => stdout
		}

		if outPath == "" {
			if err := writeDefaultConfigTo(""); err != nil {
				log.Fatalf("failed to write default config: %v", err)
			}
			return
		}

		existed := fileExists(outPath)
		if existed && !*force {
			if !confirmOverwriteDialog(outPath) {
				fmt.Fprintf(os.Stderr, "cancelled: did not overwrite %s\n", outPath)
				return
			}
		}
		if err := writeDefaultConfigTo(outPath); err != nil {
			log.Fatalf("failed to write default config: %v", err)
		}
		if existed {
			fmt.Printf("overwrote config: %s\n", outPath)
		} else {
			fmt.Printf("wrote config: %s\n", outPath)
		}
		return
	}

	/* Choose input */
	if *filePath != "" && *primary {
		fmt.Fprintln(os.Stderr, "error: --file and --primary are mutually exclusive")
		os.Exit(2)
	}

	var lines []string
	source := SourceInfo{Kind: "file"}
	if *primary {
		source.Kind = "primary"
		txt, err := readPrimaryWithXclip()
		if err != nil {
			log.Fatalf("%v", err)
		}
		if strings.TrimSpace(txt) == "" {
			// Output empty JSON (cancel semantics)
			out := Output{Line: "", Matches: []string{}, LineNumber: 0, Source: source}
			enc := json.NewEncoder(os.Stdout)
			enc.SetEscapeHTML(false)
			_ = enc.Encode(out)
			return
		}
		lines = splitLines(txt)
		// drop trailing empty line
		if len(lines) > 0 && lines[len(lines)-1] == "" {
			lines = lines[:len(lines)-1]
		}
	} else {
		if *filePath == "" {
			fmt.Fprintln(os.Stderr, "error: --file is required (or use --primary)")
			os.Exit(2)
		}
		var err error
		lines, err = readLines(*filePath)
		if err != nil {
			log.Fatalf("failed to read file: %v", err)
		}
		if len(lines) == 0 {
			// Output empty JSON (cancel semantics)
			source.Path = *filePath
			out := Output{Line: "", Matches: []string{}, LineNumber: 0, Source: source}
			enc := json.NewEncoder(os.Stdout)
			enc.SetEscapeHTML(false)
			_ = enc.Encode(out)
			return
		}
		source.Path = *filePath
	}

	/* Load config */
	cfg, err := loadConfigMaybe(*configPath)
	if err != nil {
		log.Fatalf("failed to load config: %v", err)
	}

	/* Run UI + regex logic */
	lineText, matches, lineNum, ok, _, _ := runListUIWithRules(lines, cfg, source.Kind, source.Path)

	/* Always JSON output */
	var out Output
	out.Source = source
	if ok {
		out.Line = strings.TrimRightFunc(lineText, func(r rune) bool { return r == '\r' })
		out.Matches = matches
		out.LineNumber = lineNum
	} else {
		out.Line = ""
		out.Matches = []string{}
		out.LineNumber = 0
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(out)
}
