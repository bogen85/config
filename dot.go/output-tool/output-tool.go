package main

import (
	"bufio"
	"flag"
	"fmt"
	"log"
	"os"
	"strconv"
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

func writeDefaultConfig(path string) error {
	cfg := defaultConfig()
	if path == "" {
		// stdout
		return toml.NewEncoder(os.Stdout).Encode(cfg)
	}
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return toml.NewEncoder(f).Encode(cfg)
}

func loadConfig(path string) (Config, error) {
	cfg := defaultConfig()
	if path == "" {
		return cfg, nil
	}
	_, err := toml.DecodeFile(path, &cfg)
	return cfg, err
}

func styleFrom(p ColorPair) tcell.Style {
	fg := tcell.GetColor(p.FG)
	bg := tcell.GetColor(p.BG)
	return tcell.StyleDefault.Foreground(fg).Background(bg)
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

func main() {
	filePath := flag.String("file", "", "path to a UTF-8 text file (one entry per line)")
	configPath := flag.String("config", "", "path to a config TOML (colors)")
	outputDefault := flag.Bool("output-default-config", false, "print or write a default config TOML and exit")
	flag.Parse()

	// If --output-default-config, disallow --file.
	if *outputDefault {
		if *filePath != "" {
			fmt.Fprintln(os.Stderr, "error: --file cannot be used with --output-default-config")
			os.Exit(2)
		}
		if err := writeDefaultConfig(*configPath); err != nil {
			log.Fatalf("failed to write default config: %v", err)
		}
		return
	}

	// Normal run: need --file
	if *filePath == "" {
		fmt.Fprintln(os.Stderr, "error: --file is required (or use --output-default-config)")
		os.Exit(2)
	}

	lines, err := readLines(*filePath)
	if err != nil {
		log.Fatalf("failed to read file: %v", err)
	}
	if len(lines) == 0 {
		return
	}

	cfg, err := loadConfig(*configPath)
	if err != nil {
		log.Fatalf("failed to load config: %v", err)
	}

	s, err := tcell.NewScreen()
	if err != nil {
		log.Fatalf("tcell.NewScreen: %v", err)
	}
	if err = s.Init(); err != nil {
		log.Fatalf("screen.Init: %v", err)
	}
	// We'll call s.Fini() explicitly before printing the result.

	normal := styleFrom(cfg.Colors.Normal)
	highlight := styleFrom(cfg.Colors.Highlight)
	gutterStyle := styleFrom(cfg.Colors.Gutter)
	gutterHLStyle := styleFrom(cfg.Colors.GutterHighlight)
	statusStyle := styleFrom(cfg.Colors.Status)

	cursor := 0
	offset := 0
	selected := ""
	lnWidth := digits(len(lines)) // line number width
	gutterWidth := lnWidth + 2    // "NN: "
	contentLeft := gutterWidth

	ensureCursorVisible := func() {
		_, h := s.Size()
		usable := max(0, h-1) // bottom row = status bar
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

		// Content
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

			// Gutter: right-aligned number + ": "
			numStr := strconv.Itoa(i + 1)
			pad := lnWidth - len(numStr)
			x := 0
			for j := 0; j < pad; j++ {
				s.SetContent(x, row, ' ', nil, gutStyle)
				x++
			}
			x += putStr(x, row, gutStyle, numStr, -1)
			x += putStr(x, row, gutStyle, ": ", -1)

			// Line content
			avail := max(0, w-contentLeft)
			if avail > 0 {
				putStr(contentLeft, row, rowStyle, lines[i], avail)
			}
		}

		// Status bar
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

loop:
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
				selected = lines[cursor]
				break loop
			case tcell.KeyEscape:
				selected = ""
				break loop
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
					selected = ""
					break loop
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

	// Restore terminal before printing result.
	s.Fini()
	if selected != "" {
		fmt.Println(selected)
	}
}
