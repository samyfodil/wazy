# component imports, by hand

Hand-written components for what the vendored suites cannot show: a
component supplied for a component import actually running, and the nested
machinery a supplied component goes through. Each `.wat` says what it is;
the `.wasm` beside it is what `wasm-tools parse` (1.262.0, in
`ghcr.io/samyfodil/wazy-build-c:34.0`) encodes. Those marked malformed are
refused by the type check before instantiation, and are instantiated by the
tests without it to reach the runtime's own refusal.

- `calc_root` imports a component `calc` of a declared type and runs it;
  `calc_exact`, `calc_subtype`, `calc_wrong_param` and `calc_extra_import`
  are supplied for it, and `calc_reexport_root` re-exports its instance whole.
- `fixedlist_match_root` and `fixedlist_match_y` are the case wasm-tools'
  `fixed-length-lists.wast` must accept beside the two it refuses.
- `shim_root` and `shim` supply a pure re-export shim; `shim_whole_arg_root`
  passes an instance of one whole to a nested component with code.
- `nested_whole_root` and `nested_whole_nested_iface_root` export nested
  instances whole, the second one whose component exports an interface of
  its own; `nested_iface_paths_root` reaches such a nested interface through
  a two-level alias, a func alias, an instantiate-arg and a further whole
  export; `inline_alias_root`, `inline_of_nested_root` and
  `shim_two_exports_root` alias funcs out of instances never instantiated;
  `shim_bad_index_root`, `shim_missing_arg_root`, `inline_alias_missing_root`
  and `shim_no_export_root` are the malformed ones.
- `counter_root` imports a component defining a resource and re-exports its
  instance whole; `counter` is of that type, `counter_own` is not.
