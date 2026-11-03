package native_test

import (
	"io/fs"
	"testing/fstest"
)

// The tests expect a Unix host: a /dev/null, and an /etc/hosts naming
// localhost, which is how Go's wasip1 network resolves it. Linux and macOS
// have both under the root the tests mount. Windows has neither, so the
// harness mounts these there.
var (
	devDir = nullDevDir{fstest.MapFS{"null": {Mode: fs.ModeDevice | fs.ModeCharDevice | 0o666}}}
	etcDir = fstest.MapFS{"hosts": {Data: []byte("127.0.0.1 localhost\n::1 localhost\n")}}
)

// nullDevDir opens null as a file that takes and drops whatever is written.
type nullDevDir struct{ fstest.MapFS }

func (d nullDevDir) Open(name string) (fs.File, error) {
	f, err := d.MapFS.Open(name)
	if err != nil || name != "null" {
		return f, err
	}
	return nullFile{f}, nil
}

type nullFile struct{ fs.File }

func (nullFile) Write(p []byte) (int, error) { return len(p), nil }
