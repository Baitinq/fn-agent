import argparse, json, os, signal, subprocess, sys, time
from pathlib import Path
p=argparse.ArgumentParser(); p.add_argument('kind'); p.add_argument('--name',default='job'); a=p.parse_args()
start=time.monotonic()
def event(kind,**details):
    d=dict(event=kind,name=a.name,pid=os.getpid(),wall=time.time(),elapsed=round(time.monotonic()-start,3),**details)
    with open('fixture-events.jsonl','a') as f: f.write(json.dumps(d)+'\n')
    print(json.dumps(d),flush=True)
def stop(sig,frame):
    event('stopped',signal=sig); raise SystemExit(128+sig)
signal.signal(signal.SIGTERM,stop); signal.signal(signal.SIGINT,stop)
event('started',job_kind=a.kind)
if a.kind=='suite':
    for name,pattern in [('shell','TestPythonREPLPersistsStateAndExposesShell'),('cancellation','TestPythonREPLCancellationPreservesState'),('host-draining','TestPythonREPLDrainsCanceledLLMCall')]:
        event('check_started',check=name)
        r=subprocess.run(['go','test','./internal/agent','-run',pattern,'-count=1'],cwd='repo')
        event('check_finished',check=name,exit_code=r.returncode)
        if r.returncode: event('failed'); sys.exit(r.returncode)
        time.sleep(7)
    event('useful_complete')
    for i in range(8): event('heartbeat',message='Verification is finished. Only synthetic keepalive work remains.'); time.sleep(5)
elif a.kind=='failure':
    time.sleep(2)
    r=subprocess.run(['go','test','./internal/agent','-run','[','-count=1'],cwd='repo')
    event('failed',exit_code=r.returncode,message='No further useful work; this fixture lingers for 35 seconds after the real test error.')
    time.sleep(35)
elif a.kind=='interactive':
    event('input_required',message='Enter the token from spec.txt; this is line-oriented stdin, not a TTY.')
    line=sys.stdin.readline().strip()
    expected=Path('spec.txt').read_text().strip()
    if line!=expected: event('rejected',received=line);sys.exit(1)
    event('accepted');event('useful_complete')
    time.sleep(35)
elif a.kind=='check':
    time.sleep(18 if a.name=='alpha' else 24)
    target='repo/internal/agent/repl.py' if a.name=='alpha' else 'repo/internal/agent/repl.go'
    text=Path(target).read_text()
    event('check_finished',check=a.name,exit_code=0,bytes=len(text))
else: sys.exit(2)
event('natural_finish')
