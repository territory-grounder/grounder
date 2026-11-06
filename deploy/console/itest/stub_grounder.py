import http.server, socketserver
# Minimal stand-in for the grounder auth surface used by console_authgate_integration_test.go, so the console
# nginx auth_request/error_page path can be exercised end to end: /v1/whoami answers 200 for a valid session
# cookie and 401 otherwise (the INV-01 shape); /v1/session (login) accepts the header contract and mints the
# cookie. It carries no real data and is test-only.
class H(http.server.BaseHTTPRequestHandler):
    def do_GET(self):
        if self.path == "/v1/whoami":
            if "tg_session=valid" in (self.headers.get("Cookie") or ""):
                self.send_response(200); self.send_header("Content-Type", "application/json"); self.end_headers()
                self.wfile.write(b'{"source":"operator:test"}')
            else:
                self.send_response(401); self.end_headers()
            return
        self.send_response(404); self.end_headers()
    def do_POST(self):
        if self.path == "/v1/session":
            op = self.headers.get("X-TG-Operator") or ""
            auth = self.headers.get("Authorization") or ""
            if op and auth.startswith("Bearer "):
                self.send_response(200); self.send_header("Set-Cookie", "tg_session=valid; Path=/; HttpOnly"); self.end_headers()
            else:
                self.send_response(401); self.end_headers()
            return
        self.send_response(404); self.end_headers()
    def log_message(self, *a): pass
socketserver.TCPServer.allow_reuse_address = True
socketserver.TCPServer(("0.0.0.0", 8080), H).serve_forever()
