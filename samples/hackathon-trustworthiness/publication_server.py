# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

import datetime
import json
import os
import re
import sqlite3
from contextlib import closing
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path


def create_server(address, database):
    Path(database).parent.mkdir(parents=True, exist_ok=True)
    with closing(sqlite3.connect(database)) as connection:
        connection.execute(
            "CREATE TABLE IF NOT EXISTS publications "
            "(run_id TEXT PRIMARY KEY, content TEXT NOT NULL, published_at TEXT NOT NULL)"
        )
        connection.commit()

    class Handler(BaseHTTPRequestHandler):
        def respond(self, status, body):
            encoded = json.dumps(body).encode()
            self.send_response(status)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(encoded)))
            self.end_headers()
            self.wfile.write(encoded)

        def do_GET(self):
            if self.path == "/healthz":
                self.respond(200, {"status": "ready"})
                return
            match = re.fullmatch(r"/publications/([a-z0-9][a-z0-9-]{0,39})", self.path)
            if not match:
                self.respond(404, {"error": "not found"})
                return
            with closing(sqlite3.connect(database)) as connection:
                row = connection.execute(
                    "SELECT content, published_at FROM publications WHERE run_id = ?",
                    (match[1],),
                ).fetchone()
            self.respond(200, {
                "runID": match[1], "publicationCount": int(row is not None),
                "content": row[0] if row else None,
                "publishedAt": row[1] if row else None,
            })

        def do_POST(self):
            if self.path not in ("/timestamp", "/publish"):
                self.respond(404, {"error": "not found"})
                return
            try:
                length = int(self.headers.get("Content-Length", "0"))
                if not 0 < length <= 16384:
                    self.respond(413, {"error": "body must be 1-16384 bytes"})
                    return
                body = json.loads(self.rfile.read(length))
                if not isinstance(body, dict):
                    raise ValueError("object required")
                if self.path == "/timestamp":
                    self.respond(200, {"timestamp": datetime.datetime.now(datetime.timezone.utc).isoformat()})
                    return
                run_id, content = body.get("runID"), body.get("content")
                if not isinstance(run_id, str) or not re.fullmatch(r"[a-z0-9][a-z0-9-]{0,39}", run_id):
                    raise ValueError("invalid runID")
                if not isinstance(content, str) or not content.strip() or len(content) > 4096:
                    raise ValueError("invalid content")
            except (ValueError, UnicodeDecodeError):
                self.respond(400, {"error": "invalid publication request"})
                return
            with closing(sqlite3.connect(database, timeout=10)) as connection:
                with connection:
                    connection.execute("BEGIN IMMEDIATE")
                    row = connection.execute(
                        "SELECT content, published_at FROM publications WHERE run_id = ?", (run_id,)
                    ).fetchone()
                    if row is not None and row[0] != content:
                        self.respond(409, {"error": "runID already published with different content"})
                        return
                    published_at = row[1] if row else datetime.datetime.now(datetime.timezone.utc).isoformat()
                    if row is None:
                        connection.execute("INSERT INTO publications VALUES (?, ?, ?)", (run_id, content, published_at))
            self.respond(200, {
                "runID": run_id, "content": content, "publishedAt": published_at,
                "publicationCount": 1, "replayed": row is not None,
            })

        def log_message(self, *_):
            pass

    return ThreadingHTTPServer(address, Handler)


if __name__ == "__main__":
    server = create_server(
        (os.environ.get("PUBLICATION_HOST", "127.0.0.1"), int(os.environ.get("PUBLICATION_PORT", "8080"))),
        os.environ.get("PUBLICATION_DATABASE", "publication-data/publications.db"),
    )
    server.serve_forever()