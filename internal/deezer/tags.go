package deezer

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/bogem/id3v2/v2"
	"github.com/go-flac/flacpicture"
	"github.com/go-flac/flacvorbis"
	flac "github.com/go-flac/go-flac"

	odlog "github.com/group-a-study/q2-deezer/internal/log"
)

// ArtworkFetcher fetches bytes for a Deezer artwork URL (or upgraded size).
// Supply a no-op or fake for tests so no network calls are made.
type ArtworkFetcher func(ctx context.Context, artworkURL string) ([]byte, error)

// upgradeArtworkURL follows existing patterns (gwCover uses 250x250; API
// responses use cover_medium / picture_medium). We request a reasonably large
// 500x500 variant suitable for embedded covers.
func upgradeArtworkURL(u string) string {
	if u == "" {
		return ""
	}
	for _, old := range []string{"250x250-000000-80-0-0.jpg", "250x250", "120x120", "56x56", "80x80"} {
		if strings.Contains(u, old) {
			rep := "500x500"
			if strings.Contains(old, "000000") {
				rep = "500x500-000000-80-0-0.jpg"
			}
			return strings.Replace(u, old, rep, 1)
		}
	}
	return u
}

func defaultArtworkFetcher(ctx context.Context, url string) ([]byte, error) {
	if url == "" {
		return nil, nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", downloadUserAgent)
	resp, err := downloadClient.Do(req)
	if err != nil {
		return nil, classifyNet(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("artwork CDN returned %s", resp.Status)
	}
	return io.ReadAll(resp.Body)
}

// tagFile attaches metadata (and optional cover) to a freshly written audio
// file. On any error the audio bytes are left untouched (no partial tag
// overwrite that would corrupt playback) and the error is returned for the
// caller to log as non-fatal.
func tagFile(ctx context.Context, path string, t Track, artworkURL string, trackNumber int, date string, fetch ArtworkFetcher) error {
	if fetch == nil {
		fetch = defaultArtworkFetcher
	}
	var art []byte
	if artworkURL != "" {
		if b, err := fetch(ctx, artworkURL); err == nil && len(b) > 0 {
			art = b
		} else if err != nil {
			odlog.Debug("tag: artwork fetch for %s: %v (continuing without art)", path, err)
		}
	}

	// saveTrack tags the in-progress "<name>.<ext>.part" temp file before
	// renaming it over the final name, so dispatch on the real extension
	// underneath the .part suffix.
	lower := strings.ToLower(strings.TrimSuffix(path, ".part"))
	if strings.HasSuffix(lower, ".flac") {
		return tagFLAC(path, t, art, trackNumber, date)
	}
	return tagMP3(path, t, art, trackNumber, date)
}

func tagMP3(path string, t Track, art []byte, trackNumber int, date string) (err error) {
	tag, err := id3v2.Open(path, id3v2.Options{Parse: false})
	if err != nil {
		return fmt.Errorf("id3v2 open: %w", err)
	}
	// Close closes the underlying file, flushing the write done by Save. If Save
	// succeeded, surface a Close/flush error instead of silently dropping it; if
	// Save already failed, keep that error (it's the more informative one).
	defer func() {
		if cerr := tag.Close(); cerr != nil && err == nil {
			err = fmt.Errorf("id3v2 close: %w", cerr)
		}
	}()

	tag.SetDefaultEncoding(id3v2.EncodingUTF8)
	tag.SetTitle(t.Name)
	tag.SetArtist(t.ArtistLine())
	tag.SetAlbum(t.AlbumName)
	if date != "" {
		tag.SetYear(date)
	}
	if trackNumber > 0 {
		id := tag.CommonID("Track number/Position in set")
		tag.AddTextFrame(id, id3v2.EncodingUTF8, fmt.Sprintf("%d", trackNumber))
	}
	if t.DiscNumber > 0 { // Q2
		tag.AddTextFrame(tag.CommonID("Part of a set"), id3v2.EncodingUTF8, fmt.Sprintf("%d", t.DiscNumber))
	}
	if t.AlbumArtist != "" { // Q2
		tag.AddTextFrame(tag.CommonID("Band/Orchestra/Accompaniment"), id3v2.EncodingUTF8, t.AlbumArtist)
	}
	if t.Lyrics != "" { // Q2: USLT, which TagLib (the Q2's, Navidrome's) reads as LYRICS; "XXX", no language
		tag.AddUnsynchronisedLyricsFrame(id3v2.UnsynchronisedLyricsFrame{Encoding: id3v2.EncodingUTF8,
			Language: "XXX", Lyrics: t.Lyrics})
	}
	if len(art) > 0 {
		pic := id3v2.PictureFrame{
			Encoding:    id3v2.EncodingUTF8,
			MimeType:    "image/jpeg",
			PictureType: id3v2.PTFrontCover,
			Description: "Front cover",
			Picture:     art,
		}
		tag.AddAttachedPicture(pic)
	}
	if err := tag.Save(); err != nil {
		return fmt.Errorf("id3v2 save: %w", err)
	}
	return nil
}

func tagFLAC(path string, t Track, art []byte, trackNumber int, date string) error {
	// Q2 Pod: only the metadata blocks are read; the audio after them is copied, never held.
	// go-flac's ParseFile read all of it and Marshal built the whole tagged file in memory, several
	// times the track's size in all, which ran the Q2 (no swap) out of memory on a FLAC download:
	// the Q2 froze until the kernel killed q2-deezer.
	src, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("flac open: %w", err)
	}
	defer src.Close()
	f, err := flac.ParseMetadata(src)
	if err != nil {
		return fmt.Errorf("flac parse: %w", err)
	}
	start := int64(4) // "fLaC", then each block's 4-byte header and its data
	for _, m := range f.Meta {
		start += 4 + int64(len(m.Data))
	}
	sync := make([]byte, 2)
	if _, err := src.ReadAt(sync, start); err != nil || sync[0] != 0xFF || sync[1]>>2 != 0x3E {
		return fmt.Errorf("flac parse: %w", flac.ErrorNoSyncCode)
	}

	// Preserve StreamInfo (must be first). Drop prior comment/picture blocks
	// so we control the final set.
	newMeta := make([]*flac.MetaDataBlock, 0, len(f.Meta)+2)
	if len(f.Meta) > 0 {
		newMeta = append(newMeta, f.Meta[0])
	}
	for _, m := range f.Meta[1:] {
		if m.Type != flac.VorbisComment && m.Type != flac.Picture {
			newMeta = append(newMeta, m)
		}
	}

	cmts := flacvorbis.New()
	// flacvorbis.Add only rejects malformed field names, and ours are library
	// constants, so this never fails in practice — but aggregate the errors
	// rather than silently dropping a comment on the floor.
	var addErr error
	add := func(field, val string) {
		if addErr == nil {
			addErr = cmts.Add(field, val)
		}
	}
	add(flacvorbis.FIELD_TITLE, t.Name)
	add(flacvorbis.FIELD_ARTIST, t.ArtistLine())
	add(flacvorbis.FIELD_ALBUM, t.AlbumName)
	if trackNumber > 0 {
		add(flacvorbis.FIELD_TRACKNUMBER, fmt.Sprintf("%d", trackNumber))
	}
	if t.DiscNumber > 0 { // Q2
		add("DISCNUMBER", fmt.Sprintf("%d", t.DiscNumber))
	}
	if t.AlbumArtist != "" { // Q2
		add("ALBUMARTIST", t.AlbumArtist)
	}
	if date != "" {
		add(flacvorbis.FIELD_DATE, date)
	}
	if t.Lyrics != "" { // Q2
		add("LYRICS", t.Lyrics)
	}
	if addErr != nil {
		return fmt.Errorf("flac vorbis comment: %w", addErr)
	}
	cm := cmts.Marshal()
	newMeta = append(newMeta, &cm)

	if len(art) > 0 {
		p, perr := flacpicture.NewFromImageData(flacpicture.PictureTypeFrontCover, "Front cover", art, "image/jpeg")
		if perr == nil {
			pm := p.Marshal()
			newMeta = append(newMeta, &pm)
		}
	}

	// go-flac's Save is an in-place O_TRUNC rewrite of path: a short write
	// (ENOSPC, crash) would destroy the just-downloaded audio while tagging
	// errors are treated as non-fatal by callers. Write the tagged copy next
	// to the original and atomically rename it over path only on full
	// success; on any error the original bytes are never touched.
	mode := os.FileMode(0o644)
	if fi, serr := os.Stat(path); serr == nil {
		mode = fi.Mode().Perm()
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".tag-*")
	if err != nil {
		return fmt.Errorf("flac tag temp: %w", err)
	}
	tmpName := tmp.Name()
	werr := func() error {
		if _, err := tmp.Write([]byte("fLaC")); err != nil {
			return err
		}
		for i, m := range newMeta {
			if _, err := tmp.Write(m.Marshal(i == len(newMeta)-1)); err != nil {
				return err
			}
		}
		if _, err := src.Seek(start, io.SeekStart); err != nil {
			return err
		}
		_, err := io.Copy(tmp, src)
		return err
	}()
	if werr != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return fmt.Errorf("flac tag write: %w", werr)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("flac tag close: %w", err)
	}
	_ = os.Chmod(tmpName, mode)
	if err := os.Rename(tmpName, path); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("flac tag rename: %w", err)
	}
	return nil
}
