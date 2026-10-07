#!/usr/bin/env python3
"""Run real repo tests slowly enough to observe agent scheduling."""

import argparse
import json
from pathlib import Path
import signal
import subprocess
import time
import os

parser = argparse.ArgumentParser(description=__doc__)
parser.add_argument("--log", required=True, type=Path)
parser.add_argument("--delay", type=float, default=10)
parser.add_argument("--linger", type=float, default=120)
args = parser.parse_args()
root = Path(__file__).resolve().parents[1]
started = time.monotonic()


def event(kind, **details):
    record = {
        "event": kind,
        "time": time.strftime("%Y-%m-%dT%H:%M:%S%z"),
        "elapsed": round(time.monotonic() - started, 3),
        **details,
    }
    with args.log.open("a") as log:
        log.write(json.dumps(record) + "\n")
    print(json.dumps(record), flush=True)


def stop(signum, frame):
    event("stopped", signal=signum)
    raise SystemExit(128 + signum)


signal.signal(signal.SIGTERM, stop)
signal.signal(signal.SIGINT, stop)
event("started", pid=os.getpid())
for package in ("./internal/agent", "./internal/ui", "./cmd/fn"):
    event("test_started", package=package)
    result = subprocess.run(["go", "test", package, "-count=1"], cwd=root)
    event("test_finished", package=package, exit_code=result.returncode)
    if result.returncode:
        event("failed", package=package)
        raise SystemExit(result.returncode)
    event("cooldown", seconds=args.delay)
    time.sleep(args.delay)

event("tests_complete", message="All tests passed. Only synthetic heartbeat work remains.")
deadline = time.monotonic() + args.linger
while time.monotonic() < deadline:
    event("heartbeat", message="No useful work remains; safe to stop this command.")
    time.sleep(min(5, max(0, deadline - time.monotonic())))
event("finished")
