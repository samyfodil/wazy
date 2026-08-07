// Package testfixtures holds guest components shared by more than one
// package's tests.
//
// It exists because //go:embed is package-local by language rule -- a pattern
// may not contain ".." -- so two packages that need the same guest binary
// would otherwise each keep a copy. One copy lives here and both embed it
// through this package.
package testfixtures

import _ "embed"

// RealHello is an off-the-shelf rustc wasm32-wasip2 wasi:cli/command
// component that prints "hello world".
//
// Two unrelated sets of tests need it. The WASI implementation's tests care
// what it prints -- it is the milestone proof that a real guest's println!
// reaches a host writer. The engine's tests do not care at all; they need
// *some* genuine multi-module component to exercise instantiation, the
// compile cache, and graph wiring, and this is the one that has always
// played that role.
//
//go:embed testdata/real_hello.component.wasm
var RealHello []byte
