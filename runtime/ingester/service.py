# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

"""Internal asynchronous HTTP service for standalone KnowledgeBase ingestion."""

from __future__ import annotations

import json
import os
import threading
import time
import urllib.parse
from dataclasses import asdict, dataclass
from http import HTTPStatus
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from typing import Any

import ingest

_MAX_REQUEST_BYTES = 1024 * 1024
_TERMINAL_TTL_SECONDS = 3600
_execution_lock = threading.Lock()


@dataclass
class TaskStatus:
    id: str
    state: str = "running"
    documents: int = 0
    chunks: int = 0
    sourceDigest: str = ""
    error: str = ""
    completedAt: float = 0


class TaskRegistry:
    def __init__(self) -> None:
        self._lock = threading.Lock()
        self._tasks: dict[str, TaskStatus] = {}

    def submit(
        self, task_id: str, payload: dict[str, Any], environment: dict[str, str] | None = None
    ) -> tuple[TaskStatus, bool]:
        with self._lock:
            self._remove_expired_locked()
            existing = self._tasks.get(task_id)
            if existing is not None:
                return existing, False
            status = TaskStatus(id=task_id)
            self._tasks[task_id] = status
        threading.Thread(
            target=self._execute, args=(status, payload, environment or {}), daemon=True
        ).start()
        return status, True

    def get(self, task_id: str) -> TaskStatus | None:
        with self._lock:
            self._remove_expired_locked()
            return self._tasks.get(task_id)

    def delete(self, task_id: str) -> bool:
        with self._lock:
            status = self._tasks.get(task_id)
            if status is None:
                return True
            if status.state == "running":
                return False
            del self._tasks[task_id]
            return True

    def _execute(
        self, status: TaskStatus, payload: dict[str, Any], environment: dict[str, str]
    ) -> None:
        try:
            with _execution_lock:
                previous = {name: os.environ.get(name) for name in environment}
                try:
                    os.environ.update(environment)
                    summary = ingest.ingest_task(payload)
                finally:
                    for name, value in previous.items():
                        if value is None:
                            os.environ.pop(name, None)
                        else:
                            os.environ[name] = value
            with self._lock:
                status.state = "succeeded"
                status.documents = int(summary.get("documents") or 0)
                status.chunks = int(summary.get("chunks") or 0)
                status.sourceDigest = str(summary.get("sourceDigest") or "")
                status.completedAt = time.time()
        except Exception as exc:
            with self._lock:
                status.state = "failed"
                status.error = str(exc)
                status.completedAt = time.time()

    def _remove_expired_locked(self) -> None:
        cutoff = time.time() - _TERMINAL_TTL_SECONDS
        expired = [
            task_id
            for task_id, status in self._tasks.items()
            if status.completedAt and status.completedAt < cutoff
        ]
        for task_id in expired:
            del self._tasks[task_id]


class IngestHandler(BaseHTTPRequestHandler):
    registry = TaskRegistry()

    def do_GET(self) -> None:
        if self.path == "/healthz":
            self._write_json(HTTPStatus.OK, {"status": "ok"})
            return
        task_id = self._task_id_from_path()
        if task_id is None:
            self._write_json(HTTPStatus.NOT_FOUND, {"error": "not found"})
            return
        status = self.registry.get(task_id)
        if status is None:
            self._write_json(HTTPStatus.NOT_FOUND, {"error": "task not found"})
            return
        self._write_json(HTTPStatus.OK, asdict(status))

    def do_POST(self) -> None:
        if self.path != "/v1/tasks":
            self._write_json(HTTPStatus.NOT_FOUND, {"error": "not found"})
            return
        try:
            length = int(self.headers.get("Content-Length") or "0")
        except ValueError:
            self._write_json(HTTPStatus.BAD_REQUEST, {"error": "invalid content length"})
            return
        if length <= 0 or length > _MAX_REQUEST_BYTES:
            self._write_json(HTTPStatus.REQUEST_ENTITY_TOO_LARGE, {"error": "invalid task size"})
            return
        try:
            request = json.loads(self.rfile.read(length))
        except (UnicodeDecodeError, json.JSONDecodeError):
            self._write_json(HTTPStatus.BAD_REQUEST, {"error": "invalid JSON"})
            return
        task_id = request.get("id")
        payload = request.get("payload")
        environment = request.get("environment") or {}
        valid_environment = isinstance(environment, dict) and all(
            isinstance(name, str) and isinstance(value, str)
            for name, value in environment.items()
        )
        if (
            not isinstance(task_id, str)
            or not task_id
            or not isinstance(payload, dict)
            or not valid_environment
        ):
            self._write_json(HTTPStatus.BAD_REQUEST, {"error": "id and payload are required"})
            return
        status, created = self.registry.submit(task_id, payload, environment)
        self._write_json(HTTPStatus.ACCEPTED if created else HTTPStatus.OK, asdict(status))

    def do_DELETE(self) -> None:
        task_id = self._task_id_from_path()
        if task_id is None:
            self._write_json(HTTPStatus.NOT_FOUND, {"error": "not found"})
            return
        if not self.registry.delete(task_id):
            self._write_json(HTTPStatus.CONFLICT, {"error": "task is still running"})
            return
        self.send_response(HTTPStatus.NO_CONTENT)
        self.end_headers()

    def _task_id_from_path(self) -> str | None:
        prefix = "/v1/tasks/"
        if not self.path.startswith(prefix):
            return None
        task_id = urllib.parse.unquote(self.path[len(prefix) :])
        return task_id or None

    def _write_json(self, status: HTTPStatus, value: dict[str, Any]) -> None:
        body = json.dumps(value, separators=(",", ":")).encode()
        self.send_response(status)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def log_message(self, format: str, *args: Any) -> None:
        return


def main() -> None:
    address = os.environ.get("KOGNITION_INGEST_LISTEN", "0.0.0.0:8082")
    host, port = address.rsplit(":", 1)
    server = ThreadingHTTPServer((host, int(port)), IngestHandler)
    server.serve_forever()


if __name__ == "__main__":
    main()