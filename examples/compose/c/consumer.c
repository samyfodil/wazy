// consumer.c -- the C half of the `wazy:compose/greeter@0.1.0` contract that
// imports the interface. Compiled to consumer.wasm; see README.md.
//
// `run` exercises three paths across the interface and reports what came back:
//
//   [0] greet({name: "wazy", id: 42})   -- a record with a string field
//   [1] greet-all(["a", "b"])[0]        -- a non-empty list, both directions
//   [2] the length of greet-all([])     -- the empty-list path
//
// Nothing here hardcodes provider text. Every byte of [0] and [1] arrives from
// the other component, so a wrong lift or lower shows up in the output instead
// of being masked by a local string. [2] reports the length the provider
// actually returned, not the length it was supposed to return.
//
// Ownership: the strings handed back by an import are lifted into THIS
// component's memory with cabi_realloc (a realloc wrapper), so they are ours to
// free -- and ours to move. run() moves them straight into its result list
// rather than copying, which is exactly what the generated `cabi_post_run`
// expects to free afterwards.

#include <stdint.h>
#include <stdlib.h>
#include <string.h>

#include "consumer.h"

// u32_to_dec writes v as decimal into out and returns the digit count. Written
// by hand rather than with snprintf so the component keeps an import section
// holding nothing but the greeter interface itself.
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

static void *xmalloc(size_t n) {
  void *p = malloc(n);
  if (p == NULL) {
    abort();
  }
  return p;
}

// owned_str copies a nul-terminated literal into a freshly malloc'd component
// string. Used only for the "empty-len=" report, which is ours to state -- the
// number in it still comes from the provider.
static consumer_string_t owned_str(const char *s) {
  const size_t len = strlen(s);
  uint8_t *buf = xmalloc(len);
  memcpy(buf, s, len);
  return (consumer_string_t){buf, len};
}

void exports_consumer_run(consumer_list_string_t *ret) {
  consumer_string_t *out = xmalloc(3 * sizeof(consumer_string_t));

  // [0] a record with a string field, lowered into the provider's memory and
  // its answer lifted back into ours.
  wazy_compose_greeter_visitor_t who;
  consumer_string_set(&who.name, "wazy"); // borrowed for the call; not freed.
  who.id = 42;
  wazy_compose_greeter_greet(&who, &out[0]);

  // [1] a two-element list<string> out, a two-element list<string> back. Take
  // element 0 by moving it, then release the rest of the returned list -- the
  // whole list is ours, so dropping it means freeing element 1 and the array.
  consumer_string_t two[2];
  consumer_string_set(&two[0], "a");
  consumer_string_set(&two[1], "b");
  consumer_list_string_t names = {two, 2};
  consumer_list_string_t greeted;
  wazy_compose_greeter_greet_all(&names, &greeted);
  if (greeted.len > 0) {
    out[1] = greeted.ptr[0];
    for (size_t i = 1; i < greeted.len; i++) {
      consumer_string_free(&greeted.ptr[i]);
    }
    free(greeted.ptr);
  } else {
    // The provider broke the contract. Say so rather than crashing: a wrong
    // answer that is visible is worth more here than a trap.
    out[1] = owned_str("greet-all-returned-0-elements");
  }

  // [2] the empty-list path. A zero-length list is lowered as (null, 0); the
  // reported length is whatever came back, so a provider that mishandles the
  // empty case is exposed instead of hidden.
  consumer_list_string_t none = {NULL, 0};
  consumer_list_string_t empty;
  wazy_compose_greeter_greet_all(&none, &empty);
  const size_t elen = empty.len;
  consumer_list_string_free(&empty);

  static const char label[] = "empty-len=";
  const size_t llen = sizeof(label) - 1;
  char digits[10];
  const size_t dlen = u32_to_dec((uint32_t)elen, digits);
  uint8_t *buf = xmalloc(llen + dlen);
  memcpy(buf, label, llen);
  memcpy(buf + llen, digits, dlen);
  out[2].ptr = buf;
  out[2].len = llen + dlen;

  ret->ptr = out;
  ret->len = 3;
}
