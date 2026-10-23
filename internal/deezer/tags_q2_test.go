package deezer

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/go-flac/flacvorbis"
	flac "github.com/go-flac/go-flac"
)

// Q2 Pod: the Q2 has little RAM (no swap), and a FLAC download is tens of MB. Tagging it must not
// hold the audio in memory: reading it whole, and building the tagged file whole, ran the Q2 out of
// memory, which froze it until the kernel killed q2-deezer ("Deezer isn't running").
func TestTagFLAC_StreamsTheAudio(t *testing.T) {
	raw, err := base64.StdEncoding.DecodeString(flacAudioB64)
	if err != nil {
		t.Fatal(err)
	}
	head, err := flac.ParseMetadata(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	start := 4
	for _, m := range head.Meta {
		start += 4 + len(m.Data)
	}
	// A Deezer-sized track: the fixture's frames, then 24 MB more audio-like bytes.
	const extra = 24 << 20
	path := filepath.Join(t.TempDir(), "big.flac.part")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	audio := sha256.New()
	w := io.MultiWriter(f, audio)
	if _, err := f.Write(raw[:start]); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(raw[start:]); err != nil {
		t.Fatal(err)
	}
	block := bytes.Repeat([]byte{0x5a, 0xa5, 0x3c, 0xc3}, 16<<10)
	for n := 0; n < extra; n += len(block) {
		if _, err := w.Write(block); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	want := audio.Sum(nil)

	tr := Track{Name: "Big", Artists: []Artist{{Name: "A"}}, AlbumName: "Alb"}
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	if err := tagFLAC(path, tr, makeTestJPEG(), 3, "2024"); err != nil {
		t.Fatalf("tagFLAC: %v", err)
	}
	runtime.ReadMemStats(&after)
	got := after.TotalAlloc - before.TotalAlloc
	t.Logf("tagging a %d MB FLAC allocated %d KB", extra>>20, got>>10)
	if got > 2<<20 {
		t.Errorf("tagging a %d MB FLAC allocated %d MB; it must stream the audio", extra>>20, got>>20)
	}

	// The tags are there, and the audio after them is byte for byte the original.
	out, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer out.Close()
	meta, err := flac.ParseMetadata(out)
	if err != nil {
		t.Fatalf("tagged file's metadata: %v", err)
	}
	if meta.Meta[0].Type != flac.StreamInfo {
		t.Fatalf("first block %v, want STREAMINFO", meta.Meta[0].Type)
	}
	var title string
	pictures := 0
	for _, m := range meta.Meta {
		switch m.Type {
		case flac.VorbisComment:
			c, err := flacvorbis.ParseFromMetaDataBlock(*m)
			if err != nil {
				t.Fatal(err)
			}
			if v, _ := c.Get(flacvorbis.FIELD_TITLE); len(v) > 0 {
				title = v[0]
			}
		case flac.Picture:
			pictures++
		}
	}
	if title != "Big" || pictures != 1 {
		t.Errorf("title %q, %d pictures; want Big and one cover", title, pictures)
	}
	tagged := sha256.New()
	if _, err := io.Copy(tagged, out); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(tagged.Sum(nil), want) {
		t.Error("the audio after the tags differs from the download's")
	}
}
