"""A stand-in Bot API for the image smoke test: getMe names a bot and
getUpdates returns nothing, so the service polls and reports healthy."""

import http.server
import json
import sys


class Handler(http.server.BaseHTTPRequestHandler):
    def do_POST(self):
        self.rfile.read(int(self.headers.get("Content-Length", 0)))
        method = self.path.rsplit("/", 1)[-1]
        if method == "getMe":
            result = {"id": 1, "is_bot": True, "first_name": "Smoke", "username": "SmokeBot"}
        elif method == "getUpdates":
            result = []
        else:
            result = True
        body = json.dumps({"ok": True, "result": result}).encode()
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def log_message(self, *args):
        pass


http.server.HTTPServer(("127.0.0.1", int(sys.argv[1])), Handler).serve_forever()
