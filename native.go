//go:build cgo

package main

/*
#include <stdint.h>
#include <stdlib.h>
typedef struct { void* ptr; size_t len; } cliproxy_buffer;
typedef int (*cliproxy_plugin_call_fn)(char*, uint8_t*, size_t, cliproxy_buffer*);
typedef void (*cliproxy_plugin_free_fn)(void*, size_t);
typedef void (*cliproxy_plugin_shutdown_fn)(void);
typedef int (*cliproxy_host_call_fn)(void*, const char*, const uint8_t*, size_t, cliproxy_buffer*);
typedef void (*cliproxy_host_free_fn)(void*, size_t);
typedef struct { uint32_t abi_version; void* host_ctx; cliproxy_host_call_fn call; cliproxy_host_free_fn free_buffer; } cliproxy_host_api;
typedef struct { uint32_t abi_version; cliproxy_plugin_call_fn call; cliproxy_plugin_free_fn free_buffer; cliproxy_plugin_shutdown_fn shutdown; } cliproxy_plugin_api;
extern int cliproxyPluginCall(char*, uint8_t*, size_t, cliproxy_buffer*);
extern void cliproxyPluginFree(void*, size_t);
extern void cliproxyPluginShutdown(void);
static int cliproxy_call_host(const cliproxy_host_api* host, const char* method, const uint8_t* request, size_t len, cliproxy_buffer* out) {
    return host->call(host->host_ctx, method, request, len, out);
}
static void cliproxy_free_host(const cliproxy_host_api* host, cliproxy_buffer out) {
    if (out.ptr != NULL) host->free_buffer(out.ptr, out.len);
}
*/
import "C"

import (
	"encoding/json"
	"fmt"
	"sync"
	"unsafe"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
)

var nativeHost struct {
	sync.RWMutex
	api C.cliproxy_host_api
}

//export cliproxy_plugin_init
func cliproxy_plugin_init(host *C.cliproxy_host_api, plugin *C.cliproxy_plugin_api) C.int {
	if plugin == nil || host != nil && host.abi_version != C.uint32_t(pluginabi.ABIVersion) {
		return 1
	}
	plugin.abi_version = C.uint32_t(pluginabi.ABIVersion)
	plugin.call = C.cliproxy_plugin_call_fn(C.cliproxyPluginCall)
	plugin.free_buffer = C.cliproxy_plugin_free_fn(C.cliproxyPluginFree)
	plugin.shutdown = C.cliproxy_plugin_shutdown_fn(C.cliproxyPluginShutdown)
	nativeHost.Lock()
	nativeHost.api = C.cliproxy_host_api{}
	if host != nil {
		nativeHost.api = *host
	}
	nativeHost.Unlock()
	state.directory.setLoader(readNativeCredentialEmails)
	return 0
}

func readNativeCredentialEmails() ([]credentialEmail, error) {
	nativeHost.RLock()
	defer nativeHost.RUnlock()
	if nativeHost.api.call == nil || nativeHost.api.free_buffer == nil {
		return nil, fmt.Errorf("host credential callbacks are unavailable")
	}
	method := C.CString(pluginabi.MethodHostAuthList)
	request := C.CString("{}")
	defer C.free(unsafe.Pointer(method))
	defer C.free(unsafe.Pointer(request))
	var output C.cliproxy_buffer
	status := C.cliproxy_call_host(&nativeHost.api, method, (*C.uint8_t)(unsafe.Pointer(request)), 2, &output)
	defer C.cliproxy_free_host(&nativeHost.api, output)
	if status != 0 || output.ptr == nil || output.len == 0 || output.len > 64<<20 {
		return nil, fmt.Errorf("host credential lookup failed")
	}
	var envelope pluginabi.Envelope
	if err := json.Unmarshal(C.GoBytes(output.ptr, C.int(output.len)), &envelope); err != nil || !envelope.OK {
		return nil, fmt.Errorf("host credential lookup returned an invalid response")
	}
	return decodeCredentialEmails(envelope.Result)
}

//export cliproxyPluginCall
func cliproxyPluginCall(method *C.char, request *C.uint8_t, length C.size_t, out *C.cliproxy_buffer) (result C.int) {
	if out == nil {
		return 1
	}
	out.ptr, out.len = nil, 0
	name := ""
	if method != nil {
		name = C.GoString(method)
	}
	defer func() {
		if recover() != nil {
			writeNative(out, rpcFailure(name, "plugin call failed"))
			result = 0
		}
	}()
	const maxRequestBytes = 64 << 20
	if length > maxRequestBytes || length > 0 && request == nil {
		writeNative(out, rpcFailure(name, "invalid plugin request buffer"))
		return 0
	}
	var raw []byte
	if length > 0 {
		raw = C.GoBytes(unsafe.Pointer(request), C.int(length))
	}
	response, err := handle(name, raw)
	if err != nil {
		response = rpcFailure(name, err.Error())
	}
	writeNative(out, response)
	return 0
}

func writeNative(out *C.cliproxy_buffer, raw []byte) {
	if len(raw) > 0 {
		out.ptr = C.CBytes(raw)
		out.len = C.size_t(len(raw))
	}
}

//export cliproxyPluginFree
func cliproxyPluginFree(ptr unsafe.Pointer, _ C.size_t) {
	if ptr != nil {
		C.free(ptr)
	}
}

//export cliproxyPluginShutdown
func cliproxyPluginShutdown() {
	state.quiesce()
	nativeHost.Lock()
	nativeHost.api = C.cliproxy_host_api{}
	nativeHost.Unlock()
}
