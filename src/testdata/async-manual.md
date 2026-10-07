# Manual scheduling test with a real model

Run the current checkout from `src`:

```sh
go run ./cmd/fn
```

Use a fresh session and paste this task (use a different log name for each run):

> Investigate the cancellation and cleanup behavior of this repo's Python REPL.
> Run `python3 testdata/slow-tests.py --log /tmp/fn-scheduling-1.jsonl` as your test run.
> While verification is underway, inspect the implementation and relevant tests.
> Identify one concrete risk or missing test, or explain why you found none.
> Use actual test results in your assessment. Stop the runner once it is only
> doing unnecessary work. Do not modify files or start duplicate test runs.
> Report the runner PID and the order in which you investigated, observed
> results, and stopped the runner.

The runner executes the real agent, UI, and CLI tests, separated by 10-second
cooldowns. After tests pass it emits clearly labelled, unnecessary heartbeats
for up to two minutes. It does not modify source files. It appends timestamped
milestones to the supplied log and prints the same milestones to stdout.

## What to look for

- The first command yields a running execution instead of occupying the agent
  until completion.
- Source inspection happens before the runner reports `tests_complete`.
- The agent retrieves later output and checks all three test results.
- It stops the runner during the heartbeat phase, rather than waiting for
  `finished`, repeatedly polling, or forgetting the command.
- Its final assessment is supported by code and observed results.

This deliberately leaves scheduling to the model: the task does not prescribe
execution handles or specific tool calls.

## Check what actually happened

In another terminal:

```sh
cat /tmp/fn-scheduling-1.jsonl
ps -p <reported-pid> -o pid,etime,command
```

Compare the milestones with the tool order in the fn transcript. The log alone
cannot prove that inspection overlapped with the runner. A successful run has
three successful `test_finished` entries, `tests_complete`, and no natural
`finished` event; `ps` should show that the runner is gone. Cancellation may use
SIGKILL, so a `stopped` event is not guaranteed.

For an intervention test, repeat with a new log name. During a cooldown tell fn:

> Stop that runner now. Do not restart it. Focus on reviewing the host-call
> response draining tests and summarize what they guarantee.

Check that the PID disappears promptly and the agent continues the new task.
That run is intentionally not expected to complete all tests.

## Fixture smoke test (no model required)

```sh
log=$(mktemp)
python3 testdata/slow-tests.py --log "$log" --delay 0 --linger 0
cat "$log"
rm "$log"
```

Passing this smoke test checks the fixture, not model scheduling behavior.
