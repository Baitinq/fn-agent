package ui

import (
	"bytes"
	"fmt"
	"strings"
	"testing"
)

func historyFrame(inputLines int) []string {
	var lines []string
	for i := range 12 {
		lines = append(lines, fmt.Sprintf("history-%02d", i))
	}
	for i := range inputLines {
		lines = append(lines, fmt.Sprintf("input-%02d", i))
	}
	return append(lines, "footer")
}

func TestMainScreenRendererWritesEntireInitialTranscript(t *testing.T) {
	var out bytes.Buffer
	r := newMainScreenRenderer(&out, 80, 3)
	lines := []string{"oldest", "older", "recent", "editor", "footer"}

	if err := r.render(lines, 3, 0); err != nil {
		t.Fatal(err)
	}

	output := out.String()
	for _, line := range lines {
		if !strings.Contains(output, line) {
			t.Fatalf("initial render omitted %q: %q", line, output)
		}
	}
}

func TestMainScreenRendererCommitsEraseBeforeScrolling(t *testing.T) {
	var out bytes.Buffer
	r := newMainScreenRenderer(&out, 80, 5)
	oldLines := []string{"working", "box top", "box middle", "box bottom", "footer"}
	if err := r.render(oldLines, 2, 0); err != nil {
		t.Fatal(err)
	}

	out.Reset()
	newLines := append([]string{"reasoning"}, oldLines...)
	if err := r.render(newLines, 3, 0); err != nil {
		t.Fatal(err)
	}

	output := out.String()
	firstEnd := strings.Index(output, "\x1b[?2026l")
	secondStart := strings.LastIndex(output, "\x1b[?2026h")
	if !strings.Contains(output[:firstEnd], "\x1b[2K") || secondStart <= firstEnd {
		t.Fatalf("erase was not committed before scrolling repaint: %q", output)
	}
}

func TestMainScreenRendererKeepsLivePanelOutOfScrollback(t *testing.T) {
	var out bytes.Buffer
	r := newMainScreenRenderer(&out, 80, 4)
	if err := r.renderWithLiveStart([]string{"transcript", "working", "box", "footer"}, 2, 0, 1); err != nil {
		t.Fatal(err)
	}

	out.Reset()
	lines := []string{"transcript", "new output", "working", "box", "footer"}
	if err := r.renderWithLiveStart(lines, 3, 0, 2); err != nil {
		t.Fatal(err)
	}

	output := out.String()
	scroll := strings.Index(output, "\x1b[1S")
	if scroll < 0 {
		t.Fatalf("output did not scroll: %q", output)
	}
	for _, liveLine := range lines[2:] {
		if strings.Contains(output[:scroll], liveLine) {
			t.Fatalf("live line %q was written before the transcript scrolled: %q", liveLine, output)
		}
	}
}

func TestMainScreenRendererMovesCursorWithoutRepainting(t *testing.T) {
	var out bytes.Buffer
	r := newMainScreenRenderer(&out, 80, 5)
	if err := r.render([]string{"hello"}, 0, 5); err != nil {
		t.Fatal(err)
	}

	out.Reset()
	if err := r.render([]string{"hello"}, 0, 2); err != nil {
		t.Fatal(err)
	}
	if got := out.String(); !strings.Contains(got, "\x1b[3G") || strings.Contains(got, "hello") {
		t.Fatalf("cursor-only output = %q", got)
	}
}

func TestMainScreenRendererShrinksAndGrowsWithinViewport(t *testing.T) {
	var out bytes.Buffer
	r := newMainScreenRenderer(&out, 80, 6)
	for _, inputLines := range []int{5, 2, 4, 1, 5, 2} {
		out.Reset()
		lines := historyFrame(inputLines)
		if err := r.render(lines, len(lines)-2, 3); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(out.String(), "\x1b[3J") {
			t.Fatalf("input with %d lines cleared scrollback: %q", inputLines, out.String())
		}
		wantTop := max(0, len(lines)-6)
		if r.previousViewportTop != wantTop {
			t.Fatalf("input with %d lines left viewport at %d, want %d", inputLines, r.previousViewportTop, wantTop)
		}
		if r.hardwareCursorRow != len(lines)-2 {
			t.Fatalf("hardware cursor row = %d, want %d", r.hardwareCursorRow, len(lines)-2)
		}
	}
}

func TestMainScreenRendererClearsRowsBeforeScrollingThem(t *testing.T) {
	var out bytes.Buffer
	r := newMainScreenRenderer(&out, 80, 5)
	oldLines := []string{"working", "box top", "box middle", "box bottom", "footer"}
	if err := r.render(oldLines, 2, 0); err != nil {
		t.Fatal(err)
	}

	out.Reset()
	newLines := append([]string{"reasoning"}, oldLines...)
	if err := r.render(newLines, 3, 0); err != nil {
		t.Fatal(err)
	}

	output := out.String()
	firstPaint := strings.Index(output, "reasoning")
	if firstPaint < 0 {
		t.Fatalf("did not paint new line: %q", output)
	}
	if !strings.Contains(output[:firstPaint], "\x1b[2K") {
		t.Fatalf("did not erase the old viewport before first paint: %q", output)
	}
}

func TestMainScreenRendererErasesRemovedRows(t *testing.T) {
	for _, suffix := range [][]string{{"removed", "removed"}, {"", ""}} {
		t.Run(fmt.Sprintf("suffix=%q", suffix), func(t *testing.T) {
			var out bytes.Buffer
			r := newMainScreenRenderer(&out, 80, 6)
			lines := []string{"history", "input"}
			if err := r.render(append(append([]string{}, lines...), suffix...), 1, 2); err != nil {
				t.Fatal(err)
			}
			out.Reset()
			if err := r.render(lines, 1, 2); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(out.String(), "\x1b[2K") {
				t.Fatalf("did not erase removed rows: %q", out.String())
			}
			if strings.Contains(out.String(), "history") || strings.Contains(out.String(), "\x1b[2J") || strings.Contains(out.String(), "\x1b[3J") {
				t.Fatalf("replayed history: %q", out.String())
			}
			if r.hardwareCursorRow != 1 || r.previousViewportTop != 0 {
				t.Fatalf("cursor = %d, viewport = %d", r.hardwareCursorRow, r.previousViewportTop)
			}
		})
	}
}

func TestMainScreenRendererShrinksOversizedInput(t *testing.T) {
	for _, inputLines := range []int{9, 2} {
		t.Run(fmt.Sprint(inputLines), func(t *testing.T) {
			var out bytes.Buffer
			r := newMainScreenRenderer(&out, 80, 6)
			lines := historyFrame(10)
			if err := r.render(lines, len(lines)-2, 0); err != nil {
				t.Fatal(err)
			}
			out.Reset()
			lines = historyFrame(inputLines)
			if err := r.render(lines, len(lines)-2, 0); err != nil {
				t.Fatal(err)
			}
			if strings.Contains(out.String(), "\x1b[3J") || strings.Contains(out.String(), "history-00") {
				t.Fatalf("replayed scrollback: %q", out.String())
			}
			wantTop := len(lines) - 6
			if !strings.Contains(out.String(), "\x1b[2K") {
				t.Fatalf("shrinking input did not reanchor viewport: %q", out.String())
			}
			if inputLines == 2 && !strings.Contains(out.String(), "history-09") {
				t.Fatalf("did not restore visible history: %q", out.String())
			}
			if r.previousViewportTop != wantTop || r.hardwareCursorRow != len(lines)-2 {
				t.Fatalf("viewport = %d, cursor = %d; want %d, %d", r.previousViewportTop, r.hardwareCursorRow, wantTop, len(lines)-2)
			}
			out.Reset()
			lines = historyFrame(11)
			if err := r.render(lines, len(lines)-2, 0); err != nil {
				t.Fatal(err)
			}
			if strings.Contains(out.String(), "\x1b[2J") || strings.Contains(out.String(), "\x1b[3J") || strings.Contains(out.String(), "history-00") {
				t.Fatalf("growing input replayed history: %q", out.String())
			}
		})
	}
}
