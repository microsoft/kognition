# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

import json
import tempfile
import threading
import unittest
import urllib.error
import urllib.request
from concurrent.futures import ThreadPoolExecutor
from pathlib import Path

from publication_server import create_server


class PublicationTests(unittest.TestCase):
    def setUp(self):
        self.directory = tempfile.TemporaryDirectory()
        self.addCleanup(self.directory.cleanup)
        self.database = str(Path(self.directory.name) / "publications.db")
        self.start_server()
        self.addCleanup(self.stop_server)

    def start_server(self):
        self.server = create_server(("127.0.0.1", 0), self.database)
        self.thread = threading.Thread(target=self.server.serve_forever)
        self.thread.start()
        self.url = f"http://127.0.0.1:{self.server.server_port}"

    def stop_server(self):
        self.server.shutdown()
        self.server.server_close()
        self.thread.join()

    def request(self, path, body=None):
        request = urllib.request.Request(
            self.url + path, data=json.dumps(body).encode() if body is not None else None,
            headers={"Content-Type": "application/json"},
        )
        try:
            response = urllib.request.urlopen(request, timeout=15)
        except urllib.error.HTTPError as error:
            response = error
        with response:
            return response.status, json.load(response)

    def test_publication_persists_and_replay_cannot_change_content(self):
        body = {"runID": "recording-1", "content": "Approved customer advisory"}
        self.assertEqual(self.request("/publications/recording-1")[1]["publicationCount"], 0)
        status, first = self.request("/publish", body)
        self.assertEqual(status, 200)
        self.assertFalse(first["replayed"])
        self.stop_server()
        self.start_server()
        status, replay = self.request("/publish", body)
        self.assertEqual(status, 200)
        self.assertTrue(replay["replayed"])
        self.assertEqual(first["publishedAt"], replay["publishedAt"])
        self.assertEqual(self.request("/publish", {**body, "content": "changed"})[0], 409)
        status, persisted = self.request("/publications/recording-1")
        self.assertEqual(status, 200)
        self.assertEqual(persisted["publicationCount"], 1)
        self.assertEqual(persisted["content"], body["content"])

    def test_concurrent_retries_publish_only_once(self):
        body = {"runID": "concurrent", "content": "Approved advisory"}
        with ThreadPoolExecutor(max_workers=8) as executor:
            responses = list(executor.map(lambda _: self.request("/publish", body), range(16)))
        self.assertTrue(all(status == 200 for status, _ in responses))
        self.assertEqual(sum(not receipt["replayed"] for _, receipt in responses), 1)
        self.assertEqual(len({receipt["publishedAt"] for _, receipt in responses}), 1)

    def test_invalid_requests_do_not_publish(self):
        for body in ([], {}, {"runID": "../escape", "content": "advisory"},
                     {"runID": "valid", "content": ""}, {"runID": "valid", "content": 1}):
            with self.subTest(body=body):
                self.assertEqual(self.request("/publish", body)[0], 400)
        self.assertEqual(self.request("/publications/valid")[1]["publicationCount"], 0)


if __name__ == "__main__":
    unittest.main()