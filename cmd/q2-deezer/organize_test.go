package main

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

// Where the public API puts tracks on their albums (/track/<id>), for the organized layout: 1 and
// 2 on album 8, a two-disc compilation of Various Artists'; 4 and 5 on album 7, Broadcast's.
var placed = map[string]map[string]any{
	"/track/1": placedOn(1, 1, 8, "Witch Cults"),
	"/track/2": placedOn(3, 1, 8, "Witch Cults"),
	"/track/4": placedOn(1, 1, 7, "Tender Buttons"),
	"/track/5": placedOn(2, 1, 7, "Tender Buttons"),
}

func placedOn(pos, disc, album int, title string) map[string]any {
	return map[string]any{"track_position": pos, "disk_number": disc, "release_date": "2003-04-01",
		"artist": map[string]any{"id": 5, "name": "Broadcast"}, "album": map[string]any{"id": album, "title": title}}
}

var album8 = map[string]any{"id": 8, "title": "Witch Cults of the Radio Age", "cover_medium": coverURL("pic8"),
	"release_date": "2003-04-01", "artist": map[string]any{"id": 5080, "name": "Various Artists"}}

// tree is the Deezer folder's files, from it, sorted.
func (r *rig) tree() []string {
	var out []string
	filepath.Walk(r.folder, func(p string, fi os.FileInfo, err error) error {
		if err == nil && !fi.IsDir() {
			rel, _ := filepath.Rel(r.folder, p)
			out = append(out, rel)
		}
		return nil
	})
	sort.Strings(out)
	return out
}

// Organize Files is on without .deezer/organize, and its row turns it off and on, saved.
func TestOrganizeSetting(t *testing.T) {
	r := newRigOrganize(t, goodARL, newFakeDeezer(t), "")
	waitFor(t, "signed in", 5*time.Second, func() bool { return r.d.authState() == authOK })
	row := func() []string {
		_, rows := r.ask("menu")
		for _, row := range rows {
			if row[1] == "organize" {
				return row
			}
		}
		t.Fatalf("no Organize Files row in %q", rows)
		return nil
	}
	if got := row(); got[0] != "action" || got[2] != "Organize Files: On" || got[3] != "Artist / Year - Album / 01 - Song" {
		t.Fatalf("default %q", got)
	}
	if head, _ := r.ask("organize"); head[1] != "ok" || head[2] != "New downloads go straight in Deezer/" {
		t.Fatalf("off %q", head)
	}
	if b, _ := os.ReadFile(filepath.Join(r.dir, "organize")); string(b) != "off\n" || row()[2] != "Organize Files: Off" {
		t.Fatalf("off: file %q", b)
	}
	r.stop()
	r.start(newFakeDeezer(t))
	if got := row(); got[2] != "Organize Files: Off" { // kept across a restart
		t.Fatalf("after restart %q", got)
	}
	if head, _ := r.ask("organize"); head[2] != "New downloads go in Artist/Year - Album folders" || row()[2] != "Organize Files: On" {
		t.Fatalf("on %q", head)
	}
}

// Organized downloads: each song in its album artist's and album's (year) folders, numbered in track order
// (a disc first on an album of two), from a playlist as from an album; the tags it lacked filled
// in; the card browsed by artist and album, an album's songs in track order; found again after a
// restart, an interrupted download's temp file swept from an album's folder; and, turned off, the
// next download in Deezer/ itself, as before.
func TestOrganizedDownloads(t *testing.T) {
	f := newFakeDeezer(t)
	r := newRigOrganize(t, goodARL, f, "on")
	waitFor(t, "signed in", 5*time.Second, func() bool { return r.d.authState() == authOK })
	// A playlist: 1 and 2 come, 3 (only a preview here) fails and leaves no folder behind.
	if head, _ := r.ask("download", "playlist:99"); head[2] != "Downloading 3 songs" {
		t.Fatalf("playlist %q", head)
	}
	waitFor(t, "the playlist", 5*time.Second, func() bool {
		_, rows := r.ask("downloads")
		return len(rows) > 0 && rows[0][2] == "Try the Failed Again"
	})
	cults := filepath.Join("Various Artists", "2003 - Witch Cults of the Radio Age")
	want := []string{filepath.Join(cults, "1-01 - The Be Colony [1].flac"), filepath.Join(cults, "1-03 - Mother Is the Milky Way [2].flac")}
	if got := r.tree(); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("files %q", got)
	}
	if _, err := os.Stat(filepath.Join(r.folder, "Unknown Artist")); !os.IsNotExist(err) {
		t.Fatal("the failed song's folders left")
	}
	if n := f.count("/album/8"); n != 1 {
		t.Fatalf("album 8 asked %d times", n)
	}
	tags, pic, _ := vorbis(t, filepath.Join(r.folder, want[1]))
	if tags["TITLE"] != "Mother Is the Milky Way" || tags["ARTIST"] != "Broadcast" || tags["ALBUMARTIST"] != "Various Artists" ||
		tags["ALBUM"] != "Witch Cults of the Radio Age" || tags["TRACKNUMBER"] != "3" || tags["DISCNUMBER"] != "1" ||
		tags["DATE"] != "2003" || !pic {
		t.Fatalf("tags %v %v", tags, pic)
	}
	_, rows := r.ask("tracks", "playlist:99")
	if rows[2][0] != "local" || rows[2][1] != filepath.Join(r.folder, want[1]) || rows[2][4] != "album" {
		t.Fatalf("playlist row %q", rows[2])
	}
	r.ask("clearfailed")
	// An album: one disc, so no disc in the names; all on the card plays its folder in track order.
	r.ask("download", "album:7")
	tender := filepath.Join(r.folder, "Broadcast", "2005 - Tender Buttons")
	waitFor(t, "the album", 5*time.Second, func() bool {
		_, err := os.Stat(filepath.Join(tender, "02 - Valerie [5].flac"))
		return err == nil
	})
	if _, err := os.Stat(filepath.Join(tender, "01 - Tears in the Typing Pool [4].flac")); err != nil {
		t.Fatal(err)
	}
	if tags, _, _ := vorbis(t, filepath.Join(tender, "02 - Valerie [5].flac")); tags["TRACKNUMBER"] != "2" ||
		tags["ALBUMARTIST"] != "Broadcast" || tags["DISCNUMBER"] != "" || tags["LYRICS"] != "Valerie\nValerie\n" {
		t.Fatalf("album tags %v", tags)
	}
	if _, rows = r.ask("tracks", "album:7"); rows[0][0] != "local" || rows[0][2] != "All on the card" ||
		rows[0][1] != tender+"/" || rows[0][4] != "album" {
		t.Fatalf("all on the card %q", rows[0])
	}
	// Downloaded Songs: by artist, then album, each album's songs in track order.
	_, rows = r.ask("menu")
	if rows[0][0] != "list" || rows[0][1] != "cardartists" || rows[0][3] != "4 songs on the card" {
		t.Fatalf("menu %q", rows[0])
	}
	if _, rows = r.ask("cardartists"); strings.Join(titlesOf(rows), ",") != "list Broadcast,list Various Artists" ||
		rows[0][1] != "cardalbums" || rows[0][4] != "Broadcast" || rows[0][3] != "1 album" {
		t.Fatalf("artists %q", rows)
	}
	if _, rows = r.ask("cardalbums", "Various Artists"); len(rows) != 1 || rows[0][0] != "local" ||
		rows[0][1] != filepath.Join(r.folder, cults)+"/" || rows[0][4] != "album" || rows[0][3] != "2003 · 2 songs" ||
		rows[0][2] != "Witch Cults of the Radio Age" {
		t.Fatalf("albums %q", rows)
	}
	for _, bad := range []string{"..", "../.deezer", ""} {
		if head, _ := r.ask("cardalbums", bad); head[1] != "bad" {
			t.Fatalf("cardalbums %q: %q", bad, head)
		}
	}
	// Restarted: found where they are (a temp file an interrupted download left swept).
	part := filepath.Join(tender, "03 - Lunch Hour Pops [9]-4242.flac.part")
	r.stop()
	os.WriteFile(part, []byte("x"), 0o644)
	r.start(f)
	if _, err := os.Stat(part); !os.IsNotExist(err) {
		t.Fatal("the temp file not swept")
	}
	if head, _ := r.ask("download", "album:7"); head[2] != "Already on the card or in Downloads" {
		t.Fatalf("again %q", head)
	}
	// Turned off: the next one in Deezer/ itself, listed first as Unsorted Songs, newest first.
	r.ask("organize")
	r.ask("download", "flow#6")
	waitFor(t, "the flat one", 5*time.Second, func() bool {
		_, err := os.Stat(filepath.Join(r.folder, "Corporeal - Broadcast [6].flac"))
		return err == nil
	})
	if _, rows = r.ask("cardartists"); rows[0][0] != "local" || rows[0][2] != "Unsorted Songs" || rows[0][1] != r.folder+"/" ||
		rows[0][4] != "" || len(rows) != 3 {
		t.Fatalf("artists after %q", rows)
	}
	if _, rows = r.ask("tracks", "flow"); rows[1][0] != "local" || rows[1][4] != "" {
		t.Fatalf("flat row %q", rows[1])
	}
}

// v3.2's album folders, "<album> (<year>)", are renamed year first once at start; a v3.1 one,
// without its year, when the album's next song comes, its songs found at their new paths. An
// artist's albums list oldest first, undated ones last, each by its title with its year beside.
func TestOrganizedYearFolders(t *testing.T) {
	f := newFakeDeezer(t)
	r := newRigOrganize(t, goodARL, f, "on")
	r.stop()
	broadcast := filepath.Join(r.folder, "Broadcast")
	os.Remove(filepath.Join(r.dir, "layout")) // as a v3.2 card has none
	for _, p := range []string{"Tender Buttons/01 - Tears in the Typing Pool [4].flac", "Haha Sound (2003)/01 - Pendulum [97].flac",
		"Work and Non Work/01 - Accidentals [98].flac", "Ha Ha (1999)/01 - Echo's Answer [99].flac",
		"1999 - Ha Ha/02 - Two [96].flac"} {
		os.MkdirAll(filepath.Dir(filepath.Join(broadcast, p)), 0o755)
		os.WriteFile(filepath.Join(broadcast, p), plain("x", 64), 0o644)
	}
	r.start(f)
	if _, err := os.Stat(filepath.Join(broadcast, "2003 - Haha Sound", "01 - Pendulum [97].flac")); err != nil {
		t.Fatal("v3.2's folder not renamed:", err)
	}
	if _, err := os.Stat(filepath.Join(broadcast, "Ha Ha (1999)")); err != nil { // its name now taken: left
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(r.dir, "layout")); string(b) != "year-first\n" {
		t.Fatalf("layout %q", b)
	}
	waitFor(t, "signed in", 5*time.Second, func() bool { return r.d.authState() == authOK })
	if head, _ := r.ask("download", "album:7"); head[2] != "Downloading 1 song" { // 4 is on the card already
		t.Fatalf("album %q", head)
	}
	tender := filepath.Join(broadcast, "2005 - Tender Buttons")
	waitFor(t, "the album", 5*time.Second, func() bool {
		_, err := os.Stat(filepath.Join(tender, "02 - Valerie [5].flac"))
		return err == nil
	})
	if _, err := os.Stat(filepath.Join(tender, "01 - Tears in the Typing Pool [4].flac")); err != nil {
		t.Fatal("the old folder's song not moved:", err)
	}
	if _, err := os.Stat(filepath.Join(broadcast, "Tender Buttons")); !os.IsNotExist(err) {
		t.Fatal("the old folder left")
	}
	_, rows := r.ask("tracks", "album:7")
	if rows[0][1] != tender+"/" || rows[1][1] != filepath.Join(tender, "01 - Tears in the Typing Pool [4].flac") {
		t.Fatalf("rows %q", rows[:2])
	}
	_, rows = r.ask("cardalbums", "Broadcast")
	var got []string
	for _, row := range rows {
		got = append(got, row[2]+" "+row[3])
	}
	if strings.Join(got, ",") != "Ha Ha 1999 · 1 song,Ha Ha 1999 · 1 song,Haha Sound 2003 · 1 song,"+
		"Tender Buttons 2005 · 2 songs,Work and Non Work 1 song" {
		t.Fatalf("albums %q", got)
	}
	// Once only: a folder named so since is left alone.
	r.stop()
	os.MkdirAll(filepath.Join(broadcast, "Mine (2001)"), 0o755)
	r.start(f)
	if _, err := os.Stat(filepath.Join(broadcast, "Mine (2001)")); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "signed in", 5*time.Second, func() bool { return r.d.authState() == authOK }) // before it stops
}
