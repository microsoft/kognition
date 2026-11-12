# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

from __future__ import annotations

import importlib.util
import io
import os
import tempfile
import unittest
from contextlib import redirect_stderr, redirect_stdout
from pathlib import Path
from unittest.mock import patch

from botocore.exceptions import ClientError

_SPEC = importlib.util.spec_from_file_location("s3_upload_docs", Path(__file__).with_name("upload_docs.py"))
assert _SPEC is not None and _SPEC.loader is not None
upload_docs = importlib.util.module_from_spec(_SPEC)
_SPEC.loader.exec_module(upload_docs)


class S3Store:
    def __init__(self, *, deny_create: bool = False, corrupt: bool = False) -> None:
        self.objects: dict[str, bytes] = {}
        self.deny_create = deny_create
        self.corrupt = corrupt

    def create_bucket(self, **kwargs: object) -> None:
        if self.deny_create:
            raise ClientError(
                {"Error": {"Code": "AccessDenied"}, "ResponseMetadata": {"HTTPStatusCode": 403}},
                "CreateBucket",
            )

    def put_object(self, *, Bucket: str, Key: str, Body: bytes) -> None:
        self.objects[Key] = Body

    def get_paginator(self, name: str) -> S3Store:
        if name != "list_objects_v2":
            raise AssertionError(f"Unsupported S3 operation: {name}")
        return self

    def paginate(self, *, Bucket: str, Prefix: str) -> list[dict[str, object]]:
        return [{"Contents": [{"Key": key} for key in sorted(self.objects) if key.startswith(Prefix)]}]

    def get_object(self, *, Bucket: str, Key: str) -> dict[str, io.BytesIO]:
        body = b"corrupted" if self.corrupt else self.objects[Key]
        return {"Body": io.BytesIO(body)}


class UploadDocsTests(unittest.TestCase):
    def invoke(self, environment: dict[str, str], store: S3Store) -> tuple[int, str]:
        output = io.StringIO()
        with (
            patch.dict(os.environ, environment, clear=True),
            patch("boto3.client", return_value=store),
            redirect_stdout(output),
            redirect_stderr(output),
        ):
            result = upload_docs.main()
        return result, output.getvalue()

    def test_missing_endpoint_is_a_failure(self) -> None:
        result, output = self.invoke({}, S3Store())
        self.assertNotEqual(result, 0)
        self.assertIn("KOGNITION_S3_ENDPOINT", output)

    def test_empty_input_is_a_failure(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            result, output = self.invoke(
                {"KOGNITION_S3_ENDPOINT": "http://127.0.0.1:9000", "KOGNITION_DOCS_DIR": directory},
                S3Store(),
            )
        self.assertNotEqual(result, 0)
        self.assertIn("no .md/.txt", output)

    def test_bucket_denial_is_not_ignored(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            Path(directory, "note.md").write_text("known document", encoding="utf-8")
            store = S3Store(deny_create=True)
            result, output = self.invoke(
                {"KOGNITION_S3_ENDPOINT": "http://127.0.0.1:9000", "KOGNITION_DOCS_DIR": directory},
                store,
            )
        self.assertNotEqual(result, 0)
        self.assertEqual(store.objects, {})
        self.assertIn("AccessDenied", output)

    def test_success_verifies_the_uploaded_content(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            Path(directory, "note.md").write_text("known document", encoding="utf-8")
            store = S3Store()
            result, output = self.invoke(
                {"KOGNITION_S3_ENDPOINT": "http://127.0.0.1:9000", "KOGNITION_DOCS_DIR": directory},
                store,
            )
        self.assertEqual(result, 0, output)
        self.assertEqual(store.objects, {"mark7/note.md": b"known document"})
        self.assertIn("Verified 1", output)

    def test_corrupted_readback_is_a_failure(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            Path(directory, "note.md").write_text("known document", encoding="utf-8")
            result, output = self.invoke(
                {"KOGNITION_S3_ENDPOINT": "http://127.0.0.1:9000", "KOGNITION_DOCS_DIR": directory},
                S3Store(corrupt=True),
            )
        self.assertNotEqual(result, 0)
        self.assertIn("verification", output.lower())


if __name__ == "__main__":
    unittest.main()
