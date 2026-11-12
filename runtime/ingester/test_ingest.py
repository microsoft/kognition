# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

"""Unit tests for the kognition-kb-ingester (ingest.py).

These cover the pure, deterministic logic — filtering, chunking, text
extraction, source dispatch, Azure endpoint normalization, and the embedding
dimension guard — plus source fetchers exercised through injected fakes
(no network, no cloud, no database). The pgvector persistence path is covered
by the Go integration suite (internal/memory/pgvector) against a real database.

Run:  cd runtime/ingester && python -m pytest -q
"""

from __future__ import annotations

import hashlib
import json
import os
import socket
from unittest.mock import mock_open

import httpx
import pytest
from psycopg import sql

import ingest


def test_main_prefers_projected_input_file(monkeypatch, tmp_path, capsys):
    input_file = tmp_path / "input.json"
    input_file.write_text("not-json", encoding="utf-8")
    monkeypatch.setenv("KOGNITION_INGEST_INPUT_FILE", str(input_file))
    monkeypatch.setenv("KOGNITION_INGEST_INPUT", json.dumps({"kb": {"name": "ignored"}}))

    assert ingest.main() == 2
    assert "failed to parse KOGNITION_INGEST_INPUT" in capsys.readouterr().err


def test_main_writes_provider_confirmation_to_termination_log(monkeypatch):
    task = {"kb": {"name": "docs"}, "providerIndex": {"dimension": 3}}
    confirmation = {
        "documents": 1,
        "chunks": 2,
        "sourceDigest": "sha256:" + "a" * 64,
        "backendBinding": {"providerName": "azuresearch"},
        "providerConfirmation": {
            "operationId": "team-a/kb-ingest-docs-1",
            "sourceDigest": "sha256:" + "a" * 64,
            "backendOperationId": "operation-1",
        },
    }
    projected_input = "/var/run/kognition/input/task.json"
    file_mock = mock_open(read_data=json.dumps(task))
    monkeypatch.setenv("KOGNITION_INGEST_INPUT_FILE", projected_input)
    monkeypatch.setattr(ingest, "open", file_mock, raising=False)
    monkeypatch.setattr(ingest, "ingest_task", lambda parsed: confirmation)

    assert ingest.main() == 0

    assert file_mock.call_args_list[-1].args == ("/dev/termination-log", "w")
    file_mock().write.assert_called_once_with(json.dumps(confirmation))


@pytest.mark.parametrize(
    "table",
    [
        "chunks; DROP TABLE agents",
        "public.chunks",
        "MixedCase",
        "1chunks",
        "a" * 56,
    ],
)
def test_validate_table_name_rejects_unsafe_identifiers(table):
    with pytest.raises(ValueError, match="vectorStore.table"):
        ingest._validate_table_name(table)


@pytest.mark.parametrize("table", ["chunks", "_chunks", "kb_chunks_2", "a" * 55])
def test_validate_table_name_accepts_safe_identifiers(table):
    assert ingest._validate_table_name(table) == table


class _RecordingCursor:
    def __init__(self):
        self.queries = []

    def __enter__(self):
        return self

    def __exit__(self, *_args):
        return None

    def execute(self, query, _params=None):
        self.queries.append(query)


class _RecordingConnection:
    def __init__(self):
        self.cursor_instance = _RecordingCursor()

    def cursor(self):
        return self.cursor_instance

    def commit(self):
        return None


def test_ensure_schema_composes_identifiers():
    conn = _RecordingConnection()
    ingest._ensure_schema(conn, "kb_chunks", 1536)

    assert isinstance(conn.cursor_instance.queries[1], sql.Composed)
    assert isinstance(conn.cursor_instance.queries[2], sql.Composed)


# --------------------------- _matches_filters --------------------------------
# Shared include/exclude glob logic used by both the Azure and S3 blob
# fetchers — this is the core of source prefix/pattern filtering.


def test_matches_filters_no_patterns_accepts_everything():
    assert ingest._matches_filters("docs/readme.md", [], []) is True


def test_matches_filters_include_hit():
    assert ingest._matches_filters("docs/readme.md", ["*.md"], []) is True


def test_matches_filters_include_miss():
    assert ingest._matches_filters("docs/logo.png", ["*.md"], []) is False


def test_matches_filters_exclude_hit_wins_over_include():
    assert ingest._matches_filters("docs/draft.md", ["*.md"], ["*draft*"]) is False


def test_matches_filters_exclude_only():
    assert ingest._matches_filters("docs/keep.md", [], ["*.png"]) is True
    assert ingest._matches_filters("docs/skip.png", [], ["*.png"]) is False


# --------------------------- _chunk ------------------------------------------


def test_chunk_empty_and_whitespace_return_no_chunks():
    assert ingest._chunk("", {"size": 100}) == []
    assert ingest._chunk("   \n\t  ", {"size": 100}) == []


def test_chunk_fixed_mode_slices_by_step():
    text = "abcdefghij"  # 10 chars
    chunks = ingest._chunk(text, {"size": 4, "overlap": 0, "mode": "fixed"})
    assert chunks == ["abcd", "efgh", "ij"]


def test_chunk_fixed_mode_overlap_advances_by_size_minus_overlap():
    text = "abcdefgh"  # 8 chars
    chunks = ingest._chunk(text, {"size": 4, "overlap": 2, "mode": "fixed"})
    # step = size - overlap = 2 → windows at 0,2,4,6
    assert chunks == ["abcd", "cdef", "efgh", "gh"]


def test_chunk_recursive_mode_produces_multiple_chunks_within_size():
    text = ("Kognition orchestrates cognitive agents. " * 40).strip()
    chunks = ingest._chunk(text, {"size": 128, "overlap": 16, "mode": "recursive"})
    assert len(chunks) > 1
    # Recursive splitter respects the configured chunk size as an upper bound.
    assert all(len(c) <= 128 for c in chunks)
    assert all(c.strip() for c in chunks)


def test_chunk_defaults_to_recursive_mode():
    text = "one two three four five six seven eight nine ten. " * 10
    chunks = ingest._chunk(text, {"size": 64})
    assert len(chunks) > 1


# --------------------------- _extract_text -----------------------------------


def test_extract_text_decodes_utf8():
    assert ingest._extract_text("héllo".encode("utf-8"), "notes.md") == "héllo"


def test_extract_text_is_lenient_on_invalid_bytes():
    # Invalid UTF-8 byte should be replaced, never raise.
    out = ingest._extract_text(b"ok\xff", "notes.txt")
    assert out.startswith("ok")


def test_extract_text_ignores_query_string_for_extension_detection():
    # A ".md?sig=..." URL is treated as text, not routed to the PDF path.
    assert ingest._extract_text(b"# Title", "https://x/readme.md?sig=abc") == "# Title"


# --------------------------- _fetch_inline / _fetch --------------------------


def test_fetch_inline_uses_provided_uri():
    out = list(ingest._fetch_inline({"data": "hello", "uri": "inline://custom"}))
    assert out == [("hello", "inline://custom")]


def test_fetch_inline_synthesizes_uri_from_hash_when_absent():
    (text, uri), = list(ingest._fetch_inline({"data": "hello"}))
    assert text == "hello"
    expected = hashlib.sha256(b"hello").hexdigest()[:16]
    assert uri == f"inline://{expected}"


def test_fetch_inline_empty_data_yields_empty_text():
    (text, uri), = list(ingest._fetch_inline({}))
    assert text == ""
    assert uri.startswith("inline://")


def test_fetch_inline_rejects_oversized_document(monkeypatch):
    monkeypatch.setenv("KOGNITION_INGEST_MAX_DOCUMENT_BYTES", "4")
    with pytest.raises(ValueError, match="exceeds 4 bytes"):
        list(ingest._fetch_inline({"data": "12345"}))


def test_fetch_dispatch_routes_inline():
    out = list(ingest._fetch({"type": "inline", "data": "x", "uri": "inline://x"}))
    assert out == [("x", "inline://x")]


def test_fetch_unsupported_type_raises():
    with pytest.raises(ValueError, match="unsupported source"):
        list(ingest._fetch({"type": "carrier-pigeon"}))


def test_fetch_blob_without_provider_is_unsupported():
    with pytest.raises(ValueError, match="unsupported source"):
        list(ingest._fetch({"type": "blob", "bucket": "b"}))


# --------------------------- _fetch_url --------------------------------------


def _install_http_transport(monkeypatch, handler):
    real_client = httpx.Client
    transport = httpx.MockTransport(handler)
    monkeypatch.setattr(
        ingest.httpx,
        "Client",
        lambda **kwargs: real_client(transport=transport, **kwargs),
    )
    monkeypatch.setattr(
        ingest.socket,
        "getaddrinfo",
        lambda *_args, **_kwargs: [
            (socket.AF_INET, socket.SOCK_STREAM, 6, "", ("93.184.216.34", 443))
        ],
    )


def test_fetch_url_extracts_text_and_yields_uri(monkeypatch):
    _install_http_transport(
        monkeypatch,
        lambda request: httpx.Response(200, content=b"# Doc body", request=request),
    )
    (text, uri), = list(ingest._fetch_url({"uri": "https://example.test/doc.md"}))
    assert text == "# Doc body"
    assert uri == "https://example.test/doc.md"


def test_fetch_url_rejects_private_http_by_default():
    with pytest.raises(ValueError, match="HTTPS"):
        list(ingest._fetch_url({"uri": "http://docs.default.svc/doc.md"}))


def test_fetch_url_allows_private_http_with_explicit_opt_in(monkeypatch):
    monkeypatch.setenv("KOGNITION_INGEST_ALLOW_PRIVATE_NETWORKS", "true")
    _install_http_transport(
        monkeypatch,
        lambda request: httpx.Response(200, content=b"# Local doc", request=request),
    )

    (text, uri), = list(ingest._fetch_url({"uri": "http://docs.default.svc/doc.md"}))

    assert text == "# Local doc"
    assert uri == "http://docs.default.svc/doc.md"


def test_fetch_url_rejects_private_destination(monkeypatch):
    monkeypatch.setattr(
        ingest.socket,
        "getaddrinfo",
        lambda *_args, **_kwargs: [
            (socket.AF_INET, socket.SOCK_STREAM, 6, "", ("127.0.0.1", 443))
        ],
    )
    with pytest.raises(ValueError, match="publicly routable"):
        list(ingest._fetch_url({"uri": "https://internal.test/secrets"}))


def test_fetch_url_revalidates_redirect_destination(monkeypatch):
    _install_http_transport(
        monkeypatch,
        lambda request: httpx.Response(
            302,
            headers={"location": "https://127.0.0.1/secrets"},
            request=request,
        ),
    )
    with pytest.raises(ValueError, match="publicly routable"):
        list(ingest._fetch_url({"uri": "https://example.test/doc.md"}))


def test_fetch_url_rejects_oversized_response(monkeypatch):
    monkeypatch.setenv("KOGNITION_INGEST_MAX_DOCUMENT_BYTES", "4")
    _install_http_transport(
        monkeypatch,
        lambda request: httpx.Response(200, content=b"12345", request=request),
    )
    with pytest.raises(ValueError, match="exceeds 4 bytes"):
        list(ingest._fetch_url({"uri": "https://example.test/doc.md"}))


def test_fetch_url_rejects_oversized_pdf_before_parsing(monkeypatch):
    monkeypatch.setenv("KOGNITION_INGEST_MAX_DOCUMENT_BYTES", "4")
    _install_http_transport(
        monkeypatch,
        lambda request: httpx.Response(200, content=b"12345", request=request),
    )
    parser_called = False

    def fail_if_called(_data, _uri):
        nonlocal parser_called
        parser_called = True
        raise AssertionError("oversized PDF reached the parser")

    monkeypatch.setattr(ingest, "_extract_text", fail_if_called)
    with pytest.raises(ValueError, match="exceeds 4 bytes"):
        list(ingest._fetch_url({"uri": "https://example.test/doc.pdf"}))
    assert parser_called is False


# --------------------------- _fetch_azure_blob (faked SDK) ------------------


class _FakeAzureDownloader:
    def __init__(self, chunks):
        self._chunks = chunks

    def chunks(self):
        yield from self._chunks


class _FakeAzureContainerClient:
    url = "https://account.blob.core.windows.net/kb"

    def list_blobs(self, name_starts_with):
        assert name_starts_with == "docs/"
        return [type("Blob", (), {"name": "docs/large.pdf", "size": 0})()]

    def download_blob(self, name):
        assert name == "docs/large.pdf"
        return _FakeAzureDownloader([b"12", b"345"])


def test_fetch_azure_blob_rejects_chunked_oversized_pdf(monkeypatch):
    from azure.storage.blob import ContainerClient

    monkeypatch.setenv("KOGNITION_INGEST_MAX_DOCUMENT_BYTES", "4")
    fake_client = _FakeAzureContainerClient()
    monkeypatch.setattr(
        ContainerClient,
        "from_connection_string",
        lambda *_args, **_kwargs: fake_client,
    )
    with pytest.raises(ValueError, match="exceeds 4 bytes"):
        list(
            ingest._fetch_azure_blob(
                {
                    "bucket": "kb",
                    "prefix": "docs/",
                    "credentials": {"connectionString": "AccountName=account"},
                }
            )
        )


# --------------------------- _fetch_git (faked git) -------------------------


def test_fetch_git_bounds_files_and_cleans_temporary_clone(monkeypatch):
    import subprocess

    monkeypatch.setenv("KOGNITION_INGEST_MAX_DOCUMENT_BYTES", "4")
    monkeypatch.setattr(
        socket,
        "getaddrinfo",
        lambda *_args, **_kwargs: [
            (socket.AF_INET, socket.SOCK_STREAM, 6, "", ("93.184.216.34", 443))
        ],
    )
    clone_directories = []
    commands = []

    def fake_run(command, **kwargs):
        commands.append(command)
        clone_directory = command[-1]
        clone_directories.append(clone_directory)
        with open(os.path.join(clone_directory, "large.pdf"), "wb") as document:
            document.write(b"12345")
        return subprocess.CompletedProcess(command, 0)

    monkeypatch.setattr(subprocess, "run", fake_run)
    with pytest.raises(ValueError, match="exceeds 4 bytes"):
        list(
            ingest._fetch_git(
                {"url": "https://example.test/repo.git", "branch": "release/v1"}
            )
        )

    assert commands[0][0:6] == [
        "git", "clone", "--depth", "1", "--branch", "release/v1"
    ]
    assert commands[0][6] == "--"
    assert all(not os.path.exists(path) for path in clone_directories)


# --------------------------- _fetch_s3_blob (faked boto3) --------------------
# Exercises prefix + include/exclude filtering and URI formatting end-to-end
# without touching AWS.


class _FakeBody:
    def __init__(self, data: bytes):
        self._data = data

    def read(self, size=-1) -> bytes:
        if not self._data:
            return b""
        if size < 0:
            size = len(self._data)
        chunk, self._data = self._data[:size], self._data[size:]
        return chunk


class _FakePaginator:
    def __init__(self, keys):
        self._keys = keys

    def paginate(self, Bucket, Prefix):  # noqa: N803 - boto3 kwarg names
        contents = [{"Key": k} for k in self._keys if k.startswith(Prefix)]
        yield {"Contents": contents}


class _FakeS3Client:
    def __init__(self, objects):
        self._objects = objects  # key -> bytes

    def get_paginator(self, _name):
        return _FakePaginator(list(self._objects))

    def get_object(self, Bucket, Key):  # noqa: N803 - boto3 kwarg names
        return {"Body": _FakeBody(self._objects[Key])}


def test_fetch_s3_blob_applies_prefix_and_filters(monkeypatch):
    import boto3

    objects = {
        "docs/readme.md": b"readme",
        "docs/guide.md": b"guide",
        "docs/logo.png": b"binary",          # excluded by include=*.md
        "docs/drafts/wip.md": b"wip",         # excluded by exclude pattern
        "other/skip.md": b"nope",             # excluded by prefix
    }
    monkeypatch.setattr(boto3, "client", lambda *a, **k: _FakeS3Client(objects))

    out = dict(
        (uri, text)
        for text, uri in ingest._fetch_s3_blob(
            {
                "type": "blob",
                "provider": "s3",
                "bucket": "kb",
                "prefix": "docs/",
                "include": ["*.md"],
                "exclude": ["docs/drafts/*"],
            }
        )
    )
    assert out == {
        "s3://kb/docs/readme.md": "readme",
        "s3://kb/docs/guide.md": "guide",
    }


def test_fetch_s3_blob_requires_bucket():
    with pytest.raises(ValueError, match="requires 'bucket'"):
        list(ingest._fetch_s3_blob({"type": "blob", "provider": "s3"}))


def test_fetch_s3_blob_rejects_private_custom_endpoint():
    with pytest.raises(ValueError, match="HTTPS|publicly routable"):
        list(
            ingest._fetch_s3_blob(
                {
                    "type": "blob",
                    "provider": "s3",
                    "bucket": "kb",
                    "endpoint": "http://127.0.0.1:9000",
                }
            )
        )


def test_fetch_s3_blob_rejects_oversized_object(monkeypatch):
    import boto3

    monkeypatch.setenv("KOGNITION_INGEST_MAX_DOCUMENT_BYTES", "4")
    monkeypatch.setattr(
        boto3,
        "client",
        lambda *a, **k: _FakeS3Client({"docs/large.md": b"12345"}),
    )
    with pytest.raises(ValueError, match="exceeds 4 bytes"):
        list(
            ingest._fetch_s3_blob(
                {"type": "blob", "provider": "s3", "bucket": "kb"}
            )
        )


# --------------------------- _azure_resource_root ----------------------------


@pytest.mark.parametrize(
    "endpoint,expected",
    [
        ("", ""),
        ("https://acct.openai.azure.com/openai", "https://acct.openai.azure.com"),
        (
            "https://acct.openai.azure.com/openai/deployments/x",
            "https://acct.openai.azure.com",
        ),
        (
            "https://res.services.ai.azure.com/api/projects/proj-default",
            "https://res.services.ai.azure.com",
        ),
        ("https://acct.openai.azure.com", "https://acct.openai.azure.com"),
        ("not-a-url", "not-a-url"),
    ],
)
def test_azure_resource_root(endpoint, expected):
    assert ingest._azure_resource_root(endpoint) == expected


# --------------------------- Embedder ----------------------------------------


class _FakeEmbeddingItem:
    def __init__(self, embedding):
        self.embedding = embedding


class _FakeEmbeddingResponse:
    def __init__(self, vectors):
        self.data = [_FakeEmbeddingItem(v) for v in vectors]


class _FakeEmbeddingsClient:
    def __init__(self, vectors):
        self._vectors = vectors

    @property
    def embeddings(self):
        return self

    def create(self, model, input):  # noqa: A002 - mirrors OpenAI SDK signature
        assert len(input) == len(self._vectors)
        return _FakeEmbeddingResponse(self._vectors)


def test_embedder_empty_input_returns_empty():
    emb = ingest.Embedder(client=object(), model="m", provider="openai", dimension=3)
    assert emb.embed([]) == []


def test_deterministic_embedder_is_stable_and_dimensioned():
    embedder = ingest._build_embedder({"provider": "deterministic", "model": "local"}, 8)
    first = embedder.embed(["same text", "different text"])
    second = embedder.embed(["same text"])
    assert first[0] == second[0]
    assert first[0] != first[1]
    assert len(first[0]) == 8
    assert all(-1.0 <= value <= 1.0 for value in first[0])


def test_embedder_returns_vectors_matching_dimension():
    client = _FakeEmbeddingsClient([[0.1, 0.2, 0.3], [0.4, 0.5, 0.6]])
    emb = ingest.Embedder(client=client, model="m", provider="openai", dimension=3)
    assert emb.embed(["a", "b"]) == [[0.1, 0.2, 0.3], [0.4, 0.5, 0.6]]


def test_embedder_rejects_dimension_mismatch():
    client = _FakeEmbeddingsClient([[0.1, 0.2]])  # dim 2
    emb = ingest.Embedder(client=client, model="m", provider="openai", dimension=3)
    with pytest.raises(ValueError, match="dimension mismatch"):
        emb.embed(["a"])


class _RecordingSink:
    def __init__(self):
        self.documents = []

    def replace_document(self, source_uri, chunks, embeddings, metadata):
        self.documents.append((source_uri, chunks, embeddings, metadata))


def test_ingest_sources_prepares_exact_transactional_records():
    task = {
        "chunking": {"size": 4, "overlap": 0, "mode": "fixed"},
        "sources": [
            {"type": "inline", "data": "abcdefgh", "uri": "inline://first"},
            {"type": "inline", "data": "   ", "uri": "inline://empty"},
        ],
    }
    sink = _RecordingSink()
    embedder = ingest._build_embedder(
        {"provider": "deterministic", "model": "local"}, 3
    )

    summary = ingest._ingest_sources(task, sink, embedder)

    assert summary == {
        "documents": 2,
        "chunks": 2,
        "sourceDigest": "sha256:" + hashlib.sha256(
            b"inline://first\0abcdefghinline://empty\0   "
        ).hexdigest(),
    }
    assert [(item[0], item[1], item[3]) for item in sink.documents] == [
        ("inline://first", ["abcd", "efgh"], {"sourceType": "inline", "provider": None}),
        ("inline://empty", [], {"sourceType": "inline", "provider": None}),
    ]
    assert len(sink.documents[0][2]) == 2
    assert all(len(vector) == 3 for vector in sink.documents[0][2])


def test_jsonl_chunk_sink_writes_atomic_bounded_contract(tmp_path):
    output = tmp_path / "chunks.jsonl"
    sink = ingest.JsonlChunkSink(str(output))

    sink.replace_document(
        "inline://first",
        ["alpha", "beta"],
        [[0.1, 0.2], [0.3, 0.4]],
        {"ignored": "provider protocol has no metadata field"},
    )
    sink.replace_document("inline://empty", [], [], {})
    summary = {
        "documents": 2,
        "chunks": 2,
        "sourceDigest": "sha256:" + "a" * 64,
    }
    sink.complete(summary)

    records = [json.loads(line) for line in output.read_text(encoding="utf-8").splitlines()]
    document_id = hashlib.sha256(b"inline://first").hexdigest()
    assert records == [
        {
            "type": "chunk",
            "documentId": document_id,
            "chunkId": "00000000",
            "content": "alpha",
            "sourceUri": "urn:kognition:source:inline:first",
            "location": "",
            "vector": [0.1, 0.2],
        },
        {
            "type": "chunk",
            "documentId": document_id,
            "chunkId": "00000001",
            "content": "beta",
            "sourceUri": "urn:kognition:source:inline:first",
            "location": "",
            "vector": [0.3, 0.4],
        },
        {
            "type": "summary",
            "sourceDocuments": 2,
            "indexedChunks": 2,
            "sourceDigest": summary["sourceDigest"],
        },
    ]
    assert not list(tmp_path.glob(".kognition-spool-*"))

@pytest.mark.parametrize(
    ("source", "expected"),
    [
        ("s3://product-docs/mark7/regions.md", "urn:kognition:source:s3:product-docs/mark7/regions.md"),
        ("https://docs.example.test/regions.md?sig=private-token#private", "https://docs.example.test/regions.md"),
    ],
)
def test_provider_spool_citations_are_safe_and_backend_neutral(tmp_path, source, expected):
    output = tmp_path / "chunks.jsonl"
    sink = ingest.JsonlChunkSink(str(output))
    sink.replace_document(source, ["fact"], [[1.0]], {})
    sink.complete({"documents": 1, "chunks": 1, "sourceDigest": "sha256:" + "a" * 64})
    chunk = json.loads(output.read_text(encoding="utf-8").splitlines()[0])
    assert chunk["sourceUri"] == expected
    assert "private-token" not in output.read_text(encoding="utf-8")


@pytest.mark.parametrize(
    "chunks,embeddings,error",
    [
        (["one"], [], "counts differ"),
        (["   "], [[0.1]], "chunk content"),
        (["one"], [[float("nan")]], "embedding"),
    ],
)
def test_jsonl_chunk_sink_rejects_malformed_records(tmp_path, chunks, embeddings, error):
    sink = ingest.JsonlChunkSink(str(tmp_path / "chunks.jsonl"))
    try:
        with pytest.raises(ValueError, match=error):
            sink.replace_document("inline://first", chunks, embeddings, {})
    finally:
        sink.abort()


def test_jsonl_chunk_sink_enforces_total_byte_limit_and_cleans_partial_file(tmp_path):
    output = tmp_path / "chunks.jsonl"
    sink = ingest.JsonlChunkSink(str(output), max_bytes=100, max_chunks=10)
    try:
        with pytest.raises(ValueError, match="byte limit"):
            sink.replace_document("inline://first", ["content"], [[0.1, 0.2]], {})
    finally:
        sink.abort()

    assert not output.exists()
    assert not list(tmp_path.glob(".kognition-spool-*"))


def test_ingest_task_provider_mode_completes_spool_before_secure_transport(
    monkeypatch, tmp_path
):
    spool = tmp_path / "chunks.jsonl"
    monkeypatch.setenv("KOGNITION_KNOWLEDGE_INDEX_SPOOL_FILE", str(spool))
    monkeypatch.setenv("KOGNITION_KNOWLEDGE_INDEXER_EXECUTABLE", "/trusted/indexer")
    monkeypatch.setattr(
        ingest,
        "_build_embedder",
        lambda config, dimension: object(),
    )
    summary = {
        "documents": 1,
        "chunks": 0,
        "sourceDigest": "sha256:" + "a" * 64,
    }
    monkeypatch.setattr(ingest, "_ingest_sources", lambda task, sink, embedder: summary)
    confirmation = {
        "documents": 1,
        "chunks": 0,
        "sourceDigest": summary["sourceDigest"],
        "backendBinding": {"providerName": "azuresearch"},
        "providerConfirmation": {"backendOperationId": "operation-1"},
    }

    def run(command, **kwargs):
        assert command == ["/trusted/indexer"]
        assert kwargs == {"check": True, "capture_output": True, "text": True}
        records = [json.loads(line) for line in spool.read_text().splitlines()]
        assert records == [
            {
                "type": "summary",
                "sourceDocuments": 1,
                "indexedChunks": 0,
                "sourceDigest": summary["sourceDigest"],
            }
        ]
        return type("Completed", (), {"stdout": json.dumps(confirmation)})()

    monkeypatch.setattr(ingest.subprocess, "run", run)
    result = ingest.ingest_task(
        {"kb": {"name": "kb"}, "providerIndex": {"dimension": 3}}
    )

    assert result == confirmation


def test_ingest_task_provider_mode_aborts_partial_spool_on_failure(
    monkeypatch, tmp_path
):
    spool = tmp_path / "chunks.jsonl"
    monkeypatch.setenv("KOGNITION_KNOWLEDGE_INDEX_SPOOL_FILE", str(spool))
    monkeypatch.setattr(ingest, "_build_embedder", lambda config, dimension: object())
    monkeypatch.setattr(
        ingest,
        "_ingest_sources",
        lambda task, sink, embedder: (_ for _ in ()).throw(RuntimeError("prepare failed")),
    )

    with pytest.raises(RuntimeError, match="prepare failed"):
        ingest.ingest_task(
            {"kb": {"name": "kb"}, "providerIndex": {"dimension": 3}}
        )

    assert not spool.exists()
    assert not list(tmp_path.glob(".kognition-spool-*"))


def test_ingest_task_rejects_multiple_index_backends():
    with pytest.raises(ValueError, match="mutually exclusive"):
        ingest.ingest_task(
            {
                "kb": {"name": "kb"},
                "providerIndex": {"dimension": 3},
                "vectorStore": {"dsn": "postgres://unused"},
            }
        )


# --------------------------- _build_embedder ---------------------------------


def test_build_embedder_defaults_to_openai():
    emb = ingest._build_embedder({"apiKey": "sk-test"}, dimension=1536)
    assert emb.provider == "openai"
    assert emb.model == "text-embedding-3-small"
    assert emb.dimension == 1536


def test_build_embedder_honors_provider_and_model():
    emb = ingest._build_embedder(
        {
            "provider": "azure_openai",
            "model": "text-embedding-3-large",
            "apiKey": "sk-test",
            "baseUrl": "https://acct.openai.azure.com/openai",
            "apiVersion": "2024-08-01-preview",
        },
        dimension=3072,
    )
    assert emb.provider == "azure_openai"
    assert emb.model == "text-embedding-3-large"
    assert emb.dimension == 3072


# --------------------------- _build_azure_credential -------------------------
# Azure credential selection (client-secret / workload-identity /
# managed-identity / default) for secretless auth on Azure.


def _patch_azure(monkeypatch):
    import azure.identity as ai

    class _Fake:
        def __init__(self, kind, **kwargs):
            self.kind = kind
            self.kwargs = kwargs

    monkeypatch.setattr(ai, "ClientSecretCredential", lambda **k: _Fake("client-secret", **k))
    monkeypatch.setattr(ai, "WorkloadIdentityCredential", lambda **k: _Fake("workload-identity", **k))
    monkeypatch.setattr(ai, "ManagedIdentityCredential", lambda **k: _Fake("managed-identity", **k))
    monkeypatch.setattr(ai, "DefaultAzureCredential", lambda **k: _Fake("default", **k))


def _clear_azure_env(monkeypatch):
    for k in (
        "AZURE_CREDENTIAL_TYPE",
        "AZURE_TENANT_ID",
        "AZURE_CLIENT_ID",
        "AZURE_CLIENT_SECRET",
        "AZURE_FEDERATED_TOKEN_FILE",
    ):
        monkeypatch.delenv(k, raising=False)


def test_ingester_credential_client_secret(monkeypatch):
    _patch_azure(monkeypatch)
    _clear_azure_env(monkeypatch)
    monkeypatch.setenv("AZURE_TENANT_ID", "t")
    monkeypatch.setenv("AZURE_CLIENT_ID", "c")
    monkeypatch.setenv("AZURE_CLIENT_SECRET", "s")
    assert ingest._build_azure_credential().kind == "client-secret"


def test_ingester_credential_default_when_no_secret(monkeypatch):
    _patch_azure(monkeypatch)
    _clear_azure_env(monkeypatch)
    assert ingest._build_azure_credential().kind == "default"


def test_ingester_credential_workload_identity(monkeypatch):
    _patch_azure(monkeypatch)
    _clear_azure_env(monkeypatch)
    monkeypatch.setenv("AZURE_CREDENTIAL_TYPE", "workload-identity")
    monkeypatch.setenv("AZURE_FEDERATED_TOKEN_FILE", "/t")
    cred = ingest._build_azure_credential()
    assert cred.kind == "workload-identity"
    assert cred.kwargs["token_file_path"] == "/t"


def test_ingester_credential_managed_identity(monkeypatch):
    _patch_azure(monkeypatch)
    _clear_azure_env(monkeypatch)
    monkeypatch.setenv("AZURE_CREDENTIAL_TYPE", "managed-identity")
    assert ingest._build_azure_credential().kind == "managed-identity"
