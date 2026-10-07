import ast
import asyncio
import codecs
import contextlib
import contextvars
import dataclasses
import hashlib
from html.parser import HTMLParser
import io
import itertools
import json
import math
import os
import pickle
import signal
import sys
import threading
import traceback
from typing import Optional
import urllib.parse
import urllib.request

_protocol_in = sys.stdin
_protocol_out = sys.stdout
_protocol_lock = threading.Lock()


def _send_protocol(message):
    with _protocol_lock:
        _protocol_out.write(json.dumps(message) + "\n")
        _protocol_out.flush()


_active_processes = set()
_execution_task = None
_llm_request_ids = itertools.count(1)
_llm_responses = {}
_host_out = os.fdopen(3, "w", buffering=1)
_activity_context = contextvars.ContextVar("activity", default=None)
_activities = {}
_activity_ids = itertools.count(1)
_llm_response_reader = None
_web_search_url = "https://html.duckduckgo.com/html/"

@dataclasses.dataclass
class ShellResult:
    stdout: str
    exit_code: int
    error: Optional[str] = None

@dataclasses.dataclass
class SearchResult:
    title: str
    url: str
    snippet: str

def _kill_active_processes():
    for process in _active_processes:
        if process.returncode is None:
            try:
                os.killpg(process.pid, signal.SIGKILL)
            except ProcessLookupError:
                pass

def _stop_repl(*_):
    _kill_active_processes()
    os._exit(143)

def _interrupt_execution(*_):
    if _execution_task is None:
        return
    asyncio.get_running_loop().call_soon_threadsafe(_execution_task.cancel)

signal.signal(signal.SIGTERM, _stop_repl)
signal.signal(signal.SIGINT, _interrupt_execution)

async def shell(command, timeout=None):
    """Run a shell command and return ShellResult with combined stdout/stderr."""
    if timeout is not None and (not isinstance(timeout, (int, float)) or not math.isfinite(timeout) or timeout <= 0):
        raise ValueError("timeout must be a positive finite number of seconds")
    process = await asyncio.create_subprocess_shell(
        command,
        executable="/bin/sh",
        stdout=asyncio.subprocess.PIPE,
        stderr=asyncio.subprocess.STDOUT,
        start_new_session=True,
        stdin=asyncio.subprocess.PIPE,
    )
    _active_processes.add(process)
    decoder = codecs.getincrementaldecoder("utf-8")(errors="replace")
    chunks = []

    async def read_output():
        while True:
            chunk = await process.stdout.read(65536)
            text = decoder.decode(chunk, final=not chunk)
            if text:
                chunks.append(text)
                _activity_context.get()._publish(text, progress=True)
            if not chunk:
                await process.wait()
                return

    async def write_input():
        while True:
            text = await receive()
            if text is None:
                process.stdin.close()
                return
            process.stdin.write(text.encode())
            await process.stdin.drain()

    input_task = asyncio.create_task(write_input())
    try:
        try:
            await asyncio.wait_for(read_output(), timeout)
        except asyncio.TimeoutError:
            os.killpg(process.pid, signal.SIGKILL)
            await read_output()
            return ShellResult("".join(chunks), -1, f"timeout:{timeout:g}")
        except asyncio.CancelledError:
            with contextlib.suppress(ProcessLookupError):
                os.killpg(process.pid, signal.SIGKILL)
            await process.wait()
            raise
        error: Optional[str] = None if process.returncode == 0 else f"exit status {process.returncode}"
        return ShellResult("".join(chunks), process.returncode, error)
    finally:
        if input_task is not None:
            input_task.cancel()
            await asyncio.gather(input_task, return_exceptions=True)
        _active_processes.discard(process)

class _SearchParser(HTMLParser):
    def __init__(self):
        super().__init__()
        self.results = []
        self.result = None
        self.capture = None
        self.capture_tag = None

    def handle_starttag(self, tag, attrs):
        attrs = dict(attrs)
        classes = attrs.get("class", "").split()
        if "result__a" in classes:
            self.result = {"href": attrs.get("href", ""), "title": [], "snippet": []}
            self.results.append(self.result)
            self.capture = self.result["title"]
            self.capture_tag = tag
        elif self.result is not None and "result__snippet" in classes:
            self.capture = self.result["snippet"]
            self.capture_tag = tag

    def handle_endtag(self, tag):
        if tag == self.capture_tag:
            self.capture = None
            self.capture_tag = None

    def handle_data(self, data):
        if self.capture is not None:
            self.capture.append(data)

def _search_text(parts):
    return " ".join("".join(parts).split())

def _result_url(value):
    if value.startswith("//"):
        value = "https:" + value
    parsed = urllib.parse.urlparse(value)
    target = urllib.parse.parse_qs(parsed.query).get("uddg")
    if target:
        parsed = urllib.parse.urlparse(target[0])
    if parsed.scheme not in ("http", "https"):
        return None
    if parsed.hostname and parsed.hostname.lower() == "duckduckgo.com" and parsed.path.startswith("/y.js"):
        return None
    return urllib.parse.urlunparse(parsed)

async def _read_llm_responses():
    reader = asyncio.StreamReader()
    protocol = asyncio.StreamReaderProtocol(reader)
    await asyncio.get_running_loop().connect_read_pipe(lambda: protocol, os.fdopen(4, "rb"))
    while True:
        line = await reader.readline()
        if not line:
            raise EOFError("host connection closed")
        response = json.loads(line)
        future = _llm_responses.pop(response["id"])
        future.set_result(response)


def _send_host(message):
    _host_out.write(json.dumps(message) + "\n")
    _host_out.flush()


async def llm(prompt: str) -> str:
    """Run one fresh, tool-free model call and return its response as a string."""
    global _llm_response_reader
    if not isinstance(prompt, str):
        raise TypeError("llm() prompt must be a string")
    request_id = next(_llm_request_ids)
    future = asyncio.get_running_loop().create_future()
    _llm_responses[request_id] = future
    activity = _activity_context.get()
    activity._host_futures.add(future)
    future.add_done_callback(activity._host_futures.discard)
    _send_host({"id": request_id, "prompt": prompt})
    if _llm_response_reader is None:
        _llm_response_reader = contextvars.Context().run(asyncio.create_task, _read_llm_responses())
    try:
        response = await asyncio.shield(future)
    except asyncio.CancelledError:
        if activity._cancel_host_calls or activity._task.cancelling():
            _send_host({"cancel": request_id})
        raise
    if "error" in response:
        raise RuntimeError(response["error"])
    return response["result"]


class Execution:
    def __init__(self, coroutine):
        self.id = next(_activity_ids)
        self._loop = asyncio.get_running_loop()
        self._thread = threading.get_ident()
        self._output = io.StringIO()
        self._result_output = io.StringIO()
        self._streaming = True
        self._host_futures = set()
        self._cancel_host_calls = False
        self._waiting = False
        self._input = asyncio.Queue()
        self._changed = asyncio.Event()
        self._tasks = set()
        self._coroutine = coroutine
        context = contextvars.copy_context()
        context.run(_activity_context.set, self)
        self._task = context.run(asyncio.create_task, self._run(coroutine))
        self._task.add_done_callback(self._finished)

    async def _run(self, coroutine):
        try:
            return await coroutine
        except asyncio.CancelledError:
            self._cancel_host_calls = True
            raise
        finally:
            children = self._tasks - {asyncio.current_task()}
            for task in children:
                task.cancel()
            await asyncio.gather(*children, return_exceptions=True)
            if self._host_futures:
                await asyncio.gather(*self._host_futures)

    def _finished(self, task):
        self._coroutine.close()
        if not task.cancelled():
            task.exception()
        self._changed.set()

    def _publish(self, text, progress=False):
        if not text:
            return
        if threading.get_ident() != self._thread:
            self._loop.call_soon_threadsafe(self._publish, text, progress)
            return
        self._output.write(text)
        if self._streaming:
            _send_protocol({"progress" if progress else "stream": text})
            if not progress:
                self._result_output.write(text)
        self._changed.set()

    @property
    def status(self):
        if not self._task.done():
            return "running"
        if self._task.cancelled():
            return "cancelled"
        return "failed" if self._task.exception() else "done"

    def read(self):
        text = self._output.getvalue()
        self._output.seek(0)
        self._output.truncate(0)
        if self.status == "running":
            self._changed.clear()
        return text

    async def observe(self, timeout=None):
        """Wait for new output/completion, then consume output and report state."""
        owner = _activity_context.get()
        owner._waiting = True
        try:
            if not self._changed.is_set() and self.status == "running":
                try:
                    await asyncio.wait_for(self._changed.wait(), timeout)
                except asyncio.TimeoutError:
                    pass
            return {"id": self.id, "status": self.status, "output": self.read()}
        finally:
            owner._waiting = False

    def result(self):
        return self._task.result()

    def send(self, value):
        self._input.put_nowait(value)

    async def cancel(self):
        self._cancel_host_calls = True
        self._task.cancel()
        await asyncio.gather(self._task, return_exceptions=True)

    def __repr__(self):
        return f"Execution(id={self.id}, status={self.status!r})"

    def __getstate__(self):
        raise TypeError("running executions are not persisted")


def execution(id):
    return _activities[id]


async def receive():
    return await _activity_context.get()._input.get()


class _OutputRouter:
    def write(self, text):
        activity = _activity_context.get()
        if activity is not None:
            activity._publish(text)
            return len(text)
        return len(text)

    def flush(self):
        pass


def _task_factory(loop, coroutine, **kwargs):
    task = asyncio.Task(coroutine, loop=loop, **kwargs)
    task._activity = _activity_context.get()
    if task._activity is not None:
        task._activity._tasks.add(task)
        task.add_done_callback(task._activity._tasks.discard)
    return task


def _web_search(query, max_results):
    data = urllib.parse.urlencode({"q": query}).encode()
    request = urllib.request.Request(
        _web_search_url,
        data=data,
        headers={"User-Agent": "Mozilla/5.0 (compatible; fn-agent/1.0)"},
    )
    with urllib.request.urlopen(request) as response:
        body = response.read().decode("utf-8", errors="replace")
    parser = _SearchParser()
    parser.feed(body)
    results = []
    for result in parser.results:
        url = _result_url(result["href"])
        if url is None:
            continue
        results.append(SearchResult(_search_text(result["title"]), url, _search_text(result["snippet"])))
        if len(results) == max_results:
            break
    return results

async def web_search(query, max_results=8):
    """Search DuckDuckGo and return a list of SearchResult values."""
    query = query.strip()
    if not query:
        raise ValueError("query must not be empty")
    if not isinstance(max_results, int) or not 1 <= max_results <= 20:
        raise ValueError("max_results must be between 1 and 20")
    return await asyncio.to_thread(_web_search, query, max_results)

_user_globals = {}

_builtin_names = {"shell", "web_search", "llm", "ShellResult", "SearchResult", "receive", "execution", "__builtins__"}

def _snapshot(path, objects_path):
    os.makedirs(objects_path, mode=0o700, exist_ok=True)
    values = {}
    for name, value in _user_globals.items():
        if name in _builtin_names:
            continue
        try:
            data = pickle.dumps(value)
        except Exception:
            continue
        digest = hashlib.sha256(data).hexdigest()
        object_path = os.path.join(objects_path, digest)
        if not os.path.exists(object_path):
            temporary_path = object_path + ".tmp"
            with open(temporary_path, "wb") as object_file:
                object_file.write(data)
            os.chmod(temporary_path, 0o600)
            os.replace(temporary_path, object_path)
        values[name] = digest
    with open(path, "w") as state:
        json.dump(values, state)
    os.chmod(path, 0o600)
    return {"output": ""}

def _restore(path, objects_path):
    with open(path) as state:
        values = json.load(state)
    for name in list(_user_globals):
        if name not in _builtin_names:
            del _user_globals[name]
    for name, digest in values.items():
        try:
            with open(os.path.join(objects_path, digest), "rb") as object_file:
                _user_globals[name] = pickle.load(object_file)
        except Exception:
            pass
    return {"output": ""}

async def _run_code(tree):
    result = eval(compile(tree, "<fn-repl>", "exec", flags=ast.PyCF_ALLOW_TOP_LEVEL_AWAIT), _user_globals)
    if result is not None:
        await result

async def _execute(code):
    _user_globals.update(
        shell=shell,
        web_search=web_search,
        llm=llm,
        ShellResult=ShellResult,
        SearchResult=SearchResult,
        receive=receive,
        execution=execution,
    )
    try:
        tree = ast.parse(code, mode="exec")
        value = None
        if tree.body and isinstance(tree.body[-1], ast.Expr):
            prefix = ast.Module(body=tree.body[:-1], type_ignores=[])
            if prefix.body:
                await _run_code(prefix)
            expression = ast.Expression(tree.body[-1].value)
            value = eval(compile(expression, "<fn-repl>", "eval", flags=ast.PyCF_ALLOW_TOP_LEVEL_AWAIT), _user_globals)
            if asyncio.iscoroutine(value):
                value = await value
        else:
            await _run_code(tree)
        if value is not None:
            print(repr(value), end="")
        return value
    except BaseException:
        print(traceback.format_exc(), end="")
        raise


async def _main():
    global _execution_task
    asyncio.get_running_loop().set_task_factory(_task_factory)
    sys.stdout = sys.stderr = _OutputRouter()
    while line := await asyncio.to_thread(_protocol_in.readline):
        request = json.loads(line)
        operation = request.get("op", "execute")
        if operation == "snapshot":
            response = _snapshot(request["path"], request["objects_path"])
        elif operation == "restore":
            response = _restore(request["path"], request["objects_path"])
        else:
            job = Execution(_execute(request.get("code", "")))
            _execution_task = job._task
            try:
                await asyncio.wait([job._task], timeout=request["yield_seconds"])
                if job._waiting:
                    await asyncio.gather(job._task, return_exceptions=True)
            finally:
                job._streaming = False
                _execution_task = None
            if job._task.done():
                response = {"output": job._result_output.getvalue(), "error": job.status != "done"}
            else:
                _activities[job.id] = job
                response = {"output": job.read(), "execution_id": job.id, "status": job.status}
            job._result_output = None
        _send_protocol(response)
    await asyncio.gather(*(a.cancel() for a in activities()))

asyncio.run(_main())
