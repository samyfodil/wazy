// Its own module so the guest is never built by the repo's `go build ./...`:
// it only ever compiles for GOOS=wasip1.
module wazy.examples/greeter/go

go 1.25.0
