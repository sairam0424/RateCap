import urllib.parse
import urllib.request
from dataclasses import dataclass, field


@dataclass
class AllowResult:
    allowed: bool
    retry_after_ms: int = 0


@dataclass
class _Reservation:
    key: str
    token: str


class Ticket:
    def __init__(self, client, allowed, retry_after_ms=0, reservations=None):
        self.allowed = allowed
        self.retry_after_ms = retry_after_ms
        self._client = client
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

    def __enter__(self):
        return self

    def __exit__(self, exc_type, exc_val, exc_tb):
        self.release()
        return False


class Client:
    def __init__(self, sidecar_addr):
        self._sidecar_addr = sidecar_addr.rstrip("/")

    def allow(self, key):
        query = urllib.parse.urlencode({"key": key, "skip_reservations": "true"})
        url = f"{self._sidecar_addr}/check?{query}"
        req = urllib.request.Request(url, method="GET")
        try:
            with urllib.request.urlopen(req) as resp:
                return AllowResult(allowed=True)
        except urllib.error.HTTPError as err:
            retry_after_ms = int(err.headers.get("Retry-After-Ms", 0) or 0)
            return AllowResult(allowed=False, retry_after_ms=retry_after_ms)

    def acquire(self, key):
        query = urllib.parse.urlencode({"key": key})
        url = f"{self._sidecar_addr}/check?{query}"
        req = urllib.request.Request(url, method="GET")
        try:
            with urllib.request.urlopen(req) as resp:
                reservations = self._parse_reservations(resp.headers)
                return Ticket(self, allowed=True, reservations=reservations)
        except urllib.error.HTTPError as err:
            reservations = self._parse_reservations(err.headers)
            retry_after_ms = int(err.headers.get("Retry-After-Ms", 0) or 0)
            return Ticket(self, allowed=False, retry_after_ms=retry_after_ms, reservations=reservations)

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
        with urllib.request.urlopen(req) as resp:
            if resp.status != 200:
                raise RuntimeError(f"release failed with status {resp.status}")
