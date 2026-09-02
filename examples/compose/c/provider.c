// provider.c -- the C half of the `wazy:compose/greeter@0.1.0` contract that
// exports the interface. Compiled to provider.wasm; see README.md.
//
// wit-bindgen c generates provider.h/provider.c from greeter.wit and declares
// the two functions this file defines. Everything around them -- lifting the
// incoming record and list, the return area, cabi_realloc, and the cabi_post_*
// that frees what we hand back -- lives in that generated code.
//
// Two rules govern every line below:
//
//   1. A component-model `string` is a (ptr, len) pair in linear memory, NOT a
//      nul-terminated C string. Build results with memcpy and explicit lengths.
//   2. Whatever we store in `ret` is owned by the host after we return, and the
//      generated `cabi_post_*` releases it with free(). So every buffer that
//      escapes must come from malloc, and nothing that escapes may be freed
//      here.

#include <stdint.h>
#include <stdlib.h>
#include <string.h>

#include "provider.h"

// The language tag, in one place, so the two messages cannot drift apart.
#define LANG "C"

// u32_to_dec writes v as decimal into out and returns the digit count. Written
// by hand rather than with snprintf so the component keeps a completely empty
// import section -- pulling in stdio would drag WASI along with it.
// 4294967295 is ten digits, so out[10] is always enough.
static size_t u32_to_dec(uint32_t v, char out[10]) {
  char rev[10];
  size_t n = 0;
  do {
    rev[n++] = (char)('0' + (v % 10u));
    v /= 10u;
  } while (v != 0);
  for (size_t i = 0; i < n; i++) {
    out[i] = rev[n - 1 - i];
  }
  return n;
}

// xmalloc aborts rather than handing the canonical ABI a null pointer. A guest
// that cannot allocate cannot honour the contract, and trapping here is far
// easier to debug than a corrupt string on the other side of the call.
static void *xmalloc(size_t n) {
  void *p = malloc(n);
  if (p == NULL) {
    abort();
  }
  return p;
}

// greet returns exactly: Hello, <who.name> #<who.id>! (from C)
void exports_wazy_compose_greeter_greet(exports_wazy_compose_greeter_visitor_t *who,
                                        provider_string_t *ret) {
  static const char prefix[] = "Hello, ";
  static const char infix[] = " #";
  static const char suffix[] = "! (from " LANG ")";
  const size_t plen = sizeof(prefix) - 1;
  const size_t ilen = sizeof(infix) - 1;
  const size_t slen = sizeof(suffix) - 1;

  char digits[10];
  const size_t dlen = u32_to_dec(who->id, digits);

  const size_t len = plen + who->name.len + ilen + dlen + slen;
  uint8_t *buf = xmalloc(len);

  size_t off = 0;
  memcpy(buf + off, prefix, plen);
  off += plen;
  memcpy(buf + off, who->name.ptr, who->name.len);
  off += who->name.len;
  memcpy(buf + off, infix, ilen);
  off += ilen;
  memcpy(buf + off, digits, dlen);
  off += dlen;
  memcpy(buf + off, suffix, slen);

  ret->ptr = buf;
  ret->len = len;
}

// greet_all returns one element per input, each exactly: <name> (via C)
//
// An empty input list yields an empty output list, and that path is not
// special-cased away: len 0 simply falls out of the loop below and is lowered
// as the pair (null, 0), which the canonical ABI accepts -- null is aligned
// for every alignment, and no element is ever loaded from a zero-length list.
// The one branch is on the allocation, because malloc(0) may return null and
// xmalloc would abort on it.
void exports_wazy_compose_greeter_greet_all(provider_list_string_t *names,
                                            provider_list_string_t *ret) {
  static const char suffix[] = " (via " LANG ")";
  const size_t slen = sizeof(suffix) - 1;

  provider_string_t *out = NULL;
  if (names->len > 0) {
    out = xmalloc(names->len * sizeof(provider_string_t));
  }
  for (size_t i = 0; i < names->len; i++) {
    const provider_string_t *name = &names->ptr[i];
    const size_t len = name->len + slen;
    uint8_t *buf = xmalloc(len);
    memcpy(buf, name->ptr, name->len);
    memcpy(buf + name->len, suffix, slen);
    out[i].ptr = buf;
    out[i].len = len;
  }

  ret->ptr = out;
  ret->len = names->len;
}
