package main

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/group-a-study/q2-deezer/internal/deezer"
	"github.com/group-a-study/q2-deezer/internal/q2"
)

// The download queue (contrib/shanlingq2/README, Downloads): as OpenDeezer downloads, every track
// is fetched, decrypted, tagged (title, artist, album, number, year, cover) and renamed into place
// under Deezer/ on the card: with Organize Files on, in its album's folder,
// "<album artist>/<album> (<year>)/07 - Title [id].flac", its album, disc and number looked up first
// (TrackPlacement); off, as OpenDeezer does, in Deezer/ itself as "Title - Artist [id].flac" (or
// .mp3). The Q2 plays them from there, as local music. One downloads at a time (the Q2's card and
// CPU); the ones waiting are kept in .deezer/queue.json, so they carry on after a restart or USB
// storage.

// cardReady is whether the card is there (not lent to USB storage); tests have one that is.
var cardReady = q2.CardMounted

// retryIn is how long a stalled queue (offline, signed out, the card away) waits before it tries again.
var retryIn = 30 * time.Second

// maxTries is how many times a track may be under way when q2-deezer dies (killed, out of memory,
// a crash) before the queue moves it to the failed ones, so one track can't keep the add-on down: its
// launcher restarts it, and the queue would start that track again first.
const maxTries = 3

type item struct {
	Track deezer.Track `json:"track"`
	Err   string       `json:"err,omitempty"` // a failed one's reason
	Path  string       `json:"path,omitempty"`
	Tries int          `json:"tries,omitempty"` // the times it was under way when q2-deezer died
}

type downloads struct {
	d      *daemon
	folder string // Deezer/ on the card
	file   string // .deezer/queue.json

	mu        sync.Mutex
	wmu       sync.Mutex // one queue.json write at a time: they share its .tmp
	waiting   []*item
	cur       *item
	cancel    context.CancelFunc // the current download's
	dropped   bool               // the current one removed from the queue, not stopped by a quit
	failed    []*item
	done      []*item           // this session's, oldest first
	have      map[string]string // track id: its file in the folder
	scanned   time.Time
	stalled   string // why the queue doesn't move now; "" when it does
	wake      chan struct{}
	saveTimer *time.Timer
}

func newDownloads(d *daemon, folder, file string) *downloads {
	dl := &downloads{d: d, folder: folder, file: file, wake: make(chan struct{}, 1), have: map[string]string{}}
	var kept []*item
	if b, err := os.ReadFile(file); err == nil {
		json.Unmarshal(b, &kept)
	}
	for _, it := range kept {
		if it != nil && it.Track.ID != "" {
			dl.waiting = append(dl.waiting, &item{Track: it.Track, Tries: it.Tries})
		}
	}
	dl.sweep()
	dl.rescan()
	return dl
}

// leftover is a download's or its tagging's temp file (deezer.saveTrack, tagFLAC): "<name>-<n>.flac.part"
// and "<that>.tag-<n>".
var leftover = regexp.MustCompile(`\.(?i:flac|mp3)\.part(\.tag-[0-9]+)?$`)

// walk calls fn for each file in the folder and its Artist/Album folders (hidden ones skipped).
func (dl *downloads) walk(fn func(path, name string)) {
	filepath.WalkDir(dl.folder, func(p string, de fs.DirEntry, err error) error {
		switch {
		case err != nil: // unreadable, or no folder yet
		case de.IsDir() && p != dl.folder && strings.HasPrefix(de.Name(), "."):
			return filepath.SkipDir
		case !de.IsDir():
			fn(p, de.Name())
		}
		return nil
	})
}

// sweep deletes the temp files an interrupted download left (q2-deezer stopped or killed under
// way); nothing downloads yet, so none is in use. A FLAC's is tens of MB.
func (dl *downloads) sweep() {
	dl.walk(func(path, name string) {
		if leftover.MatchString(name) {
			if err := os.Remove(path); err != nil {
				log.Printf("sweep: %v", err)
			}
		}
	})
}

// named is a downloaded file's track id: OpenDeezer's names, and the organized ones, end
// " [<id>].<ext>".
var named = regexp.MustCompile(` \[(-?[0-9]+)\]\.(?i:flac|mp3)$`)

// rescan reads the folder, and its Artist/Album folders, for the tracks already downloaded.
func (dl *downloads) rescan() {
	have := map[string]string{}
	dl.walk(func(path, name string) {
		if m := named.FindStringSubmatch(name); m != nil {
			have[m[1]] = path
		}
	})
	dl.mu.Lock()
	dl.have, dl.scanned = have, time.Now()
	dl.mu.Unlock()
}

// place is where a track downloads and its file's name ("" for OpenDeezer's): with Organize Files
// on, its album's folder and its number on the album, the tags it lacked filled in (a playlist's
// track has no number, disc or album artist). A lookup that fails but for the network files it
// by what the track has.
func (dl *downloads) place(dz *deezer.Client, t deezer.Track) (deezer.Track, string, string, error) {
	if !dl.d.organized() {
		return t, dl.folder, "", nil
	}
	p, err := dz.TrackPlacement(t)
	if deezer.IsNoNetwork(err) {
		return t, "", "", err
	}
	if err != nil {
		log.Printf("place %s: %v", t.ID, err)
		p = deezer.Placement{Album: t.AlbumName, Number: t.TrackNumber, Year: t.Year}
		if len(t.Artists) > 0 {
			p.Artist = t.Artists[0].Name
		}
	}
	if p.Album != "" {
		t.AlbumName = p.Album
	}
	if t.TrackNumber == 0 {
		t.TrackNumber = p.Number
	}
	if t.Year == "" {
		t.Year = p.Year
	}
	if p.Discs > 1 {
		t.DiscNumber = p.Disc
	}
	t.AlbumArtist = p.Artist
	year := p.Year // the album's, which the folder names
	if year == "" {
		year = t.Year
	}
	artist := filepath.Join(dl.folder, deezer.FolderName(p.Artist, "Unknown Artist"))
	dir := filepath.Join(artist, deezer.AlbumFolderName(t.AlbumName, year, "Unknown Album"))
	dl.renamed(filepath.Join(artist, deezer.FolderName(t.AlbumName, "Unknown Album")), dir)
	return t, dir, deezer.OrganizedName(t, p), nil
}

// renamed moves an album's folder named as v3.1 named them, without its year, to its name now, so
// the album's songs stay together; the songs on the card read again, at their new paths.
func (dl *downloads) renamed(old, now string) {
	if old == now {
		return
	}
	if fi, err := os.Stat(old); err != nil || !fi.IsDir() {
		return
	}
	if _, err := os.Stat(now); err == nil { // both there: the new one is used, the old one left
		return
	}
	if err := os.Rename(old, now); err != nil {
		log.Printf("rename %s: %v", filepath.Base(old), err)
		return
	}
	dl.rescan()
}

// order is the local row context for songs in dir: "album" in an album's folder, so the Q2 lists
// and plays them in track order (by name); "" in Deezer/ itself, newest first.
func (dl *downloads) order(dir string) string {
	if dir != dl.folder {
		return "album"
	}
	return ""
}

// fresh rescans the folder when the last scan is a minute old (files deleted or added by hand).
func (dl *downloads) fresh() {
	dl.mu.Lock()
	old := time.Since(dl.scanned) > time.Minute
	dl.mu.Unlock()
	if old {
		dl.rescan()
	}
}

// count is how many tracks are on the card.
func (dl *downloads) count() int {
	dl.mu.Lock()
	defer dl.mu.Unlock()
	return len(dl.have)
}

// state is a track's: "done" (its file), "downloading", "waiting", "failed", or "".
func (dl *downloads) state(id string) (string, string) {
	dl.mu.Lock()
	defer dl.mu.Unlock()
	if p, ok := dl.have[id]; ok {
		return "done", p
	}
	if dl.cur != nil && dl.cur.Track.ID == id {
		return "downloading", ""
	}
	for _, it := range dl.waiting {
		if it.Track.ID == id {
			return "waiting", ""
		}
	}
	for _, it := range dl.failed {
		if it.Track.ID == id {
			return "failed", it.Err
		}
	}
	return "", ""
}

// add queues the tracks not on the card or queued already (a failed one is tried again); it
// returns how many it queued.
func (dl *downloads) add(ts []deezer.Track) int {
	dl.mu.Lock()
	n := 0
	queued := map[string]bool{}
	if dl.cur != nil {
		queued[dl.cur.Track.ID] = true
	}
	for _, it := range dl.waiting {
		queued[it.Track.ID] = true
	}
	for _, t := range ts {
		if _, on := dl.have[t.ID]; on || queued[t.ID] || t.ID == "" {
			continue
		}
		queued[t.ID] = true
		dl.failed = drop(dl.failed, t.ID)
		dl.waiting = append(dl.waiting, &item{Track: t})
		n++
	}
	dl.mu.Unlock()
	if n > 0 {
		dl.save()
		dl.poke()
	}
	return n
}

func drop(l []*item, id string) []*item {
	out := l[:0]
	for _, it := range l {
		if it.Track.ID != id {
			out = append(out, it)
		}
	}
	return out
}

// remove takes a track out of the queue: waiting, or stopping it if it is the one downloading.
func (dl *downloads) remove(id string) bool {
	dl.mu.Lock()
	n := len(dl.waiting)
	dl.waiting = drop(dl.waiting, id)
	found := len(dl.waiting) != n
	if dl.cur != nil && dl.cur.Track.ID == id && dl.cancel != nil {
		dl.dropped = true
		dl.cancel()
		found = true
	}
	dl.mu.Unlock()
	if found {
		dl.save()
	}
	return found
}

// clear empties the waiting tracks.
func (dl *downloads) clear() int {
	dl.mu.Lock()
	n := len(dl.waiting)
	dl.waiting = nil
	dl.mu.Unlock()
	dl.save()
	return n
}

// retry queues the failed tracks again (id "" for all of them).
func (dl *downloads) retry(id string) int {
	dl.mu.Lock()
	var again []deezer.Track
	keep := dl.failed[:0]
	for _, it := range dl.failed {
		if id == "" || it.Track.ID == id {
			again = append(again, it.Track)
		} else {
			keep = append(keep, it)
		}
	}
	dl.failed = keep
	dl.mu.Unlock()
	return dl.add(again)
}

func (dl *downloads) clearFailed() {
	dl.mu.Lock()
	dl.failed = nil
	dl.mu.Unlock()
}

// busy is whether a track is downloading: the Q2 kept awake.
func (dl *downloads) busy() bool {
	dl.mu.Lock()
	defer dl.mu.Unlock()
	return dl.cur != nil
}

// pending is whether anything is downloading or waiting.
func (dl *downloads) pending() bool {
	dl.mu.Lock()
	defer dl.mu.Unlock()
	return dl.cur != nil || len(dl.waiting) > 0
}

func (dl *downloads) poke() {
	select {
	case dl.wake <- struct{}{}:
	default:
	}
}

// save writes the waiting tracks (the one downloading first) to the card, a moment after the last
// change, so a whole album queued is one write.
func (dl *downloads) save() {
	dl.mu.Lock()
	defer dl.mu.Unlock()
	if dl.saveTimer != nil {
		dl.saveTimer.Stop()
	}
	dl.saveTimer = time.AfterFunc(500*time.Millisecond, dl.write)
}

func (dl *downloads) write() {
	dl.wmu.Lock()
	defer dl.wmu.Unlock()
	dl.mu.Lock()
	var l []*item
	if dl.cur != nil && !dl.dropped {
		l = append(l, &item{Track: dl.cur.Track, Tries: dl.cur.Tries})
	}
	for _, it := range dl.waiting {
		l = append(l, &item{Track: it.Track, Tries: it.Tries})
	}
	dl.mu.Unlock()
	if len(l) == 0 {
		if err := os.Remove(dl.file); err != nil && !os.IsNotExist(err) {
			log.Printf("queue: %v", err)
		}
		return
	}
	b, _ := json.Marshal(l)
	if cardReady() {
		if err := q2.WriteFile(dl.file, b); err != nil {
			log.Printf("queue: %v", err)
		}
	}
}

// stall puts it back first in the queue: the queue waits (why), then tries again.
func (dl *downloads) stall(it *item, why string) {
	dl.mu.Lock()
	dl.cur, dl.cancel = nil, nil
	dl.waiting = append([]*item{it}, dl.waiting...)
	dl.stalled = why
	dl.mu.Unlock()
}

// run downloads the waiting tracks one after another until ctx ends (the add-on stopping, which
// leaves the one downloading for next time).
func (dl *downloads) run(ctx context.Context) {
	for {
		dl.mu.Lock()
		stalled := dl.stalled != ""
		dl.mu.Unlock()
		if !stalled {
			for dl.next(ctx) {
			}
		}
		wait := time.NewTimer(retryIn)
		select {
		case <-ctx.Done():
			wait.Stop()
			dl.write()
			return
		case <-dl.wake:
			dl.mu.Lock()
			dl.stalled = ""
			dl.mu.Unlock()
		case <-wait.C:
			dl.mu.Lock()
			dl.stalled = ""
			dl.mu.Unlock()
		}
		wait.Stop()
	}
}

// next downloads the first waiting track; false when there is none, or the queue stalled.
func (dl *downloads) next(ctx context.Context) bool {
	dl.mu.Lock()
	if dl.cur != nil || len(dl.waiting) == 0 || ctx.Err() != nil {
		dl.mu.Unlock()
		return false
	}
	it := dl.waiting[0]
	dl.waiting = dl.waiting[1:]
	dl.cur, dl.dropped = it, false
	one, cancel := context.WithCancel(ctx)
	dl.cancel = cancel
	dl.mu.Unlock()
	defer cancel()
	if !cardReady() {
		dl.stall(it, "The card is away")
		return false
	}
	dz, err := dl.d.session()
	switch {
	case errors.Is(err, errNoARL):
		dl.stall(it, "Sign in to Deezer")
		return false
	case deezer.IsARLExpired(err):
		dl.stall(it, "Your sign-in expired: sign in again")
		return false
	case err != nil:
		dl.stall(it, "Deezer can't be reached")
		return false
	case !dz.Account().Premium:
		dl.stall(it, "Downloads need a paid Deezer plan")
		return false
	}
	if it.Tries >= maxTries { // under way each time q2-deezer died: not again by itself
		log.Printf("download %s: q2-deezer stopped %d times under way", it.Track.ID, it.Tries)
		it.Err, it.Tries = "it stopped Deezer each time", 0
		dl.mu.Lock()
		dl.failed = append(dl.failed, it)
		dl.cur, dl.cancel = nil, nil
		dl.mu.Unlock()
		dl.save()
		return true
	}
	track, dir, name, err := dl.place(dz, it.Track)
	if err != nil {
		dl.stall(it, "Deezer can't be reached")
		return false
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		dl.stall(it, "Couldn't make Deezer/ on the card")
		return false
	}
	// Counted on the card first, so a death under way counts; taken back once it returns.
	dl.mu.Lock()
	it.Tries++
	dl.mu.Unlock()
	dl.write()
	path, err := dz.SaveTrackInfoAs(one, track, dir, name)
	dl.mu.Lock()
	it.Tries--
	dropped := dl.dropped
	dl.mu.Unlock()
	if err != nil && dir != dl.folder { // an album's folders, if this made them: empty, gone
		os.Remove(dir)
		os.Remove(filepath.Dir(dir))
	}
	switch {
	case err == nil:
		log.Printf("downloaded %s", filepath.Base(path))
		dl.mu.Lock()
		it.Path = path
		dl.have[it.Track.ID] = path
		dl.done = append(dl.done, it)
		if len(dl.done) > 200 {
			dl.done = dl.done[len(dl.done)-200:]
		}
		dl.cur, dl.cancel = nil, nil
		dl.mu.Unlock()
	case dropped:
		dl.mu.Lock()
		dl.cur, dl.cancel = nil, nil
		dl.mu.Unlock()
	case ctx.Err() != nil: // stopping: it carries on next time
		dl.stall(it, "")
		return false
	case deezer.IsNoNetwork(err):
		dl.stall(it, "Deezer can't be reached")
		return false
	case deezer.IsARLExpired(err):
		dl.d.setAuth(authExpired)
		dl.stall(it, "Your sign-in expired: sign in again")
		return false
	case deezer.IsPremiumRequired(err):
		dl.stall(it, "Downloads need a paid Deezer plan")
		return false
	default:
		log.Printf("download %s: %v", it.Track.ID, err)
		it.Err = reason(err)
		dl.mu.Lock()
		dl.failed = append(dl.failed, it)
		dl.cur, dl.cancel = nil, nil
		dl.mu.Unlock()
	}
	dl.save()
	return true
}

// reason is a failure as the Q2 shows it.
func reason(err error) string {
	switch {
	case deezer.IsPreviewOnly(err), deezer.IsNoFullMedia(err):
		return "not available in full here"
	case os.IsPermission(err) || errors.Is(err, os.ErrPermission):
		return "the card refused it"
	}
	msg := err.Error()
	if len(msg) > 60 {
		msg = msg[:60]
	}
	return msg
}

// snapshot is the queue as the Q2's page shows it.
func (dl *downloads) snapshot() (cur *item, waiting, failed, done []item, stalled string) {
	dl.mu.Lock()
	defer dl.mu.Unlock()
	if dl.cur != nil {
		c := *dl.cur
		cur = &c
	}
	for _, it := range dl.waiting {
		waiting = append(waiting, *it)
	}
	for _, it := range dl.failed {
		failed = append(failed, *it)
	}
	for i := len(dl.done) - 1; i >= 0; i-- {
		done = append(done, *dl.done[i])
	}
	return cur, waiting, failed, done, dl.stalled
}
