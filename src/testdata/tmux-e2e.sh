#!/usr/bin/env bash
set -euo pipefail

root=$(cd "$(dirname "$0")/.." && pwd)
socket="fn-e2e-$$"
session=fn-e2e
binary=$(mktemp "${TMPDIR:-/tmp}/fn-e2e.XXXXXX")
render_log=$(mktemp "${TMPDIR:-/tmp}/fn-render-log.XXXXXX")
tmux=(tmux -L "$socket")
cleanup() { "${tmux[@]}" kill-server 2>/dev/null || true; rm -f "$binary" "$render_log"; }
trap cleanup EXIT

cd "$root"
go test -c -o "$binary" ./internal/ui
"${tmux[@]}" new-session -d -x 60 -y 18 -s "$session" /bin/sh
"${tmux[@]}" set-option -t "$session" remain-on-exit on
"${tmux[@]}" send-keys -t "$session" "FN_TMUX_HARNESS=1 '$binary' -test.run '^TestTmuxHarness$' -test.count=1" Enter
# Type before initialization finishes. Startup input must survive terminal-mode
# setup and appear in the editor rather than as stray text above the UI.
"${tmux[@]}" send-keys -t "$session" -l STARTUP-DRAFT

capture() { "${tmux[@]}" capture-pane -p -t "$session" -S -; }
capture_visible() { "${tmux[@]}" capture-pane -p -t "$session"; }
capture_history() {
  local size
  size=$("${tmux[@]}" display-message -p -t "$session" '#{history_size}')
  ((size > 0)) && "${tmux[@]}" capture-pane -p -t "$session" -S "-$size" -E -1
}
wait_until_idle() {
  for _ in $(seq 1 200); do capture_visible | grep -q 'Type a message' && return 0; sleep .05; done
  echo 'timed out waiting for idle editor' >&2; capture_visible >&2; return 1
}
wait_for() {
  local pattern=$1
  for _ in $(seq 1 200); do capture | grep -q "$pattern" && return 0; sleep .025; done
  echo "timed out waiting for $pattern" >&2; capture >&2; return 1
}
wait_for '│ STARTUP-DRAFT'
"${tmux[@]}" send-keys -t "$session" Escape

# Stream in a short pane so transcript growth reaches native tmux history.
"${tmux[@]}" resize-pane -t "$session" -y 12
"${tmux[@]}" send-keys -t "$session" -l stream
"${tmux[@]}" send-keys -t "$session" Enter
wait_for '321 context'
wait_until_idle
all=$(capture)
grep -q 'STREAM-LINE-01' <<<"$all"
grep -q 'STREAM-LINE-32' <<<"$all"

# No frame of the live status/editor/footer may be preserved in scrollback.
if rg -q '^[⠋⠙⠹⠸⠼⠴⠦⠧⠇⠏] Working…' <<<"$all" ||
  [[ $(rg -c '^╭─+╮$' <<<"$all") -ne 1 ]] ||
  [[ $(rg -c '^╰─+╯$' <<<"$all") -ne 1 ]] ||
  [[ $(rg -c '^│ Type a message…' <<<"$all") -ne 1 ]] ||
  [[ $(rg -c '^tmux-e2e ' <<<"$all") -ne 1 ]]; then
  echo 'live UI leaked into tmux scrollback' >&2
  printf '%s\n' "$all" >&2
  exit 1
fi
"${tmux[@]}" resize-pane -t "$session" -y 40

# Recall wrapped entries, including one taller than the terminal, without
# replaying the transcript or clearing native scrollback.
wrapped="HISTORY-WRAPPED-$(printf 'word %.0s' $(seq 1 35))WRAPPED-END"
oversized="HISTORY-HUGE-$(printf 'word %.0s' $(seq 1 300))HUGE-END"
for entry in HISTORY-SHORT "$wrapped" "$oversized"; do
  "${tmux[@]}" send-keys -t "$session" -l "$entry"
  "${tmux[@]}" send-keys -t "$session" Enter
  wait_for "${entry##* }[>]"
done
sleep .2
history_before=$(capture | rg -c 'STREAM-LINE-01')
"${tmux[@]}" pipe-pane -t "$session" "cat > '$render_log'"
for _ in 1 2 3; do
  "${tmux[@]}" send-keys -t "$session" Up
  sleep .15
  "${tmux[@]}" send-keys -t "$session" Up
  sleep .15
  "${tmux[@]}" send-keys -t "$session" Up
  sleep .15
  visible=$("${tmux[@]}" capture-pane -p -t "$session")
  rg -q '^│ HISTORY-SHORT' <<<"$visible"
  if rg -q '^│ .*word|^│ .*HUGE-END|^│ .*WRAPPED-END' <<<"$visible"; then
    echo 'input history left stale wrapped rows' >&2; printf '%s\n' "$visible" >&2; exit 1
  fi
  for _ in 1 2 3; do
    "${tmux[@]}" send-keys -t "$session" Down
    sleep .15
  done
done
"${tmux[@]}" send-keys -t "$session" -l CURSOR-AFTER-HISTORY
sleep .15
"${tmux[@]}" capture-pane -p -t "$session" | rg -q '^│ CURSOR-AFTER-HISTORY'
read -r cursor_x cursor_y <<<"$("${tmux[@]}" display-message -p -t "$session" '#{cursor_x} #{cursor_y}')"
test "$cursor_x" = 22
"${tmux[@]}" capture-pane -p -t "$session" | sed -n "$((cursor_y + 1))p" | rg -q '^│ CURSOR-AFTER-HISTORY'
"${tmux[@]}" send-keys -t "$session" Escape
sleep .15
"${tmux[@]}" pipe-pane -t "$session"
test "$(capture | rg -c 'STREAM-LINE-01')" = "$history_before"
python3 - "$render_log" <<'PYCHECK'
import pathlib, sys
output = pathlib.Path(sys.argv[1]).read_bytes()
assert b'\x1b[3J' not in output, 'input history cleared scrollback'
assert b'STREAM-LINE-01' not in output, 'input history replayed the conversation'
PYCHECK

# Shift+Enter queues while plain Enter steers; the steer must run first.
"${tmux[@]}" send-keys -t "$session" -l stream
"${tmux[@]}" send-keys -t "$session" Enter
wait_for 'STREAM-LINE-03'
"${tmux[@]}" send-keys -t "$session" -l queued-order
"${tmux[@]}" send-keys -t "$session" S-Enter
"${tmux[@]}" send-keys -t "$session" -l steer-order
"${tmux[@]}" send-keys -t "$session" Enter
wait_for 'ECHO<queued-order>'
all=$(capture)
steer_line=$(grep -n 'ECHO<steer-order>' <<<"$all" | head -1 | cut -d: -f1)
queue_line=$(grep -n 'ECHO<queued-order>' <<<"$all" | head -1 | cut -d: -f1)
[[ -n "$steer_line" && -n "$queue_line" && "$steer_line" -lt "$queue_line" ]]

# tmux's native search and selection can reach finalized transcript output.
"${tmux[@]}" copy-mode -t "$session"
"${tmux[@]}" send-keys -t "$session" -X history-top
"${tmux[@]}" send-keys -t "$session" -X search-forward 'STREAM-LINE-01'
"${tmux[@]}" send-keys -t "$session" -X select-line
"${tmux[@]}" send-keys -t "$session" -X copy-selection-and-cancel
"${tmux[@]}" show-buffer | grep -q 'STREAM-LINE-01'

# Stay scrolled up while another response continues rendering.
"${tmux[@]}" send-keys -t "$session" -l stream
"${tmux[@]}" send-keys -t "$session" Enter
sleep .15
"${tmux[@]}" copy-mode -u -t "$session"
"${tmux[@]}" send-keys -t "$session" -X page-up
scroll_before=$("${tmux[@]}" display-message -p -t "$session" '#{scroll_position}')
sleep .8
scroll_after=$("${tmux[@]}" display-message -p -t "$session" '#{scroll_position}')
[[ "$scroll_before" -gt 0 && "$scroll_after" == "$scroll_before" ]]
"${tmux[@]}" send-keys -t "$session" -X cancel

# Multiline input and hardware cursor positioning.
"${tmux[@]}" send-keys -t "$session" -l alpha
"${tmux[@]}" send-keys -t "$session" C-j
"${tmux[@]}" send-keys -t "$session" -l beta
sleep .05
visible=$("${tmux[@]}" capture-pane -p -t "$session")
grep -q '│ alpha' <<<"$visible"
grep -q '│ beta' <<<"$visible"
"${tmux[@]}" send-keys -t "$session" Enter
wait_for 'ECHO<alpha'

# Tool call, result, and streamed final response share the logical transcript.
"${tmux[@]}" send-keys -t "$session" -l tools
"${tmux[@]}" send-keys -t "$session" Enter
wait_for 'LIVE-PARTIAL'
wait_for '654 context'
wait_until_idle
all=$(capture)
grep -q '\$ printf tool-output' <<<"$all"
grep -q '^ tool-output' <<<"$all"
grep -q 'tool turn complete' <<<"$all"

# Enter while active steers the same response instead of starting a follow-up.
"${tmux[@]}" send-keys -t "$session" -l steertest
"${tmux[@]}" send-keys -t "$session" Enter
wait_for 'STEER-WAIT'
"${tmux[@]}" send-keys -t "$session" -l change-direction
"${tmux[@]}" send-keys -t "$session" Enter
wait_for 'STEERED<change-direction>'
wait_for '901 context'
wait_until_idle

# A sustained tool stream remains live without rendering every individual
# chunk. Capture raw pane output and count synchronized renderer frames.
"${tmux[@]}" send-keys -t "$session" -l toolburst
wait_for '│ toolburst'
printf -v pipe_command 'cat > %q' "$render_log"
"${tmux[@]}" pipe-pane -t "$session" "$pipe_command"
sleep .05
"${tmux[@]}" send-keys -t "$session" Enter
wait_for 'BURST-60'
! capture | grep -q '876 context'
wait_for '876 context'
wait_until_idle
"${tmux[@]}" pipe-pane -t "$session"
sleep .05
read -r render_frames render_replays render_bytes < <(python3 - "$render_log" <<'PY'
import pathlib
import sys
data = pathlib.Path(sys.argv[1]).read_bytes()
print(data.count(b"\x1b[?2026h"), data.count(b"\x1b[3J"), len(data))
PY
)
if [[ "$render_frames" -lt 25 || "$render_frames" -gt 60 ]]; then
  echo "tool burst caused $render_frames renderer frames, want 25-60" >&2
  exit 1
fi
if [[ "$render_replays" -ne 0 ]]; then
  echo "tool burst caused $render_replays full transcript replays, want 0" >&2
  exit 1
fi
if [[ "$render_bytes" -gt 60000 ]]; then
  echo "tool burst wrote $render_bytes terminal bytes, want at most 60000" >&2
  exit 1
fi
all=$(capture)
grep -q '50 earlier lines hidden' <<<"$all"
! grep -q 'BURST-01' <<<"$all"
grep -q 'BURST-60' <<<"$all"
grep -q 'burst complete' <<<"$all"
if rg -q '^[⠋⠙⠹⠸⠼⠴⠦⠧⠇⠏] Working…' <<<"$all" ||
  [[ $(rg -c '^│ Type a message…' <<<"$all") -ne 1 ]]; then
  echo 'live UI leaked after tool burst' >&2
  printf '%s\n' "$all" >&2
  exit 1
fi

# Ctrl+C cancels the active task and returns pending input to the editor.
"${tmux[@]}" send-keys -t "$session" -l cancel
"${tmux[@]}" send-keys -t "$session" Enter
wait_for 'WAITING-FOR-CANCEL'
"${tmux[@]}" send-keys -t "$session" -l queued-after-cancel
"${tmux[@]}" send-keys -t "$session" S-Enter
"${tmux[@]}" send-keys -t "$session" -l steer-after-cancel
"${tmux[@]}" send-keys -t "$session" Enter
"${tmux[@]}" send-keys -t "$session" -l draft-after-cancel
"${tmux[@]}" send-keys -t "$session" C-c
wait_for 'Canceled'
wait_for 'REPL unresponsive; restarted from last checkpoint'
visible=$("${tmux[@]}" capture-pane -p -t "$session")
grep -q '│ queued-after-cancel' <<<"$visible"
grep -q '│ steer-after-cancel' <<<"$visible"
grep -q '│ draft-after-cancel' <<<"$visible"

# Escape clears the editor without cancelling or exiting.
"${tmux[@]}" send-keys -t "$session" Escape
"${tmux[@]}" send-keys -t "$session" -l discard-me
"${tmux[@]}" send-keys -t "$session" Escape
sleep .05
visible=$("${tmux[@]}" capture-pane -p -t "$session")
! grep -q 'discard-me' <<<"$visible"

# Pi-style resize replay must retain the semantic transcript at both widths.
"${tmux[@]}" resize-window -t "$session" -x 36 -y 12
sleep .2
all=$(capture)
[[ $(grep -c 'STREAM-LINE-01' <<<"$all") -eq 3 ]]
[[ $(grep -c 'STREAM-LINE-32' <<<"$all") -eq 3 ]]
grep -q 'Canceled' <<<"$all"
if capture_history | grep -q 'Type a message'; then
  echo 'resize preserved the input bar in tmux scrollback' >&2
  capture_history >&2
  exit 1
fi
if capture_history | rg -q '^[⠋⠙⠹⠸⠼⠴⠦⠧⠇⠏] Working…|^╭─+╮$|^╰─+╯$|^tmux-e2e '; then
  echo 'resize preserved live UI in tmux scrollback' >&2
  capture_history >&2
  exit 1
fi
"${tmux[@]}" resize-window -t "$session" -x 72 -y 24
sleep .2

# Let the mutable editor become taller than the original live region.
for i in $(seq -w 1 14); do
  "${tmux[@]}" send-keys -t "$session" -l "EDIT-$i"
  [[ "$i" == 14 ]] || "${tmux[@]}" send-keys -t "$session" C-j
done
wait_for '│ EDIT-14'
visible=$("${tmux[@]}" capture-pane -p -t "$session")
grep -q '│ EDIT-14' <<<"$visible"
"${tmux[@]}" send-keys -t "$session" Enter
wait_for '777 context'
wait_until_idle
all=$(capture)
grep -q 'EDIT-01' <<<"$all"
grep -q 'EDIT-14' <<<"$all"

# A second consecutive Ctrl+C exits and leaves the terminal usable.
"${tmux[@]}" send-keys -t "$session" C-c
"${tmux[@]}" send-keys -t "$session" C-c
wait_for '^PASS$'
"${tmux[@]}" send-keys -t "$session" "printf 'TERMINAL-CLEAN\\n'" Enter
wait_for 'TERMINAL-CLEAN'
if capture | grep -q '│  ype a message'; then
  echo 'exit overwrote the input cursor cell' >&2
  exit 1
fi

printf 'tmux e2e passed: streaming, active-loop steering, throttled tool bursts (%s frames, %s bytes, no full replays), tools, history, scroll anchoring, dynamic multiline input, cancellation, resize, and cleanup\n' "$render_frames" "$render_bytes"
