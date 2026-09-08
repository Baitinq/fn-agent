package agent

import (
	"bufio"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"
)

//go:embed repl.py
var pythonREPLScript string

const (
	replInterruptGracePeriod = time.Second
	replCheckpointTimeout    = 5 * time.Second
)

var errREPLInterruptTimeout = errors.New("Python REPL did not respond to interrupt")

type pythonREPL struct {
	mu                sync.Mutex
	stdinMu           sync.Mutex
	llm               func(context.Context, string) (string, error)
	cmd               *exec.Cmd
	stdin             io.WriteCloser
	stdout            *bufio.Reader
	stderr            strings.Builder
	started           bool
	checkpoint        string
	checkpointObjects string
	recoveryReason    error
	notices           []string
}

type replResult struct {
	Output   string `json:"output"`
	Error    bool   `json:"error"`
	HostCall string `json:"host_call"`
	ID       int    `json:"id"`
	Prompt   string `json:"prompt"`
}

func newPythonREPL(llm func(context.Context, string) (string, error)) *pythonREPL {
	return &pythonREPL{llm: llm}
}

func (r *pythonREPL) startProcess() error {
	if r.cmd != nil {
		return nil
	}
	r.stderr.Reset()
	cmd := exec.Command("python3", "-u", "-c", pythonREPLScript)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	// Detached descendants may keep stderr open after the REPL exits.
	cmd.WaitDelay = replInterruptGracePeriod
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		_ = stdin.Close()
		return err
	}
	cmd.Stderr = &r.stderr
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start Python REPL: %w", err)
	}
	r.cmd = cmd
	r.stdin = stdin
	r.stdout = bufio.NewReader(stdout)
	r.started = true
	return nil
}

func (r *pythonREPL) stop() {
	if r.cmd == nil {
		return
	}
	r.recoveryReason = errors.New("Python REPL stopped")
	_ = r.stdin.Close()
	_ = syscall.Kill(-r.cmd.Process.Pid, syscall.SIGTERM)
	wait := make(chan struct{})
	go func(cmd *exec.Cmd) {
		_ = cmd.Wait()
		close(wait)
	}(r.cmd)
	select {
	case <-wait:
	case <-time.After(replInterruptGracePeriod):
		_ = syscall.Kill(-r.cmd.Process.Pid, syscall.SIGKILL)
		<-wait
	}
	_ = syscall.Kill(-r.cmd.Process.Pid, syscall.SIGKILL)
	r.cmd, r.stdin, r.stdout = nil, nil, nil
}

func (r *pythonREPL) close() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.stop()
}

func (r *pythonREPL) start() error {
	if r.cmd != nil {
		return nil
	}
	if err := r.startProcess(); err != nil {
		return err
	}
	if r.checkpoint != "" {
		ctx, cancel := context.WithTimeout(context.Background(), replCheckpointTimeout)
		defer cancel()
		if err := r.operationLocked(ctx, "restore", r.checkpoint, r.checkpointObjects); err != nil {
			r.stop()
			r.notices = append(r.notices, "REPL recovery failed; the last checkpoint was preserved: "+err.Error())
			return fmt.Errorf("restore Python REPL checkpoint: %w", err)
		}
	}
	if r.recoveryReason != nil {
		notice := "Python REPL restarted from last checkpoint; changes since that checkpoint were lost"
		if r.checkpoint == "" {
			notice = "Python REPL restarted with variables cleared (no checkpoint available)"
		}
		if errors.Is(r.recoveryReason, errREPLInterruptTimeout) {
			notice = "REPL unresponsive; restarted from last checkpoint. Changes from the interrupted call may be lost."
			if r.checkpoint == "" {
				notice = "REPL unresponsive; restarted with variables cleared (no checkpoint available)."
			}
		} else {
			notice += " (" + r.recoveryReason.Error() + ")"
		}
		r.notices = append(r.notices, notice)
		r.recoveryReason = nil
	}
	return nil
}

func (r *pythonREPL) recover(err error) error {
	if r.cmd != nil {
		return err
	}
	r.recoveryReason = err
	if restartErr := r.start(); restartErr != nil {
		return errors.Join(err, restartErr)
	}
	return fmt.Errorf("%s: %w", r.notices[len(r.notices)-1], err)
}

func (r *pythonREPL) takeNotices() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	notices := r.notices
	r.notices = nil
	return notices
}

func (r *pythonREPL) execute(ctx context.Context, code string) (string, bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return "", false, err
	}
	if err := r.start(); err != nil {
		return "", false, err
	}
	output, failed, err := r.requestLocked(ctx, map[string]string{"code": code})
	if err != nil {
		return output, failed, r.recover(err)
	}
	return output, failed, nil
}

func (r *pythonREPL) snapshot(path, objectsPath string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.start(); err != nil {
		return err
	}
	snapshot := func() error {
		ctx, cancel := context.WithTimeout(context.Background(), replCheckpointTimeout)
		defer cancel()
		return r.operationLocked(ctx, "snapshot", path+".tmp", objectsPath)
	}
	if err := snapshot(); err != nil {
		if r.cmd != nil {
			return err
		}
		err = r.recover(err)
		if r.cmd == nil {
			return err
		}
		if err := snapshot(); err != nil {
			return r.recover(err)
		}
	}
	if err := os.Rename(path+".tmp", path); err != nil {
		return err
	}
	r.checkpoint, r.checkpointObjects = path, objectsPath
	return nil
}

func (r *pythonREPL) restore(path, objectsPath string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.startProcess(); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), replCheckpointTimeout)
	defer cancel()
	if err := r.operationLocked(ctx, "restore", path, objectsPath); err != nil {
		r.stop()
		return err
	}
	r.checkpoint, r.checkpointObjects = path, objectsPath
	return nil
}

func (r *pythonREPL) operationLocked(ctx context.Context, op, path, objectsPath string) error {
	output, failed, err := r.requestLocked(ctx, map[string]string{"op": op, "path": path, "objects_path": objectsPath})
	if err != nil {
		return err
	}
	if failed {
		return errors.New(output)
	}
	return nil
}

func (r *pythonREPL) requestLocked(ctx context.Context, request map[string]string) (string, bool, error) {
	payload, err := json.Marshal(request)
	if err != nil {
		return "", false, err
	}
	if _, err := r.writeTo(r.stdin, append(payload, '\n')); err != nil {
		r.stop()
		return "", false, err
	}
	return r.readResult(ctx)
}

func (r *pythonREPL) writeTo(stdin io.Writer, data []byte) (int, error) {
	r.stdinMu.Lock()
	defer r.stdinMu.Unlock()
	return stdin.Write(data)
}

func (r *pythonREPL) readResult(ctx context.Context) (string, bool, error) {
	type readResult struct {
		line []byte
		err  error
	}
	hostCallError := make(chan error, 1)
	cancel := ctx.Done()
	var canceled error
	var interruptDeadline <-chan time.Time
	for {
		read := make(chan readResult, 1)
		go func(stdout *bufio.Reader) {
			line, err := stdout.ReadBytes('\n')
			read <- readResult{line: line, err: err}
		}(r.stdout)

		var result readResult
		for {
			select {
			case <-cancel:
				canceled = ctx.Err()
				cancel = nil
				_ = r.cmd.Process.Signal(syscall.SIGINT)
				interruptDeadline = time.After(replInterruptGracePeriod)
			case <-interruptDeadline:
				r.stop()
				<-read
				return "", true, errors.Join(errREPLInterruptTimeout, canceled)
			case err := <-hostCallError:
				r.stop()
				return "", true, err
			case result = <-read:
				goto received
			}
		}

	received:
		if result.err != nil {
			r.stop()
			if stderr := strings.TrimSpace(r.stderr.String()); stderr != "" {
				return "", true, fmt.Errorf("Python REPL stopped: %s", stderr)
			}
			return "", true, fmt.Errorf("Python REPL stopped: %w", result.err)
		}
		var response replResult
		if err := json.Unmarshal(result.line, &response); err != nil {
			r.stop()
			return "", true, fmt.Errorf("invalid Python REPL response: %w", err)
		}
		if response.HostCall == "llm" {
			go r.runLLMHostCall(ctx, r.stdin, response.ID, response.Prompt, hostCallError)
			continue
		}
		if canceled != nil {
			return "", true, canceled
		}
		return response.Output, response.Error, nil
	}
}

func (r *pythonREPL) runLLMHostCall(ctx context.Context, stdin io.Writer, id int, prompt string, hostCallError chan<- error) {
	text, callErr := r.llm(ctx, prompt)
	hostResponse := map[string]any{"id": id, "result": text}
	if callErr != nil {
		hostResponse = map[string]any{"id": id, "error": callErr.Error()}
	}
	data, _ := json.Marshal(hostResponse)
	if _, err := r.writeTo(stdin, append(data, '\n')); err != nil {
		select {
		case hostCallError <- err:
		default:
		}
	}
}

func formatREPLError(err error) string {
	if errors.Is(err, context.Canceled) {
		return ""
	}
	return "REPL error: " + err.Error()
}
