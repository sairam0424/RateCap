import threading
from http.server import BaseHTTPRequestHandler, HTTPServer
from urllib.parse import parse_qs, urlparse


class FakeSidecar:
    def __init__(self, handler):
        self._handler = handler
        self.requests = []
        server = self

        class _Handler(BaseHTTPRequestHandler):
            def log_message(self, *args):
                pass

            def do_GET(self):
                self._dispatch()

            def do_POST(self):
                self._dispatch()

            def _dispatch(self):
                parsed = urlparse(self.path)
                query = {k: v[0] for k, v in parse_qs(parsed.query).items()}
                server.requests.append((self.command, parsed.path, query))
                status, headers = server._handler(self.command, parsed.path, query)
                self.send_response(status)
                for key, value in headers.items():
                    self.send_header(key, value)
                self.end_headers()

        self._httpd = HTTPServer(("127.0.0.1", 0), _Handler)
        self._thread = threading.Thread(target=self._httpd.serve_forever, daemon=True)

    @property
    def url(self):
        host, port = self._httpd.server_address
        return f"http://{host}:{port}"

    def __enter__(self):
        self._thread.start()
        return self

    def __exit__(self, *exc):
        self._httpd.shutdown()
        self._httpd.server_close()
