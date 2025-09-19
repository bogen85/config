package main

import (
	"bufio"
	"flag"
	"fmt"
	"log"
	"os"
	"strconv"
	"unicode/utf8"

	"github.com/gdamore/tcell/v2"
)

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
	flag.Parse()

	if *filePath == "" {
		fmt.Fprintln(os.Stderr, "error: --file is required")
		os.Exit(2)
	}

	lines, err := readLines(*filePath)
	if err != nil {
		log.Fatalf("failed to read file: %v", err)
	}
	if len(lines) == 0 {
		return
	}

	s, err := tcell.NewScreen()
	if err != nil {
		log.Fatalf("tcell.NewScreen: %v", err)
	}
	if err = s.Init(); err != nil {
		log.Fatalf("screen.Init: %v", err)
	}
	// We'll call s.Fini() explicitly at exit before printing.

	def := tcell.StyleDefault
	normal := def.Foreground(tcell.ColorWhite).Background(tcell.ColorBlack)
	highlight := def.Foreground(tcell.ColorBlack).Background(tcell.ColorWhite)
	gutterStyle := def.Foreground(tcell.ColorGray).Background(tcell.ColorBlack)
	gutterHLStyle := def.Foreground(tcell.ColorBlack).Background(tcell.ColorWhite)
	statusStyle := def.Foreground(tcell.ColorBlack).Background(tcell.ColorYellow) // status bar

	cursor := 0
	offset := 0
	selected := ""
	lnWidth := digits(len(lines))         // width for line numbers
	gutterWidth := lnWidth + 2            // e.g., "  12: "
	contentLeft := gutterWidth            // text area starts after gutter

	ensureCursorVisible := func() {
		_, h := s.Size()
		usable := max(0, h-1) // bottom row reserved for status bar
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

		// Draw content rows
		for row := 0; row < usable; row++ {
			i := offset + row
			if i >= len(lines) {
				// clear empty rows with normal bg
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

			// Gutter: right-aligned line number + ": "
			fillRow(row, rowStyle) // ensure background spans full width for highlight
			numStr := strconv.Itoa(i + 1)
			pad := lnWidth - len(numStr)
			x := 0
			for j := 0; j < pad; j++ {
				s.SetContent(x, row, ' ', nil, gutStyle)
				x++
			}
			x += putStr(x, row, gutStyle, numStr, -1)
			x += putStr(x, row, gutStyle, ": ", -1)

			// Content text, truncated to available width
			avail := max(0, w-contentLeft)
			if avail > 0 {
				putStr(contentLeft, row, rowStyle, lines[i], avail)
			}
			// Remaining cells already filled by fillRow(row, rowStyle)
		}

		// Draw status bar (bottom row)
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
				selected = "" // explicit: cancel
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

	// Restore the original screen *before* printing so the line shows up in the normal buffer.
	s.Fini()

	if selected != "" {
		fmt.Println(selected)
	}
}
