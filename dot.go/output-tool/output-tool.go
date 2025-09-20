package main

import (
	"bufio"
	"bytes"
	"flag"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/BurntSushi/toml"
	"github.com/gdamore/tcell/v2"
)

type ColorPair struct {
	FG string `toml:"fg"`
	BG string `toml:"bg"`
}

type Config struct {
	Colors struct {
		Normal          ColorPair `toml:"normal"`
		Highlight       ColorPair `toml:"highlight"`
		Gutter          ColorPair `toml:"gutter"`
		GutterHighlight ColorPair `toml:"gutter_highlight"`
		Status          ColorPair `toml:"status"`
	} `toml:"colors"`
}

func defaultConfig() Config {
	var c Config
	c.Colors.Normal = ColorPair{FG: "white", BG: "black"}
	c.Colors.Highlight = ColorPair{FG: "black", BG: "white"}
	c.Colors.Gutter = ColorPair{FG: "gray", BG: "black"}
	c.Colors.GutterHighlight = ColorPair{FG: "black", BG: "white"}
	c.Colors.Status = ColorPair{FG: "black", BG: "yellow"}
	return c
}

func styleFrom(p ColorPair) tcell.Style {
	return tcell.StyleDefault.Foreground(tcell.GetColor(p.FG)).Background(tcell.GetColor(p.BG))
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

	// Read PRIMARY selection
	out, err := exec.Command("xclip", "-o", "-selection", "primary").Output()
	if err != nil {
		return "", fmt.Errorf("reading PRIMARY via xclip failed: %w", err)
	}

	// Clear PRIMARY by setting it to empty input
	cmd := exec.Command("xclip", "-selection", "primary", "-in")
	cmd.Stdin = bytes.NewReader(nil) // empty
	_ = cmd.Run()                    // best-effort

	return string(out), nil
}

func splitLines(s string) []string {
	// Accept any file type; assume UTF-8 text lines.
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

func writeDefaultConfigTo(path string, force bool) error {
	cfg := defaultConfig()
	// stdout when empty path
	if path == "" {
		return toml.NewEncoder(os.Stdout).Encode(cfg)
	}
	if err := ensureDir(path); err != nil {
		return err
	}
	// handle overwrite outside; this function just writes
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return toml.NewEncoder(f).Encode(cfg)
}

func loadConfigMaybe(path string) (Config, error) {
	// If empty: check default per-user path; if exists, load; else built-in defaults.
	cfg := defaultConfig()
	var tryPath string

	if path == "" || path == "::default::" {
		dp, err := defaultConfigPath()
		if err == nil {
			tryPath = dp
		}
	} else {
		tryPath = path
	}

	if tryPath != "" {
		if _, err := os.Stat(tryPath); err == nil {
			_, err = toml.DecodeFile(tryPath, &cfg)
			return cfg, err
		}
	}
	return cfg, nil
}

func confirmOverwriteDialog(target string) bool {
	s, err := tcell.NewScreen()
	if err != nil {
		// If tcell init fails, fall back to refusing overwrite
		return false
	}
	if err = s.Init(); err != nil {
		return false
	}
	defer s.Fini()

	def := tcell.StyleDefault
	bg := def.Background(tcell.ColorReset)
	s.Clear()
	w, h := s.Size()

	msgLines := []string{
		"Configuration file already exists:",
		target,
		"",
		"Overwrite it?  [y]es / [n]o",
	}
	// draw simple centered box
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

	// border & fill
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
	// text
	for i, m := range msgLines {
		start := x0 + 2
		for j, r := range m {
			s.SetContent(start+j, y0+1+i, r, nil, bg)
		}
	}
	s.Show()

	for {
		ev := s.PollEvent()
		switch e := ev.(type) {
		case *tcell.EventResize:
			s.Sync()
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

func runListUI(lines []string, cfg Config) (string, bool) {
	if len(lines) == 0 {
		return "", false
	}
	s, err := tcell.NewScreen()
	if err != nil {
		fmt.Fprintln(os.Stderr, "tcell.NewScreen:", err)
		return "", false
	}
	if err = s.Init(); err != nil {
		fmt.Fprintln(os.Stderr, "screen.Init:", err)
		return "", false
	}
	defer s.Fini()

	normal := styleFrom(cfg.Colors.Normal)
	highlight := styleFrom(cfg.Colors.Highlight)
	gutterStyle := styleFrom(cfg.Colors.Gutter)
	gutterHLStyle := styleFrom(cfg.Colors.GutterHighlight)
	statusStyle := styleFrom(cfg.Colors.Status)

	cursor := 0
	offset := 0
	lnWidth := digits(len(lines))
	gutterWidth := lnWidth + 2
	contentLeft := gutterWidth

	ensureCursorVisible := func() {
		_, h := s.Size()
		usable := max(0, h-1)
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

	draw := func() {
		s.Clear()
		w, h := s.Size()
		usable := max(0, h-1)

		for row := 0; row < usable; row++ {
			i := offset + row
			if i >= len(lines) {
				fillRow(row, normal)
				continue
			}
			hl := (i == cursor)
			rowStyle := normal
			gutStyle := gutterStyle
			if hl {
				rowStyle = highlight
				gutStyle = gutterHLStyle
			}
			fillRow(row, rowStyle)

			numStr := strconv.Itoa(i + 1)
			pad := lnWidth - len(numStr)
			x := 0
			for j := 0; j < pad; j++ {
				s.SetContent(x, row, ' ', nil, gutStyle)
				x++
			}
			x += putStr(x, row, gutStyle, numStr, -1)
			x += putStr(x, row, gutStyle, ": ", -1)

			avail := max(0, w-contentLeft)
			if avail > 0 {
				putStr(contentLeft, row, rowStyle, lines[i], avail)
			}
		}

		if h > 0 {
			statusRow := h - 1
			fillRow(statusRow, statusStyle)
			lineNum := cursor + 1
			charCount := utf8.RuneCountInString(lines[cursor])
			status := fmt.Sprintf(" %d/%d  |  chars: %d  |  ↑/↓ PgUp/PgDn Home/End  Enter=select  q/Esc=quit ",
				lineNum, len(lines), charCount)
			putStr(0, statusRow, statusStyle, status, -1)
		}
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
				return lines[cursor], true
			case tcell.KeyEscape:
				return "", false
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
				usable := max(0, h-1)
				cursor = clamp(cursor-usable, 0, len(lines)-1)
				ensureCursorVisible()
				draw()
			case tcell.KeyPgDn:
				_, h := s.Size()
				usable := max(0, h-1)
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
					return "", false
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
				}
			}
		}
	}
}

func main() {
	filePath := flag.String("file", "", "path to a UTF-8 text file (one entry per line)")
	configPath := flag.String("config", "", "path to a config TOML (use ::default:: for per-user default path)")
	outputDefault := flag.Bool("output-default-config", false, "print or write a default config TOML and exit")
	force := flag.Bool("force", false, "allow overwriting existing config when outputting defaults")
	primary := flag.Bool("primary", false, "use PRIMARY selection via xclip as input (mutually exclusive with --file)")
	flag.Parse()

	// Validate mutual exclusions for this step:
	if *outputDefault {
		if *filePath != "" {
			fmt.Fprintln(os.Stderr, "error: --file cannot be used with --output-default-config")
			os.Exit(2)
		}
		// Where to write defaults?
		var outPath string
		if *configPath == "::default::" {
			dp, err := defaultConfigPath()
			if err != nil {
				log.Fatalf("cannot resolve default config path: %v", err)
			}
			outPath = dp
		} else {
			outPath = *configPath // may be empty => stdout
		}

		// If writing to a file, guard overwrites.
		if outPath != "" {
			if _, err := os.Stat(outPath); err == nil && !*force {
				// Confirm via tcell dialog
				if !confirmOverwriteDialog(outPath) {
					// user declined
					return
				}
			}
		}
		if err := writeDefaultConfigTo(outPath, *force); err != nil {
			log.Fatalf("failed to write default config: %v", err)
		}
		return
	}

	// Normal run: choose input source
	if *filePath != "" && *primary {
		fmt.Fprintln(os.Stderr, "error: --file and --primary are mutually exclusive")
		os.Exit(2)
	}
	var lines []string
	if *primary {
		txt, err := readPrimaryWithXclip()
		if err != nil {
			log.Fatalf("%v", err)
		}
		// If empty, exit quietly
		if strings.TrimSpace(txt) == "" {
			return
		}
		lines = splitLines(txt)
		// Remove any trailing empty last line caused by split
		if len(lines) > 0 && len(lines[len(lines)-1]) == 0 {
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
			return
		}
	}

	// Load config (explicit path, ::default::, or per-user default; else built-ins)
	cfg, err := loadConfigMaybe(*configPath)
	if err != nil {
		log.Fatalf("failed to load config: %v", err)
	}

	// Run UI
	selected, ok := runListUI(lines, cfg)

	// After UI closes, print selection if any
	if ok && selected != "" {
		// Avoid double trailing newline if input contained CRLF; fmt.Println is fine.
		fmt.Println(strings.TrimRightFunc(selected, func(r rune) bool { return r == '\r' }))
	}
}

/*
Build (non-modules env like yours):
  (export GO111MODULE=off; export GOPATH=/usr/share/gocode; go build -o output-tool output-tool.go)

Examples:
  # Write default config to stdout
  ./output-tool --output-default-config

  # Write default config to per-user path (~/.local/share/user-dev-tooling/<exe>/config.toml)
  ./output-tool --output-default-config --config=::default::

  # Same, but require confirmation if file exists (no --force)
  ./output-tool --output-default-config --config=./cfg.toml

  # Load config (per-user default path)
  ./output-tool --file=./data.txt --config=::default::

  # Use PRIMARY selection via xsel
  ./output-tool --primary
*/
