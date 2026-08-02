package sysfs

import (
	"github.com/samyfodil/wazy"
	"github.com/samyfodil/wazy/sys"
)

// FSConfig is the type assertion that used to be the only way to reach
// WithSysFSMount, back when sys.FS was still experimental.
//
// Deprecated: wazy.FSConfig declares WithSysFSMount directly now -- call it on
// the wazy.FSConfig, no assertion needed. This interface remains so existing
// `config.(sysfs.FSConfig).WithSysFSMount(...)` code keeps compiling.
type FSConfig interface {
	WithSysFSMount(fs sys.FS, guestPath string) wazy.FSConfig
}
