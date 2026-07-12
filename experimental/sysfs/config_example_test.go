package sysfs_test

import (
	"io/fs"
	"testing/fstest"

	"github.com/samyfodil/wazy"
	"github.com/samyfodil/wazy/experimental/sysfs"
)

var moduleConfig wazy.ModuleConfig

// This example shows how to adapt a fs.FS to a sys.FS
func ExampleAdaptFS() {
	m := fstest.MapFS{
		"a/b.txt": &fstest.MapFile{Mode: 0o666},
		".":       &fstest.MapFile{Mode: 0o777 | fs.ModeDir},
	}
	root := &sysfs.AdaptFS{FS: m}

	moduleConfig = wazy.NewModuleConfig().
		WithFSConfig(wazy.NewFSConfig().(sysfs.FSConfig).WithSysFSMount(root, "/"))

	// Output:
}

// This example shows how to configure a sysfs.DirFS
func ExampleDirFS() {
	root := sysfs.DirFS(".")

	moduleConfig = wazy.NewModuleConfig().
		WithFSConfig(wazy.NewFSConfig().(sysfs.FSConfig).WithSysFSMount(root, "/"))

	// Output:
}

// This example shows how to configure a sysfs.ReadFS
func ExampleReadFS() {
	root := sysfs.DirFS(".")
	readOnly := &sysfs.ReadFS{FS: root}

	moduleConfig = wazy.NewModuleConfig().
		WithFSConfig(wazy.NewFSConfig().(sysfs.FSConfig).WithSysFSMount(readOnly, "/"))

	// Output:
}
