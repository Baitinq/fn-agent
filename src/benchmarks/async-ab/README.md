# Blocking vs yielding REPL: real-model A/B test

## Bottom line

80 valid trials do **not establish an end-to-end improvement** from the current yielding implementation. It exposes real capabilities, but this model used more orchestration, and the two-command case regressed sharply. Keep this experimental; do not interpret execution-unit-test success as evidence of agent effectiveness.

## Setup

- Baseline: committed `6abb661` (blocking REPL). Treatment: the uncommitted yielding implementation frozen before these runs. Source hashes are in `summary.json`.
- Model: `openai/gpt-6.1-sol`, reasoning `medium`. Same provider configuration and model for both arms. No second model was evaluated.
- Five cases × eight repetitions × two arms = **80 valid runs**, fresh sessions. Ten preliminary runs are excluded: their fixture had an argument-name collision, and two Go test names were stale. Corrected fixture and exact test names were validated before scored runs.
- Same frozen reviewed source and fixture for both arms. Source was deliberately held constant: evaluating different source would confound answer quality with harness behavior.
- Paired by case/repetition; randomized arm/case order (seed 42); four concurrent trials; 150-second deadline per trial. Not simultaneous one-to-one pairs. Shared provider/cache/CPU conditions remain a limitation.
- The suite case ran actual named Go tests with deliberate slow cooldowns and useless heartbeats. Other long-work fixtures were controlled delays around real source checks, stdin interaction, or an actual invalid Go test-regex error. These are controlled scheduling tasks, not a large production-repository benchmark.
- Prompts did not prescribe `spawn`, `Popen`, or execution-handle syntax. The model could use its existing Python abilities in either arm.

## Overall results

| Measure | Blocking | Yielding |
|---|---:|---:|
| Strict successful tasks | 16/40 | 15/40 |
| Successful tasks, allowing incidental inaccuracies | 17/40 | 15/40 |
| Normal final answer before deadline | 31/40 | 27/40 |
| Mechanical task requirements satisfied | 36/40 | 38/40 |
| Mean elapsed time, including timeout runs | 72.6s | 85.3s |
| Median elapsed time | 50.2s | 71.8s |
| REPL tool calls | 583 | 934 |
| Model requests | 614 | 961 |
| Input tokens, including cached | 3,803,452 | 6,066,458 |
| Cached input tokens | 1,199,798 | 2,226,355 |
| Output tokens | 71,721 | 79,798 |
| Tool error events | 5 | 9 |
| Fixture still alive when model finished | 0 | 0 |
| Fixture still alive after harness close | 0 | 0 |

Token counts are reported usage, not dollar costs. Input includes cache hits; no pricing assumptions were made. Time includes unsuccessful runs bounded at 150 seconds; it is not time-to-correct-answer. Returning an incorrect answer quickly is not a win.

## Per case

| Case | Strict success: old → new | Median seconds: old → new | Normal finals: old → new | Mechanics: old → new |
|---|---:|---:|---:|---:|
| Slow tests + cancellation review | 1/8 → 2/8 | 133.3 → 97.8 | 5/8 → 7/8 | 4/8 → 6/8 |
| Early test failure + stop | 0/8 → 2/8 | 80.7 → 133.2 | 5/8 → 4/8 | 8/8 → 8/8 |
| Interactive stdin + stop | 3/8 → 2/8 | 71.0 → 96.6 | 5/8 → 4/8 | 8/8 → 8/8 |
| Two independent delayed checks | 4/8 → 1/8 | 40.3 → 147.8 | 8/8 → 4/8 | 8/8 → 8/8 |
| Short source-inspection control | 8/8 → 8/8 | 12.7 → 11.6 | 8/8 → 8/8 | 8/8 → 8/8 |

## Uncertainty

- Strict success difference (new − old): -2.5 percentage points. Paired, within-case bootstrap 95% interval: [-17.5, 15.0] pp. Too wide to establish superiority or equivalence.
- Mean elapsed-time difference: +12.7s; bootstrap interval [-6.0, 31.4]s. No clear overall latency benefit.
- Mean tool-call difference: +8.78 calls/run; interval [2.02, 15.40].
- Mean input-token difference: +56,575/run; interval [13,696, 98,188]. The orchestration/input overhead is clearer than any quality benefit.
- 10,000 bootstrap resamples (seed 314159), preserving equal case weighting. Intervals are exploratory: only eight repetitions per case, no preregistered hypothesis, and case selection was purposive. They do not account for judge mistakes or generalize to all models/tasks.

## What the traces explain

1. **The baseline was not helpless.** It used raw process APIs (`Popen`/`create_subprocess_*`) in 24/40 runs versus 1/40 in the treatment. A persistent Python namespace already lets a capable model retain subprocess handles and continue working. Therefore the comparison is not "concurrency versus no concurrency"; it is implicit Python-managed concurrency versus harness-managed yielding.
2. **The new affordance works, but availability is not good scheduling.** Interactive input and stopping running work were mechanically achievable. However, models still sometimes failed to deliver an accurate final explanation or retrieve correct final values.
3. **The two-check case is the clearest regression.** Both checks usually completed, but the treatment repeatedly inspected statuses/handles and frequently ran to the deadline instead of producing a verified final answer. Waiting/observation calls can themselves become yielded executions. More available control points made bookkeeping larger, not necessarily decision-making better.
4. **New API mistakes are real overhead.** Traces contain incomplete `.result()` reads, wrong handle IDs/names, and confusion between synchronous handle methods and awaitable methods. Background assignments may not exist yet; the model must coordinate that state. These are separate from intentional fixture failures.
5. **Answer fidelity was weak in both arms on source-heavy tasks.** Blinded grading found invented byte counts/results and inaccurate descriptions of cancellation or response ownership. This limits what completion time alone tells us. It is not established that yielding caused these hallucinations.
6. **The short control did not show a meaningful capability benefit.** It was solved by both arms. Long-work gains should not be assumed from the presence of an execution ID.

## Scoring and audit

- Mechanical checks come from fixture events, exit results, and whether fixture PIDs were alive before/after harness shutdown. Model claims of success alone do not count. Stopping a fixture via SIGKILL may not write a stopped event, so PID liveness and absence of natural completion were used.
- Strict success requires: normal final answer, mechanical requirements, a complete answer, and no material wrong result/source claims. A correct intermediate observation without a final answer is not a completed task.
- A fresh tool-free model graded final answers using the common reviewed-source ground truth and actual fixture logs; arm labels were withheld. Source-accuracy failures were audited in separate calls against the full actual Python/Go source. `grades.json` retains those judgements. This is an LLM judge, not independent human ground truth.
- A sensitivity score tolerates incidental wording/duration errors but not wrong results or invented core mechanisms. Its conclusion is likewise not a treatment win.
- The observational-call classifier is a rough code-text heuristic, stored in metrics but not used to declare success. It is not a reliable measure of productive attention. Duplicate starts and raw process-API usage are also trace heuristics.
- Harness shutdown cleans up remaining commands, but that does not retroactively give the model credit for deliberate cleanup. Benchmark runner explicitly cleans up any escaped fixture processes after recording results.

## Recommendation

Keep generic yielding experimental rather than treating this POC as an established improvement. Preserve the ability to observe and intervene, but simplify ownership/observation before expanding features:
- Make choosing to wait for the next event easy and unambiguous; avoid accumulating waiter executions or repeatedly requesting status.
- Reduce the competing concepts exposed to the model (explicit spawned activities plus automatically yielded executions plus raw subprocess handles).
- Evaluate a less aggressive observation budget, e.g. 1–2 seconds, against 250ms. This is a hypothesis, not a measured fix.
- Re-run the problematic two-check case first, then this full suite and at least one other model. Do not add a second reasoning loop or a larger job scheduler in response to these results.

## Artifacts and reproduction

`metrics.json` / `metrics.csv`: all 80 scored outcomes. `summary.json`: source hashes, settings, aggregate statistics and intervals. `prompts.json`, `fixture.py`, and `driver.go.txt`: exact benchmark inputs/driver. `grades.json`: blinded judgements and audits.
Raw sessions, timestamped tool events, fixture logs, isolated workspaces and compiled old/new binaries remain locally at `src/benchmarks/runs/async-ab-20260930-150114`. The runs directory is git-ignored; the compact report artifacts are not.
For reproduction, compile the driver as `cmd/ab/main.go` inside isolated copies of each source snapshot. For every fresh sandbox, symlink `repo` to the same reviewed source snapshot, copy the fixture, write `spec.txt` with `CONFIRM-42`, and supply the prompt. Run the driver with `--dir`, `--prompt`, `--output`, and `--timeout 150s`. The old/new driver is identical. Do not run fixtures against different reviewed source versions.

No implementation changes, commits, pushes, or reinstalls were made as part of this evaluation.
