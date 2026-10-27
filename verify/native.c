#include <dlfcn.h>
#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>

typedef struct { void *ptr; size_t len; } buffer;
typedef struct {
    uint32_t abi_version;
    int (*call)(char *, uint8_t *, size_t, buffer *);
    void (*free_buffer)(void *, size_t);
    void (*shutdown)(void);
} plugin_api;
typedef int (*init_fn)(const void *, plugin_api *);

static int verify_call(plugin_api *api, const char *request, int expect_success) {
    buffer response = {0};
    int rc = api->call("plugin.register", (uint8_t *)request, strlen(request), &response);
    int valid = (expect_success ? rc == 0 : rc != 0) && response.ptr && response.len;
    if (response.ptr) api->free_buffer(response.ptr, response.len);
    if (!valid) fprintf(stderr, "unexpected native lifecycle response\n");
    return valid;
}

int main(int argc, char **argv) {
    if (argc != 2) return 2;
    void *library = dlopen(argv[1], RTLD_NOW | RTLD_LOCAL);
    if (!library) { fprintf(stderr, "%s\n", dlerror()); return 1; }
    init_fn initialize = (init_fn)dlsym(library, "cliproxy_plugin_init");
    plugin_api api = {0};
    if (!initialize || initialize(NULL, &api) != 0 || api.abi_version != 1 || !api.call || !api.free_buffer || !api.shutdown) {
        fprintf(stderr, "invalid native plugin ABI\n"); return 1;
    }
    /* Empty settings expose the form; malformed lifecycle input must fail registration. */
    int lifecycle_valid = verify_call(&api, "{}", 1) && verify_call(&api, "{", 0);
    api.shutdown();
    if (!lifecycle_valid) return 1;
    puts("keeper usage push native ABI and lifecycle verified");
    /* Go shared libraries remain mapped for the process lifetime. */
    return 0;
}
