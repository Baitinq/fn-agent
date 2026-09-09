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
			if got := strings.Count(out.String(), "\x1b[2K"); got != len(suffix) {
				t.Fatalf("cleared %d rows, want %d: %q", got, len(suffix), out.String())
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
			if !strings.Contains(out.String(), "\x1b[2J") {
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
