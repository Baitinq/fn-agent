# Simplified yielding REPL: three-arm repeat

## Verdict

**This simplification regressed. Do not install it as an improvement.** The experiment suggests that removing discovery/explicit waiting while retaining live shared-state executions makes the interface harder for this model, not easier. Fewer named primitives did not mean less cognitive complexity.

## Changes tested

- Observation budget increased from 250ms to 2 seconds.
- One model-facing execution handle: `execution(id)`, `observe()`, `result()`, `send()`, `cancel()`.
- `observe()` deliberately waits for output or completion and consumes output in the returned dictionary. Observation waits do not produce another yielded execution.
- Removed the tool-level `wait` flag and public `spawn`, `activities`, and global `wait` APIs. Underlying owner/task cleanup was retained.
- Private obsolete helpers were removed and the owner class renamed after the experiment. These edits were unit-tested but not model-benchmarked; frozen evaluated source is retained in raw artifacts.

## Experiment

- **120 new runs**, five cases × eight repetitions × three arms: original blocking baseline (`6abb661`), previous 250ms yielding implementation, simplified 2s implementation.
- Same `openai/gpt-6.1-sol` model, `medium` reasoning, fresh sessions, same prompts, same fixture, same frozen reviewed source, 150-second deadline. Four concurrent trials; randomized order, seed 43.
- Both comparators were rerun contemporaneously; previous-run results were not substituted. Baseline and previous binaries were preserved from the first experiment.
- The frozen reviewed source is the *previous* yielding implementation for every arm. That avoids different code-review targets, but creates a significant source/tool mismatch for the simplified arm: the model can read examples of `activities()` that are unavailable in its current runtime. This is a real failure mode for source-inspecting agents, but it prevents isolating the intrinsic merits of the new API.
- Cases are controlled tasks with real source checks/Go tests and synthetic delays, not representative production workloads. Eight repetitions per case is modest. One model only.

## Results

| Measure | Blocking | Previous yielding | Simplified |
|---|---:|---:|---:|
| Correct, complete, mechanically verified | 17/40 | 21/40 | 14/40 |
| Final answer before deadline | 28/40 | 33/40 | 22/40 |
| Mechanical requirements satisfied | 36/40 | 40/40 | 33/40 |
| Median elapsed time | 82.7s | 83.9s | 126.3s |
| Mean elapsed time | 84.2s | 84.5s | 102.9s |
| REPL calls | 664 | 788 | 985 |
| Model requests | 692 | 821 | 1,007 |
| Input tokens, including cached | 4,211,287 | 5,149,376 | 6,200,365 |
| Cached input tokens | 1,362,371 | 1,900,343 | 2,266,199 |
| Output tokens | 73,626 | 73,744 | 73,972 |
| Tool errors | 7 | 8 | 191 |
| Live fixture at final answer/deadline | 0 | 0 | 5 |
| Live fixture after harness close | 0 | 0 | 0 |

Elapsed time includes deadline runs; it is not time-to-correct-answer. Tokens are reported usage, not estimated dollars. Input includes cache hits. Final correctness was judged from final answers against reviewed source and actual fixture logs; intermediate success without a final answer is incomplete.

## Per-case verified success

| Case | Blocking | Previous | Simplified |
|---|---:|---:|---:|
| suite | 0/8 | 3/8 | 1/8 |
| failure | 2/8 | 2/8 | 1/8 |
| interactive | 3/8 | 4/8 | 1/8 |
| dual | 4/8 | 4/8 | 3/8 |
| short | 8/8 | 8/8 | 8/8 |

## Failure analysis

- **Interactive execution is the sharpest regression:** only two of eight simplified runs satisfied mechanics, versus eight of eight in both comparators. Traces show looping on unavailable discovery APIs rather than reliably retrieving the execution ID already returned by the tool.
- **Repeated unavailable-API calls dominate tool errors.** `activities()` NameErrors recur extensively. Removing discovery without a replacement made recovery from forgotten IDs difficult, and source inspection reinforced obsolete names. A hard failure is repeatedly retried instead of prompting recovery.
- **Waiting remains a behavioral issue.** The simplified model used `observe()` in some runs, but often kept checking state/reading source. The two-check case still completed mechanically yet frequently lacked a timely final answer.
- **A combined observe operation adds a new semantic obligation:** it consumes output. Calling `read()` immediately afterward produces empty output; unit tests were updated to use the returned observation. Models must learn this as well as ownership of async assignments.
- **2 seconds did not eliminate coordination overhead.** Compared with the contemporaneous baseline, the simplified arm was slower, made more calls and used more input tokens. This experiment changes interface and interval together, so it does not establish whether 2s itself is better/worse than 250ms.
- **Comparator variability is considerable.** The previous version scored better in this repeat than the first experiment; baseline completion also changed. This is why this report compares contemporaneous runs, not only historic totals.

## Uncertainty and judging

- Simplified minus blocking, paired within-case bootstrap 95% intervals: success [-22.5, 7.5] percentage points; mean elapsed time [6.4, 30.8] seconds.
- Simplified minus previous yielding, paired within-case bootstrap 95% intervals: success [-35.0, 0.0] percentage points; mean elapsed time [2.8, 33.2] seconds.
- 10,000 resamples, seed 314160, fixed equal case weighting. Exploratory intervals, not a preregistered hypothesis test; they omit grading error/provider variation.
- Answers were graded by fresh tool-free model calls using full actual reviewed Python/Go source and fixture logs, with experiment arm withheld. Grades are preserved. This remains an LLM judge, not independent human ground truth. The direct-source rubric is not identical to the initial report’s strict/sensitivity two-stage rubric, so compare arms within this repeat.
- Objective completion, elapsed time, errors, fixture results and PID cleanup independently support the regression finding. No escaped fixture remained after shutdown; the benchmark runner checked and cleaned up escaped children after recording harness behavior.

## Next decision

Do not expand scheduling or install this variant based on these results. Leave it experimental. A reasonable next experiment is an interface that retains simple execution discovery and explicit deliberate waiting, tested on tasks that do not review obsolete harness APIs. Test that one change separately from the observation-budget change. This is a hypothesis, not an evaluated fix.

## Artifacts

`metrics.json`, `summary.json`, `grades.json`, exact `prompts.json`, `fixture.py`, and `driver.go.txt` are retained beside this report.
Full timestamped transcripts, fixture logs, compiled binaries and evaluated simplified source are locally at `src/benchmarks/runs/async-ab-simplified-20260930-162456`. The common reviewed source is at `src/benchmarks/runs/async-ab-20260930-150114/review`. Raw runs are git-ignored.

Full Go tests, race checks and tmux behavior checks verify mechanics, not model scheduling quality. No commit, push or reinstall was performed.
