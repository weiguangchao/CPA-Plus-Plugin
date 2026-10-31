#!/usr/bin/env python3
"""Exercise the built C ABI against a local CPA API fixture, optionally a CPA host."""

import argparse
import base64
import ctypes
import datetime
import http.server
import json
from pathlib import Path
import secrets
import shutil
import socket
import subprocess
import tempfile
import threading
import time
import urllib.error
import urllib.request


class Fixture:
    def __init__(self, key):
        self.key = key
        self.condition = threading.Condition()
        self.queries = 0
        self.lists = 0
        self.quotas = {}

    def handler(self):
        fixture = self

        class Handler(http.server.BaseHTTPRequestHandler):
            def log_message(self, *_args):
                pass

            def do_GET(self):
                self.respond()

            def do_POST(self):
                self.respond()

            def respond(self):
                if self.headers.get("Authorization") != f"Bearer {fixture.key}":
                    self.send_error(401)
                    return
                with fixture.condition:
                    if self.command == "GET" and self.path == "/v8/management/credentials":
                        fixture.lists += 1
                        payload = {"files": [
                            {"id": name, "auth_index": f"index-{name}", "provider": "codex",
                             "id_token": {"chatgpt_account_id": f"account-{name}"}} for name in ["a", "b"]
                        ]}
                    elif self.command == "POST" and self.path == "/v8/management/requests/api-call":
                        request = json.loads(self.rfile.read(int(self.headers.get("Content-Length", 0))))
                        if (request.get("method") != "GET"
                                or request.get("url") != "https://chatgpt.com/backend-api/wham/usage"
                                or request.get("header", {}).get("Authorization") != "Bearer $TOKEN$"
                                or request["header"].get("Chatgpt-Account-Id") != "account-" + request["auth_index"][-1]):
                            self.send_error(400)
                            return
                        payload = {"status_code": 200, "body": json.dumps(fixture.quotas[request["auth_index"]])}
                        fixture.queries += 1
                        fixture.condition.notify_all()
                    else:
                        self.send_error(404)
                        return
                raw = json.dumps(payload).encode()
                self.send_response(200)
                self.send_header("Content-Type", "application/json")
                self.send_header("Content-Length", str(len(raw)))
                self.end_headers()
                self.wfile.write(raw)

        return Handler


class Buffer(ctypes.Structure):
    _fields_ = [("ptr", ctypes.c_void_p), ("len", ctypes.c_size_t)]


CALL = ctypes.CFUNCTYPE(ctypes.c_int, ctypes.c_char_p, ctypes.c_void_p, ctypes.c_size_t, ctypes.POINTER(Buffer))
FREE = ctypes.CFUNCTYPE(None, ctypes.c_void_p, ctypes.c_size_t)
SHUTDOWN = ctypes.CFUNCTYPE(None)


class PluginAPI(ctypes.Structure):
    _fields_ = [("abi_version", ctypes.c_uint32), ("call", CALL), ("free_buffer", FREE), ("shutdown", SHUTDOWN)]


def quota(five, week, five_reset=None, week_reset=None):
    return {"rate_limit": {
        "primary_window": {"limit_window_seconds": 18000, "used_percent": (1 - five) * 100, "reset_at": five_reset},
        "secondary_window": {"limit_window_seconds": 604800, "used_percent": (1 - week) * 100, "reset_at": week_reset},
    }}


def wait_for(predicate, description):
    deadline = time.monotonic() + 10
    wake = threading.Event()
    while time.monotonic() < deadline:
        if predicate():
            return
        wake.wait(0.02)
    raise RuntimeError(f"Timed out: {description}")


def abi_smoke(library, fixture, url):
    lib = ctypes.CDLL(str(library))
    init = lib.cliproxy_plugin_init
    init.argtypes = [ctypes.c_void_p, ctypes.POINTER(PluginAPI)]
    init.restype = ctypes.c_int
    api = PluginAPI()
    assert init(None, ctypes.byref(api)) == 0 and api.abi_version == 1

    def call(method, request):
        raw = json.dumps(request).encode()
        data = ctypes.create_string_buffer(raw)
        response = Buffer()
        rc = api.call(method.encode(), data, len(raw), ctypes.byref(response))
        try:
            envelope = json.loads(ctypes.string_at(response.ptr, response.len))
        finally:
            api.free_buffer(response.ptr, response.len)
        assert rc == 0 and envelope["ok"], envelope
        return envelope["result"]

    request = {"Provider": "codex", "Model": "gpt-5.5", "Candidates": [
        {"ID": "a", "Provider": "codex"}, {"ID": "b", "Provider": "codex"},
    ]}
    config = f"cpa_url: {url}\nmanagement_key: {json.dumps(fixture.key)}\n"
    lifecycle = {"config_yaml": base64.b64encode(config.encode()).decode()}
    now = datetime.datetime.now(datetime.timezone.utc)
    soon = (now + datetime.timedelta(minutes=10)).isoformat()
    later = (now + datetime.timedelta(minutes=20)).isoformat()
    week = (now + datetime.timedelta(days=4)).isoformat()
    cases = [
        ("five-hour ordering", quota(.5, .5, later, week), quota(.5, .5, soon, week), "b"),
        ("first pick skips weekly-only account",
         {"rate_limit": {"primary_window": {"limit_window_seconds": 604800,
                                             "used_percent": 10, "reset_at": soon}}},
         quota(.5, .5, later, week), "b"),
        ("weekly exhaustion filter", quota(.5, .5, later, week), quota(.5, 0, soon, week), "a"),
        ("weekly fallback", quota(.5, .5, None, soon), quota(.5, .5, None, later), "a"),
        ("no reset delegates", quota(.5, .5), quota(.5, .5), ""),
        ("both windows required", quota(0, .5, soon, week), quota(.5, 0, later, week), ""),
    ]
    try:
        blank = {"config_yaml": base64.b64encode(f"cpa_url: {url}\n".encode()).decode()}
        initial = call("plugin.register", blank)
        fields = {field["Name"] for field in initial["metadata"]["ConfigFields"]}
        assert [field["Name"] for field in initial["metadata"]["ConfigFields"]] == [
            "cpa_url", "management_key", "request_timeout", "retry_interval", "cache_ttl",
        ], "unexpected form field order"
        assert next(field for field in initial["metadata"]["ConfigFields"]
                    if field["Name"] == "cache_ttl")["Default"] == "10m", "cache_ttl must default to 10m"
        assert all(any("\u4e00" <= char <= "\u9fff" for char in field["Description"])
                   for field in initial["metadata"]["ConfigFields"]), "field descriptions must be Chinese"
        assert "management_key" in fields, "form is missing direct management key input"
        assert "management_key_env" not in fields and "model_aliases" not in fields, fields
        assert "schedule_across_priorities" not in fields, fields
        assert initial["capabilities"]["scheduler_across_priorities"] is True
        legacy = {"config_yaml": base64.b64encode((config + "schedule_across_priorities: false\n").encode()).decode()}
        updated = call("plugin.reconfigure", legacy)
        assert updated["capabilities"]["scheduler_across_priorities"] is True, "legacy setting disabled cross-priority scheduling"
        call("plugin.reconfigure", {"config_yaml": base64.b64encode((config + "cache_ttl: 0\n").encode()).decode()})
        call("plugin.reconfigure", blank)
        print("PASS native ABI: cross-priority scheduling always enabled; form option removed")
        assert fixture.key not in json.dumps(initial), "registration metadata exposed the management key"
        for _ in range(10):
            pick = call("scheduler.pick", request)
            assert not pick.get("Handled") and not pick.get("Reject"), pick
        with fixture.condition:
            assert (fixture.lists, fixture.queries) == (0, 0), "unconfigured plugin called API"
        print("PASS native ABI: direct-key form available without credentials; no API calls")
        for i, (name, a, b, expected) in enumerate(cases):
            with fixture.condition:
                fixture.quotas = {"index-a": a, "index-b": b}
                before = fixture.queries
            result = call("plugin.reconfigure", lifecycle)
            assert result["capabilities"]["scheduler"]
            expected_version = (Path(__file__).resolve().parents[1] / "VERSION").read_text().strip()
            assert result["metadata"]["Version"] == expected_version, "plugin version differs from VERSION"
            with fixture.condition:
                assert fixture.queries == before, "plugin startup must not query quotas"
                cold_counts = (fixture.lists, fixture.queries)
            pinned_a = {**request, "Options": {"Metadata": {"pinned_auth_id": "a"}},
                        "Candidates": request["Candidates"][:1]}
            pinned_pick = call("scheduler.pick", pinned_a)
            assert not pinned_pick.get("Handled") and not pinned_pick.get("Reject") and not pinned_pick.get("AuthID"), pinned_pick
            with fixture.condition:
                assert (fixture.lists, fixture.queries) == cold_counts, "cold pinned request called API"
            first_pick = call("scheduler.pick", request)
            assert not first_pick.get("Reject"), first_pick
            if expected:
                assert first_pick.get("Handled") and first_pick.get("AuthID") == expected, first_pick
            else:
                assert not first_pick.get("Handled") and not first_pick.get("AuthID"), first_pick
            with fixture.condition:
                assert fixture.condition.wait_for(lambda: fixture.queries >= before + 2, 10), name

            def selected():
                pick = call("scheduler.pick", request)
                assert not pick.get("Reject", False), pick
                if expected:
                    return pick.get("Handled") and pick.get("AuthID") == expected
                return not pick.get("Handled") and not pick.get("AuthID")

            wait_for(selected, name)
            print(f"PASS native ABI: {name}")
            if i == 0:
                with fixture.condition:
                    fixture.quotas = {"index-a": quota(.5, .5, soon, week),
                                      "index-b": quota(.5, .5, later, week)}
                    counts = (fixture.lists, fixture.queries)
                for _ in range(20):
                    assert call("scheduler.pick", request).get("AuthID") == "b", "sticky account changed"
                with fixture.condition:
                    assert (fixture.lists, fixture.queries) == counts, "cache hit called API"
                print("PASS native ABI: sticky cache hits issue no API calls")
                for candidate in request["Candidates"]:
                    pinned = {**request, "Options": {"Metadata": {"pinned_auth_id": candidate["ID"]}},
                              "Candidates": [candidate]}
                    pinned_pick = call("scheduler.pick", pinned)
                    assert not pinned_pick.get("Handled") and not pinned_pick.get("Reject") and not pinned_pick.get("AuthID"), pinned_pick
                assert call("scheduler.pick", request).get("AuthID") == "b", "pinned request replaced ordinary selection"
                with fixture.condition:
                    assert (fixture.lists, fixture.queries) == counts, "pinned request called API"
                print("PASS native ABI: pinned requests delegate without changing ordinary cache")
                only_a = {**request, "Candidates": request["Candidates"][:1]}
                refreshed = call("scheduler.pick", only_a)
                assert refreshed.get("Handled") and refreshed.get("AuthID") == "a", refreshed

                def selected_a():
                    pick = call("scheduler.pick", only_a)
                    return pick.get("Handled") and pick.get("AuthID") == "a"

                wait_for(selected_a, "candidate removal")
                with fixture.condition:
                    assert (fixture.lists, fixture.queries) == (counts[0] + 1, counts[1] + 1)
                print("PASS native ABI: candidate removal refreshes remaining account")
        call("plugin.quiesce", {})
        call("plugin.shutdown", {})
        assert not call("scheduler.pick", request).get("Handled")
        print("PASS native ABI: quiesce and shutdown")
    finally:
        api.shutdown()


def host_smoke(cpa, library, fixture, fixture_url, key):
    with fixture.condition:
        before = (fixture.lists, fixture.queries)
    with socket.socket() as sock:
        sock.bind(("127.0.0.1", 0))
        port = sock.getsockname()[1]
    with tempfile.TemporaryDirectory(prefix="cpa-plus-host-smoke-") as tmp:
        root = Path(tmp)
        plugins = root / "plugins"
        plugins.mkdir()
        shutil.copy2(library, plugins / library.name)
        config = root / "config.yaml"
        config.write_text(f"""config-version: 8
server:
  host: "127.0.0.1"
  port: {port}
management:
  allow-remote: false
  secret-key: {json.dumps(key)}
  disable-control-panel: true
oauth:
  auth-dir: {json.dumps(str(root / 'auths'))}
plugins:
  enabled: true
  dir: {json.dumps(str(plugins))}
  configs:
    cpa-plus:
      enabled: true
      priority: 100
      cpa_url: {json.dumps(fixture_url)}
      management_key: {json.dumps(key)}
""")
        with (root / "host.log").open("w+") as log:
            process = subprocess.Popen([str(cpa), "--config", str(config), "--local-model"], cwd=root,
                                       stdout=log, stderr=subprocess.STDOUT)
            try:
                last_payload = None

                def loaded():
                    nonlocal last_payload
                    if process.poll() is not None:
                        log.seek(0)
                        raise RuntimeError("CPA exited during plugin load:\n" + log.read()[-6000:])
                    request = urllib.request.Request(f"http://127.0.0.1:{port}/v8/management/plugins",
                                                     headers={"Authorization": f"Bearer {key}"})
                    try:
                        with urllib.request.urlopen(request, timeout=1) as response:
                            payload = json.load(response)
                            last_payload = payload
                    except (urllib.error.URLError, TimeoutError):
                        return False
                    return any(p["id"] == "cpa-plus" and p["registered"] and p["effective_enabled"]
                               for p in payload["plugins"])

                try:
                    wait_for(loaded, "real CPA plugin loading")
                except RuntimeError as error:
                    log.seek(0)
                    details = log.read()[:8000].replace(key, "[test key]")
                    raise RuntimeError(f"{error}\nPlugins: {last_payload}\nCPA log:\n{details}") from error
                print("PASS CPA host: cpa-plus registered and enabled")
                with fixture.condition:
                    assert (fixture.lists, fixture.queries) == before, "idle CPA host queried quotas"
                print("PASS CPA host: idle plugin issues no API calls")
            finally:
                process.terminate()
                try:
                    process.wait(timeout=5)
                except subprocess.TimeoutExpired:
                    process.kill()
                    process.wait()


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("library", type=Path)
    parser.add_argument("--cpa", type=Path)
    args = parser.parse_args()
    library = args.library.resolve(strict=True)
    key = secrets.token_hex(24)
    fixture = Fixture(key)
    server = http.server.ThreadingHTTPServer(("127.0.0.1", 0), fixture.handler())
    thread = threading.Thread(target=server.serve_forever, daemon=True)
    thread.start()
    url = f"http://127.0.0.1:{server.server_port}"
    try:
        abi_smoke(library, fixture, url)
        if args.cpa:
            host_smoke(args.cpa.resolve(strict=True), library, fixture, url, key)
    finally:
        server.shutdown()
        server.server_close()
        thread.join()


if __name__ == "__main__":
    main()
