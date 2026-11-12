# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

import subprocess
import sys
from pathlib import Path

import pytest


RUNNER = Path(__file__).resolve().parents[1] / "run_bounded.py"


def invoke(*arguments: str) -> subprocess.CompletedProcess:
    return subprocess.run(
        [sys.executable, str(RUNNER), *arguments],
        capture_output=True,
        text=True,
        timeout=15,
    )


@pytest.mark.parametrize("exit_code", [0, 7])
def test_preserves_command_output_and_exit_status(exit_code: int) -> None:
    result = invoke("--timeout", "5", "--", sys.executable, "-c",
                    f"import sys; print('output with spaces'); sys.exit({exit_code})")
    assert result.returncode == exit_code
    assert "output with spaces" in result.stdout
    assert f"exit={exit_code}" in result.stdout


def test_deadline_terminates_blocked_command_and_descendants() -> None:
    child = "import threading; print('child ready', flush=True); threading.Event().wait()"
    parent = (
        "import subprocess, sys, threading; "
        f"subprocess.Popen([sys.executable, '-c', {child!r}]); "
        "threading.Event().wait()"
    )
    result = invoke("--timeout", "2", "--", sys.executable, "-c", parent)
    assert result.returncode == 124
    assert "child ready" in result.stdout
    assert "TIMEOUT" in result.stderr
    assert "exit=124" in result.stdout


@pytest.mark.parametrize("timeout", ["0", "-1", "nan", "inf"])
def test_rejects_unbounded_deadlines(timeout: str) -> None:
    result = invoke("--timeout", timeout, "--", sys.executable, "-c", "pass")
    assert result.returncode == 2
    assert "greater than zero" in result.stderr


def test_missing_executable_fails_immediately() -> None:
    result = invoke("--", "kognition-nonexistent-test-executable")
    assert result.returncode == 127
    assert "Cannot start command" in result.stderr


def test_missing_command_is_an_error() -> None:
    result = invoke("--timeout", "5")
    assert result.returncode == 2


@pytest.mark.skipif(sys.platform != "win32", reason="Windows taskkill regression")
def test_partial_taskkill_failure_preserves_timeout_status() -> None:
    probe = (
        "import runpy, subprocess, sys\n"
        "original_run = subprocess.run\n"
        "def partial_taskkill(*args, **kwargs):\n"
        "    result = original_run(*args, **kwargs)\n"
        "    result.returncode = 255\n"
        "    return result\n"
        "subprocess.run = partial_taskkill\n"
        f"sys.argv = [{str(RUNNER)!r}, '--timeout', '1', '--', sys.executable, "
        "'-c', 'import threading; threading.Event().wait()']\n"
        f"runpy.run_path({str(RUNNER)!r}, run_name='__main__')\n"
    )
    result = subprocess.run(
        [sys.executable, "-c", probe], capture_output=True, text=True, timeout=30,
    )
    assert result.returncode == 124
    assert "incomplete tree cleanup" in result.stderr
    assert "Traceback" not in result.stderr
    assert "exit=124" in result.stdout


def test_pytest_configuration_terminates_blocked_test(tmp_path: Path) -> None:
    blocked_test = tmp_path / "test_blocked.py"
    blocked_test.write_text(
        "import threading\ndef test_blocked():\n    threading.Event().wait()\n",
        encoding="utf-8",
    )
    result = subprocess.run(
        [sys.executable, str(RUNNER), "--timeout", "30", "--", sys.executable,
         "-m", "pytest", "-c", str(RUNNER.parent.parent / "pytest.ini"),
         "-o", "timeout=1", str(blocked_test), "-q"],
        capture_output=True,
        text=True,
        timeout=45,
    )
    assert result.returncode == 1
    assert "Timeout" in result.stdout + result.stderr
    assert "test_blocked" in result.stdout + result.stderr
    assert "exit=124" not in result.stdout