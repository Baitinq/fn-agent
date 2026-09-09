package ui

import (
	"fmt"
	"io"
	"strings"

	tui "github.com/grindlemire/go-tui"
)

// mainScreenRenderer is a small Go port of Pi's main-screen renderer. Logical
// lines grow into native terminal history; only addressable changed lines are
// rewritten. Changes entirely above the viewport are retained for the next
// replay instead of clearing the visible screen.
type mainScreenRenderer struct {
	out                 io.Writer
	width, height       int
	previousLines       []string
	previousWidth       int
	previousHeight      int
	cursorRow           int
	hardwareCursorRow   int
	previousViewportTop int
}

func newMainScreenRenderer(out io.Writer, width, height int) *mainScreenRenderer {
	return &mainScreenRenderer{out: out, width: width, height: height}
}

func (r *mainScreenRenderer) resize(width, height int) {
	r.width, r.height = max(width, 1), max(height, 1)
}

func (r *mainScreenRenderer) render(lines []string, cursorRow, cursorCol int) error {
	width, height := max(r.width, 1), max(r.height, 1)
	validateLines := func(start, end int) error {
		for i := start; i < end; i++ {
			if w := lineWidth(lines[i]); w > width {
				return fmt.Errorf("rendered line %d exceeds terminal width (%d > %d)", i, w, width)
			}
		}
		return nil
	}
	widthChanged := r.previousWidth != 0 && r.previousWidth != width
	heightChanged := r.previousHeight != 0 && r.previousHeight != height
	previousBufferLength := height
	if r.previousHeight > 0 {
		previousBufferLength = r.previousViewportTop + r.previousHeight
	}
	prevViewportTop := r.previousViewportTop
	if heightChanged {
		prevViewportTop = max(0, previousBufferLength-height)
	}

	fullRender := func(clear bool) error {
		var b strings.Builder
		b.WriteString("\x1b[?2026h")
		if len(r.previousLines) == 0 {
			// Input can be echoed before raw mode is established. The first
			// frame owns the current line, so clear that echo without touching
			// the shell output and scrollback above it.
			b.WriteString("\r\x1b[2K")
		}
		if clear {
			b.WriteString("\x1b[2J\x1b[H\x1b[3J")
		}
		for i, line := range lines {
			if i > 0 {
				b.WriteString("\r\n")
			}
			b.WriteString(line)
		}
		b.WriteString("\x1b[?2026l")
		if _, err := io.WriteString(r.out, b.String()); err != nil {
			return err
		}
		r.cursorRow = max(0, len(lines)-1)
		r.hardwareCursorRow = r.cursorRow
		r.previousViewportTop = max(0, max(height, len(lines))-height)
		r.commit(lines, width, height)
		return r.positionCursor(cursorRow, cursorCol, len(lines))
	}

	if len(r.previousLines) == 0 && !widthChanged && !heightChanged {
		if err := validateLines(0, len(lines)); err != nil {
			return err
		}
		return fullRender(false)
	}
	if widthChanged || heightChanged {
		if err := validateLines(0, len(lines)); err != nil {
			return err
		}
		return fullRender(true)
	}

	firstChanged, lastChanged := -1, -1
	for i := prevViewportTop; i < max(len(lines), len(r.previousLines)); i++ {
		oldLine, newLine := "", ""
		if i < len(r.previousLines) {
			oldLine = r.previousLines[i]
		}
		if i < len(lines) {
			newLine = lines[i]
		}
		if oldLine != newLine {
			if firstChanged < 0 {
				firstChanged = i
			}
			lastChanged = i
		}
	}
	shrinking := len(lines) < len(r.previousLines)
	if shrinking {
		if firstChanged < 0 {
			firstChanged = len(lines)
		}
		lastChanged = len(r.previousLines) - 1
	}
	if firstChanged < 0 {
		r.previousViewportTop = prevViewportTop
		r.commit(lines, width, height)
		return r.positionCursor(cursorRow, cursorCol, len(lines))
	}
	reanchor := shrinking && max(0, len(lines)-height) < prevViewportTop
	if reanchor {
		// The input moved above the addressable screen. Repaint only the
		// visible tail, leaving terminal scrollback intact.
		prevViewportTop = max(0, len(lines)-height)
		firstChanged = prevViewportTop
		lastChanged = len(lines) - 1
	}
	if err := validateLines(firstChanged, min(lastChanged+1, len(lines))); err != nil {
		return err
	}

	appendStart := len(lines) > len(r.previousLines) && firstChanged == len(r.previousLines) && firstChanged > 0
	viewportTop := prevViewportTop
	hardwareRow := r.hardwareCursorRow
	viewportBottom := prevViewportTop + height - 1
	moveTarget := firstChanged
	if appendStart {
		moveTarget--
	}

	var b strings.Builder
	b.WriteString("\x1b[?2026h")
	if reanchor {
		b.WriteString("\x1b[2J\x1b[H")
		hardwareRow = prevViewportTop
	}
	if moveTarget > viewportBottom {
		currentScreenRow := min(max(hardwareRow-prevViewportTop, 0), height-1)
		if n := height - 1 - currentScreenRow; n > 0 {
			fmt.Fprintf(&b, "\x1b[%dB", n)
		}
		n := moveTarget - viewportBottom
		b.WriteString(strings.Repeat("\r\n", n))
		prevViewportTop += n
		viewportTop += n
		hardwareRow = moveTarget
	}
	currentScreenRow := hardwareRow - prevViewportTop
	targetScreenRow := moveTarget - viewportTop
	if d := targetScreenRow - currentScreenRow; d > 0 {
		fmt.Fprintf(&b, "\x1b[%dB", d)
	} else if d < 0 {
		fmt.Fprintf(&b, "\x1b[%dA", -d)
	}
	if appendStart {
		b.WriteString("\r\n")
	} else {
		b.WriteByte('\r')
	}

	for i := firstChanged; i <= lastChanged; i++ {
		if i > firstChanged {
			b.WriteString("\r\n")
		}
		b.WriteString("\x1b[2K")
		if i < len(lines) {
			b.WriteString(lines[i])
		}
	}
	b.WriteString("\x1b[?2026l")
	if _, err := io.WriteString(r.out, b.String()); err != nil {
		return err
	}

	r.cursorRow = max(0, len(lines)-1)
	r.hardwareCursorRow = lastChanged
	r.previousViewportTop = max(prevViewportTop, lastChanged-height+1)
	r.commit(lines, width, height)
	return r.positionCursor(cursorRow, cursorCol, len(lines))
}

func (r *mainScreenRenderer) commit(lines []string, width, height int) {
	r.previousLines = append(r.previousLines[:0], lines...)
	r.previousWidth, r.previousHeight = width, height
}

func (r *mainScreenRenderer) positionCursor(row, col, total int) error {
	if total == 0 || row < 0 {
		_, err := io.WriteString(r.out, "\x1b[?25l")
		return err
	}
	row = min(max(row, 0), total-1)
	col = max(col, 0)
	var b strings.Builder
	if d := row - r.hardwareCursorRow; d > 0 {
		fmt.Fprintf(&b, "\x1b[%dB", d)
	} else if d < 0 {
		fmt.Fprintf(&b, "\x1b[%dA", -d)
	}
	fmt.Fprintf(&b, "\x1b[%dG\x1b[?25h", col+1)
	_, err := io.WriteString(r.out, b.String())
	r.hardwareCursorRow = row
	return err
}

func (r *mainScreenRenderer) stop() error {
	if len(r.previousLines) == 0 {
		return nil
	}
	var b strings.Builder
	target := len(r.previousLines)
	if d := target - r.hardwareCursorRow; d > 0 {
		fmt.Fprintf(&b, "\x1b[%dB", d)
	} else if d < 0 {
		fmt.Fprintf(&b, "\x1b[%dA", -d)
	}
	b.WriteString("\r\n\x1b[0m\x1b[?25h")
	_, err := io.WriteString(r.out, b.String())
	return err
}

func lineWidth(s string) int { return tui.StringWidth(stripANSI(s)) }

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
