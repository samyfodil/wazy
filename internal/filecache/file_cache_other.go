//go:build !windows

package filecache

import "os"

func openEntry(path string) (*os.File, error) { return os.Open(path) }

func replaceEntry(tmp, path string) error { return os.Rename(tmp, path) }

func removeEntry(path string) error { return os.Remove(path) }
