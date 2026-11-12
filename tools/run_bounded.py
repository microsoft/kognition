# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

"""Run a test command with a wall-clock deadline, including its child processes."""

import argparse
import math
import os
import signal
import subprocess
import sys
import time


def positive_seconds(value: str) -> float:
    seconds = float(value)
    if not math.isfinite(seconds) or seconds <= 0:
        raise argparse.ArgumentTypeError("timeout must be finite and greater than zero")
    return seconds


def terminate_tree(process: subprocess.Popen) -> None:
    try:
        if os.name == "nt":
            result = subprocess.run(
                ["taskkill", "/PID", str(process.pid), "/T", "/F"],
                stdin=subprocess.DEVNULL,
                timeout=10,
                check=False,
            )
            if result.returncode:
                print("[bounded] WARNING: taskkill reported incomplete tree cleanup; "
                      "check for surviving child processes", file=sys.stderr, flush=True)
        else:
            try:
                os.killpg(process.pid, signal.SIGKILL)
            except ProcessLookupError:
                pass
        process.wait(timeout=10)
    except (OSError, subprocess.TimeoutExpired) as error:
        print(f"[bounded] WARNING: tree cleanup failed: {error}", file=sys.stderr, flush=True)
        try:
            if process.poll() is None:
                process.kill()
            process.wait(timeout=5)
        except (OSError, subprocess.TimeoutExpired) as cleanup_error:
            print(f"[bounded] WARNING: command may still be running: {cleanup_error}",
                  file=sys.stderr, flush=True)


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--timeout", type=positive_seconds, default=1800)
    parser.add_argument("command", nargs=argparse.REMAINDER)
    args = parser.parse_args()
    command = args.command
    if command and command[0] == "--":
        command = command[1:]
    if not command:
        parser.error("a test command is required after --")

    print(f"[bounded] Deadline: {args.timeout:g}s; command: {command!r}", flush=True)
    started = time.monotonic()
    try:
        process = subprocess.Popen(
            command,
            stdin=subprocess.DEVNULL,
            start_new_session=os.name != "nt",
        )
    except OSError as error:
        print(f"[bounded] Cannot start command: {error}", file=sys.stderr, flush=True)
        return 127

    try:
        return_code = process.wait(timeout=args.timeout)
    except subprocess.TimeoutExpired:
        print("[bounded] TIMEOUT: terminating test process tree", file=sys.stderr, flush=True)
        return_code = 124
    except KeyboardInterrupt:
        return_code = 130
    finally:
        if process.poll() is None:
            terminate_tree(process)
    print(f"[bounded] Finished in {time.monotonic() - started:.1f}s; exit={return_code}", flush=True)
    return return_code


if __name__ == "__main__":
    sys.exit(main())