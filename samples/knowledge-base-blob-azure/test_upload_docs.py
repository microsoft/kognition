# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

import importlib.util
import os
from pathlib import Path
import sys
import types
import unittest
from unittest.mock import MagicMock, patch


spec = importlib.util.spec_from_file_location("azure_sample_upload", Path(__file__).with_name("upload_docs.py"))
upload = importlib.util.module_from_spec(spec)
spec.loader.exec_module(upload)


class GovernedUploadTests(unittest.TestCase):
    def test_empty_corpus_fails_in_governed_profile(self):
        with patch.dict(os.environ, {"KOGNITION_SAMPLE_PROFILE_PATH": "private-profile.json"}), patch.object(upload, "_resolve_files", return_value=[]):
            self.assertEqual(upload.main(), 1)

    def test_missing_sdk_fails_in_governed_profile(self):
        with patch.dict(os.environ, {"KOGNITION_SAMPLE_PROFILE_PATH": "private-profile.json"}), patch.object(upload, "_resolve_files", return_value=["product.md"]), patch.dict(sys.modules, {"azure.storage.blob": None}):
            self.assertEqual(upload.main(), 1)

    def run_upload(self, downloaded):
        client = MagicMock()
        client.list_blobs.return_value = [types.SimpleNamespace(name="mark7/product.md")]
        blob = client.get_blob_client.return_value
        blob.download_blob.return_value.readall.return_value = downloaded
        module = types.ModuleType("azure.storage.blob")
        module.ContainerClient = MagicMock()
        module.ContainerClient.from_connection_string.return_value = client
        with patch.dict(os.environ, {
            "KOGNITION_SAMPLE_PROFILE_PATH": "private-profile.json",
            "KOGNITION_BLOB_CONNECTION_STRING": "fake-local-fixture",
            "KOGNITION_BLOB_PREFIX": "mark7",
        }), patch.object(upload, "_resolve_files", return_value=["product.md"]), patch.object(Path, "read_bytes", return_value=b"source"), patch("builtins.open", unittest.mock.mock_open(read_data=b"source")), patch.dict(sys.modules, {"azure.storage.blob": module}):
            return upload.main()

    def test_corrupted_round_trip_fails(self):
        self.assertEqual(self.run_upload(b"wrong"), 1)

    def test_verified_round_trip_succeeds(self):
        self.assertEqual(self.run_upload(b"source"), 0)

    def test_legacy_empty_corpus_retains_skip(self):
        with patch.dict(os.environ, {}, clear=True), patch.object(upload, "_resolve_files", return_value=[]):
            self.assertEqual(upload.main(), 0)


if __name__ == "__main__":
    unittest.main()
