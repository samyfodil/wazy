// greet.c — the C guest for the `wazy:examples/greeter` world.
//
// Bindings are generated from greet.wit by `wit-bindgen c` (see README.md),
// which produces greeter.h/greeter.c and declares the one function this file
// has to define. Everything else — the canonical ABI lifting of the incoming
// string, the return area, cabi_realloc, and the cabi_post_greet that frees
// what we return — lives in that generated code.

#include <stdlib.h>
#include <string.h>

#include "greeter.h"

// A component-model `string` is a (ptr, len) pair in linear memory, NOT a
// nul-terminated C string, so build the greeting with memcpy and lengths.
//
// The buffer handed back in `ret` is owned by the host after this returns:
// the generated `cabi_post_greet` free()s it once the host has copied it out.
void exports_greeter_greet(greeter_string_t *name, greeter_string_t *ret) {
  static const char prefix[] = "Hello, ";
  static const char suffix[] = "! (from C)";
  const size_t plen = sizeof(prefix) - 1;
  const size_t slen = sizeof(suffix) - 1;

  const size_t len = plen + name->len + slen;
  uint8_t *buf = malloc(len);
  if (!buf) {
    abort();
  }

  memcpy(buf, prefix, plen);
  memcpy(buf + plen, name->ptr, name->len);
  memcpy(buf + plen + name->len, suffix, slen);

  ret->ptr = buf;
  ret->len = len;
}
