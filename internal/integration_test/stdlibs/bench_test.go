package native_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/samyfodil/wazy"
	"github.com/samyfodil/wazy/imports/wasi_snapshot_preview1"
	"github.com/samyfodil/wazy/internal/testing/require"
	"github.com/samyfodil/wazy/sys"
)

func BenchmarkZig(b *testing.B) {
	c := wazy.NewRuntimeConfigCompiler()
	runtBenches(b, context.Background(), c, zigTestCase)
}

func BenchmarkWasip1(b *testing.B) {
	c := wazy.NewRuntimeConfigCompiler()
	runtBenches(b, context.Background(), c, wasip1TestCase)
}

type testCase struct {
	name, dir    string
	readTestCase func(fpath string, fname string) (_ []byte, c wazy.ModuleConfig, stdout, stderr *os.File, err error)
}

var (
	zigTestCase = testCase{
		name: "zig",
		dir:  "testdata/zig/",
		readTestCase: func(fpath string, fname string) (_ []byte, c wazy.ModuleConfig, stdout, stderr *os.File, err error) {
			bin, err := os.ReadFile(fpath)
			c, stdout, stderr = defaultModuleConfig()
			c = c.WithFSConfig(wazy.NewFSConfig().WithDirMount(".", "/")).
				WithArgs("test.wasm")
			return bin, c, stdout, stderr, err
		},
	}
	wasip1TestCase = testCase{
		name: "wasip1",
		dir:  "testdata/go/",
		readTestCase: func(fpath string, fname string) (_ []byte, c wazy.ModuleConfig, stdout, stderr *os.File, err error) {
			if !strings.HasSuffix(fname, ".test") {
				return nil, nil, nil, nil, nil
			}
			bin, err := os.ReadFile(fpath)
			if err != nil {
				return nil, nil, nil, nil, err
			}
			out, err := exec.Command("go", "env", "GOROOT").Output()
			if err != nil {
				return nil, nil, nil, nil, err
			}
			goroot := string(bytes.TrimRight(out, "\n"))

			fsuffixstripped := strings.ReplaceAll(fname, ".test", "")
			inferredpath := strings.ReplaceAll(fsuffixstripped, "_", "/")
			testdir := filepath.Join(goroot, "src", inferredpath)
			err = os.Chdir(testdir)

			sysroot := filepath.VolumeName(testdir) + string(os.PathSeparator)
			normalizedTestdir := normalizeOsPath(testdir)

			fsConfig := wazy.NewFSConfig().WithDirMount(sysroot, "/")
			if runtime.GOOS == "windows" {
				fsConfig = fsConfig.WithFSMount(devDir, "/dev").WithFSMount(etcDir, "/etc")
			}
			c, stdout, stderr = defaultModuleConfig()
			c = c.WithFSConfig(fsConfig).
				WithEnv("PWD", normalizedTestdir).
				WithEnv("GOWASIRUNTIME", "wazy")

			args := []string{fname, "-test.short", "-test.v"}
			if runtime.GOOS == "windows" {
				c = c.WithEnv("GOROOT", normalizeOsPath(goroot))
				// Two tests ask for what a Windows host cannot do. No Windows
				// file name holds a colon or a backslash, and Go skips the first
				// on Windows itself. The second renames a directory while files
				// in it are open, which Windows refuses; Go accepts that failure
				// on Windows, but the test runs in the guest, where GOOS is
				// wasip1.
				args = append(args, "-test.skip=^(TestDirFSPathsValid|TestRootRaceRenameDir)$")
			}
			c = c.WithArgs(args...)

			return bin, c, stdout, stderr, err
		},
	}
)

func runtBenches(b *testing.B, ctx context.Context, rc wazy.RuntimeConfig, tc testCase) {
	cwd, _ := os.Getwd()
	files, err := os.ReadDir(tc.dir)
	require.NoError(b, err)
	for _, f := range files {
		fname := f.Name()
		// Ensure we are on root dir.
		err = os.Chdir(cwd)
		require.NoError(b, err)

		fpath := filepath.Join(cwd, tc.dir, fname)
		bin, modCfg, stdout, stderr, err := tc.readTestCase(fpath, fname)
		require.NoError(b, err)
		if bin == nil {
			continue
		}

		b.Run("Compile/"+fname, func(b *testing.B) {
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				r := wazy.NewRuntimeWithConfig(ctx, rc)
				_, err := r.CompileModule(ctx, bin)
				require.NoError(b, err)
				require.NoError(b, r.Close(ctx))
			}
		})
		b.Run("Run/"+fname, func(b *testing.B) {
			r := wazy.NewRuntimeWithConfig(ctx, rc)
			wasi_snapshot_preview1.MustInstantiate(ctx, r)
			b.Cleanup(func() { r.Close(ctx) })

			cm, err := r.CompileModule(ctx, bin)
			require.NoError(b, err)

			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				// Instantiate in the loop as _start cannot be called multiple times.
				m, err := r.InstantiateModule(ctx, cm, modCfg)
				requireZeroExitCode(b, err, stdout, stderr)
				require.NoError(b, m.Close(ctx))
			}
		})
	}
}

// Normalize an absolute path to a Unix-style path, regardless if it is a Windows path.
func normalizeOsPath(path string) string {
	// Remove volume name. This is '/' on *Nix and 'C:' (with C being any letter identifier).
	root := filepath.VolumeName(path)
	testdirnoprefix := path[len(root):]
	// Normalizes all the path separators to a Unix separator.
	testdirnormalized := strings.ReplaceAll(testdirnoprefix, string(os.PathSeparator), "/")
	return testdirnormalized
}

func defaultModuleConfig() (c wazy.ModuleConfig, stdout, stderr *os.File) {
	var err error
	// Note: do not use os.Stdout or os.Stderr as they will mess up the `-bench` output to be fed to the benchstat tool.
	stdout, err = os.CreateTemp("", "")
	if err != nil {
		panic(err)
	}
	stderr, err = os.CreateTemp("", "")
	if err != nil {
		panic(err)
	}
	c = wazy.NewModuleConfig().
		WithSysNanosleep().
		WithSysNanotime().
		WithSysWalltime().
		WithRandSource(rand.Reader).
		// Some tests require Stdout and Stderr to be present.
		WithStdout(stdout).
		WithStderr(stderr)
	return
}

func requireZeroExitCode(b *testing.B, err error, stdout, stderr *os.File) {
	b.Helper()
	if se, ok := err.(*sys.ExitError); ok {
		if se.ExitCode() != 0 { // Don't err on success.
			stdout.Seek(0, io.SeekStart)
			stderr.Seek(0, io.SeekStart)
			stdoutBytes, _ := io.ReadAll(stdout)
			stderrBytes, _ := io.ReadAll(stderr)
			require.NoError(b, err, "stdout: %s\nstderr: %s", string(stdoutBytes), string(stderrBytes))
		}
	} else if err != nil {
		stdout.Seek(0, io.SeekStart)
		stderr.Seek(0, io.SeekStart)
		stdoutBytes, _ := io.ReadAll(stdout)
		stderrBytes, _ := io.ReadAll(stderr)
		require.NoError(b, err, "stdout: %s\nstderr: %s", string(stdoutBytes), string(stderrBytes))
	}
}
