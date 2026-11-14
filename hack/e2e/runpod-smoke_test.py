"""Exercise the smoke script's cleanup helpers without Kubernetes or paid GPUs."""

import http.server
import json
import os
from pathlib import Path
import subprocess
import tempfile
import threading
import unittest
import urllib.parse


SCRIPT = Path(__file__).with_name("runpod-smoke.sh").resolve()


class SmokeHelpersTest(unittest.TestCase):
    def setUp(self):
        self.requests = []
        self.responses = []
        owner = self

        class Handler(http.server.BaseHTTPRequestHandler):
            def do_GET(self):
                owner.requests.append((self.command, self.path))
                status, body = owner.responses.pop(0) if owner.responses else (500, {})
                self.send_response(status)
                self.end_headers()
                self.wfile.write(json.dumps(body).encode())

            do_DELETE = do_GET

            def log_message(self, *_):
                pass

        self.server = http.server.ThreadingHTTPServer(("127.0.0.1", 0), Handler)
        self.thread = threading.Thread(target=self.server.serve_forever, daemon=True)
        self.thread.start()

    def tearDown(self):
        self.server.shutdown()
        self.server.server_close()
        self.thread.join()

    def run_helper(self, command, extra_env=None):
        env = dict(os.environ, RUNPOD_API_KEY="test-token",
                   RUNPOD_BASE_URL=f"http://127.0.0.1:{self.server.server_port}/v2")
        env.update(extra_env or {})
        return subprocess.run(
            ["bash", "-c", 'source "$1"; ' + command, "test", str(SCRIPT)],
            env=env, text=True, capture_output=True, timeout=10,
        )

    def test_name_filter_uses_every_page(self):
        self.responses = [
            (200, {"pods": [{"id": "unrelated", "name": "other"}],
                   "pagination": {"hasNextPage": True, "nextCursor": "next+/="}}),
            (200, {"pods": [{"id": "expected", "name": "target", "status": "RUNNING"}],
                   "pagination": {"hasNextPage": False, "nextCursor": None}}),
        ]
        result = self.run_helper('runpod_list_by_name target')
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(result.stdout, "expected\n")
        self.assertEqual(self.requests[0], ("GET", "/v2/pods"))
        self.assertEqual(urllib.parse.parse_qs(urllib.parse.urlsplit(self.requests[1][1]).query),
                         {"cursor": ["next+/="]})

    def test_bad_second_page_does_not_emit_partial_matches(self):
        self.responses = [
            (200, {"pods": [{"id": "partial", "name": "target"}],
                   "pagination": {"hasNextPage": True, "nextCursor": "next"}}),
            (502, {}),
        ]
        result = self.run_helper('runpod_list_by_name target')
        self.assertNotEqual(result.returncode, 0)
        self.assertEqual(result.stdout, "")

    def test_repeated_cursor_fails(self):
        page = {"pods": [], "pagination": {"hasNextPage": True, "nextCursor": "same"}}
        self.responses = [(200, page), (200, page)]
        self.assertNotEqual(self.run_helper('runpod_list_by_name target').returncode, 0)
        self.assertEqual(len(self.requests), 2)

    def test_delete_confirms_absence(self):
        self.responses = [(204, {}), (404, {})]
        result = self.run_helper('delete_runpod_by_id test-pod')
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(self.requests, [("DELETE", "/v2/pods/test-pod"), ("GET", "/v2/pods/test-pod")])

    def test_delete_failure_is_not_success(self):
        self.responses = [(403, {})]
        self.assertNotEqual(self.run_helper('delete_runpod_by_id test-pod').returncode, 0)

    def test_orphan_lookup_failure_fails(self):
        self.responses = [(401, {})]
        result = self.run_helper('EXTERNAL_NAMES=(target); final_orphan_check')
        self.assertNotEqual(result.returncode, 0)

    def test_binding_cleanup_wait(self):
        for mode in ("delayed", "timeout", "api-error"):
            with self.subTest(mode=mode), tempfile.TemporaryDirectory() as directory:
                kubectl = Path(directory) / "kubectl"
                counter = Path(directory) / "calls"
                kubectl.write_text('''#!/usr/bin/env python3
import os
from pathlib import Path
import sys

if "delete" in sys.argv:
    sys.exit(0)
counter = Path(os.environ["TEST_COUNTER"])
calls = int(counter.read_text()) + 1 if counter.exists() else 1
counter.write_text(str(calls))
mode = os.environ["TEST_MODE"]
if mode == "api-error":
    sys.exit(1)
if mode == "timeout" or calls < 3:
    print("test-binding")
''')
                kubectl.chmod(0o755)
                result = self.run_helper(
                    'sleep() { SECONDS=$((SECONDS + 45)); }; '
                    'delete_pod_and_verify_cleanup test-pod',
                    {"KUBECTL": str(kubectl), "TEST_COUNTER": str(counter), "TEST_MODE": mode},
                )
                if mode == "delayed":
                    self.assertEqual(result.returncode, 0, result.stderr)
                    self.assertEqual(counter.read_text(), "3")
                else:
                    self.assertNotEqual(result.returncode, 0)
                    self.assertIn("bindings" if mode == "api-error" else "still exists", result.stderr)

    def test_preflight_stops_on_lookup_failure(self):
        for check in ("apply_control_plane_prereqs", "check_test_resources_absent"):
            with self.subTest(check=check):
                result = self.run_helper(
                    'broken_kubectl() { return 1; }; KUBECTL=broken_kubectl; ' + check,
                )
                self.assertNotEqual(result.returncode, 0)
                self.assertIn("could not check", result.stderr)


if __name__ == "__main__":
    unittest.main()
