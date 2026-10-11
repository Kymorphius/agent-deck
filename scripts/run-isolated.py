#!/usr/bin/env python3
"""matrix/hardened: isolated test HOME, shared Go caches, deterministic cleanup.

Usage: scripts/run-isolated.py go test ./internal/example/...
Requires Python 3 and Go. This isolates configuration, not launchd/tmux or the OS.
"""
import json
import os
from pathlib import Path
import signal
import subprocess
import sys
import tempfile


def main():
    command = sys.argv[1:] or ['go', 'test', './...']
    # Resolve BEFORE changing HOME; retain modules and downloaded Go toolchains too.
    cache = json.loads(subprocess.check_output(
        ['go', 'env', '-json', 'GOPATH', 'GOMODCACHE', 'GOCACHE', 'GOTOOLCHAIN'], text=True))
    for key in ('GOPATH', 'GOMODCACHE', 'GOCACHE'):
        if not cache.get(key) or cache[key] == 'off':
            raise RuntimeError('A persistent Go cache is required: ' + key)
    child = None
    interrupted = None

    def stop(signum, _frame):
        nonlocal interrupted
        interrupted = signum
        if child is not None:
            try:
                os.killpg(child.pid, signum)
            except ProcessLookupError:
                pass

    previous = {s: signal.signal(s, stop) for s in (signal.SIGINT, signal.SIGTERM, signal.SIGHUP)}
    try:
        with tempfile.TemporaryDirectory(prefix='agent-deck-test-home-') as root:
            env = {key: os.environ[key] for key in ('PATH', 'LANG', 'LC_ALL', 'TERM') if key in os.environ}
            env.update(cache)
            env.update(HOME=root, TZ='UTC', GOTMPDIR=root + '/tmp', TMPDIR=root + '/tmp',
                       XDG_CONFIG_HOME=root + '/config', XDG_DATA_HOME=root + '/data',
                       XDG_CACHE_HOME=root + '/cache', XDG_STATE_HOME=root + '/state',
                       XDG_RUNTIME_DIR=root + '/run')
            for key in ('TMPDIR', 'XDG_CONFIG_HOME', 'XDG_DATA_HOME', 'XDG_CACHE_HOME',
                        'XDG_STATE_HOME', 'XDG_RUNTIME_DIR'):
                Path(env[key]).mkdir(mode=0o700)
            if interrupted:
                return 128 + interrupted
            child = subprocess.Popen(command, env=env, start_new_session=True)
            if interrupted:
                stop(interrupted, None)
            while True:
                try:
                    code = child.wait(timeout=0.2)
                    break
                except subprocess.TimeoutExpired:
                    if interrupted:
                        try:
                            code = child.wait(timeout=3)
                        except subprocess.TimeoutExpired:
                            os.killpg(child.pid, signal.SIGKILL)
                            code = child.wait()
                        break
            return 128 + interrupted if interrupted else (code if code >= 0 else 128 - code)
    finally:
        for sig, handler in previous.items():
            signal.signal(sig, handler)


if __name__ == '__main__':
    sys.exit(main())
