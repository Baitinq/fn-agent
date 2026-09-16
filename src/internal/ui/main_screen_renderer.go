package ui

import (
	"fmt"
	"io"
	"strings"

	"github.com/charmbracelet/x/ansi"
)

type mainScreenRenderer struct {
	out io.Writer

	width  int
	height int

	previousLines       []string
	previousViewportTop int
	hardwareCursorRow   int
	forceReplay         bool
}

func newMainScreenRenderer(out io.Writer, width, height int) *mainScreenRenderer {
	return &mainScreenRenderer{out: out, width: width, height: height}
}

func (r *mainScreenRenderer) resize(width, height int) {
	width, height = max(width, 1), max(height, 1)
	r.forceReplay = width != r.width || height != r.height
	r.width, r.height = width, height
}

func (r *mainScreenRenderer) render(lines []string, cursorRow, cursorCol int) error {
	return r.renderSized(lines, cursorRow, cursorCol, r.width, r.height)
}

func (r *mainScreenRenderer) renderSized(lines []string, cursorRow, cursorCol, width, height int) error {
	if len(lines) == 0 || width <= 0 || height <= 0 {
		return nil
	}

	resized := r.forceReplay || width != r.width || height != r.height
	firstChanged := r.firstChangedLine(lines)
	lastChanged := r.lastChangedLine(lines)
	if !resized && firstChanged < 0 {
		var b strings.Builder
		b.WriteString("\x1b[?2026h")
		writeVerticalMove(&b, cursorRow-r.hardwareCursorRow)
		fmt.Fprintf(&b, "\x1b[%dG\x1b[?25h\x1b[?2026l", cursorCol+1)
		if _, err := io.WriteString(r.out, b.String()); err != nil {
			return err
		}
		r.commit(lines, cursorRow, width, height, r.previousViewportTop)
		return nil
	}

	validateFrom := firstChanged
	if resized {
		validateFrom = max(0, len(lines)-height)
	}
	if err := validateLines(lines, validateFrom, width); err != nil {
		return err
	}

	newViewportTop := max(0, len(lines)-height)
	replay := resized || len(r.previousLines) == 0 || newViewportTop < r.previousViewportTop
	if !replay && len(lines) > len(r.previousLines) && firstChanged <= r.viewportBottom() && len(lines)-1 > r.viewportBottom() {
		clearStart := max(firstChanged, r.previousViewportTop)
		currentScreenRow := min(max(r.hardwareCursorRow-r.previousViewportTop, 0), height-1)
		clearScreenRow := clearStart - r.previousViewportTop
		var clear strings.Builder
		clear.WriteString("\x1b[?2026h")
		writeVerticalMove(&clear, clearScreenRow-currentScreenRow)
		clear.WriteString("\r\x1b[J")
		writeVerticalMove(&clear, currentScreenRow-clearScreenRow)
		clear.WriteString("\x1b[?2026l")
		if _, err := io.WriteString(r.out, clear.String()); err != nil {
			return err
		}
	}

	var b strings.Builder
	b.WriteString("\x1b[?2026h")
	paintStart := firstChanged
	paintEnd := len(lines) - 1
	if replay {
		paintStart = newViewportTop
		if len(r.previousLines) == 0 && !resized {
			paintStart = 0
			b.WriteString("\r\x1b[2K")
		} else {
			b.WriteString("\x1b[2J\x1b[H")
		}
	} else if len(lines) == len(r.previousLines) {
		paintEnd = lastChanged
		writeVerticalMove(&b, paintStart-r.hardwareCursorRow)
		for i := paintStart; i <= paintEnd; i++ {
			if i > paintStart {
				b.WriteString("\x1b[1B")
			}
			b.WriteString("\r\x1b[2K")
			b.WriteString(lines[i])
		}
	} else if paintStart > r.viewportBottom() {
		lastRow := paintStart - 1
		writeVerticalMove(&b, lastRow-r.hardwareCursorRow)
		b.WriteString("\r\n")
	} else {
		writeVerticalMove(&b, paintStart-r.hardwareCursorRow)
		b.WriteString("\r\x1b[J")
	}

	if replay || len(lines) != len(r.previousLines) {
		for i := paintStart; i < len(lines); i++ {
			if i > paintStart {
				b.WriteString("\r\n")
			}
			b.WriteString(lines[i])
		}
	}

	if paintStart >= len(lines) {
		paintEnd = paintStart
	}
	viewportTop := newViewportTop
	if !replay {
		viewportTop = max(r.previousViewportTop, newViewportTop)
	}
	writeVerticalMove(&b, cursorRow-paintEnd)
	fmt.Fprintf(&b, "\x1b[%dG", cursorCol+1)
	if cursorRow < 0 {
		b.WriteString("\x1b[?25l")
	} else {
		b.WriteString("\x1b[?25h")
	}
	b.WriteString("\x1b[?2026l")

	if _, err := io.WriteString(r.out, b.String()); err != nil {
		return err
	}
	r.commit(lines, cursorRow, width, height, viewportTop)
	return nil
}

func (r *mainScreenRenderer) firstChangedLine(lines []string) int {
	limit := min(len(lines), len(r.previousLines))
	for i := min(r.previousViewportTop, limit); i < limit; i++ {
		if lines[i] != r.previousLines[i] {
			return i
		}
	}
	if len(lines) != len(r.previousLines) {
		return limit
	}
	return -1
}

func (r *mainScreenRenderer) lastChangedLine(lines []string) int {
	for i := min(len(lines), len(r.previousLines)) - 1; i >= 0; i-- {
		if lines[i] != r.previousLines[i] {
			return i
		}
	}
	return max(len(lines), len(r.previousLines)) - 1
}

func (r *mainScreenRenderer) viewportBottom() int {
	return r.previousViewportTop + r.height - 1
}

func validateLines(lines []string, start, width int) error {
	for i := start; i < len(lines); i++ {
		if lineWidth(lines[i]) > width {
			return fmt.Errorf("rendered line %d is wider than the terminal (%d > %d)", i, lineWidth(lines[i]), width)
		}
	}
	return nil
}

func writeVerticalMove(b *strings.Builder, rows int) {
	if rows > 0 {
		fmt.Fprintf(b, "\x1b[%dB", rows)
	} else if rows < 0 {
		fmt.Fprintf(b, "\x1b[%dA", -rows)
	}
}

func (r *mainScreenRenderer) commit(lines []string, cursorRow, width, height, viewportTop int) {
	r.previousLines = append(r.previousLines[:0], lines...)
	r.width = width
	r.height = height
	r.previousViewportTop = viewportTop
	r.hardwareCursorRow = cursorRow
	r.forceReplay = false
}

func (r *mainScreenRenderer) stop() error {
	if len(r.previousLines) == 0 {
		return nil
	}
	var b strings.Builder
	writeVerticalMove(&b, len(r.previousLines)-1-r.hardwareCursorRow)
	b.WriteString("\r\n\x1b[0m\x1b[?25h")
	_, err := io.WriteString(r.out, b.String())
	return err
}

func lineWidth(s string) int { return ansi.StringWidth(stripANSI(s)) }

func sanitizeTerminalText(s string) string {
	s = stripANSI(s)
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")
	s = strings.ReplaceAll(s, "\t", "    ")
	return strings.Map(func(r rune) rune {
		if r < ' ' && r != '\n' || r >= 0x7f && r < 0xa0 {
			return -1
		}
		return r
	}, s)
}

func stripANSI(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); {
		if s[i] == 0x1b {
			i = ansiSequenceEnd(s, i)
			continue
		}
		b.WriteByte(s[i])
		i++
	}
	return b.String()
}

func ansiSequenceEnd(s string, start int) int {
	i := start + 1
	if i >= len(s) {
		return i
	}
	switch s[i] {
	case '[':
		i++
		for i < len(s) {
			c := s[i]
			i++
			if c >= 0x40 && c <= 0x7e {
				break
			}
		}
	case ']':
		i++
		for i < len(s) {
			if s[i] == 0x07 {
				i++
				break
			}
			if s[i] == 0x1b && i+1 < len(s) && s[i+1] == '\\' {
				i += 2
				break
			}
			i++
		}
	default:
		i++
	}
	return i
}
