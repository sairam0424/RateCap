import os
import sys
import unittest

# unittest discover with -s <tests-dir> (no -t) treats tests/ as the top-level
# dir, so tests/__init__.py is never imported as a package init and can't put
# src/ on sys.path for us — this module is the only thing that runs before
# `from ratecap import Client` below, so the bootstrap has to live here.
_SRC_DIR = os.path.join(
    os.path.dirname(os.path.dirname(os.path.abspath(__file__))), "src"
)
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

    def test_allow_returns_rate_limit_limit_and_remaining_on_429(self):
        def handler(method, path, query, headers):
            return 429, {
                "Retry-After-Ms": "750",
                "RateLimit-Limit": "500",
                "RateLimit-Remaining": "0",
            }

        with FakeSidecar(handler) as sidecar:
            client = Client(sidecar.url)
            result = client.allow("user-1")
            self.assertFalse(result.allowed)
            self.assertEqual(result.rate_limit_limit, 500)
            self.assertEqual(result.rate_limit_remaining, 0)

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
                return 200, {
                    "Concurrency-Token-0": "tok-abc",
                    "Concurrency-Key-0": "user-1",
                }
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

    def test_acquire_returns_rate_limit_limit_and_remaining_on_429(self):
        def handler(method, path, query, headers):
            return 429, {
                "Retry-After-Ms": "750",
                "RateLimit-Limit": "500",
                "RateLimit-Remaining": "0",
            }

        with FakeSidecar(handler) as sidecar:
            client = Client(sidecar.url)
            ticket = client.acquire("user-1")
            self.assertFalse(ticket.allowed)
            self.assertEqual(ticket.rate_limit_limit, 500)
            self.assertEqual(ticket.rate_limit_remaining, 0)

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
                    {
                        "key": headers.get("X-Ratecap-Concurrency-Key"),
                        "token": headers.get("X-Ratecap-Concurrency-Token"),
                    }
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
                return 200, {
                    "Concurrency-Token-0": "tok-abc",
                    "Concurrency-Key-0": "user-1",
                }
            if path == "/release":
                release_calls.append(
                    {
                        "query": dict(query),
                        "header_key": headers.get("X-Ratecap-Concurrency-Key"),
                        "header_token": headers.get("X-Ratecap-Concurrency-Token"),
                    }
                )
                return 200, {}
            return 404, {}

        with FakeSidecar(handler) as sidecar:
            client = Client(sidecar.url)
            ticket = client.acquire("user-1")
            ticket.release()

        self.assertEqual(len(release_calls), 1)
        self.assertEqual(
            release_calls[0]["query"],
            {},
            "expected /release to send nothing via the query string",
        )
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
                return 200, {
                    "Concurrency-Token-0": "tok-abc",
                    "Concurrency-Key-0": "user-1",
                }
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
                return 200, {
                    "Concurrency-Token-0": "tok-abc",
                    "Concurrency-Key-0": "user-1",
                }
            if path == "/release":
                release_calls.append(dict(query))
                return 200, {}
            return 404, {}

        with FakeSidecar(handler) as sidecar:
            client = Client(sidecar.url)
            with client.acquire("user-1") as ticket:
                self.assertTrue(ticket.allowed)

        self.assertEqual(len(release_calls), 1)


class TestCostAndPriority(unittest.TestCase):
    def test_allow_sends_cost_query_param_when_given(self):
        captured = {}

        def handler(method, path, query, headers):
            captured.update(query)
            return 200, {}

        with FakeSidecar(handler) as sidecar:
            client = Client(sidecar.url)
            client.allow("user-1", cost=5)
            self.assertEqual(captured.get("cost"), "5")

    def test_allow_omits_cost_query_param_by_default(self):
        captured = {}

        def handler(method, path, query, headers):
            captured.update(query)
            return 200, {}

        with FakeSidecar(handler) as sidecar:
            client = Client(sidecar.url)
            client.allow("user-1")
            self.assertNotIn("cost", captured)

    def test_allow_sends_priority_header_when_given(self):
        captured = {}

        def handler(method, path, query, headers):
            captured.update(headers)
            return 200, {}

        with FakeSidecar(handler) as sidecar:
            client = Client(sidecar.url)
            client.allow("user-1", priority="critical")
            # Python's http.server.HTTPServer normalizes header names via
            # urllib's AbstractHTTPHandler.do_open (str.title() at every
            # hyphen boundary) regardless of the casing passed to Request(),
            # matching this file's own established convention of asserting
            # "X-Ratecap-Concurrency-Key" rather than "X-RateCap-...".
            self.assertEqual(captured.get("X-Ratecap-Priority"), "critical")

    def test_allow_sends_route_header_when_given(self):
        captured = {}

        def handler(method, path, query, headers):
            captured.update(headers)
            return 200, {}

        with FakeSidecar(handler) as sidecar:
            client = Client(sidecar.url)
            client.allow("user-1", route="POST /v1/charges")
            self.assertEqual(captured.get("X-Ratecap-Route"), "POST /v1/charges")

    def test_acquire_sends_route_header_when_given(self):
        captured_headers = {}

        def handler(method, path, query, headers):
            if path == "/check":
                captured_headers.update(headers)
            return 200, {}

        with FakeSidecar(handler) as sidecar:
            client = Client(sidecar.url)
            client.acquire("user-1", route="POST /v1/charges")
            self.assertEqual(
                captured_headers.get("X-Ratecap-Route"), "POST /v1/charges"
            )

    def test_acquire_sends_cost_and_priority(self):
        captured_query = {}
        captured_headers = {}

        def handler(method, path, query, headers):
            if path == "/check":
                captured_query.update(query)
                captured_headers.update(headers)
            return 200, {}

        with FakeSidecar(handler) as sidecar:
            client = Client(sidecar.url)
            client.acquire("user-1", cost=1500, priority="critical")
            self.assertEqual(captured_query.get("cost"), "1500")
            self.assertEqual(captured_headers.get("X-Ratecap-Priority"), "critical")


class TestRefund(unittest.TestCase):
    def test_refund_sends_refund_headers(self):
        refund_calls = []

        def handler(method, path, query, headers):
            if path == "/check":
                return 200, {}
            if path == "/release":
                refund_calls.append(dict(headers))
                return 200, {}
            return 404, {}

        with FakeSidecar(handler) as sidecar:
            client = Client(sidecar.url)
            ticket = client.acquire("user-1", cost=1500)
            ticket.refund(1200)

        self.assertEqual(len(refund_calls), 1)
        self.assertEqual(refund_calls[0].get("X-Ratecap-Refund-Key"), "user-1")
        self.assertEqual(refund_calls[0].get("X-Ratecap-Refund-Amount"), "1200")

    def test_refund_raises_on_non_200(self):
        def handler(method, path, query, headers):
            if path == "/check":
                return 200, {}
            if path == "/release":
                return 500, {}
            return 404, {}

        with FakeSidecar(handler) as sidecar:
            client = Client(sidecar.url)
            ticket = client.acquire("user-1", cost=1500)
            with self.assertRaises(RuntimeError):
                ticket.refund(1200)


class TestTimeout(unittest.TestCase):
    def test_default_timeout_is_applied(self):
        def handler(method, path, query, headers):
            return 200, {}

        with FakeSidecar(handler) as sidecar:
            client = Client(sidecar.url)
            # No direct way to observe urllib's internal timeout without a
            # slow server; assert the attribute is set to a sane positive
            # default instead, and that a custom value overrides it.
            self.assertGreater(client._timeout, 0)

    def test_custom_timeout_is_stored(self):
        client = Client("http://localhost:8080", timeout=2.5)
        self.assertEqual(client._timeout, 2.5)


class TestRetry(unittest.TestCase):
    def test_retries_on_connection_error_up_to_max_retries(self):
        attempts = []

        def handler(method, path, query, headers):
            attempts.append(1)
            if len(attempts) < 3:
                raise ConnectionResetError("simulated transient failure")
            return 200, {}

        with FakeSidecar(handler) as sidecar:
            client = Client(sidecar.url, max_retries=3, backoff_base=0.01)
            result = client.allow("user-1")
            self.assertTrue(result.allowed)
        self.assertEqual(len(attempts), 3)

    def test_no_retry_by_default(self):
        attempts = []

        def handler(method, path, query, headers):
            attempts.append(1)
            raise ConnectionResetError("simulated failure")

        with FakeSidecar(handler) as sidecar:
            client = Client(sidecar.url)
            with self.assertRaises(Exception):
                client.allow("user-1")
        self.assertEqual(len(attempts), 1)

    def test_gives_up_after_max_retries_exhausted(self):
        attempts = []

        def handler(method, path, query, headers):
            attempts.append(1)
            raise ConnectionResetError("simulated permanent failure")

        with FakeSidecar(handler) as sidecar:
            client = Client(sidecar.url, max_retries=2, backoff_base=0.01)
            with self.assertRaises(Exception):
                client.allow("user-1")
        self.assertEqual(len(attempts), 3)  # 1 initial + 2 retries


class TestTLS(unittest.TestCase):
    def test_ca_file_none_uses_default_context(self):
        client = Client("https://localhost:8443")
        self.assertIsNone(client._ssl_context)

    def test_ca_file_builds_custom_ssl_context(self):
        import ssl
        import tempfile

        with tempfile.NamedTemporaryFile(suffix=".pem", delete=False) as f:
            # A syntactically-plausible-but-fake CA file is enough to prove
            # the client attempts to build a context from it; a real TLS
            # handshake against it is exercised by the sidecar/core's own
            # existing mTLS integration tests, not this SDK's unit suite.
            f.write(b"-----BEGIN CERTIFICATE-----\nMIIB\n-----END CERTIFICATE-----\n")
            ca_path = f.name

        try:
            with self.assertRaises(ssl.SSLError):
                Client("https://localhost:8443", ca_file=ca_path)
        finally:
            import os

            os.unlink(ca_path)


if __name__ == "__main__":
    unittest.main()
