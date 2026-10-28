// q2-deezer is a Q2 Pod card app (contrib/shanlingq2/README) that does Deezer as OpenDeezer does.
// The Q2 browses the signed-in account's music and Deezer's catalogue on its own page, over
// Q2SPOT/1, and what it picks is downloaded, decrypted and tagged into Deezer/ on the card by
// OpenDeezer's client (internal/deezer), in Artist/Year - Album folders unless Organize Files is
// off; every song then plays from the card, as local music. In Stream mode (stream.go) a press plays
// the song instead, fetched into a hidden folder that keeps none. A paid plan is needed, as for
// OpenDeezer's downloads. It runs from the card's .deezer folder (copied to /tmp first) and is
// signed in from a phone with the account's arl cookie.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/group-a-study/q2-deezer/internal/deezer"
	"github.com/group-a-study/q2-deezer/internal/q2"
	"github.com/group-a-study/q2-deezer/internal/q2spot"
	"github.com/group-a-study/q2-deezer/internal/tlsroots"
)

const version = "q2-v3.5"

// transport carries every request to Deezer: TLS checked against the add-on's own roots, as the
// firmware has none. Tests answer for Deezer instead.
var transport = func() http.RoundTripper { return tlsroots.Client(0).Transport }

// idleFor is how long it stays with nothing to download and its page unused, before it stops.
var idleFor = 15 * time.Minute

// The sign-in, as last found: what the Q2's page shows without asking Deezer each time.
const (
	authUnknown = iota // not asked yet
	authNoARL          // no arl on the card
	authOK             // signed in, with a paid plan
	authFree           // signed in, without one: no downloads
	authExpired        // Deezer refused the arl
	authOffline        // Deezer couldn't be reached
)

type daemon struct {
	dir     string
	state   *q2.State
	cache   *cache
	signin  *signin
	dl      *downloads
	st      *streamer
	browse  string
	control string

	mu   sync.Mutex
	dz   *deezer.Client
	arl  string
	auth int
	qual string
	org  bool      // Organize Files: Artist/Year - Album folders
	strm bool      // Stream mode: a press plays a song, not downloads it
	used time.Time // the Q2's page, last
	quit chan struct{}
	once sync.Once
}

// signinPort is where the sign-in page listens (the next free one if taken).
var signinPort = 8090

func main() {
	dir := flag.String("dir", "/mnt/mmc/.deezer", "the add-on's folder on the card")
	card := flag.String("card", "/mnt/mmc", "the card's mount point; downloads go to its Deezer folder")
	statusFile := flag.String("status-file", "/tmp/q2-deezer.state", "the status file Q2 Pod reads")
	browse := flag.String("browse-socket", "/tmp/q2-deezer.browse.sock", "Q2SPOT/1 for the Q2's page")
	control := flag.String("control-socket", "/tmp/q2-deezer.sock", "datagrams: q quits")
	flag.IntVar(&signinPort, "signin-port", signinPort, "the sign-in page's port, on the Wi-Fi")
	flag.Parse()
	debug.SetGCPercent(50)
	debug.SetMemoryLimit(40 << 20)
	q2.Card = *card
	q2.SetupLog("q2-deezer")
	d := start(*dir, filepath.Join(*card, "Deezer"), *statusFile, *browse, *control)
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP)
	go func() { <-sig; d.shutdown() }()
	d.run()
	os.Exit(0)
}

func start(dir, folder, statusFile, browse, control string) *daemon {
	deezer.UseTransport(transport())
	tlsroots.DateURL = "http://www.deezer.com/"
	d := &daemon{dir: dir, state: &q2.State{Path: statusFile}, cache: newCache(), browse: browse, control: control,
		used: time.Now(), quit: make(chan struct{})}
	d.qual = readQuality(dir)
	d.org = readOrganize(dir)
	d.strm = readMode(dir)
	d.dl = newDownloads(d, folder, filepath.Join(dir, "queue.json"))
	base := strings.TrimSuffix(statusFile, ".state") // Q2 Pod's names: /tmp/q2-deezer.play, .playing
	d.st = newStreamer(d, filepath.Join(dir, "stream"), base+".play", base+".playing")
	d.startSignin(signinPort)
	d.client() // reads the arl, if the card has one
	d.writeState()
	go d.checkAuth()
	if _, err := q2.Control(control, func(cmd []byte) {
		if len(cmd) > 0 && cmd[0] == 'q' {
			d.shutdown()
		}
	}); err != nil {
		log.Printf("control socket: %v", err)
	}
	go func() {
		if err := q2spot.Serve(browse, d.device); err != nil {
			log.Printf("browse socket: %v", err)
		}
	}()
	log.Printf("%s starting", version)
	return d
}

// run downloads until asked to stop, writes the status file every second, asks Deezer again while
// it couldn't be reached, and stops by itself once idle: nothing to download, its page unused.
func (d *daemon) run() {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { d.dl.run(ctx); close(done) }()
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for n := 1; ; n++ {
		select {
		case <-d.quit:
			cancel()
			<-done // the download under way ended, its .part gone; the queue saved
			d.st.stop()
			d.state.Remove()
			os.Remove(d.browse)
			os.Remove(d.control)
			log.Printf("stopped")
			return
		case <-t.C:
			d.writeState()
			if n%30 == 0 && d.authState() == authOffline {
				go d.checkAuth()
			}
			d.mu.Lock()
			idle := time.Since(d.used)
			d.mu.Unlock()
			if !d.dl.pending() && !d.st.active() && idle > idleFor {
				log.Printf("idle: stopping")
				d.shutdown()
			}
		}
	}
}

func (d *daemon) shutdown() { d.once.Do(func() { close(d.quit) }) }

func (d *daemon) touch() {
	d.mu.Lock()
	d.used = time.Now()
	d.mu.Unlock()
}

// client is the session for the card's arl, made when the file appears or changes; nil without one.
// It isn't signed in yet: session does that.
func (d *daemon) client() *deezer.Client {
	b, _ := os.ReadFile(filepath.Join(d.dir, "arl"))
	arl := strings.TrimSpace(string(b))
	d.mu.Lock()
	defer d.mu.Unlock()
	if arl != d.arl || (arl == "" && d.auth != authNoARL) {
		d.arl, d.dz, d.auth = arl, nil, authUnknown
		if arl != "" {
			d.dz = deezer.New(arl)
			d.dz.SetQuality(levels[d.qual])
		} else {
			d.auth = authNoARL
		}
		d.cache.clear()
	}
	return d.dz
}

var errNoARL = errors.New("no arl on the card")

// session is the card's session, signed in (Login fetches the tokens once; the client signs in
// again by itself when Deezer rotates them). A free account signs in, but downloads refuse it.
func (d *daemon) session() (*deezer.Client, error) {
	dz := d.client()
	if dz == nil {
		return nil, errNoARL
	}
	if !dz.LoggedIn() {
		if err := dz.Login(); err != nil {
			switch {
			case deezer.IsARLExpired(err):
				d.setAuth(authExpired)
			case deezer.IsNoNetwork(err):
				d.setAuth(authOffline)
			}
			return nil, err
		}
	}
	if dz.Account().Premium {
		d.setAuth(authOK)
	} else {
		d.setAuth(authFree)
	}
	return dz, nil
}

func (d *daemon) authState() int {
	d.client()
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.auth
}

// setAuth notes the sign-in, and rewrites the status file when it changed (the sign-in page's code
// comes or goes); a sign-in that works again starts the queue.
func (d *daemon) setAuth(a int) {
	d.mu.Lock()
	changed := d.auth != a
	d.auth = a
	d.mu.Unlock()
	if changed {
		d.writeState()
		if a == authOK {
			d.dl.poke()
		}
	}
}

// checkAuth asks Deezer whether the card's arl works.
func (d *daemon) checkAuth() {
	if _, err := d.session(); err != nil && !errors.Is(err, errNoARL) {
		log.Printf("sign-in: %v", err)
	}
}

// signedIn is a new arl saved by the sign-in page.
func (d *daemon) signedIn() {
	d.client()
	d.checkAuth()
}

// writeState is what the Q2 reads: whether to hold standby off (a track downloading), and, while
// not signed in with a paid plan, the sign-in page's address, PIN and code.
func (d *daemon) writeState() {
	hold := "0"
	if d.dl.busy() || d.st.busy() {
		hold = "1"
	}
	kv := map[string]string{"hold": hold, "version": version}
	if d.authState() != authOK && d.signin != nil {
		d.signin.fields(kv)
	}
	d.state.Write(kv)
}

// Qualities, as the setting names them, and the client's level for each: a download is in the best
// format the account and the track have at or below it.
var qualities = []string{"flac", "mp3_320", "mp3_128"}
var levels = map[string]int{"flac": deezer.QualityLossless, "mp3_320": deezer.QualityHigh,
	"mp3_128": deezer.QualityNormal}
var qualityNames = map[string]string{"flac": "FLAC", "mp3_320": "MP3 320", "mp3_128": "MP3 128"}

func readQuality(dir string) string {
	b, _ := os.ReadFile(filepath.Join(dir, "quality"))
	q := strings.TrimSpace(string(b))
	if _, ok := levels[q]; !ok {
		return "flac"
	}
	return q
}

func (d *daemon) quality() string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.qual
}

func (d *daemon) setQuality(q string) error {
	if _, ok := levels[q]; !ok {
		return os.ErrInvalid
	}
	if err := q2.WriteFile(filepath.Join(d.dir, "quality"), []byte(q+"\n")); err != nil {
		return err
	}
	d.mu.Lock()
	d.qual = q
	if d.dz != nil {
		d.dz.SetQuality(levels[q])
	}
	d.mu.Unlock()
	return nil
}

// Organize Files, .deezer/organize: "on" (the default, and anything but "off") files each new
// download as Deezer/<album artist>/<album> (<year>)/07 - Title [id].flac; "off" as OpenDeezer
// does, every one in Deezer/ as "Title - Artist [id].flac". Files already downloaded stay where
// they are.
func readOrganize(dir string) bool {
	b, _ := os.ReadFile(filepath.Join(dir, "organize"))
	return strings.TrimSpace(string(b)) != "off"
}

func (d *daemon) organized() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.org
}

func (d *daemon) setOrganize(on bool) error {
	v := "off"
	if on {
		v = "on"
	}
	if err := q2.WriteFile(filepath.Join(d.dir, "organize"), []byte(v+"\n")); err != nil {
		return err
	}
	d.mu.Lock()
	d.org = on
	d.mu.Unlock()
	return nil
}

// Stream mode, .deezer/mode: "stream" plays a pressed song (stream.go); "download" (the default, and
// anything else) downloads it.
func readMode(dir string) bool {
	b, _ := os.ReadFile(filepath.Join(dir, "mode"))
	return strings.TrimSpace(string(b)) == "stream"
}

func (d *daemon) streaming() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.strm
}

func (d *daemon) setMode(stream bool) error {
	v := "download"
	if stream {
		v = "stream"
	}
	if err := q2.WriteFile(filepath.Join(d.dir, "mode"), []byte(v+"\n")); err != nil {
		return err
	}
	d.mu.Lock()
	d.strm = stream
	d.mu.Unlock()
	return nil
}

// signOut removes the arl from the card; the downloads stay, and the queue waits for a sign-in.
func (d *daemon) signOut() error {
	err := os.Remove(filepath.Join(d.dir, "arl"))
	if os.IsNotExist(err) {
		err = nil
	}
	d.client()
	d.writeState()
	return err
}

func plural(n int, one string) string {
	if n == 1 {
		return "1 " + one
	}
	return fmt.Sprintf("%d %ss", n, one)
}
