package main

import (
	"context"
	"io"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/group-a-study/q2-deezer/internal/deezer"
	"github.com/group-a-study/q2-deezer/internal/q2"
)

// Stream mode (.deezer/mode "stream"; anything else, "download" the default, downloads): a press on
// a song plays it and the rest of its list after it, and keeps none of them. q2-deezer fetches them
// in order into a session's folder, .deezer/stream/<seq>/ ("01 - Title [id].flac", decrypted and
// tagged as a download is), and names that folder in /tmp/q2-deezer.play ("<seq> <folder>"). Q2 Pod
// (q2-pod's patch/addon.c) plays the folder from its first song once one is there, adds each song
// that comes after to the end of that queue, and leaves the path of the one playing in
// /tmp/q2-deezer.playing. A session fetches streamAhead songs past that one and waits for the Q2 to
// reach them, deleting the ones more than streamBehind before it. A new session takes the songs it
// needs from the old ones (a rename), then deletes them. .deezer is a hidden folder, which Q2 Pod's
// library scan skips, so streamed songs never join the library.

const (
	streamMax    = 200 // songs a session plays at most
	streamAhead  = 2   // songs fetched past the one playing
	streamBehind = 5   // songs kept before it, for Previous
)

// streamIdle is how long a session waits for the Q2 to move on (paused, or playing something else)
// before it ends; tests shorten it.
var streamIdle = time.Hour

type streamer struct {
	d             *daemon
	root          string // .deezer/stream on the card
	play, playing string // the files shared with Q2 Pod

	mu     sync.Mutex
	cancel context.CancelFunc
	done   chan struct{} // closed when the session's goroutine has ended
	cur    string        // the song fetching now, by id
}

func newStreamer(d *daemon, root, play, playing string) *streamer {
	return &streamer{d: d, root: root, play: play, playing: playing}
}

// start plays ts in a new session, which replaces the one before (stopped first).
func (s *streamer) start(ts []deezer.Track) {
	if len(ts) > streamMax {
		ts = ts[:streamMax]
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	s.mu.Lock()
	if s.cancel != nil {
		s.cancel()
	}
	prev := s.done
	s.cancel, s.done = cancel, done
	s.mu.Unlock()
	seq := time.Now().UnixMilli()
	go func() {
		defer close(done)
		if prev != nil {
			<-prev
		}
		s.run(ctx, seq, filepath.Join(s.root, strconv.FormatInt(seq, 10)), ts)
	}()
}

// stop ends the session and waits for it; Q2 Pod's files go with it.
func (s *streamer) stop() {
	s.mu.Lock()
	if s.cancel != nil {
		s.cancel()
	}
	done := s.done
	s.mu.Unlock()
	if done != nil {
		<-done
	}
	os.Remove(s.play)
	os.Remove(s.playing)
}

// active is whether a session is fetching or waiting to.
func (s *streamer) active() bool {
	s.mu.Lock()
	done := s.done
	s.mu.Unlock()
	if done == nil {
		return false
	}
	select {
	case <-done:
		return false
	default:
		return true
	}
}

// busy is whether a song is being fetched: the Q2 kept awake.
func (s *streamer) busy() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cur != ""
}

// loading is whether the song id is the one being fetched.
func (s *streamer) loading(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return id != "" && s.cur == id
}

func (s *streamer) setCur(id string) {
	s.mu.Lock()
	s.cur = id
	s.mu.Unlock()
}

// run is a session: the songs of ts the old sessions have moved into dir, the old sessions deleted,
// dir named to Q2 Pod, then the rest fetched in order, each once the Q2 is near enough to it.
func (s *streamer) run(ctx context.Context, seq int64, dir string, ts []deezer.Track) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		log.Printf("stream: %v", err)
		return
	}
	names := make([]string, len(ts))
	for i, t := range ts {
		names[i] = deezer.OrganizedName(t, deezer.Placement{Number: i + 1, Tracks: len(ts)})
	}
	s.reuse(dir, ts, names)
	if err := q2.WriteFile(s.play, []byte(strconv.FormatInt(seq, 10)+" "+dir+"\n")); err != nil {
		log.Printf("stream: %v", err)
		return
	}
	log.Printf("stream: %d songs from %s", len(ts), ts[0].Name)
	for i, t := range ts {
		if songAt(dir, t.ID) == "" {
			if !s.near(ctx, dir, i) {
				return
			}
			s.setCur(t.ID)
			s.fetch(ctx, dir, names[i], t)
			s.setCur("")
		}
		s.trim(dir)
		if ctx.Err() != nil {
			return
		}
	}
}

// reuse moves into dir the songs of ts the old sessions hold, under their names in this one, and
// deletes the old sessions.
func (s *streamer) reuse(dir string, ts []deezer.Track, names []string) {
	sessions, _ := os.ReadDir(s.root)
	old := map[string]string{}
	for _, se := range sessions {
		if p := filepath.Join(s.root, se.Name()); se.IsDir() && p != dir {
			files, _ := os.ReadDir(p)
			for _, f := range files {
				if m := named.FindStringSubmatch(f.Name()); m != nil {
					old[m[1]] = filepath.Join(p, f.Name())
				}
			}
		}
	}
	for i, t := range ts {
		if p, ok := old[t.ID]; ok {
			if err := os.Rename(p, filepath.Join(dir, names[i]+filepath.Ext(p))); err != nil {
				log.Printf("stream: %v", err)
			}
		}
	}
	for _, se := range sessions {
		if p := filepath.Join(s.root, se.Name()); p != dir {
			os.RemoveAll(p)
		}
	}
}

// songAt is the song id's file in dir, "" when it isn't there.
func songAt(dir, id string) string {
	files, _ := os.ReadDir(dir)
	for _, f := range files {
		if m := named.FindStringSubmatch(f.Name()); m != nil && m[1] == id {
			return filepath.Join(dir, f.Name())
		}
	}
	return ""
}

// position is the place in dir's order of the song Q2 Pod plays from it (0 the first), -1 when it
// plays none of them (not yet, or another queue).
func (s *streamer) position(dir string) int {
	b, _ := os.ReadFile(s.playing)
	p := strings.TrimSpace(string(b))
	rest, ok := strings.CutPrefix(p, dir+"/")
	if !ok {
		return -1
	}
	num, _, _ := strings.Cut(rest, " ")
	if i := strings.IndexByte(num, '-'); i >= 0 { // a disc's "2-07"
		num = num[i+1:]
	}
	n, err := strconv.Atoi(num)
	if err != nil || n < 1 {
		return -1
	}
	return n - 1
}

// near waits until the song at i is at most streamAhead past the one playing (before the Q2 plays
// any, the first is): false when the session ended, or the Q2 stayed put for streamIdle.
func (s *streamer) near(ctx context.Context, dir string, i int) bool {
	last, since := -2, time.Now()
	for {
		pos := s.position(dir)
		if i <= max(pos, 0)+streamAhead {
			return true
		}
		if pos != last {
			last, since = pos, time.Now()
		} else if time.Since(since) > streamIdle {
			log.Printf("stream: the Q2 stayed on one song; stopping")
			return false
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(time.Second):
		}
	}
}

// trim deletes dir's songs more than streamBehind before the one playing.
func (s *streamer) trim(dir string) {
	pos := s.position(dir)
	if pos < 0 {
		return
	}
	files, _ := os.ReadDir(dir)
	for _, f := range files {
		num, _, _ := strings.Cut(f.Name(), " ")
		if n, err := strconv.Atoi(num); err == nil && n-1 < pos-streamBehind {
			os.Remove(filepath.Join(dir, f.Name()))
		}
	}
}

// fetch puts t in dir as name: a copy of the download on the card, if it is one, else fetched,
// decrypted and tagged as a download is. A song Deezer won't send is left out (the queue goes on
// without it); one that fails for the network is tried again.
func (s *streamer) fetch(ctx context.Context, dir, name string, t deezer.Track) {
	if st, have := s.d.dl.state(t.ID); st == "done" {
		if err := copyFile(have, filepath.Join(dir, name+filepath.Ext(have))); err == nil {
			return
		}
	}
	for try := 0; ctx.Err() == nil; try++ {
		dz, err := s.d.session()
		if err == nil {
			if !dz.Account().Premium {
				return
			}
			if _, err = dz.SaveTrackInfoAs(ctx, t, dir, name); err == nil {
				return
			}
		}
		if ctx.Err() != nil || !deezer.IsNoNetwork(err) || try >= 5 {
			log.Printf("stream %s: %v", t.ID, err)
			return
		}
		select {
		case <-ctx.Done():
		case <-time.After(5 * time.Second):
		}
	}
}

// copyFile copies src to dst through dst's .part, renamed into place once whole.
func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	part := dst + ".part"
	out, err := os.Create(part)
	if err != nil {
		return err
	}
	if _, err = io.Copy(out, in); err == nil {
		err = out.Sync()
	}
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(part, dst)
	}
	if err != nil {
		os.Remove(part)
	}
	return err
}
