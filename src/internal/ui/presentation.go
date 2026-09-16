package ui

import (
	"fmt"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"charm.land/glamour/v2"
	"charm.land/glamour/v2/styles"
	"github.com/alecthomas/chroma/v2/quick"
	"github.com/charmbracelet/x/ansi"
	tui "github.com/grindlemire/go-tui"
	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/extension"
	"github.com/yuin/goldmark/text"

	"github.com/Baitinq/fn-agent/src/internal/assert"
)

func (s *fnUI) render(width int, viewportHeight ...int) ([]string, int, int) {
	width = max(width, 10)
	s.setTextareaWidth(max(width-4, 1))
	if len(s.undoOptions) > 0 {
		height := 20
		if len(viewportHeight) > 0 {
			height = viewportHeight[0]
		}
		return s.renderUndoSelector(width, height)
	}
	lines := s.frameLines[:0]
	for i := range s.messages {
		lines = append(lines, s.renderedMessageLines(&s.messages[i], width)...)
		lines = append(lines, "")
	}
	if s.reasoningText.Len() > 0 {
		lines = append(lines, renderedMessageLines(message{role: "reasoning", text: s.reasoningText.String()}, width)...)
		lines = append(lines, "")
	}
	if s.streamingText.Len() > 0 {
		lines = append(lines, s.renderedStreamingText(width)...)
		lines = append(lines, "")
	}
	s.liveStart = len(lines)
	if s.responding {
		if s.retryAttempt > 0 {
			remaining := max(time.Until(s.retryDeadline), 0)
			seconds := int((remaining + time.Second - 1) / time.Second)
			status := fmt.Sprintf(" Retrying (%d/%d) in %ds... (Esc to cancel)", s.retryAttempt, s.retryMaxAttempts, seconds)
			status += workingDurationLabel(s.requestStartedAt, time.Now())
			lines = append(lines, ansiRGBStyle(piAmber, "", false, false, spinnerFrames[s.spinnerFrame])+ansi256FG(242, truncateCells(status, width-lineWidth(spinnerFrames[s.spinnerFrame]))))
		} else {
			status := " Working…" + workingDurationLabel(s.requestStartedAt, time.Now())
			lines = append(lines, ansiRGBStyle(piAccent, "", false, false, spinnerFrames[s.spinnerFrame])+ansi256FG(242, truncateCells(status, width-lineWidth(spinnerFrames[s.spinnerFrame]))))
		}
	}
	for _, p := range s.pendingInputs {
		lines = append(lines, renderPendingLine(p, width))
	}
	if len(s.pendingInputs) > 0 {
		lines = append(lines, ansi256FG(245, truncateCells("↳ Ctrl+↑ to edit all queued messages", width)))
	}
	editor, crow, ccol := renderEditor(s.textarea.Text(), s.textarea.CursorPos(), width)
	cursorRow := len(lines) + crow
	lines = append(lines, editor...)
	lines = append(lines, renderFooter(s.modelName, s.reasoningEffort, s.contextTokens, s.cwd, s.sessionID, max(width-2, 0)))
	if len(viewportHeight) > 0 {
		filler := max(viewportHeight[0]-len(lines), 0)
		if filler > 0 {
			lines = append(make([]string, filler), lines...)
			cursorRow += filler
		}
	}
	s.frameLines = lines
	return lines, cursorRow, ccol
}

func (s *fnUI) renderUndoSelector(width, height int) ([]string, int, int) {
	assert.That(len(s.undoOptions) > 0, "render undo selector without options")
	assert.That(s.undoSelected >= 0 && s.undoSelected < len(s.undoOptions), "render undo selector with invalid selection")
	visible := max(1, height-4)
	start := max(0, s.undoSelected-visible/2)
	start = min(start, max(0, len(s.undoOptions)-visible))
	end := min(len(s.undoOptions), start+visible)
	lines := []string{ansiRGBStyle(piBlue, "", false, false, truncateCells(" Select a turn to undo to", width)), ""}
	for i := start; i < end; i++ {
		prefix, color := "  ", piGray
		if i == s.undoSelected {
			prefix, color = "› ", piBlue
		}
		text := strings.ReplaceAll(sanitizeTerminalText(s.undoOptions[i].text), "\n", " ")
		lines = append(lines, ansiRGBStyle(color, "", false, false, truncateCells(prefix+text, width)))
	}
	lines = append(lines, "", ansi256FG(245, truncateCells(" ↑/↓ select · Enter undo · Esc cancel", width)))
	if filler := max(height-len(lines), 0); filler > 0 {
		lines = append(make([]string, filler), lines...)
	}
	s.frameLines = lines
	return lines, -1, 0
}

func (s *fnUI) renderedMessageLines(msg *message, width int) []string {
	now := time.Now()
	duration := ""
	if msg.toolState == "pending" {
		duration = toolDurationLabel(*msg, now)
	}
	if msg.renderedWidth == width && msg.renderedLines != nil && msg.renderedToolDuration == duration {
		return msg.renderedLines
	}
	lines := strings.Split(renderedMessageAt(*msg, width, now), "\n")
	msg.renderedWidth, msg.renderedLines, msg.renderedToolDuration = width, lines, duration
	return lines
}

func renderedMessageLines(msg message, width int) []string {
	return strings.Split(renderedMessage(msg, width), "\n")
}
func renderedMessage(msg message, width int) string {
	return renderedMessageAt(msg, width, time.Now())
}

func renderedMessageAt(msg message, width int, now time.Time) string {
	width = max(width, 10)
	contentWidth := max(width-2, 8)
	var lines []string
	switch msg.role {
	case "you":
		rail := ansiRGBStyle(piAccent, piUserMessageBg, false, false, "▌")
		lines = append(lines, piBoxLine(rail, width, piText, piUserMessageBg, false))
		for _, line := range wrapPlain(msg.text, max(contentWidth-1, 1)) {
			lines = append(lines, piBoxLine(rail+" "+line, width, piText, piUserMessageBg, false))
		}
		lines = append(lines, piBoxLine(rail, width, piText, piUserMessageBg, false))
	case "reasoning":
		for _, line := range wrapPlain(msg.text, contentWidth) {
			lines = append(lines, " "+ansiRGBStyle(piGray, "", false, true, line))
		}
	case "agent":
		lines = append(lines, renderedMarkdownLines(msg.text, contentWidth)...)
	case "system":
		for _, line := range wrapPlain(msg.text, contentWidth) {
			lines = append(lines, " "+ansi256FG(242, line))
		}
	case "status":
		for _, line := range wrapPlain(msg.text, width-3) {
			lines = append(lines, " "+ansiRGBStyle(piGreen, "", false, false, "✓")+ansi256FG(242, " "+line))
		}
	case "error":
		for _, line := range wrapPlain(msg.text, contentWidth) {
			lines = append(lines, " "+ansiRGBStyle(piError, "", false, false, line))
		}
	case "tool":
		return renderedToolMessage(msg, width, now)
	default:
		for _, line := range wrapPlain(msg.text, contentWidth) {
			lines = append(lines, " "+ansiRGBStyle(piText, "", false, false, line))
		}
	}
	return strings.Join(lines, "\n")
}

type footerPart struct {
	text  string
	color string
}

func renderFooter(model, effort string, contextTokens int64, cwd, sessionID string, width int) string {
	parts := []footerPart{{model, piText}, {" (", piDim}, {effort, piGray}, {")  ·  ", piDim}, {formatTokenCount(contextTokens) + " context", piGray}, {"  ·  ", piDim}, {cwd, piGray}, {"  ·  ", piDim}, {sessionID, piGray}}
	var out strings.Builder
	for _, part := range parts {
		if width <= 0 {
			break
		}
		text := truncateCells(part.text, width)
		out.WriteString(ansiRGBStyle(part.color, "", false, false, text))
		width -= lineWidth(text)
	}
	return out.String()
}

func reasoningEffortColor(string) string {
	return piGray
}

const (
	piText          = "212;212;212"
	piGray          = "128;128;128"
	piDim           = "102;102;102"
	piAccent        = "138;190;183"
	piBlue          = "129;162;190"
	piGreen         = "181;189;104"
	piAmber         = "240;198;116"
	piError         = "204;102;102"
	piUserMessageBg = "36;44;56"
	piToolBg        = "38;40;46"
)

func workingDurationLabel(startedAt, now time.Time) string {
	if startedAt.IsZero() {
		return ""
	}
	return " (" + formatToolDuration(now.Sub(startedAt)) + ")"
}

func completedRequestMessage(startedAt, finishedAt time.Time) string {
	if startedAt.IsZero() {
		return ""
	}
	return "Done in " + formatToolDuration(finishedAt.Sub(startedAt)) + " at " + finishedAt.Format("15:04")
}

func formatToolDuration(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	seconds := int(d / time.Second)
	if seconds < 60 {
		return fmt.Sprintf("%ds", seconds)
	}
	minutes := seconds / 60
	if minutes < 60 {
		return fmt.Sprintf("%dm %02ds", minutes, seconds%60)
	}
	hours := minutes / 60
	if hours < 24 {
		return fmt.Sprintf("%dh %02dm", hours, minutes%60)
	}
	days := hours / 24
	if remainingHours := hours % 24; remainingHours > 0 {
		return fmt.Sprintf("%dd %02dh", days, remainingHours)
	}
	return fmt.Sprintf("%dd %02dm", days, minutes%60)
}

func toolDurationLabel(msg message, now time.Time) string {
	if msg.toolStartedAt.IsZero() {
		return ""
	}
	end := msg.toolFinishedAt
	if end.IsZero() {
		elapsed := max(now.Sub(msg.toolStartedAt), 0).Truncate(time.Second)
		return " (" + formatToolDuration(elapsed) + ")"
	}
	return " (" + formatToolDuration(end.Sub(msg.toolStartedAt)) + ")"
}

func renderedMarkdownLines(text string, width int) []string {
	text = sanitizeTerminalText(text)
	style := styles.ASCIIStyleConfig
	zero := uint(0)
	style.Document.Margin = &zero
	style.Heading.Color = stringPointer("#F0C674")
	style.Heading.Bold = boolPointer(true)
	style.H1.Prefix, style.H2.Prefix = "", ""
	style.H1.Underline = boolPointer(true)
	style.Strikethrough.BlockPrefix, style.Strikethrough.BlockSuffix = "", ""
	style.Strikethrough.CrossedOut = boolPointer(true)
	style.Emph.BlockPrefix, style.Emph.BlockSuffix = "", ""
	style.Emph.Italic = boolPointer(true)
	style.Strong.BlockPrefix, style.Strong.BlockSuffix = "", ""
	style.Strong.Bold = boolPointer(true)
	style.Code.Color = stringPointer("#8ABEB7")
	style.Code.BlockPrefix, style.Code.BlockSuffix = "", ""
	style.Link.Color = stringPointer("#666666")
	style.LinkText.Color = stringPointer("#81A2BE")
	style.LinkText.Underline = boolPointer(true)
	style.BlockQuote.Color = stringPointer("#808080")
	style.BlockQuote.Italic = boolPointer(true)
	style.HorizontalRule.Color = stringPointer("#808080")
	style.CodeBlock.Margin = &zero
	style.CodeBlock.Color = stringPointer("#B5BD68")
	style.CodeBlock.Chroma = styles.DarkStyleConfig.CodeBlock.Chroma
	style.CodeBlock.Chroma.Text.Color = stringPointer("#B5BD68")
	style.CodeBlock.Chroma.Comment.Color = stringPointer("#6A9955")
	style.CodeBlock.Chroma.Keyword.Color = stringPointer("#569CD6")
	style.CodeBlock.Chroma.NameFunction.Color = stringPointer("#DCDCAA")
	style.CodeBlock.Chroma.Name.Color = stringPointer("#9CDCFE")
	style.CodeBlock.Chroma.LiteralString.Color = stringPointer("#CE9178")
	style.CodeBlock.Chroma.LiteralNumber.Color = stringPointer("#B5CEA8")
	style.CodeBlock.Chroma.KeywordType.Color = stringPointer("#4EC9B0")
	style.CodeBlock.Chroma.Operator.Color = stringPointer("#D4D4D4")
	style.CodeBlock.Chroma.Punctuation.Color = stringPointer("#D4D4D4")
	renderer, err := glamour.NewTermRenderer(
		glamour.WithStyles(style),
		glamour.WithWordWrap(max(width-1, 1)),
		glamour.WithPreservedNewLines(),
		glamour.WithChromaFormatter("terminal16m"),
	)
	if err != nil {
		panic(err)
	}
	rendered, err := renderer.Render(escapeMarkdownHTML(text))
	if err != nil {
		panic(err)
	}
	rendered = strings.Trim(rendered, "\n")
	if rendered == "" {
		return nil
	}
	var lines []string
	for _, line := range strings.Split(rendered, "\n") {
		wrapped := ansi.Hardwrap(trimMarkdownPadding(line), width-1, true)
		for line := range strings.SplitSeq(wrapped, "\n") {
			lines = append(lines, " "+line)
		}
	}
	return lines
}

func trimMarkdownPadding(line string) string {
	end := 0
	for i := 0; i < len(line); {
		if line[i] == 0x1b {
			i = ansiSequenceEnd(line, i)
			continue
		}
		r, size := utf8.DecodeRuneInString(line[i:])
		i += size
		if !unicode.IsSpace(r) {
			end = i
		}
	}
	if end == 0 {
		return ""
	}
	return line[:end] + "\x1b[0m\x1b]8;;\x1b\\"
}

func stringPointer(s string) *string { return &s }
func boolPointer(b bool) *bool       { return &b }

func escapeMarkdownHTML(source string) string {
	marked := make([]bool, len(source))
	mark := func(segments *text.Segments) {
		for i := range segments.Len() {
			segment := segments.At(i)
			for j := segment.Start; j < segment.Stop; j++ {
				marked[j] = true
			}
		}
	}
	document := goldmark.New(goldmark.WithExtensions(extension.GFM)).Parser().Parse(text.NewReader([]byte(source)))
	_ = ast.Walk(document, func(node ast.Node, entering bool) (ast.WalkStatus, error) {
		if !entering {
			return ast.WalkContinue, nil
		}
		switch node := node.(type) {
		case *ast.RawHTML:
			mark(node.Segments)
		case *ast.HTMLBlock:
			mark(node.Lines())
			if node.HasClosure() {
				for i := node.ClosureLine.Start; i < node.ClosureLine.Stop; i++ {
					marked[i] = true
				}
			}
		}
		return ast.WalkContinue, nil
	})

	var escaped strings.Builder
	for i := 0; i < len(source); i++ {
		if marked[i] && source[i] == '<' {
			escaped.WriteString("&lt;")
		} else if marked[i] && source[i] == '>' {
			escaped.WriteString("&gt;")
		} else {
			escaped.WriteByte(source[i])
		}
	}
	return escaped.String()
}

func renderedToolMessage(msg message, width int, now time.Time) string {
	bg := piToolBg
	inner := max(width-2, 1)
	lines := []string{piBoxLine("", width, piText, bg, false)}
	duration := toolDurationLabel(msg, now)
	state, stateColor := "", piGray
	if msg.toolState == "success" {
		state, stateColor = "✓ ", piGreen
	} else if msg.toolState == "error" {
		state, stateColor = "✕ ", piError
	}
	if msg.toolCommand != "" {
		command := sanitizeTerminalText(msg.toolCommand)
		var commandLines []string
		bold := true
		if msg.toolName == "repl" {
			header := " repl" + duration
			if state != "" {
				header = " " + ansiRGBStyle(stateColor, bg, false, false, state) + ansiRGBStyle(piGray, bg, false, false, "repl"+duration)
			}
			lines = append(lines, piBoxLine(header, width, piGray, bg, false))
			commandLines, bold = highlightedPythonLines(command, inner, bg), false
		} else {
			if duration != "" {
				lines = append(lines, piBoxLine(" "+strings.TrimSpace(duration), width, piGray, bg, false))
			}
			command = "$ " + command
			if msg.toolName == "web_search" {
				command = `web_search "` + sanitizeTerminalText(msg.toolCommand) + `"`
			}
			commandLines = wrapPlain(command, inner)
		}
		for _, line := range commandLines {
			lines = append(lines, piBoxLine(" "+line, width, piText, bg, bold))
		}
	} else if msg.toolName != "" {
		lines = append(lines, piBoxLine(" "+sanitizeTerminalText(msg.toolName)+duration, width, piGray, bg, false))
	}
	if msg.toolResult != "" {
		result := strings.TrimSuffix(sanitizeTerminalText(msg.toolResult), "\n")
		outputLines := wrapPlain(result, inner)
		lines = append(lines, piBoxLine("", width, piGray, bg, false))
		if len(outputLines) > toolPreviewLines {
			head, tail := toolPreviewLines/2, toolPreviewLines-toolPreviewLines/2
			for _, line := range outputLines[:head] {
				lines = append(lines, piBoxLine(" "+line, width, piGray, bg, false))
			}
			lines = append(lines, piBoxLine(fmt.Sprintf(" ⋯ %d lines omitted ⋯", len(outputLines)-toolPreviewLines), width, piDim, bg, false))
			outputLines = outputLines[len(outputLines)-tail:]
		}
		for _, line := range outputLines {
			lines = append(lines, piBoxLine(" "+line, width, piGray, bg, false))
		}
	}
	lines = append(lines, piBoxLine("", width, piText, bg, false))
	return strings.Join(lines, "\n")
}

func highlightedPythonLines(code string, width int, bg string) []string {
	lines := wrapPlain(code, width)
	for i, line := range lines {
		var highlighted strings.Builder
		_ = quick.Highlight(&highlighted, line, "python", "terminal16m", "github-dark")
		lines[i] = strings.ReplaceAll(highlighted.String(), "\x1b[0m", "\x1b[0m\x1b[48;2;"+bg+"m")
	}
	return lines
}

func piBoxLine(text string, width int, fg, bg string, bold bool) string {
	text += strings.Repeat(" ", max(width-lineWidth(text), 0))
	baseStyle := strings.TrimSuffix(ansiRGBStyle(fg, bg, bold, false, ""), "\x1b[0m")
	text = strings.ReplaceAll(text, "\x1b[0m", baseStyle)
	return ansiRGBStyle(fg, bg, bold, false, text)
}

func renderPendingLine(p pendingInput, width int) string {
	label := "Queued: "
	if p.kind == "steer" {
		label = "Steering: "
	}
	text := strings.ReplaceAll(p.text, "\n", " ")
	return ansi256FG(245, label+truncateCells(text, max(width-lineWidth(label), 0)))
}

func renderEditor(text string, cursor, width int) ([]string, int, int) {
	inner := max(width-4, 1)
	rs := []rune(text)
	cursor = clusterToRuneIndex(text, cursor)
	var rows []string
	var curRow, curCol int
	start := 0
	paragraphs := strings.Split(text, "\n")
	runeBase := 0
	for pi, p := range paragraphs {
		pr := []rune(p)
		if len(pr) == 0 {
			rows = append(rows, "")
			if cursor == runeBase {
				curRow = len(rows) - 1
				curCol = 0
			}
		} else {
			for len(pr) > 0 {
				n := fittingRunes(pr, inner)
				chunk := string(pr[:n])
				rowStart := runeBase + start
				rowEnd := rowStart + n
				if cursor >= rowStart && cursor <= rowEnd {
					curRow = len(rows)
					curCol = lineWidth(string(rs[rowStart:cursor]))
				}
				rows = append(rows, chunk)
				pr = pr[n:]
				start += n
			}
		}
		runeBase += len([]rune(p))
		start = 0
		if pi < len(paragraphs)-1 {
			runeBase++
		}
	}
	if len(rows) == 0 {
		rows = []string{""}
	}
	top := "╭" + strings.Repeat("─", max(width-2, 1)) + "╮"
	bottom := "╰" + strings.Repeat("─", max(width-2, 1)) + "╯"
	out := []string{ansi256FG(39, top)}
	for _, row := range rows {
		shown, color := row, 252
		if text == "" && row == "" {
			shown, color = truncateCells("Type a message…", inner), 242
		}
		out = append(out, ansi256FG(39, "│")+" "+ansi256FG(color, shown)+strings.Repeat(" ", max(inner-lineWidth(shown), 0))+" "+ansi256FG(39, "│"))
	}
	out = append(out, ansi256FG(39, bottom))
	return out, curRow + 1, curCol + 2
}

func wrapPlain(text string, width int) []string {
	width = max(width, 1)
	var out []string
	for _, p := range strings.Split(text, "\n") {
		if p == "" {
			out = append(out, "")
			continue
		}
		if tui.StringWidth(p) <= width {
			out = append(out, strings.TrimSpace(p))
			continue
		}
		r := []rune(p)
		for len(r) > 0 {
			n := fittingRunes(r, width)
			if n < len(r) {
				for i := n; i > 0; i-- {
					if unicode.IsSpace(r[i-1]) {
						n = i - 1
						break
					}
				}
				if n == 0 {
					n = fittingRunes(r, width)
				}
			}
			out = append(out, strings.TrimSpace(string(r[:n])))
			r = r[n:]
			for len(r) > 0 && unicode.IsSpace(r[0]) {
				r = r[1:]
			}
		}
	}
	return out
}
func fittingRunes(r []rune, width int) int {
	if len(r) == 0 {
		return 0
	}
	text, used, consumed := string(r), 0, 0
	for len(text) > 0 {
		_, w, size, runes := tui.NextClusterRunes(text)
		if size == 0 {
			break
		}
		if used+w > width {
			if consumed == 0 {
				return runes
			}
			return consumed
		}
		used += w
		consumed += runes
		text = text[size:]
	}
	return consumed
}

func clusterToRuneIndex(text string, clusterPos int) int {
	if clusterPos <= 0 {
		return 0
	}
	clusters, runes := 0, 0
	for len(text) > 0 && clusters < clusterPos {
		_, _, size, n := tui.NextClusterRunes(text)
		if size == 0 {
			break
		}
		text = text[size:]
		runes += n
		clusters++
	}
	return runes
}

func runeToClusterIndex(text string, runePos int) int {
	if runePos <= 0 {
		return 0
	}
	clusters, runes := 0, 0
	for len(text) > 0 && runes < runePos {
		_, _, size, n := tui.NextClusterRunes(text)
		if size == 0 {
			break
		}
		text = text[size:]
		runes += n
		clusters++
	}
	return clusters
}

func truncateCells(s string, width int) string {
	if width <= 0 {
		return ""
	}
	r := []rune(s)
	return string(r[:fittingRunes(r, width)])
}
func ansi256FG(c int, text string) string {
	return fmt.Sprintf("\x1b[0m\x1b[38;5;%dm%s\x1b[0m", c, text)
}
func ansiRGBStyle(fg, bg string, bold, italic bool, text string) string {
	var codes []string
	if bold {
		codes = append(codes, "1")
	}
	if italic {
		codes = append(codes, "3")
	}
	if fg != "" {
		codes = append(codes, "38;2;"+fg)
	}
	if bg != "" {
		codes = append(codes, "48;2;"+bg)
	}
	return "\x1b[0m\x1b[" + strings.Join(codes, ";") + "m" + text + "\x1b[0m"
}
func formatTokenCount(n int64) string {
	if n < 1000 {
		return fmt.Sprintf("%d", n)
	}
	return fmt.Sprintf("%dk", (n+500)/1000)
}
