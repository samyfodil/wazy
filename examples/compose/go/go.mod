// Its own module so the guests are never built by the repo's `go build ./...`:
// they only ever compile for GOOS=wasip1.
module wazy.examples/compose/go

go 1.25.0
