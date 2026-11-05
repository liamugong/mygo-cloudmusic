package av3a

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// The helper protocol is one command: decode <mp4> <wav>. The wrapper must
// report a missing output rather than let MPV load a zero byte file.
func TestDecodeWithHelper(t *testing.T) {
	dir := t.TempDir()
	helper := filepath.Join(dir, "helper")
	script := "#!/bin/sh\nprintf 'RIFF' > \"$3\"\n"
	if err := os.WriteFile(helper, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MYGO_AV3A_BINARY", helper)
	out := filepath.Join(dir, "out.wav")
	if err := Decode(context.Background(), "in.mp4", out); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(out); err != nil || fi.Size() == 0 {
		t.Fatalf("output: %v %v", fi, err)
	}
}

// A helper that cannot run is not offered as installed: a copy that lost
// its executable bit used to be returned by BinaryPath and then failed to
// start with a bare permission error.
func TestBinaryPathRequiresExecutable(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("executable bits are not used on Windows")
	}
	dir := t.TempDir()
	helper := filepath.Join(dir, "helper")
	if err := os.WriteFile(helper, []byte("#!/bin/sh\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MYGO_AV3A_BINARY", helper)
	if got := BinaryPath(); got != "" {
		t.Fatalf("BinaryPath() = %q, want empty", got)
	}
	if Ready() {
		t.Fatal("Ready() = true for a helper that cannot run")
	}
	err := Decode(context.Background(), "in.mp4", filepath.Join(dir, "out.wav"))
	if !errors.Is(err, ErrNotExecutable) {
		t.Fatalf("Decode() = %v, want ErrNotExecutable", err)
	}
}
