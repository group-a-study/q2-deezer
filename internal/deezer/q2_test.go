package deezer

import (
	"bytes"
	"context"
	"crypto/cipher"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/go-flac/flacvorbis"
	flac "github.com/go-flac/go-flac"
	"golang.org/x/crypto/blowfish"
)

// Q2 Pod's additions (q2.go): the favourites, an album's details, and a looked-up track's download.

func TestFavorites(t *testing.T) {
	c := New("dummy")
	loggedInPremium(c)
	c.mu.Lock()
	c.userID = "42"
	c.mu.Unlock()
	var bodies []string
	c.http = &http.Client{Transport: rtFunc(func(r *http.Request) (*http.Response, error) {
		b, _ := io.ReadAll(r.Body)
		bodies = append(bodies, string(b))
		var sb strings.Builder
		from, n := 0, 100
		if strings.Contains(string(b), `"index":100`) {
			from, n = 100, 3
		}
		if strings.Contains(string(b), `"tab":"albums"`) {
			sb.WriteString(`{"error":{},"results":{"TAB":{"albums":{"data":[`)
			for i := from; i < from+n; i++ {
				if i > from {
					sb.WriteByte(',')
				}
				fmt.Fprintf(&sb, `{"ALB_ID":"%d","ALB_TITLE":"Album %d","ART_ID":7,"ART_NAME":"Broadcast","ALB_PICTURE":"p%d"}`, i, i, i)
			}
		} else {
			sb.WriteString(`{"error":{},"results":{"TAB":{"artists":{"data":[`)
			fmt.Fprintf(&sb, `{"ART_ID":"9","ART_NAME":"Stereolab","NB_FAN":"1200"}`)
		}
		sb.WriteString(`]}}}}`)
		return jsonResponse([]byte(sb.String())), nil
	})}
	albums, err := c.FavoriteAlbums()
	if err != nil || len(albums) != 103 || albums[102].Name != "Album 102" || albums[0].Artists[0].Name != "Broadcast" ||
		albums[0].ArtworkURL != gwCover("p0") {
		t.Fatalf("albums %d %v %+v", len(albums), err, albums[:1])
	}
	if !strings.Contains(bodies[0], `"user_id":"42"`) || !strings.Contains(bodies[1], `"index":100`) {
		t.Fatalf("bodies %q", bodies)
	}
	artists, err := c.FavoriteArtists()
	if err != nil || len(artists) != 1 || artists[0].ID != "9" || artists[0].NbFans != 1200 {
		t.Fatalf("artists %+v %v", artists, err)
	}
}

// Deezer's pageProfile sends the whole tab whatever nb and index ask, with its total (an account
// with 239 albums, 141 playlists and 234 artists, 2026-10-22): one request, not one per 100 until
// a short page that never comes. Without a total, a second page bringing nothing new ends it; an
// empty profile's TAB is [].
func TestProfileWholeTab(t *testing.T) {
	c := New("dummy")
	loggedInPremium(c)
	calls, total, empty := 0, true, false
	c.http = &http.Client{Transport: rtFunc(func(r *http.Request) (*http.Response, error) {
		b, _ := io.ReadAll(r.Body)
		calls++
		if empty {
			return jsonResponse([]byte(`{"error":[],"results":{"TAB":[],"DATA":{}}}`)), nil
		}
		tab := "albums"
		if strings.Contains(string(b), `"tab":"playlists"`) {
			tab = "playlists"
		}
		var sb strings.Builder
		fmt.Fprintf(&sb, `{"error":[],"results":{"TAB":{"%s":{"data":[`, tab)
		for i := 0; i < 239; i++ {
			if i > 0 {
				sb.WriteByte(',')
			}
			if tab == "albums" {
				fmt.Fprintf(&sb, `{"ALB_ID":"%d","ALB_TITLE":"Album %d","ART_ID":"7","ART_NAME":"Broadcast","ALB_PICTURE":"p"}`, 1000+i, i)
			} else {
				fmt.Fprintf(&sb, `{"PLAYLIST_ID":"%d","TITLE":"Pl %d","NB_SONG":12,"PLAYLIST_PICTURE":"p"}`, 1000+i, i)
			}
		}
		sb.WriteString(`]`)
		if total {
			sb.WriteString(`,"count":239,"total":239`)
		}
		fmt.Fprintf(&sb, `},"%s_suggest":{"data":[],"count":0,"total":0}}}}`, tab)
		return jsonResponse([]byte(sb.String())), nil
	})}
	albums, err := c.FavoriteAlbums()
	if err != nil || len(albums) != 239 || albums[238].Name != "Album 238" || calls != 1 {
		t.Fatalf("albums: %d, %v, %d requests", len(albums), err, calls)
	}
	calls = 0
	pls, err := c.Playlists()
	if err != nil || len(pls) != 239 || pls[0].TrackCount != 12 || calls != 1 {
		t.Fatalf("playlists: %d, %v, %d requests", len(pls), err, calls)
	}
	calls, total = 0, false
	if albums, err = c.FavoriteAlbums(); err != nil || len(albums) != 239 || calls != 2 {
		t.Fatalf("albums without a total: %d, %v, %d requests", len(albums), err, calls)
	}
	calls, empty = 0, true
	if albums, err = c.FavoriteAlbums(); err != nil || len(albums) != 0 || calls != 1 {
		t.Fatalf("no albums: %d, %v, %d requests", len(albums), err, calls)
	}
}

func TestAlbumInfo(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/album/302127" {
			t.Errorf("path %s", r.URL.Path)
		}
		io.WriteString(w, `{"id":302127,"title":"Discovery","cover_medium":"https://x/250x250.jpg","release_date":"2001-03-07","artist":{"id":27,"name":"Daft Punk"}}`)
	}))
	defer ts.Close()
	c := New("dummy")
	c.restURLOverride = ts.URL
	a, year, err := c.AlbumInfo("302127")
	if err != nil || a.Name != "Discovery" || a.ArtworkURL != "https://x/250x250.jpg" || a.Artists[0].Name != "Daft Punk" || year != "2001" {
		t.Fatalf("%+v %q %v", a, year, err)
	}
}

// SaveTrackInfo keeps SaveTrack's gate: signed in, with a paid plan.
func TestSaveTrackInfoGate(t *testing.T) {
	c := New("dummy")
	if _, err := c.SaveTrackInfo(context.Background(), Track{ID: "1"}, t.TempDir()); err == nil || !strings.Contains(err.Error(), "not logged in") {
		t.Fatalf("signed out: %v", err)
	}
	c.mu.Lock()
	c.apiToken = "tok"
	c.mu.Unlock()
	if _, err := c.SaveTrackInfo(context.Background(), Track{ID: "1"}, t.TempDir()); !errors.Is(err, ErrPremiumRequired) {
		t.Fatalf("free: %v", err)
	}
}

// A FLAC download is tagged as a FLAC: Vorbis comments in its metadata, the audio as decrypted,
// the file still starting fLaC (OpenDeezer's temp name lost the extension tagFile goes by, so it
// was tagged as an MP3: an ID3 tag before fLaC).
func TestSaveTrackFLACTagged(t *testing.T) {
	audio := append([]byte("fLaC"), 0x80, 0, 0, 34)
	audio = append(audio, make([]byte, 34)...)
	audio = append(audio, 0xff, 0xf8) // a frame's sync code
	for i := 0; len(audio) < 9000; i++ {
		audio = append(audio, byte(i*7))
	}
	enc := append([]byte(nil), audio...)
	blk, _ := blowfish.NewCipher(BlowfishKey("77"))
	for i := 0; i+chunkSize <= len(enc); i += chunkSize {
		if (i/chunkSize)%3 == 0 {
			cipher.NewCBCEncrypter(blk, stripeIV).CryptBlocks(enc[i:i+chunkSize], enc[i:i+chunkSize])
		}
	}
	cdn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.ServeContent(w, r, "x", time.Time{}, bytes.NewReader(enc))
	}))
	defer cdn.Close()
	c := New("dummy")
	c.mu.Lock()
	c.apiToken, c.licenseToken, c.userID, c.canHQ, c.canHiFi = "tok", "lic", "42", true, true
	c.mu.Unlock()
	c.SetQuality(QualityLossless)
	c.http = &http.Client{Transport: rtFunc(func(r *http.Request) (*http.Response, error) {
		if strings.Contains(r.URL.Path, "gw-light.php") {
			return jsonResponse([]byte(`{"error":{},"results":{"TRACK_TOKEN":"t77","GAIN":"0"}}`)), nil
		}
		return jsonResponse([]byte(`{"data":[{"media":[{"format":"FLAC","sources":[{"url":"` + cdn.URL + `/77"}]}]}]}`)), nil
	})}
	dir := t.TempDir()
	tr := Track{ID: "77", Name: "Pendulum", Artists: []Artist{{Name: "Broadcast"}}, AlbumName: "Haha Sound", TrackNumber: 3, Year: "2003"}
	path, err := c.SaveTrackInfo(context.Background(), tr, dir)
	if err != nil || filepath.Base(path) != "Pendulum - Broadcast [77].flac" {
		t.Fatalf("%s %v", path, err)
	}
	f, err := flac.ParseFile(path)
	if err != nil {
		b, _ := os.ReadFile(path)
		t.Fatalf("not a FLAC: %v (%q)", err, b[:8])
	}
	tags := map[string]string{}
	for _, m := range f.Meta {
		if m.Type == flac.VorbisComment {
			vc, _ := flacvorbis.ParseFromMetaDataBlock(*m)
			for _, kv := range vc.Comments {
				k, v, _ := strings.Cut(kv, "=")
				tags[k] = v
			}
		}
	}
	if tags["TITLE"] != "Pendulum" || tags["ALBUM"] != "Haha Sound" || tags["TRACKNUMBER"] != "3" || tags["DATE"] != "2003" {
		t.Fatalf("tags %v", tags)
	}
	if !bytes.Equal([]byte(f.Frames), audio[42:]) {
		t.Fatal("the audio isn't the decrypted stream")
	}
	if left, _ := filepath.Glob(filepath.Join(dir, "*.part")); len(left) > 0 {
		t.Fatalf("left %v", left)
	}
}

// The organized layout's names: numbered in track order, the [id] kept however long the title, and
// nothing a FAT card refuses.
func TestOrganizedNames(t *testing.T) {
	tr := Track{ID: "42", Name: "Song"}
	for _, c := range []struct {
		p    Placement
		want string
	}{
		{Placement{Number: 7}, "07 - Song [42]"},
		{Placement{Number: 7, Disc: 2, Discs: 2}, "2-07 - Song [42]"},
		{Placement{Number: 7, Disc: 1, Discs: 1}, "07 - Song [42]"},
		{Placement{Number: 7, Tracks: 120}, "007 - Song [42]"},
		{Placement{Number: 104, Tracks: 120}, "104 - Song [42]"},
		{Placement{}, "Song [42]"},
	} {
		if got := OrganizedName(tr, c.p); got != c.want {
			t.Errorf("%+v: %q, want %q", c.p, got, c.want)
		}
	}
	if got := OrganizedName(Track{ID: "1", Name: `AC/DC: "Live"?`}, Placement{Number: 1}); got != `01 - AC_DC_ _Live__ [1]` {
		t.Errorf("unsafe %q", got)
	}
	long := OrganizedName(Track{ID: "123456789", Name: strings.Repeat("é", 200)}, Placement{Number: 1})
	if !strings.HasSuffix(long, " [123456789]") || len(long) > 180 || !utf8.ValidString(long) {
		t.Errorf("long %q (%d bytes)", long, len(long))
	}
	for _, c := range [][4]string{{"Tender Buttons", "2005", "2005 - Tender Buttons"}, {"Tender Buttons", "", "Tender Buttons"},
		{"Tender Buttons", "05", "Tender Buttons"}, {"", "2005", "2005 - Unknown"}, {"AC/DC: Live?", "1992", "1992 - AC_DC_ Live_"},
		{strings.Repeat("a", 150), "2005", "2005 - " + strings.Repeat("a", 93)}} {
		if got := AlbumFolderName(c[0], c[1], "Unknown"); got != c[2] {
			t.Errorf("AlbumFolderName(%q, %q) = %q, want %q", c[0], c[1], got, c[2])
		}
	}
	if got := OldAlbumFolderNames("AC/DC: Live?", "1992", "Unknown"); strings.Join(got, "|") != "AC_DC_ Live_|AC_DC_ Live_ (1992)" {
		t.Errorf("OldAlbumFolderNames = %q", got)
	}
	if got := OldAlbumFolderNames("Tender Buttons", "", "Unknown"); strings.Join(got, "|") != "Tender Buttons" {
		t.Errorf("OldAlbumFolderNames, no year = %q", got)
	}
	for in, want := range map[string]string{"": "Unknown", " . ": "Unknown", "track": "track", "AC/DC": "AC_DC",
		"Mr. Bungle.": "Mr. Bungle", " Björk ": "Björk", strings.Repeat("a", 150): strings.Repeat("a", 100)} {
		if got := FolderName(in, "Unknown"); got != want {
			t.Errorf("FolderName(%q) = %q, want %q", in, got, want)
		}
	}
}

// Lyrics embed as LRC when Deezer has them in time, its untimed blank lines left out, else as
// plain text with its line ends tidied.
func TestLyricsText(t *testing.T) {
	synced := Lyrics{Plain: "a\r\nb", Synced: []LyricLine{{TimeMS: 1500, Text: "One "}, {Text: ""},
		{TimeMS: 3_723_456, Text: "Two"}}}
	if got := LyricsText(synced); got != "[00:01.50]One\n[62:03.45]Two\n" {
		t.Errorf("synced %q", got)
	}
	if got := LyricsText(Lyrics{Plain: "One\r\nTwo\r\n"}); got != "One\nTwo\n" {
		t.Errorf("plain %q", got)
	}
	if got := LyricsText(Lyrics{Plain: "One", Synced: []LyricLine{{Text: " "}}}); got != "One\n" { // no timed line: plain
		t.Errorf("blank synced %q", got)
	}
	if got := LyricsText(Lyrics{}); got != "" {
		t.Errorf("none %q", got)
	}
}

// The package's tests answer for Deezer alone; the LRCLIB tests below ask it on purpose.
func init() { lrclibSearch = "" }

// lrclibClient answers LRCLIB's search with replies in turn (a status, then a body) and Deezer's
// song.getLyrics with one synced line; deezer counts Deezer's answers, asked the queries LRCLIB got.
func lrclibClient(t *testing.T, replies []string, deezer *int, asked *[]string) *Client {
	t.Helper()
	old, wait := lrclibSearch, lrclibWait
	lrclibSearch, lrclibWait = "https://lrclib.test/api/search", 0
	t.Cleanup(func() { lrclibSearch, lrclibWait = old, wait })
	c := New("dummy")
	loggedInPremium(c)
	c.http = &http.Client{Transport: rtFunc(func(r *http.Request) (*http.Response, error) {
		if strings.Contains(r.URL.Path, "gw-light.php") {
			*deezer++
			return jsonResponse([]byte(`{"error":{},"results":{"LYRICS_TEXT":"Early","LYRICS_SYNC_JSON":[{"milliseconds":"1000","line":"Early"}]}}`)), nil
		}
		if r.Header.Get("User-Agent") != lrclibAgent {
			t.Errorf("User-Agent %q", r.Header.Get("User-Agent"))
		}
		*asked = append(*asked, r.URL.RawQuery)
		if len(replies) == 0 {
			t.Fatal("asked LRCLIB once too often")
		}
		status, body, _ := strings.Cut(replies[0], " ")
		replies = replies[1:]
		resp := jsonResponse([]byte(body))
		resp.StatusCode, resp.Status = map[string]int{"200": 200, "503": 503, "404": 404}[status], status
		return resp, nil
	})}
	return c
}

// A download's lyrics are LRCLIB's first synced result for the title and first artist, its credit
// lines gone, asked again while it's busy; Deezer's early-timed lines only when LRCLIB has none.
func TestLyricsTagLRCLIB(t *testing.T) {
	tr := Track{ID: "8086126", Name: "Rolling in the Deep", Artists: []Artist{{Name: "Adele"}, {Name: "Guest"}}}
	var deezer int
	var asked []string
	c := lrclibClient(t, []string{"503 {}", "503 {}",
		`200 [{"syncedLyrics":null},{"syncedLyrics":"[00:00.50] Producer : Paul Epworth\r\n[00:01.00] \r\n[00:05.10] There's a fire starting in my heart\r\n[00:09.72] Reaching a fever pitch\r\n[03:45.05] \r\n"}]`},
		&deezer, &asked)
	want := "[00:01.00] \n[00:05.10] There's a fire starting in my heart\n[00:09.72] Reaching a fever pitch\n[03:45.05]\n"
	if got := c.LyricsTag(tr); got != want || deezer != 0 {
		t.Errorf("LRCLIB's: %q, Deezer asked %d times", got, deezer)
	}
	if len(asked) != 3 || asked[0] != "artist_name=Adele&track_name=Rolling+in+the+Deep" {
		t.Errorf("queries %q", asked)
	}

	for _, reply := range []string{`200 []`, `200 [{"syncedLyrics":"[00:01.00] Lyrics : Someone\n"}]`, `404 {}`} {
		deezer, asked = 0, nil
		c = lrclibClient(t, []string{reply}, &deezer, &asked)
		if got := c.LyricsTag(tr); got != "[00:01.00]Early\n" || deezer != 1 || len(asked) != 1 {
			t.Errorf("%s: %q, Deezer asked %d times, LRCLIB %d", reply, got, deezer, len(asked))
		}
	}

	deezer, asked = 0, nil
	c = lrclibClient(t, []string{"503 {}", "503 {}", "503 {}"}, &deezer, &asked)
	if got := c.LyricsTag(tr); got != "[00:01.00]Early\n" || len(asked) != lrclibTries {
		t.Errorf("busy throughout: %q after %d requests", got, len(asked))
	}
}

func TestLrcBody(t *testing.T) {
	for in, want := range map[string]string{
		"[00:01.00] Composer : A\n\n[00:02.00] Lyricist : B\n[00:03.00] Sung\n": "[00:03.00] Sung\n",
		"[ar:Adele]\n[00:01.00] A : B\n[00:02.00] Sung\n":                       "[ar:Adele]\n[00:01.00] A : B\n[00:02.00] Sung\n",
		"[00:01.00] Credits : only\n":                                           "",
		"[00:01.00]\n[00:02.00] Sung\n":                                         "[00:01.00]\n[00:02.00] Sung\n",
	} {
		if got := lrcBody(in); got != want {
			t.Errorf("lrcBody(%q) = %q, want %q", in, got, want)
		}
	}
}
