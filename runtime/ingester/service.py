# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

"""Internal asynchronous HTTP service for standalone KnowledgeBase ingestion."""

from __future__ import annotations

import json
import os
import re
import threading
import time
import urllib.parse
from dataclasses import asdict, dataclass
from http import HTTPStatus
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from typing import Any

import ingest

_MAX_REQUEST_BYTES = 1024 * 1024
_MAX_ENV_VALUE_LENGTH = 2048
_TERMINAL_TTL_SECONDS = 3600
_execution_lock = threading.Lock()

_ALLOWED_ENV_VARS = frozenset({
    "AZURE_CREDENTIAL_TYPE",
    "AZURE_TENANT_ID",
    "AZURE_CLIENT_ID",
    "AZURE_CLIENT_SECRET",
    "AZURE_FEDERATED_TOKEN_FILE",
})

_ALLOWED_CREDENTIAL_TYPES = frozenset({
    "client-secret",
    "workload-identity",
    "managed-identity",
    "default",
})

_SAFE_IDENTIFIER_PATTERN = re.compile(r"^[a-zA-Z0-9_.-]+$")


_DEFAULT_ALLOWED_TOKEN_FILE_DIR = "/var/run/secrets"


def _get_allowed_token_dir() -> str:
    return os.environ.get("KOGNITION_ALLOWED_TOKEN_FILE_DIR", _DEFAULT_ALLOWED_TOKEN_FILE_DIR)


def _validate_environment(environment: Any) -> tuple[bool, str]:
    """Validate client-supplied environment dictionary against strict allowlist and format rules.

    Returns (is_valid, error_message). The error message never echoes sensitive values.
    """
    if not isinstance(environment, dict):
        return False, "environment must be an object"
    for key, value in environment.items():
        if not isinstance(key, str) or key not in _ALLOWED_ENV_VARS:
            return False, f"environment variable '{key}' is not allowed"
        if not isinstance(value, str):
            return False, f"environment variable '{key}' must be a string"
        if len(value) > _MAX_ENV_VALUE_LENGTH:
            return False, f"environment variable '{key}' exceeds maximum length of {_MAX_ENV_VALUE_LENGTH}"
        if any(ord(c) < 32 or ord(c) == 127 for c in value):
            return False, f"environment variable '{key}' contains invalid characters"

        if key == "AZURE_CREDENTIAL_TYPE":
            if value.lower() not in _ALLOWED_CREDENTIAL_TYPES:
                return False, f"invalid value for '{key}'"
        elif key in ("AZURE_TENANT_ID", "AZURE_CLIENT_ID"):
            if not _SAFE_IDENTIFIER_PATTERN.match(value):
                return False, f"invalid value for '{key}'"
        elif key == "AZURE_CLIENT_SECRET":
            if any(c.isspace() for c in value) or not value.isascii() or not value.isprintable():
                return False, f"invalid value for '{key}'"
        elif key == "AZURE_FEDERATED_TOKEN_FILE":
            if not os.path.isabs(value):
                return False, f"invalid value for '{key}'"
            parts = value.replace("\\", "/").split("/")
            if ".." in parts:
                return False, f"invalid value for '{key}'"
            allowed_dir = os.path.abspath(_get_allowed_token_dir())
            norm_val = os.path.abspath(value)
            if norm_val == allowed_dir:
                return False, f"invalid value for '{key}'"
            try:
                if os.path.commonpath([norm_val, allowed_dir]) != allowed_dir:
                    return False, f"invalid value for '{key}'"
            except ValueError:
                return False, f"invalid value for '{key}'"
            real_allowed = os.path.realpath(allowed_dir)
            real_val = os.path.realpath(value)
            if real_val == real_allowed:
                return False, f"invalid value for '{key}'"
            try:
                if os.path.commonpath([real_val, real_allowed]) != real_allowed:
                    return False, f"invalid value for '{key}'"
            except ValueError:
                return False, f"invalid value for '{key}'"
    return True, ""


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
        # Defense-in-depth: only permit allowlisted variables even if called directly
        safe_environment = {
            name: value
            for name, value in environment.items()
            if name in _ALLOWED_ENV_VARS and isinstance(value, str)
        }
        try:
            with _execution_lock:
                previous = {name: os.environ.get(name) for name in safe_environment}
                try:
                    os.environ.update(safe_environment)
                    summary = ingest.ingest_task(payload, env=safe_environment)
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
        except Exception as exc:  # noqa: BLE001
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
        if not isinstance(task_id, str) or not task_id or not isinstance(payload, dict):
            self._write_json(HTTPStatus.BAD_REQUEST, {"error": "id and payload are required"})
            return

        environment = request.get("environment")
        if environment is None:
            environment = {}
        valid_environment, err_msg = _validate_environment(environment)
        if not valid_environment:
            self._write_json(HTTPStatus.BAD_REQUEST, {"error": err_msg})
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
    # Initialize trusted service configuration from daemon environment at startup.
    ingest.set_trusted_service_config(
        indexer_executable=os.environ.get(
            "KOGNITION_KNOWLEDGE_INDEXER_EXECUTABLE",
            "/app/kognition-knowledge-indexer",
        ),
        allow_private_networks=(
            os.environ.get("KOGNITION_INGEST_ALLOW_PRIVATE_NETWORKS", "").lower() == "true"
        ),
        trusted_embedding_hosts=os.environ.get("KOGNITION_INGEST_TRUSTED_EMBEDDING_HOSTS", "").split(","),
    )
    address = os.environ.get("KOGNITION_INGEST_LISTEN", "0.0.0.0:8082")
    host, port = address.rsplit(":", 1)
    server = ThreadingHTTPServer((host, int(port)), IngestHandler)
    server.serve_forever()


if __name__ == "__main__":
    main()