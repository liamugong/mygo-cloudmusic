package player

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"netease-music-mygo/internal/netease"
)

// A song whose stream metadata cannot be written is not kept: lookup finds
// a song only through its JSON, so the audio alone would take room and
// never play from the cache. The song goes back to its temporary file, so
// playback can still go on while it lasts.
func TestCacheRemovesSongWithoutMetadata(t *testing.T) {
	dir := t.TempDir()
	c := NewCache(dir, 1<<30)
	// A directory in the way makes the metadata write fail, while the song
	// itself still renames into place.
	if err := os.Mkdir(filepath.Join(dir, "1-exhigh.json"), 0o700); err != nil {
		t.Fatal(err)
	}
	st := &netease.Stream{Level: "exhigh", Type: "mp3"}
	f := c.part(st)
	if f == nil {
		t.Fatal("no temporary file for the song")
	}
	if _, err := f.Write(make([]byte, 100)); err != nil {
		t.Fatal(err)
	}
	f.Close()

	part := f.Name()
	path, ok := c.keep(1, st, part, 100)
	if ok {
		t.Fatal("kept a song whose metadata could not be written")
	}
	if path != part {
		t.Fatalf("keep = %q, want the part %q", path, part)
	}
	if _, err := os.Stat(filepath.Join(dir, "1-exhigh.mp3")); !os.IsNotExist(err) {
		t.Errorf("left the audio file: %v", err)
	}
	if _, err := os.Stat(part); err != nil {
		t.Errorf("part removed as well: %v", err)
	}
	if _, _, ok := c.lookup(1, "exhigh"); ok {
		t.Error("lookup found a song with no metadata")
	}
}

// A song whose metadata cannot be written still plays from its temporary
// file, and leaves no unusable entry behind: a cache write failure must
// not stop playback, and must not take room it can never serve.
func TestStreamPlaysWithoutMetadata(t *testing.T) {
	b, err := os.ReadFile("testdata/seek.mp3")
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write(b) }))
	defer srv.Close()
	dir := t.TempDir()
	c := NewCache(dir, 1<<30)
	if err := os.Mkdir(filepath.Join(dir, "1-exhigh.json"), 0o700); err != nil {
		t.Fatal(err)
	}
	ref := decodeAll(t, b)
	s, err := openStream(c.download(1, &netease.Stream{URL: srv.URL, Size: int64(len(b)), Level: "exhigh", Type: "mp3"}), int64(len(ref)/2)*1000/44100)
	if err != nil {
		t.Fatal(err)
	}
	var got []float32
	buf := make([]float32, 4096)
	for {
		n, err := s.Read(buf)
		got = append(got, buf[:n]...)
		if err != nil || n == 0 {
			break
		}
	}
	s.Close()
	if len(got) != len(ref) || maxDiff(got, ref) != 0 {
		t.Errorf("played %d of %d samples, off by up to %.4f", len(got), len(ref), maxDiff(got, ref))
	}
	if _, err := os.Stat(filepath.Join(dir, "1-exhigh.mp3")); !os.IsNotExist(err) {
		t.Errorf("left an audio file with no metadata: %v", err)
	}
	if _, _, ok := c.lookup(1, "exhigh"); ok {
		t.Error("lookup found a song with no metadata")
	}
}
