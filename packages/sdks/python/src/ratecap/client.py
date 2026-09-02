import ssl
import time
import urllib.parse
import urllib.request
from dataclasses import dataclass, field


@dataclass
class AllowResult:
    allowed: bool
    retry_after_ms: int = 0
    rate_limit_limit: int = 0
    rate_limit_remaining: int = 0


@dataclass
class _Reservation:
    key: str
    token: str


class Ticket:
    def __init__(
        self,
        client,
        key,
        allowed,
        retry_after_ms=0,
        rate_limit_limit=0,
        rate_limit_remaining=0,
        reservations=None,
    ):
        self.allowed = allowed
        self.retry_after_ms = retry_after_ms
        self.rate_limit_limit = rate_limit_limit
        self.rate_limit_remaining = rate_limit_remaining
        self._client = client
        self._key = key
        self._reservations = reservations or []

    def release(self):
        errors = []
        for reservation in self._reservations:
            try:
                self._client._release_one(reservation)
            except Exception as exc:
                errors.append(f"{reservation.key}: {exc}")
        if errors:
            raise RuntimeError("failed to release reservation(s): " + "; ".join(errors))

    def refund(self, refund_amount):
        url = f"{self._client._sidecar_addr}/release"
        req = urllib.request.Request(
            url,
            method="POST",
            headers={
                "X-RateCap-Refund-Key": self._key,
                "X-RateCap-Refund-Amount": str(refund_amount),
            },
        )
        try:
            with self._client._urlopen(req) as resp:
                if resp.status != 200:
                    raise RuntimeError(f"refund failed with status {resp.status}")
        except urllib.error.HTTPError as err:
            raise RuntimeError(f"refund failed with status {err.code}") from err

    def __enter__(self):
        return self

    def __exit__(self, exc_type, exc_val, exc_tb):
        self.release()
        return False


class Client:
    def __init__(
        self, sidecar_addr, timeout=5.0, max_retries=0, backoff_base=0.1, ca_file=None
    ):
        self._sidecar_addr = sidecar_addr.rstrip("/")
        self._timeout = timeout
        self._max_retries = max_retries
        self._backoff_base = backoff_base
        self._ssl_context = None
        if ca_file is not None:
            self._ssl_context = ssl.create_default_context(cafile=ca_file)

    def _urlopen(self, req):
        attempt = 0
        while True:
            try:
                kwargs = {"timeout": self._timeout}
                if self._ssl_context is not None:
                    kwargs["context"] = self._ssl_context
                return urllib.request.urlopen(req, **kwargs)
            except urllib.error.HTTPError:
                raise
            except Exception:
                if attempt >= self._max_retries:
                    raise
                time.sleep(self._backoff_base * (2**attempt))
                attempt += 1

    def allow(self, key, cost=None, priority=None, route=None):
        params = {"key": key, "skip_reservations": "true"}
        if cost is not None:
            params["cost"] = str(cost)
        query = urllib.parse.urlencode(params)
        url = f"{self._sidecar_addr}/check?{query}"
        headers = {}
        if priority:
            headers["x-ratecap-priority"] = priority
        if route:
            headers["x-ratecap-route"] = route
        req = urllib.request.Request(url, method="GET", headers=headers)
        try:
            with self._urlopen(req) as resp:
                return AllowResult(allowed=True)
        except urllib.error.HTTPError as err:
            retry_after_ms = int(err.headers.get("Retry-After-Ms", 0) or 0)
            rate_limit_limit = int(err.headers.get("RateLimit-Limit", 0) or 0)
            rate_limit_remaining = int(err.headers.get("RateLimit-Remaining", 0) or 0)
            return AllowResult(
                allowed=False,
                retry_after_ms=retry_after_ms,
                rate_limit_limit=rate_limit_limit,
                rate_limit_remaining=rate_limit_remaining,
            )

    def acquire(self, key, cost=None, priority=None, route=None):
        params = {"key": key}
        if cost is not None:
            params["cost"] = str(cost)
        query = urllib.parse.urlencode(params)
        url = f"{self._sidecar_addr}/check?{query}"
        headers = {}
        if priority:
            headers["x-ratecap-priority"] = priority
        if route:
            headers["x-ratecap-route"] = route
        req = urllib.request.Request(url, method="GET", headers=headers)
        try:
            with self._urlopen(req) as resp:
                reservations = self._parse_reservations(resp.headers)
                return Ticket(self, key, allowed=True, reservations=reservations)
        except urllib.error.HTTPError as err:
            reservations = self._parse_reservations(err.headers)
            retry_after_ms = int(err.headers.get("Retry-After-Ms", 0) or 0)
            rate_limit_limit = int(err.headers.get("RateLimit-Limit", 0) or 0)
            rate_limit_remaining = int(err.headers.get("RateLimit-Remaining", 0) or 0)
            return Ticket(
                self,
                key,
                allowed=False,
                retry_after_ms=retry_after_ms,
                rate_limit_limit=rate_limit_limit,
                rate_limit_remaining=rate_limit_remaining,
                reservations=reservations,
            )

    def _parse_reservations(self, headers):
        reservations = []
        i = 0
        while True:
            token = headers.get(f"Concurrency-Token-{i}")
            if not token:
                break
            key = headers.get(f"Concurrency-Key-{i}", "")
            reservations.append(_Reservation(key=key, token=token))
            i += 1
        return reservations

    def _release_one(self, reservation):
        url = f"{self._sidecar_addr}/release"
        req = urllib.request.Request(
            url,
            method="POST",
            headers={
                "X-RateCap-Concurrency-Key": reservation.key,
                "X-RateCap-Concurrency-Token": reservation.token,
            },
        )
        with self._urlopen(req) as resp:
            if resp.status != 200:
                raise RuntimeError(f"release failed with status {resp.status}")
