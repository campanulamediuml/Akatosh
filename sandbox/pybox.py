#!/usr/bin/env python3
"""极简 Python 代码执行沙箱的 HTTP wrapper。

隔离完全由容器承担：internal 网络（物理断网）、read-only 根文件系统、
tmpfs /tmp、内存/CPU/PID 限额、no-new-privileges、非 root 用户。
这里只负责一件事：收 code → subprocess 限时执行 → 回 stdout/stderr/退出码。

POST /run  {"code": "print(1)"}
→ {"stdout": "1\n", "stderr": "", "exit_code": 0, "timed_out": false, "duration_ms": 42}
"""
import json
import subprocess
import sys
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

TIMEOUT_S = 20          # 单次执行硬超时，到点强杀（文档提取类任务放宽到 20s）
MAX_CODE_BYTES = 1 << 20   # 代码最大 1MB
MAX_OUTPUT = 64 * 1024     # stdout/stderr 各截到 64KB（防刷屏占内存）


def run_code(code: str) -> dict:
    start = time.monotonic()
    try:
        p = subprocess.run(
            [sys.executable, "-c", code],
            capture_output=True, text=True, errors="replace",
            timeout=TIMEOUT_S, cwd="/tmp",
            # 干净的最小环境：不继承任何容器环境变量
            env={"PATH": "/usr/local/bin:/usr/bin:/bin", "HOME": "/tmp"},
        )
        out = {
            "stdout": p.stdout[-MAX_OUTPUT:],
            "stderr": p.stderr[-MAX_OUTPUT:],
            "exit_code": p.returncode,
            "timed_out": False,
        }
    except subprocess.TimeoutExpired as e:
        out = {
            "stdout": e.stdout[-MAX_OUTPUT:] if isinstance(e.stdout, str) else "",
            "stderr": f"（执行超过 {TIMEOUT_S}s，已强杀）",
            "exit_code": -1,
            "timed_out": True,
        }
    out["duration_ms"] = int((time.monotonic() - start) * 1000)
    return out


class Handler(BaseHTTPRequestHandler):
    def do_POST(self):
        if self.path != "/run":
            self.send_error(404)
            return
        try:
            n = min(int(self.headers.get("Content-Length") or 0), MAX_CODE_BYTES)
            body = json.loads(self.rfile.read(n))
            code = str(body.get("code") or "")
        except Exception as e:
            return self._json(400, {"error": f"请求体不是合法 JSON: {e}"})
        if not code.strip():
            return self._json(400, {"error": "code 为空"})
        self._json(200, run_code(code))

    def do_GET(self):
        # 探活
        if self.path == "/health":
            return self._json(200, {"ok": True})
        self.send_error(404)

    def _json(self, status: int, obj: dict):
        body = json.dumps(obj, ensure_ascii=False).encode()
        self.send_response(status)
        self.send_header("Content-Type", "application/json; charset=utf-8")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def log_message(self, fmt, *args):  # 每请求日志没价值，静音
        pass


if __name__ == "__main__":
    ThreadingHTTPServer(("0.0.0.0", 8000), Handler).serve_forever()
