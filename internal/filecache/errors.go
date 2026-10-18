package filecache

import "errors"

// These are exposed by the public compilation cache API, see
// wazy.NewCompilationCacheWithDirReadOnly.
var (
	ErrMiss        = errors.New("compilation cache: required entry missing")
	ErrStale       = errors.New("compilation cache: stale entry")
	ErrCorrupt     = errors.New("compilation cache: corrupt entry")
	ErrUnsupported = errors.New("compilation cache: engine can't load modules from a persistent cache")
	ErrReadOnly    = errors.New("compilation cache: read-only")
)

// ReadOnly reports whether c only serves hits: a module it doesn't hold must
// fail rather than be compiled, and it is never written.
func ReadOnly(c Cache) bool {
	r, ok := c.(interface{ ReadOnly() bool })
	return ok && r.ReadOnly()
}
