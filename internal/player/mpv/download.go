package mpv

import (
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

var (
	baseDownloadURL  = "https://github.com/vicnoah/mygo-cloudmusic/releases/download/libmpv-" + LibVersion + "/"
	cacheDirOverride string
)

// SetBaseURL overrides the download release base URL (useful for testing or mirrors).
func SetBaseURL(u string) {
	baseDownloadURL = u
}

// SetCacheDir overrides the cache directory for testing.
func SetCacheDir(d string) {
	cacheDirOverride = d
	invalidateDownloadState()
}

// CacheDir returns the directory path where downloaded libmpv binaries are kept.
func CacheDir() (string, error) {
	if cacheDirOverride != "" {
		return cacheDirOverride, nil
	}
	cache, err := os.UserCacheDir()
	if err != nil {
		cache = os.TempDir()
	}
	return filepath.Join(cache, "mygo-cloudmusic", "libmpv-"+LibVersion), nil
}

// TargetFile returns the target library file path inside CacheDir for the current platform.
func TargetFile() (string, error) {
	dir, err := CacheDir()
	if err != nil {
		return "", err
	}
	asset, ok := NativeAssets[Platform()]
	if !ok {
		return "", fmt.Errorf("mpv: unsupported platform %s", Platform())
	}
	return filepath.Join(dir, asset.Name), nil
}

// IsDownloaded reports whether the platform library is present in the cache
// and matches the release metadata. Size is always checked; when the release
// records a SHA256 the digest is verified too. Digest results are cached by
// path, size and mtime so status polling does not re-hash a multi-megabyte
// dylib on every frame.
func IsDownloaded() bool {
	p, err := TargetFile()
	if err != nil {
		return false
	}
	asset, ok := NativeAssets[Platform()]
	if !ok {
		return false
	}
	fi, err := os.Stat(p)
	if err != nil || fi.IsDir() || fi.Size() <= 1024 {
		return false
	}
	if asset.Size > 0 && fi.Size() != asset.Size {
		return false
	}
	if asset.SHA256 == "" {
		return true
	}
	if verified, ok := fileVerified(p, fi); ok && verified {
		return true
	}
	sum, err := fileSHA256(p)
	if err != nil || !strings.EqualFold(sum, asset.SHA256) {
		return false
	}
	markFileVerified(p, fi)
	return true
}

var (
	verifyMu       sync.Mutex
	verifyPath     string
	verifySize     int64
	verifyModTime  time.Time
	verifyAt       time.Time
	verifyCacheTTL = 5 * time.Minute
)

func fileVerified(path string, fi os.FileInfo) (bool, bool) {
	verifyMu.Lock()
	defer verifyMu.Unlock()
	if verifyPath != path || verifySize != fi.Size() || !verifyModTime.Equal(fi.ModTime()) {
		return false, false
	}
	if time.Since(verifyAt) > verifyCacheTTL {
		return false, false
	}
	return true, true
}

func markFileVerified(path string, fi os.FileInfo) {
	verifyMu.Lock()
	verifyPath, verifySize, verifyModTime = path, fi.Size(), fi.ModTime()
	verifyAt = time.Now()
	verifyMu.Unlock()
}

func invalidateVerifyCache() {
	verifyMu.Lock()
	verifyPath = ""
	verifyAt = time.Time{}
	verifyMu.Unlock()
}

func invalidateDownloadState() {
	invalidateVerifyCache()
	resetReadyCache()
}

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// closeLibraryHandleFn releases a handle returned by openLibrary. Unix
// builds install purego.Dlclose here; platforms without a dlclose binding
// keep the no-op.
var closeLibraryHandleFn = func(uintptr) {}

// SystemPath searches system paths to see if libmpv is already installed on
// the host. Probes are opened and closed again so checking every launch does
// not leak loader handles.
func SystemPath() string {
	for _, name := range SystemLibNames() {
		h, err := openLibrary(name)
		if err != nil || h == 0 {
			continue
		}
		sym, _ := librarySymbol(h, "mpv_create")
		closeLibraryHandleFn(h)
		if sym != 0 {
			return name
		}
	}
	return ""
}

var (
	readyMu    sync.Mutex
	readyOK    bool
	readySet   bool
	readyAt    time.Time
	readyTTL   = 30 * time.Second
	readyProbe = probeMPV
	readyNow   = time.Now
)

// Ready reports whether libmpv can actually be instantiated, not merely
// whether a file exists. A downloaded file that is truncated or corrupt is
// not usable. The answer is cached briefly because player and UI code query
// it frequently; a failed probe is retried after the same short TTL.
func Ready() bool {
	readyMu.Lock()
	defer readyMu.Unlock()
	now := readyNow()
	if readySet && now.Sub(readyAt) < readyTTL {
		return readyOK
	}
	readyOK = readyProbe()
	readySet = true
	readyAt = now
	return readyOK
}

func resetReadyCache() {
	readyMu.Lock()
	readySet = false
	readyAt = time.Time{}
	readyMu.Unlock()
}

// probeMPV proves the library can be loaded by creating a headless instance
// with a null audio output and destroying it immediately.
func probeMPV() bool {
	if SystemPath() == "" && !IsDownloaded() {
		return false
	}
	inst, err := NewInstance(map[string]string{"ao": "null"})
	if err != nil {
		return false
	}
	// A successful mpv_create followed by a failed initialize can still
	// return an instance because the client's raw return check predates the
	// purego sign-extension fix. Require the version property as proof.
	version := inst.GetProperty("mpv-version")
	inst.Close()
	return version != ""
}

var (
	dlMu   sync.Mutex
	cancel context.CancelFunc
)

// Download downloads and unpacks the platform's libmpv binary into CacheDir.
func Download(ctx context.Context, onProgress func(done, total int64)) error {
	dlMu.Lock()
	defer dlMu.Unlock()

	asset, ok := NativeAssets[Platform()]
	if !ok {
		return fmt.Errorf("mpv: no precompiled asset for platform %s", Platform())
	}

	dir, err := CacheDir()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("mpv: mkdir cache: %w", err)
	}

	targetPath := filepath.Join(dir, asset.Name)
	downloadURL := baseDownloadURL + asset.Asset

	req, err := http.NewRequestWithContext(ctx, "GET", downloadURL, nil)
	if err != nil {
		return err
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("mpv: fetch %s: %w", downloadURL, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("mpv: download failed: HTTP %s", resp.Status)
	}

	total := resp.ContentLength
	if total <= 0 {
		total = asset.GzSize
	}

	// Decompress gzip on the fly
	gzr, err := gzip.NewReader(&progressReader{
		r:          resp.Body,
		total:      total,
		onProgress: onProgress,
	})
	if err != nil {
		return fmt.Errorf("mpv: gzip reader: %w", err)
	}
	defer gzr.Close()

	tmpFile := targetPath + ".part"
	f, err := os.OpenFile(tmpFile, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0755)
	if err != nil {
		return err
	}

	hasher := sha256.New()
	mw := io.MultiWriter(f, hasher)

	if _, err := io.Copy(mw, gzr); err != nil {
		f.Close()
		_ = os.Remove(tmpFile)
		return fmt.Errorf("mpv: unpack: %w", err)
	}
	_ = f.Sync()
	f.Close()

	if asset.SHA256 != "" {
		sum := hex.EncodeToString(hasher.Sum(nil))
		if sum != asset.SHA256 {
			_ = os.Remove(tmpFile)
			return errors.New("mpv: checksum mismatch for downloaded library")
		}
	}

	if err := os.Rename(tmpFile, targetPath); err != nil {
		return err
	}

	invalidateDownloadState()
	return nil
}

// Delete removes the downloaded libmpv files from the cache directory.
func Delete() error {
	dlMu.Lock()
	defer dlMu.Unlock()
	dir, err := CacheDir()
	if err != nil {
		return err
	}
	err = os.RemoveAll(dir)
	invalidateDownloadState()
	return err
}

type progressReader struct {
	r          io.Reader
	done       int64
	total      int64
	onProgress func(done, total int64)
}

func (p *progressReader) Read(b []byte) (int, error) {
	n, err := p.r.Read(b)
	p.done += int64(n)
	if p.onProgress != nil {
		p.onProgress(p.done, p.total)
	}
	return n, err
}
