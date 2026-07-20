import os
import sys
import unittest

# unittest discover with -s <tests-dir> (no -t) treats tests/ as the top-level
# dir, so tests/__init__.py is never imported as a package init and can't put
# src/ on sys.path for us — this module is the only thing that runs before
# `from ratecap import Client` below, so the bootstrap has to live here.
_SRC_DIR = os.path.join(os.path.dirname(os.path.dirname(os.path.abspath(__file__))), "src")
if _SRC_DIR not in sys.path:
    sys.path.insert(0, _SRC_DIR)

from ratecap import Client

# `tests.fake_sidecar` only resolves when cwd is packages/sdks/python (cwd is
# implicitly on sys.path as ''), which breaks the repo-root invocation
# `-s packages/sdks/python/tests` where no `tests` package exists at cwd.
# unittest discover with no -t always puts start_dir itself on sys.path, so a
# bare import resolves identically from both invocations.
from fake_sidecar import FakeSidecar


class TestAllow(unittest.TestCase):
    def test_returns_true_on_200(self):
        with FakeSidecar(lambda method, path, query, headers: (200, {})) as sidecar:
            client = Client(sidecar.url)
            result = client.allow("user-1")
            self.assertTrue(result.allowed)

    def test_returns_false_with_retry_after_on_429(self):
        def handler(method, path, query, headers):
            return 429, {"Retry-After-Ms": "750"}

        with FakeSidecar(handler) as sidecar:
            client = Client(sidecar.url)
            result = client.allow("user-1")
            self.assertFalse(result.allowed)
            self.assertEqual(result.retry_after_ms, 750)

    def test_requests_skip_reservations(self):
        captured = {}

        def handler(method, path, query, headers):
            captured.update(query)
            return 200, {}

        with FakeSidecar(handler) as sidecar:
            client = Client(sidecar.url)
            client.allow("user-1")
            self.assertEqual(captured.get("skip_reservations"), "true")


class TestAcquire(unittest.TestCase):
    def test_acquire_returns_allowed_true_on_200(self):
        def handler(method, path, query, headers):
            if path == "/check":
                return 200, {"Concurrency-Token-0": "tok-abc", "Concurrency-Key-0": "user-1"}
            return 200, {}

        with FakeSidecar(handler) as sidecar:
            client = Client(sidecar.url)
            ticket = client.acquire("user-1")
            self.assertTrue(ticket.allowed)

    def test_acquire_does_not_send_skip_reservations(self):
        captured = {}

        def handler(method, path, query, headers):
            if path == "/check":
                captured.update(query)
                return 200, {}
            return 200, {}

        with FakeSidecar(handler) as sidecar:
            client = Client(sidecar.url)
            client.acquire("user-1")
            self.assertNotIn("skip_reservations", captured)

    def test_release_releases_every_reservation(self):
        release_calls = []

        def handler(method, path, query, headers):
            if path == "/check":
                return 200, {
                    "Concurrency-Token-0": "tok-abc",
                    "Concurrency-Key-0": "user-1",
                    "Concurrency-Token-1": "tok-xyz",
                    "Concurrency-Key-1": "fleet",
                }
            if path == "/release":
                release_calls.append(
                    {"key": headers.get("X-Ratecap-Concurrency-Key"), "token": headers.get("X-Ratecap-Concurrency-Token")}
                )
                return 200, {}
            return 404, {}

        with FakeSidecar(handler) as sidecar:
            client = Client(sidecar.url)
            ticket = client.acquire("user-1")
            ticket.release()

        self.assertEqual(len(release_calls), 2)
        by_key = {c["key"]: c["token"] for c in release_calls}
        self.assertEqual(by_key["user-1"], "tok-abc")
        self.assertEqual(by_key["fleet"], "tok-xyz")

    def test_release_reads_from_header_not_query(self):
        release_calls = []

        def handler(method, path, query, headers):
            if path == "/check":
                return 200, {"Concurrency-Token-0": "tok-abc", "Concurrency-Key-0": "user-1"}
            if path == "/release":
                release_calls.append({"query": dict(query), "header_key": headers.get("X-Ratecap-Concurrency-Key"), "header_token": headers.get("X-Ratecap-Concurrency-Token")})
                return 200, {}
            return 404, {}

        with FakeSidecar(handler) as sidecar:
            client = Client(sidecar.url)
            ticket = client.acquire("user-1")
            ticket.release()

        self.assertEqual(len(release_calls), 1)
        self.assertEqual(release_calls[0]["query"], {}, "expected /release to send nothing via the query string")
        self.assertEqual(release_calls[0]["header_key"], "user-1")
        self.assertEqual(release_calls[0]["header_token"], "tok-abc")

    def test_release_is_noop_when_no_token_was_issued(self):
        release_called = []

        def handler(method, path, query, headers):
            if path == "/release":
                release_called.append(True)
                return 200, {}
            return 429, {}

        with FakeSidecar(handler) as sidecar:
            client = Client(sidecar.url)
            ticket = client.acquire("user-1")
            ticket.release()

        self.assertEqual(release_called, [])

    def test_release_raises_when_a_reservation_fails_to_release(self):
        def handler(method, path, query, headers):
            if path == "/check":
                return 200, {"Concurrency-Token-0": "tok-abc", "Concurrency-Key-0": "user-1"}
            if path == "/release":
                return 500, {}
            return 404, {}

        with FakeSidecar(handler) as sidecar:
            client = Client(sidecar.url)
            ticket = client.acquire("user-1")
            with self.assertRaises(RuntimeError):
                ticket.release()

    def test_context_manager_auto_releases(self):
        release_calls = []

        def handler(method, path, query, headers):
            if path == "/check":
                return 200, {"Concurrency-Token-0": "tok-abc", "Concurrency-Key-0": "user-1"}
            if path == "/release":
                release_calls.append(dict(query))
                return 200, {}
            return 404, {}

        with FakeSidecar(handler) as sidecar:
            client = Client(sidecar.url)
            with client.acquire("user-1") as ticket:
                self.assertTrue(ticket.allowed)

        self.assertEqual(len(release_calls), 1)


if __name__ == "__main__":
    unittest.main()
