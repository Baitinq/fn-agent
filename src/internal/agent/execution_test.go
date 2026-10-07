package agent

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func yieldingExecute(t *testing.T, repl *pythonREPL, code string) (int, string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	output, failed, err := repl.executeStreaming(ctx, code, func(string, bool) {})
	if err != nil || failed {
		t.Fatalf("yielding execute = %q, failed=%v, error=%v", output, failed, err)
	}
	var id int
	if _, err := fmt.Sscanf(output, "Execution %d is running.", &id); err != nil {
		t.Fatalf("execution did not yield: %q (%v)", output, err)
	}
	return id, output
}

func TestExecutionYieldsAndSharesNamespace(t *testing.T) {
	repl := newPythonREPL(nil)
	t.Cleanup(repl.close)
	id, output := yieldingExecute(t, repl, `print("started")
answer = await receive()
print(answer)
answer * 2`)
	if !strings.Contains(output, "started\n") {
		t.Fatalf("partial output missing: %q", output)
	}
	output = executeCompleteForTest(t, repl, fmt.Sprintf(`job = execution(%d)
print(job.status)
print(6 * 7)
job.send(21)
observation = await job.observe()
print(observation["output"], end="")
print(job.result())
print(answer)`, id))
	if output != "running\n42\n21\n4242\n21\n" {
		t.Fatalf("completed execution = %q", output)
	}
}

func TestExecutionShellYieldsAndAcceptsInput(t *testing.T) {
	repl := newPythonREPL(nil)
	t.Cleanup(repl.close)
	id, output := yieldingExecute(t, repl, `result = await shell("echo ready; read line; echo received:$line")
result.stdout`)
	if !strings.Contains(output, "ready\n") {
		t.Fatalf("missing partial shell output: %q", output)
	}
	output = executeCompleteForTest(t, repl, fmt.Sprintf(`job = execution(%d)
job.send("hello\n")
while job.status == "running":
    job.read()
    await job.observe()
print(job.result())
print(result.exit_code)`, id))
	if output != "ready\nreceived:hello\n\n0\n" {
		t.Fatalf("shell interaction = %q", output)
	}
}

func TestExecutionLLMYieldsAndOutlivesToolContext(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	repl := newPythonREPL(func(ctx context.Context, prompt string) (string, error) {
		if prompt == "slow" {
			close(started)
			select {
			case <-release:
			case <-ctx.Done():
				return "", ctx.Err()
			}
		}
		return "answer: " + prompt, nil
	})
	t.Cleanup(repl.close)
	id, _ := yieldingExecute(t, repl, `await llm("slow")`)
	// yieldingExecute cancelled its tool context; the execution must remain alive.
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("host call did not start")
	}
	output := executeCompleteForTest(t, repl, fmt.Sprintf(`job = execution(%d)
print(job.status)
print(await llm("fast"))`, id))
	if output != "running\nanswer: fast\n" {
		t.Fatalf("concurrent host call = %q", output)
	}
	close(release)
	output = executeCompleteForTest(t, repl, `await job.observe(); print(job.result())`)
	if output != "answer: slow\n" {
		t.Fatalf("background host result = %q", output)
	}
}

func TestExecutionFailureIsInspectable(t *testing.T) {
	repl := newPythonREPL(nil)
	t.Cleanup(repl.close)
	id, _ := yieldingExecute(t, repl, `await receive()
raise ValueError("late failure")`)
	output := executeCompleteForTest(t, repl, fmt.Sprintf(`job = execution(%d)
job.send(None)
observation = await job.observe()
print(job.status)
print(observation["output"])`, id))
	if !strings.HasPrefix(output, "failed\n") || !strings.Contains(output, "ValueError: late failure") {
		t.Fatalf("failure observation = %q", output)
	}
	output, failed, err := repl.execute(t.Context(), `job.result()`)
	if err != nil || !failed || !strings.Contains(output, "late failure") {
		t.Fatalf("failed result = %q, failed=%v, error=%v", output, failed, err)
	}
	if output := executeCompleteForTest(t, repl, `print(42)`); output != "42\n" {
		t.Fatalf("failure poisoned next execution: %q", output)
	}
}

func TestExecutionCancellationKillsShell(t *testing.T) {
	repl := newPythonREPL(nil)
	t.Cleanup(repl.close)
	id, output := yieldingExecute(t, repl, `await shell("echo $$; exec sleep 60")`)
	lines := strings.Split(strings.TrimSpace(output), "\n")
	pid, err := strconv.Atoi(lines[len(lines)-1])
	if err != nil {
		t.Fatalf("pid missing: %q", output)
	}
	output = executeCompleteForTest(t, repl, fmt.Sprintf(`job = execution(%d)
await job.cancel()
print(job.status)`, id))
	if output != "cancelled\n" {
		t.Fatalf("cancellation = %q", output)
	}
	if err := syscall.Kill(pid, 0); !errors.Is(err, syscall.ESRCH) {
		t.Fatalf("cancelled execution's shell is alive: %v", err)
	}
}

func TestExecutionCancellationDoesNotCancelOtherExecutions(t *testing.T) {
	started := make(chan struct{})
	repl := newPythonREPL(func(ctx context.Context, prompt string) (string, error) {
		close(started)
		<-ctx.Done()
		return "", ctx.Err()
	})
	t.Cleanup(repl.close)
	id, _ := yieldingExecute(t, repl, `value = await receive()`)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	result := make(chan error, 1)
	go func() {
		_, _, err := repl.execute(ctx, `await llm("foreground")`)
		result <- err
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("host call did not start")
	}
	cancel()
	if err := <-result; !errors.Is(err, context.Canceled) {
		t.Fatalf("foreground cancellation = %v", err)
	}
	output := executeCompleteForTest(t, repl, fmt.Sprintf(`job = execution(%d)
print(job.status)
job.send(42)
await job.observe()
print(value)`, id))
	if output != "running\n42\n" {
		t.Fatalf("unrelated execution was cancelled: %q", output)
	}
}

func TestExecutionFastCallsKeepNormalOutput(t *testing.T) {
	repl := newPythonREPL(nil)
	t.Cleanup(repl.close)
	var streams, progress strings.Builder
	output, failed, err := repl.executeStreaming(t.Context(), `print("hello"); result = await shell("printf command"); result.exit_code`, func(text string, isProgress bool) {
		if isProgress {
			progress.WriteString(text)
		} else {
			streams.WriteString(text)
		}
	})
	if err != nil || failed || output != "hello\n0" || streams.String() != "hello\n0" || progress.String() != "command" {
		t.Fatalf("fast call = %q, streams=%q, progress=%q, failed=%v, err=%v", output, streams.String(), progress.String(), failed, err)
	}
}

func TestObserveWaitsWithoutYieldingAnotherExecution(t *testing.T) {
	repl := newPythonREPL(nil)
	t.Cleanup(repl.close)
	id, _ := yieldingExecute(t, repl, `import asyncio
await asyncio.sleep(5)
42`)
	output, failed, err := repl.executeStreaming(t.Context(), fmt.Sprintf(`job = execution(%d); await job.observe()`, id), func(string, bool) {})
	if err != nil || failed || strings.Contains(output, "Execution ") || !strings.Contains(output, "'status': 'done'") {
		t.Fatalf("observe yielded or failed: %q failed=%v err=%v", output, failed, err)
	}
}

func executeCompleteForTest(t *testing.T, repl *pythonREPL, code string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	output, failed, err := repl.execute(ctx, code)
	if err != nil || failed {
		t.Fatalf("execution = %q, failed=%v, err=%v", output, failed, err)
	}
	return output
}
