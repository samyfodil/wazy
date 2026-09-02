// This directory is a nested module on purpose: it holds prebuilt guest .wasm
// (a componentize-py or jco guest is tens of megabytes) and a nested module is
// excluded from the parent module's zip, so "go get github.com/samyfodil/wazy"
// does not download them. Build instructions are in each language's README.
module github.com/samyfodil/wazy/compose

go 1.25
