#!/usr/bin/env python3
"""Exercise the built C ABI with dummy requests and no upstream connections."""

import argparse
import base64
import ctypes
import json
from pathlib import Path


class Buffer(ctypes.Structure):
    _fields_ = [("ptr", ctypes.c_void_p), ("len", ctypes.c_size_t)]


CALL = ctypes.CFUNCTYPE(
    ctypes.c_int, ctypes.c_char_p, ctypes.c_void_p, ctypes.c_size_t,
    ctypes.POINTER(Buffer),
)
FREE = ctypes.CFUNCTYPE(None, ctypes.c_void_p, ctypes.c_size_t)
HOST_CALL = ctypes.CFUNCTYPE(
    ctypes.c_int, ctypes.c_void_p, ctypes.c_char_p, ctypes.c_void_p,
    ctypes.c_size_t, ctypes.POINTER(Buffer),
)
SHUTDOWN = ctypes.CFUNCTYPE(None)


class PluginAPI(ctypes.Structure):
    _fields_ = [
        ("abi_version", ctypes.c_uint32), ("call", CALL),
        ("free_buffer", FREE), ("shutdown", SHUTDOWN),
    ]


class HostAPI(ctypes.Structure):
    _fields_ = [
        ("abi_version", ctypes.c_uint32), ("host_ctx", ctypes.c_void_p),
        ("call", HOST_CALL), ("free_buffer", FREE),
    ]


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("library", type=Path)
    args = parser.parse_args()
    library = ctypes.CDLL(str(args.library.resolve()))
    library.cliproxy_plugin_init.argtypes = [ctypes.c_void_p, ctypes.POINTER(PluginAPI)]
    library.cliproxy_plugin_init.restype = ctypes.c_int
    api = PluginAPI()
    assert library.cliproxy_plugin_init(None, ctypes.byref(api)) == 0
    assert api.abi_version == 1

    def call(method, payload):
        raw = json.dumps(payload).encode()
        request = ctypes.create_string_buffer(raw)
        output = Buffer()
        result = api.call(method.encode(), request, len(raw), ctypes.byref(output))
        try:
            response = json.loads(ctypes.string_at(output.ptr, output.len))
        finally:
            api.free_buffer(output.ptr, output.len)
        assert result == 0 and response["ok"], response
        return response["result"]

    registration = call("plugin.register", {"schema_version": 6})
    assert registration["metadata"]["Name"] == "cpa-plugin-limit"
    assert registration["capabilities"]["scheduler"]
    assert registration["capabilities"]["scheduler_across_priorities"]
    fields = {field["Name"]: field for field in registration["metadata"]["ConfigFields"]}
    assert fields["requests_per_minute"]["Default"] == 5
    assert fields["max_concurrency"]["Default"] == 1
    assert fields["auth_indexes"]["Default"] == []
    assert fields["account_groups"]["Default"] == {}
    assert fields["account_limits"]["Default"] == {}
    assert fields["account_display"]["Default"] == "email"
    assert fields["account_display"]["Type"] == "enum"
    assert fields["routing_mode"]["Default"] == "capacity-aware"
    assert fields["session_affinity_ttl_seconds"]["Default"] == 3600

    host_buffers = {}
    host_calls = []

    @HOST_CALL
    def host_call(ctx, method, request, length, output):
        assert ctx == 123
        assert method == b"host.auth.list"
        assert json.loads(ctypes.string_at(request, length)) == {}
        host_calls.append(method)
        response = json.dumps({"ok": True, "result": {"files": [
            {"auth_index": "dummy-account", "email": "alice@example.com",
             "account": "private-credential-secret", "label": "private-label"},
            {"auth_index": "another-dummy-account", "email": "bob@example.com"},
        ]}}).encode()
        buffer = ctypes.create_string_buffer(response)
        pointer = ctypes.addressof(buffer)
        host_buffers[pointer] = buffer
        output[0] = Buffer(pointer, len(response))
        return 0

    @FREE
    def host_free(pointer, length):
        assert pointer in host_buffers and length > 0
        del host_buffers[pointer]

    host = HostAPI(1, 123, host_call, host_free)
    assert library.cliproxy_plugin_init(ctypes.byref(host), ctypes.byref(api)) == 0

    def before(request_id):
        result = call("request.intercept_before", {"RequestID": request_id})
        assert not result["Terminate"], result

    def after(request_id, account="dummy-account"):
        return call("request.intercept_after", {
            "RequestID": request_id, "SourceFormat": "openai-response",
            "Metadata": {"selected_auth_index": account},
        })

    def complete(request_id):
        call("request.complete", {"RequestID": request_id, "Outcome": "succeeded"})

    before("first")
    assert not after("first")["Terminate"]
    before("busy")
    busy = after("busy")
    assert busy["Terminate"] and busy["StatusCode"] == 429
    assert busy["ResponseHeaders"]["Retry-After"] == ["1"]
    complete("busy")
    complete("first")
    complete("first")
    for i in range(4):
        request_id = f"next-{i}"
        before(request_id)
        assert not after(request_id)["Terminate"]
        complete(request_id)
    before("rate-limited")
    limited = after("rate-limited")
    body = json.loads(base64.b64decode(limited["ResponseBody"]))
    assert limited["StatusCode"] == 429
    assert body["error"]["code"] == "account_rate_limit_exceeded"
    assert 1 <= int(limited["ResponseHeaders"]["Retry-After"][0]) <= 60
    complete("rate-limited")

    before("isolated")
    assert not after("isolated", "another-dummy-account")["Terminate"]
    complete("isolated")
    assert not host_calls, "email lookup ran on the admission path"
    resource = call("management.handle", {"Method": "GET", "Path": "/status"})
    status = json.loads(base64.b64decode(resource["Body"]))
    assert status["admitted"] == 6 and status["rejected"] == 2
    assert status["active_requests"] == 0
    assert status["active_slots"] == 0 and status["window_requests"] == 6
    assert status["rate_rejected"] == 1 and status["concurrency_rejected"] == 1
    assert status["defaults"] == {"requests_per_minute": 5, "max_concurrency": 1}
    assert status["scope"]["all_credentials"]
    assert "dummy-account" not in json.dumps(status)
    assert status["account_display"] == "email"
    assert status["email_lookup_available"]
    assert {row["label"] for row in status["items"]} == {"alice@example.com", "bob@example.com"}
    assert "private-" not in json.dumps(status)
    assert len(host_calls) == 1 and not host_buffers, "host callback buffers leaked"
    for mode in ["masked_email", "fingerprint"]:
        call("plugin.reconfigure", {"schema_version": 6, "config_yaml": base64.b64encode(f"account_display: {mode}".encode()).decode()})
        resource = call("management.handle", {"Method": "GET", "Path": "/status"})
        displayed = json.loads(base64.b64decode(resource["Body"]))
        assert displayed["admitted"] == 6 and displayed["rejected"] == 2
        if mode == "masked_email":
            assert {row["label"] for row in displayed["items"]} == {"al***@example.com", "bo***@example.com"}
            assert "alice@example.com" not in json.dumps(displayed)
        else:
            assert all("label" not in row for row in displayed["items"])
    assert len(host_calls) == 1 and not host_buffers
    assert library.cliproxy_plugin_init(None, ctypes.byref(api)) == 0
    call("plugin.reconfigure", {"schema_version": 6})
    fallback_resource = call("management.handle", {"Method": "GET", "Path": "/status"})
    fallback = json.loads(base64.b64decode(fallback_resource["Body"]))
    assert not fallback["email_lookup_available"]
    assert all("label" not in row for row in fallback["items"])
    assert fallback["admitted"] == 6 and fallback["rejected"] == 2
    call("plugin.reconfigure", {"schema_version": 6, "config_yaml": base64.b64encode(b"requests_per_minute: 2\nmax_concurrency: 0").decode()})
    selected = []
    for i in range(5):
        request_id = f"routing-{i}"
        started = call("request.intercept_before", {"RequestID": request_id, "Headers": {"Session-Id": ["dummy-hot-session"]}})
        picked = call("scheduler.pick", {"Provider": "codex", "Model": "dummy-model", "Options": {"Headers": started["Headers"]}, "Candidates": [{"ID": "route-a"}, {"ID": "route-b"}]})
        assert picked["Handled"] and picked["AuthID"] in {"route-a", "route-b"}
        admitted = call("request.intercept_after", {"RequestID": request_id, "Headers": started["Headers"], "Metadata": {"selected_auth_index": picked["AuthID"], "selected_auth_id": picked["AuthID"]}})
        assert admitted["ClearHeaders"] == ["X-Cpa-Limit-Request"]
        if i < 4:
            assert not admitted["Terminate"], admitted
            selected.append(picked["AuthID"])
        else:
            assert admitted["StatusCode"] == 429 and admitted["ResponseHeaders"]["Retry-After"]
        complete(request_id)
    assert selected == ["route-a", "route-a", "route-b", "route-b"], selected
    print("Native scheduler smoke passed: sticky session, full-account failover, pool exhaustion, internal header removal")
    call("plugin.quiesce", {})
    assert call("request.intercept_before", {"RequestID": "stopped"})["StatusCode"] == 503
    api.shutdown()
    print("Native ABI smoke passed: registration, account isolation, concurrency, RPM, release, host email callbacks, redaction, shutdown")


if __name__ == "__main__":
    main()
