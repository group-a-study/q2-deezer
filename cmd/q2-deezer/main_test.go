package main

import (
	"bytes"
	"crypto/cipher"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/jpeg"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-flac/flacvorbis"
	flac "github.com/go-flac/go-flac"
	"golang.org/x/crypto/blowfish"

	"github.com/group-a-study/q2-deezer/internal/deezer"
	"github.com/group-a-study/q2-deezer/internal/q2spot"
)

// A track's file: fLaC, a STREAMINFO block (the last metadata block), then its frames: a sync
// code and its own bytes.
func plain(id string, n int) []byte {
	b := append([]byte("fLaC"), 0x80, 0, 0, 34)
	b = append(b, make([]byte, 34)...)
	b = append(b, 0xff, 0xf8)
	for i := 0; len(b) < n; i++ {
		b = append(b, id[i%len(id)]^byte(i))
	}
	return b
}

// encrypt is BF_CBC_STRIPE, as Deezer's CDN serves a track: every third full 2048-byte chunk.
func encrypt(id string, p []byte) []byte {
	out := append([]byte(nil), p...)
	blk, _ := blowfish.NewCipher(deezer.BlowfishKey(id))
	for i := 0; i+2048 <= len(out); i += 2048 {
		if (i/2048)%3 == 0 {
			cipher.NewCBCEncrypter(blk, []byte{0, 1, 2, 3, 4, 5, 6, 7}).CryptBlocks(out[i:i+2048], out[i:i+2048])
		}
	}
	return out
}

var goodARL = strings.Repeat("ab12", 48) // the shape of a real one: 192 hex digits

// fakeDeezer answers for Deezer's hosts: the gateway, the media service, the public API, the CDN and
// the image host, each request's host made the first part of its path.
type fakeDeezer struct {
	srv     *httptest.Server
	files   map[string][]byte
	mu      sync.Mutex
	calls   []string // gateway methods and API paths, in order
	free    bool
	offline bool
	formats []string                 // the media service's last request's formats
	hold    map[string]chan struct{} // a track whose CDN body waits for its channel to close
	slow    chan struct{}            // when set, pageProfile and playlist.getSongs answer once it is closed
}

func (f *fakeDeezer) RoundTrip(r *http.Request) (*http.Response, error) {
	f.mu.Lock()
	offline := f.offline
	f.mu.Unlock()
	if offline {
		return nil, &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("network is unreachable")}
	}
	u, _ := url.Parse(f.srv.URL)
	r2 := r.Clone(r.Context())
	r2.URL.Scheme, r2.URL.Host, r2.URL.Path = "http", u.Host, "/"+r.URL.Host+r.URL.Path
	r2.Host, r2.RequestURI = "", ""
	return http.DefaultTransport.RoundTrip(r2)
}

func (f *fakeDeezer) set(fn func()) {
	f.mu.Lock()
	fn()
	f.mu.Unlock()
}

func (f *fakeDeezer) called(what string) {
	f.mu.Lock()
	f.calls = append(f.calls, what)
	f.mu.Unlock()
}

func (f *fakeDeezer) count(what string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.calls {
		if c == what {
			n++
		}
	}
	return n
}

var titles = map[string]string{"1": "The Be Colony", "2": "Mother Is the Milky Way", "3": "Ritual Looks",
	"4": "Tears in the Typing Pool", "5": "Valerie", "6": "Corporeal"}
var seconds = map[string]int{"1": 30, "2": 20, "3": 10, "4": 10, "5": 10, "6": 10}

// cover is a real JPEG, as the image host serves: FLAC's picture block reads its size.
var cover = func() []byte {
	var b bytes.Buffer
	jpeg.Encode(&b, image.NewRGBA(image.Rect(0, 0, 4, 4)), nil)
	return b.Bytes()
}()

func gwTrack(id string) map[string]any {
	return map[string]any{"SNG_ID": id, "SNG_TITLE": titles[id], "ART_ID": "5", "ART_NAME": "Broadcast",
		"ALB_TITLE": "Witch Cults", "ALB_PICTURE": "pic" + id, "DURATION": strconv.Itoa(seconds[id])}
}

func restTrack(id string, album bool, pos int) map[string]any {
	t := map[string]any{"id": json.Number(id), "title": titles[id], "duration": seconds[id], "track_position": pos,
		"artist": map[string]any{"id": 5, "name": "Broadcast"}}
	if album {
		t["album"] = map[string]any{"title": "Witch Cults", "cover_medium": coverURL("rest" + id)}
	}
	return t
}

func coverURL(md5 string) string {
	return "https://e-cdns-images.dzcdn.net/images/cover/" + md5 + "/250x250-000000-80-0-0.jpg"
}

var album7 = map[string]any{"id": 7, "title": "Tender Buttons", "cover_medium": coverURL("pic7"),
	"release_date": "2005-09-19", "artist": map[string]any{"id": 5, "name": "Broadcast"}}

func newFakeDeezer(t *testing.T) *fakeDeezer {
	f := &fakeDeezer{files: map[string][]byte{}, hold: map[string]chan struct{}{}}
	for id, n := range map[string]int{"1": 40 << 10, "2": 30 << 10, "3": 15<<10 + 77, "4": 10 << 10, "5": 10 << 10, "6": 9 << 10} {
		f.files[id] = encrypt(id, plain(id, n))
	}
	mux := http.NewServeMux()
	reply := func(w http.ResponseWriter, results any) {
		json.NewEncoder(w).Encode(map[string]any{"error": []any{}, "results": results})
	}
	list := func(ids ...string) map[string]any {
		var data []any
		for _, id := range ids {
			data = append(data, gwTrack(id))
		}
		return map[string]any{"data": data}
	}
	mux.HandleFunc("/www.deezer.com/ajax/gw-light.php", func(w http.ResponseWriter, r *http.Request) {
		method := r.URL.Query().Get("method")
		var args map[string]any
		json.NewDecoder(r.Body).Decode(&args)
		f.called(method)
		f.mu.Lock()
		free := f.free
		f.mu.Unlock()
		if !strings.Contains(r.Header.Get("Cookie"), "arl="+goodARL) {
			reply(w, map[string]any{"checkForm": "", "USER": map[string]any{"USER_ID": 0}})
			return
		}
		if method != "deezer.getUserData" && r.URL.Query().Get("api_token") != "tok" {
			json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"VALID_TOKEN_REQUIRED": "Invalid CSRF token"},
				"results": map[string]any{}})
			return
		}
		f.mu.Lock()
		gate := f.slow
		f.mu.Unlock()
		if gate != nil && (method == "deezer.pageProfile" || method == "playlist.getSongs") {
			<-gate
		}
		switch method {
		case "deezer.getUserData":
			offers := []any{map[string]string{"title": "Deezer Premium"}}
			if free {
				offers = nil
			}
			reply(w, map[string]any{"checkForm": "tok", "OFFERS": offers, "USER": map[string]any{"USER_ID": "42",
				"BLOG_NAME": "Ana", "OPTIONS": map[string]any{"license_token": "lic", "web_hq": !free, "web_lossless": !free}}})
		case "deezer.pageProfile":
			tab, _ := args["tab"].(string)
			data := map[string][]any{
				"playlists": {map[string]any{"PLAYLIST_ID": 99, "TITLE": "Haunted", "NB_SONG": "3"}},
				"albums":    {map[string]any{"ALB_ID": "7", "ALB_TITLE": "Tender Buttons", "ART_ID": 5, "ART_NAME": "Broadcast", "ALB_PICTURE": "pic7"}},
				"artists":   {map[string]any{"ART_ID": "5", "ART_NAME": "Broadcast", "NB_FAN": 1200}},
			}[tab]
			reply(w, map[string]any{"TAB": map[string]any{tab: map[string]any{"data": data, "total": len(data)}}})
		case "playlist.getSongs":
			reply(w, list("1", "2", "3"))
		case "favorite_song.getList":
			reply(w, list("2", "1"))
		case "song.getData":
			id, _ := args["sng_id"].(string)
			tr := gwTrack(id)
			tr["TRACK_TOKEN"], tr["GAIN"] = "tok"+id, "-8.5"
			reply(w, tr)
		case "radio.getUserRadio":
			reply(w, list("6", "5"))
		case "smart.getSmartRadio":
			reply(w, list("5", "4"))
		default:
			t.Errorf("gateway method %s", method)
		}
	})
	mux.HandleFunc("/media.deezer.com/v1/get_url", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Tokens []string `json:"track_tokens"`
			Media  []struct {
				Formats []map[string]string `json:"formats"`
			} `json:"media"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		id := strings.TrimPrefix(body.Tokens[0], "tok")
		f.mu.Lock()
		f.formats = nil
		for _, m := range body.Media[0].Formats {
			f.formats = append(f.formats, m["format"])
		}
		formats := f.formats
		f.mu.Unlock()
		if id == "3" { // not available in full in this region: only its preview
			json.NewEncoder(w).Encode(map[string]any{"data": []any{map[string]any{"errors": []any{
				map[string]any{"code": 2002, "message": "Track token has no sufficient rights on requested media"}}}}})
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"data": []any{map[string]any{"media": []any{map[string]any{
			"format": formats[0], "sources": []any{map[string]string{"url": "https://cdn.test/stream/" + id}}}}}}})
	})
	mux.HandleFunc("/api.deezer.com/", func(w http.ResponseWriter, r *http.Request) {
		p := strings.TrimPrefix(r.URL.Path, "/api.deezer.com")
		f.called(p)
		var v any
		switch p {
		case "/track/3":
			v = map[string]any{"id": 3, "preview": "https://cdn.test/preview/3"}
		case "/album/7/tracks":
			v = map[string]any{"data": []any{restTrack("4", false, 1), restTrack("5", false, 2)}, "total": 2}
		case "/album/7":
			v = album7
		case "/track/1", "/track/2", "/track/4", "/track/5": // where each sits on its album: organize_test.go
			v = placed[p]
		case "/album/8":
			v = album8
		case "/album/8/tracks":
			v = map[string]any{"data": []any{map[string]any{"id": 1, "track_position": 1, "disk_number": 1},
				map[string]any{"id": 2, "track_position": 3, "disk_number": 1},
				map[string]any{"id": 11, "track_position": 1, "disk_number": 2}}, "total": 3}
		case "/artist/5":
			v = map[string]any{"id": 5, "name": "Broadcast", "nb_fan": 1200}
		case "/artist/5/top":
			v = map[string]any{"data": []any{restTrack("2", true, 0), restTrack("1", true, 0)}}
		case "/artist/5/albums":
			v = map[string]any{"data": []any{album7}}
		case "/artist/5/related":
			v = map[string]any{"data": []any{map[string]any{"id": 9, "name": "Stereolab"}}}
		case "/search":
			v = map[string]any{"data": []any{restTrack("4", true, 0)}}
		case "/search/album":
			v = map[string]any{"data": []any{album7}}
		case "/search/artist":
			v = map[string]any{"data": []any{map[string]any{"id": 5, "name": "Broadcast"}}}
		case "/search/playlist":
			v = map[string]any{"data": []any{map[string]any{"id": 99, "title": "Haunted", "nb_tracks": 3,
				"user": map[string]any{"name": "Ana"}}}}
		case "/chart/0":
			v = map[string]any{"tracks": map[string]any{"data": []any{restTrack("1", true, 0)}}}
		default:
			t.Errorf("api path %s", p)
		}
		json.NewEncoder(w).Encode(v)
	})
	mux.HandleFunc("/cdn.test/stream/", func(w http.ResponseWriter, r *http.Request) {
		id := strings.TrimPrefix(r.URL.Path, "/cdn.test/stream/")
		f.mu.Lock()
		hold := f.hold[id]
		f.mu.Unlock()
		if hold != nil {
			w.Header().Set("Content-Length", strconv.Itoa(len(f.files[id])))
			w.Write(f.files[id][:4096])
			w.(http.Flusher).Flush()
			select {
			case <-hold:
			case <-r.Context().Done():
				return
			}
			w.Write(f.files[id][4096:])
			return
		}
		http.ServeContent(w, r, id, time.Time{}, bytes.NewReader(f.files[id]))
	})
	mux.HandleFunc("/cdn.test/preview/3", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("ID3 a preview"))
	})
	mux.HandleFunc("/e-cdns-images.dzcdn.net/images/cover/", func(w http.ResponseWriter, r *http.Request) {
		f.called("cover " + strings.Split(r.URL.Path, "/")[4])
		w.Write(cover)
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

type rig struct {
	d      *daemon
	dir    string
	tmp    string
	card   string
	folder string
	sock   string
	done   chan struct{}
	t      *testing.T
}

// newRig has Organize Files off: every download in Deezer/ itself, as OpenDeezer names them.
func newRig(t *testing.T, arl string, f *fakeDeezer) *rig { return newRigOrganize(t, arl, f, "off") }

// newRigOrganize writes organize to .deezer/organize first; "" leaves none (the default, on).
func newRigOrganize(t *testing.T, arl string, f *fakeDeezer, organize string) *rig {
	tmp := t.TempDir()
	r := &rig{tmp: tmp, card: filepath.Join(tmp, "card"), sock: filepath.Join(tmp, "browse.sock"), t: t}
	r.dir, r.folder = filepath.Join(r.card, ".deezer"), filepath.Join(r.card, "Deezer")
	os.MkdirAll(r.dir, 0o755)
	if arl != "" {
		os.WriteFile(filepath.Join(r.dir, "arl"), []byte(arl+"\n"), 0o600)
	}
	if organize != "" {
		os.WriteFile(filepath.Join(r.dir, "organize"), []byte(organize+"\n"), 0o644)
	}
	r.start(f)
	return r
}

func (r *rig) start(f *fakeDeezer) {
	cardReady = func() bool { return true }
	retryIn = 200 * time.Millisecond
	transport = func() http.RoundTripper { return f }
	r.d = start(r.dir, r.folder, filepath.Join(r.tmp, "state"), r.sock, filepath.Join(r.tmp, "ctl.sock"))
	r.done = make(chan struct{})
	done := r.done
	go func() { r.d.run(); close(done) }()
	r.t.Cleanup(r.stop)
}

func (r *rig) stop() {
	r.d.shutdown()
	<-r.done
}

func (r *rig) ask(op string, args ...string) ([]string, [][]string) {
	r.t.Helper()
	var c net.Conn
	var err error
	for i := 0; i < 50; i++ {
		if c, err = net.Dial("unix", r.sock); err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err != nil {
		r.t.Fatal(err)
	}
	defer c.Close()
	line := "5\t" + op
	for _, a := range args {
		line += "\t" + q2spot.Escape(a)
	}
	c.Write([]byte("Q2SPOT/1\n" + line + "\n"))
	c.(*net.UnixConn).CloseWrite()
	b, _ := io.ReadAll(c)
	lines := strings.Split(strings.TrimSuffix(string(b), "\n"), "\n")
	fields := func(l string) []string {
		fs := strings.Split(l, "\t")
		for i := range fs {
			fs[i], _ = q2spot.Unescape(fs[i])
		}
		return fs
	}
	var rows [][]string
	for _, l := range lines[2:] {
		rows = append(rows, fields(l))
	}
	return fields(lines[1]), rows
}

func (r *rig) state() map[string]string {
	b, _ := os.ReadFile(filepath.Join(r.tmp, "state"))
	kv := map[string]string{}
	for _, l := range strings.Split(string(b), "\n") {
		if k, v, ok := strings.Cut(l, "="); ok {
			kv[k] = v
		}
	}
	return kv
}

// downloaded is the folder's files.
func (r *rig) downloaded() []string {
	es, _ := os.ReadDir(r.folder)
	var names []string
	for _, e := range es {
		names = append(names, e.Name())
	}
	sort.Strings(names)
	return names
}

func titlesOf(rows [][]string) []string {
	var out []string
	for _, r := range rows {
		out = append(out, r[0]+" "+r[2])
	}
	return out
}

func waitFor(t *testing.T, what string, d time.Duration, ok func() bool) {
	t.Helper()
	for end := time.Now().Add(d); !ok(); time.Sleep(10 * time.Millisecond) {
		if time.Now().After(end) {
			t.Fatalf("timed out waiting for %s", what)
		}
	}
}

// vorbis is a FLAC's Vorbis comments, and whether it has a picture.
func vorbis(t *testing.T, path string) (map[string]string, bool, []byte) {
	t.Helper()
	f, err := flac.ParseFile(path)
	if err != nil {
		b, _ := os.ReadFile(path)
		t.Fatalf("%s: %v (%d bytes: %q)", path, err, len(b), b[:min(len(b), 48)])
	}
	tags, pic := map[string]string{}, false
	for _, m := range f.Meta {
		switch m.Type {
		case flac.VorbisComment:
			c, err := flacvorbis.ParseFromMetaDataBlock(*m)
			if err != nil {
				t.Fatal(err)
			}
			for _, kv := range c.Comments {
				k, v, _ := strings.Cut(kv, "=")
				tags[k] = v
			}
		case flac.Picture:
			pic = true
		}
	}
	return tags, pic, []byte(f.Frames)
}

// Not signed in: the page offers the sign-in code, which the status file carries; nothing else.
func TestSignedOut(t *testing.T) {
	signinPort = freePort(t)
	r := newRig(t, "", newFakeDeezer(t))
	head, rows := r.ask("menu")
	if head[1] != "ok" || head[2] != "refresh" || strings.Join(titlesOf(rows), ",") !=
		"remote Sign In from Your Phone,info A paid Deezer plan is needed,action Stop Deezer" {
		t.Fatalf("menu %q %q", head, rows)
	}
	st := r.state()
	if len(st["pin"]) != 6 || st["hold"] != "0" || st["version"] != version || (st["addr"] != "" && st["qr"] == "") {
		t.Fatalf("state %v", st)
	}
	if head, _ := r.ask("playlists"); head[1] != "login" {
		t.Fatalf("playlists %q", head)
	}
	if head, _ := r.ask("caps"); head[2] != version {
		t.Fatalf("caps %q", head)
	}
}

// The sign-in page: the PIN or the code's key; an ARL is checked with Deezer, a free account
// refused, a paid one saved; then the code leaves the status file and the page lists the music.
func TestSignin(t *testing.T) {
	signinPort = freePort(t)
	f := newFakeDeezer(t)
	r := newRig(t, "", f)
	base := fmt.Sprintf("http://127.0.0.1:%d", r.d.signin.port)
	jar, _ := cookiejar.New(nil)
	hc := &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	post := func(path string, body any) (int, map[string]any) {
		b, _ := json.Marshal(body)
		req, _ := http.NewRequest("POST", base+"/api/"+path, bytes.NewReader(b))
		req.Header.Set("X-Q2", "1")
		resp, err := hc.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var out map[string]any
		json.NewDecoder(resp.Body).Decode(&out)
		return resp.StatusCode, out
	}
	if code, _ := post("arl", map[string]string{"arl": goodARL}); code != 401 {
		t.Fatalf("unauthenticated arl: %d", code)
	}
	if code, _ := post("login", map[string]string{"pin": "x"}); code != 403 {
		t.Fatalf("wrong pin: %d", code)
	}
	if resp, err := hc.Get(base + "/?k=" + r.d.signin.key); err != nil || resp.StatusCode != 302 {
		t.Fatalf("key: %v %v", resp, err)
	}
	if code, out := post("arl", map[string]string{"arl": "not an arl"}); code != 400 || !strings.Contains(out["error"].(string), "ARL") {
		t.Fatalf("shape: %d %v", code, out)
	}
	if code, out := post("arl", map[string]string{"arl": strings.Repeat("cd", 96)}); code != 403 || !strings.Contains(out["error"].(string), "didn't accept") {
		t.Fatalf("refused: %d %v", code, out)
	}
	f.set(func() { f.free = true })
	if code, out := post("arl", map[string]string{"arl": goodARL}); code != 403 || !strings.Contains(out["error"].(string), "no paid plan") {
		t.Fatalf("free: %d %v", code, out)
	}
	if _, err := os.Stat(filepath.Join(r.dir, "arl")); !os.IsNotExist(err) {
		t.Fatal("a free account's arl saved")
	}
	f.set(func() { f.free = false })
	if code, out := post("arl", map[string]string{"arl": " " + goodARL + "\n"}); code != 200 || out["name"] != "Ana" || out["plan"] != "Deezer Premium" {
		t.Fatalf("good: %d %v", code, out)
	}
	if b, _ := os.ReadFile(filepath.Join(r.dir, "arl")); string(b) != goodARL+"\n" {
		t.Fatalf("arl file %q", b)
	}
	waitFor(t, "the code gone", 5*time.Second, func() bool { _, has := r.state()["pin"]; return !has })
	if _, rows := r.ask("menu"); len(rows) != 11 || rows[1][2] != "Liked Songs" {
		t.Fatalf("menu %q", rows)
	}
	if code, _ := post("signout", map[string]string{}); code != 200 {
		t.Fatal("signout")
	}
	waitFor(t, "the code back", 5*time.Second, func() bool { return r.state()["pin"] != "" })
}

func freePort(t *testing.T) int {
	l, err := net.Listen("tcp4", ":0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

// A free account (its arl put on the card by hand) is told downloads need a paid plan.
func TestFreeAccount(t *testing.T) {
	f := newFakeDeezer(t)
	f.free = true
	r := newRig(t, goodARL, f)
	waitFor(t, "the free account", 5*time.Second, func() bool {
		_, rows := r.ask("menu")
		return rows[0][2] == "This account has no paid plan"
	})
	if head, _ := r.ask("download", "playlist:99#1"); head[1] != "premium" {
		t.Fatalf("download %q", head)
	}
}

// The menu, the lists, and downloads: a song, a whole list, an album with its own details in its
// tags; each a file named as OpenDeezer names them, decrypted and tagged, then a local row.
func TestDownloads(t *testing.T) {
	f := newFakeDeezer(t)
	r := newRig(t, goodARL, f)
	waitFor(t, "signed in", 5*time.Second, func() bool { return r.d.authState() == authOK })
	head, rows := r.ask("menu")
	want := "local Downloaded Songs,list Liked Songs,list Playlists,list Albums,list Artists,search Search Deezer," +
		"list Downloads,action Quality: FLAC,action Organize Files: Off,list Account,action Stop Deezer"
	if head[2] != "refresh" || strings.Join(titlesOf(rows), ",") != want || rows[0][1] != r.folder+"/" ||
		rows[0][3] != "0 songs on the card" || rows[1][1] != "tracks" || rows[1][4] != "liked" || rows[1][5] != "download" {
		t.Fatalf("menu %q", rows)
	}
	if _, rows = r.ask("playlists"); strings.Join(titlesOf(rows), ",") != "list Flow,list Top Charts,list Haunted" ||
		rows[2][1] != "tracks" || rows[2][4] != "playlist:99" || rows[2][5] != "download" || rows[2][3] != "3 songs" {
		t.Fatalf("playlists %q", rows)
	}
	_, rows = r.ask("tracks", "playlist:99")
	if strings.Join(titlesOf(rows), ",") != "action Download All,action The Be Colony,action Mother Is the Milky Way,action Ritual Looks" ||
		rows[0][4] != "playlist:99" || rows[0][3] != "3 songs" || rows[2][1] != "download" || rows[2][4] != "playlist:99#2" ||
		rows[2][3] != "Broadcast" {
		t.Fatalf("playlist %q", rows)
	}
	// One song: decrypted, tagged and named as OpenDeezer does, then a local row.
	if head, _ := r.ask("download", "playlist:99#2"); head[1] != "ok" || head[2] != "Downloading" {
		t.Fatalf("download %q", head)
	}
	name := "Mother Is the Milky Way - Broadcast [2].flac"
	waitFor(t, "the file", 5*time.Second, func() bool { return strings.Join(r.downloaded(), ",") == name })
	tags, pic, frames := vorbis(t, filepath.Join(r.folder, name))
	if tags["TITLE"] != "Mother Is the Milky Way" || tags["ARTIST"] != "Broadcast" || tags["ALBUM"] != "Witch Cults" || !pic {
		t.Fatalf("tags %v %v", tags, pic)
	}
	if !bytes.Equal(frames, plain("2", 30<<10)[42:]) {
		t.Fatal("the audio isn't the decrypted file's")
	}
	_, rows = r.ask("tracks", "playlist:99")
	if rows[2][0] != "local" || rows[2][1] != filepath.Join(r.folder, name) || rows[2][3] != "Broadcast · on the card" ||
		rows[0][3] != "3 songs · 1 on the card" {
		t.Fatalf("after %q", rows)
	}
	if head, _ := r.ask("download", "playlist:99#2"); head[2] != "Already on the card" {
		t.Fatalf("again %q", head)
	}
	// The whole list: the rest queued; one (3) has only a preview here, so it fails, and isn't saved.
	if head, _ := r.ask("download", "playlist:99"); head[2] != "Downloading 2 songs" {
		t.Fatalf("all %q", head)
	}
	waitFor(t, "the list", 5*time.Second, func() bool {
		_, rows := r.ask("downloads")
		return len(rows) > 0 && rows[0][2] == "Try the Failed Again"
	})
	if got := strings.Join(r.downloaded(), ","); got != "Mother Is the Milky Way - Broadcast [2].flac,The Be Colony - Broadcast [1].flac" {
		t.Fatalf("files %s", got)
	}
	_, rows = r.ask("downloads")
	if strings.Join(titlesOf(rows), ",") != "action Try the Failed Again,action Clear the Failed,action Ritual Looks,"+
		"local The Be Colony,local Mother Is the Milky Way" || rows[2][3] != "Failed (not available in full here): press to try again" {
		t.Fatalf("downloads %q", rows)
	}
	if _, rows = r.ask("tracks", "playlist:99"); rows[3][3] != "Broadcast · failed: press to try again" {
		t.Fatalf("failed row %q", rows[3])
	}
	r.ask("clearfailed")
	// An album: its title, cover, year and each song's number in the tags, from the album's details.
	if _, rows = r.ask("albums"); strings.Join(titlesOf(rows), ",") != "list Tender Buttons" || rows[0][4] != "album:7" {
		t.Fatalf("albums %q", rows)
	}
	if head, _ := r.ask("download", "album:7"); head[2] != "Downloading 2 songs" { // Play/Pause held on its row
		t.Fatalf("album %q", head)
	}
	valerie := filepath.Join(r.folder, "Valerie - Broadcast [5].flac")
	waitFor(t, "the album", 5*time.Second, func() bool { _, err := os.Stat(valerie); return err == nil })
	tags, pic, _ = vorbis(t, valerie)
	if tags["ALBUM"] != "Tender Buttons" || tags["DATE"] != "2005" || tags["TRACKNUMBER"] != "2" || !pic ||
		f.count("cover pic7") == 0 {
		t.Fatalf("album tags %v %v", tags, pic)
	}
	_, rows = r.ask("tracks", "album:7")
	if rows[0][0] != "local" || rows[0][2] != "All on the card" || rows[0][1] != r.folder+"/" {
		t.Fatalf("all on the card %q", rows[0])
	}
	if _, rows = r.ask("menu"); rows[0][3] != "4 songs on the card" || rows[6][3] != "4 songs downloaded" {
		t.Fatalf("menu after %q", rows)
	}
	// Quality: the media service is asked for MP3 320 at most; the file is an MP3.
	if head, _ := r.ask("quality"); head[2] != "Downloads in MP3 320" {
		t.Fatalf("quality %q", head)
	}
	if b, _ := os.ReadFile(filepath.Join(r.dir, "quality")); string(b) != "mp3_320\n" {
		t.Fatalf("quality file %q", b)
	}
	r.ask("download", "flow#6")
	waitFor(t, "the MP3", 5*time.Second, func() bool {
		_, err := os.Stat(filepath.Join(r.folder, "Corporeal - Broadcast [6].mp3"))
		return err == nil
	})
	f.mu.Lock()
	formats := strings.Join(f.formats, ",")
	f.mu.Unlock()
	if formats != "MP3_320,MP3_128,MP3_64,MP3_MISC" {
		t.Fatalf("formats %s", formats)
	}
	// Signing out keeps the downloads.
	r.ask("signout")
	if _, rows = r.ask("menu"); rows[0][0] != "remote" || rows[2][2] != "Downloaded Songs" || rows[2][3] != "5 songs on the card" {
		t.Fatalf("signed out %q", rows)
	}
}

// Liked Songs, an artist's page, Flow and the chart, and search.
func TestBrowse(t *testing.T) {
	f := newFakeDeezer(t)
	r := newRig(t, goodARL, f)
	if _, rows := r.ask("tracks", "liked"); strings.Join(titlesOf(rows), ",") != "action Download All,action Mother Is the Milky Way,action The Be Colony" {
		t.Fatalf("liked %q", rows)
	}
	_, rows := r.ask("artists")
	if len(rows) != 1 || rows[0][1] != "artist" || rows[0][4] != "5" {
		t.Fatalf("artists %q", rows)
	}
	_, rows = r.ask("artist", "5")
	if strings.Join(titlesOf(rows), ",") != "list Top Songs,list Artist Mix,list Tender Buttons,list Stereolab" ||
		rows[0][4] != "artisttop:5" || rows[1][4] != "artistmix:5" || rows[2][4] != "album:7" || rows[3][1] != "artist" || rows[3][3] != "Similar artist" {
		t.Fatalf("artist %q", rows)
	}
	if _, rows = r.ask("tracks", "artisttop:5"); len(rows) != 3 || rows[1][2] != "Mother Is the Milky Way" {
		t.Fatalf("top %q", rows)
	}
	if _, rows = r.ask("tracks", "artistmix:5"); len(rows) != 3 || rows[1][2] != "Valerie" {
		t.Fatalf("mix %q", rows)
	}
	if _, rows = r.ask("tracks", "flow"); len(rows) != 3 || rows[1][2] != "Corporeal" {
		t.Fatalf("flow %q", rows)
	}
	if _, rows = r.ask("tracks", "chart"); len(rows) != 2 || rows[1][2] != "The Be Colony" {
		t.Fatalf("chart %q", rows)
	}
	_, rows = r.ask("search", "tears")
	if strings.Join(titlesOf(rows), ",") != "list Songs,list Albums,list Artists,list Playlists" || rows[0][1] != "found" ||
		rows[0][4] != "tracks:tears" || rows[0][3] != "1 song" {
		t.Fatalf("search %q", rows)
	}
	if _, rows = r.ask("found", "tracks:tears"); len(rows) != 2 || rows[1][4] != "search:tears#4" {
		t.Fatalf("found tracks %q", rows)
	}
	if _, rows = r.ask("found", "playlists:tears"); len(rows) != 1 || rows[0][4] != "playlist:99" {
		t.Fatalf("found playlists %q", rows)
	}
	if n := f.count("/search"); n != 1 {
		t.Fatalf("%d searches", n)
	}
	if head, _ := r.ask("tracks", "spotify:playlist:1"); head[1] != "bad" {
		t.Fatalf("bad list %q", head)
	}
	if _, rows = r.ask("account"); rows[0][2] != "Ana" || rows[0][3] != "Deezer Premium" || rows[1][1] != "signout" {
		t.Fatalf("account %q", rows)
	}
}

// A list slower than the Q2 waits (answerIn): Loading, asked again ("refresh") while the one fetch
// carries on, then the list; a held download of a list still loading, a toast.
func TestSlowList(t *testing.T) {
	f := newFakeDeezer(t)
	gate := make(chan struct{})
	var opened sync.Once
	open := func() { opened.Do(func() { close(gate) }) }
	t.Cleanup(open) // before the fake's Close, which waits for its handlers
	f.set(func() { f.slow = gate })
	answerIn = 100 * time.Millisecond
	t.Cleanup(func() { answerIn = 8 * time.Second })
	r := newRig(t, goodARL, f)
	for i := 0; i < 2; i++ {
		if head, rows := r.ask("albums"); head[1] != "ok" || head[2] != "refresh" || len(rows) != 1 || rows[0][2] != "Loading…" {
			t.Fatalf("albums while loading: %q %q", head, rows)
		}
	}
	if head, _ := r.ask("download", "playlist:99"); head[1] != "unavailable" || !strings.HasPrefix(head[2], "Still loading") {
		t.Fatalf("download while loading: %q", head)
	}
	if n := f.count("deezer.pageProfile"); n != 1 {
		t.Fatalf("%d pageProfile requests", n)
	}
	open()
	waitFor(t, "the albums", 5*time.Second, func() bool {
		head, rows := r.ask("albums")
		return head[2] == "" && len(rows) == 1 && rows[0][2] == "Tender Buttons"
	})
	if n := f.count("deezer.pageProfile"); n != 1 {
		t.Fatalf("%d pageProfile requests", n)
	}
}

// The queue: kept on the card while it waits, so a stop (USB storage) leaves it for next time;
// the Q2 held awake while a song downloads; a waiting one removed; and offline it waits, then
// carries on by itself.
func TestQueue(t *testing.T) {
	f := newFakeDeezer(t)
	gate := make(chan struct{})
	f.hold["1"] = gate
	r := newRig(t, goodARL, f)
	r.ask("download", "playlist:99")
	waitFor(t, "downloading", 5*time.Second, func() bool { return r.state()["hold"] == "1" })
	_, rows := r.ask("downloads")
	if strings.Join(titlesOf(rows), ",") != "info Downloading The Be Colony,action Remove All Waiting,action Mother Is the Milky Way,action Ritual Looks" {
		t.Fatalf("queue %q", rows)
	}
	waitFor(t, "the queue saved", 5*time.Second, func() bool {
		b, _ := os.ReadFile(filepath.Join(r.dir, "queue.json"))
		return strings.Count(string(b), `"track"`) == 3
	})
	// Stopped mid-song: no part left, the three kept.
	r.stop()
	if names := r.downloaded(); len(names) != 0 {
		t.Fatalf("left %q", names)
	}
	b, _ := os.ReadFile(filepath.Join(r.dir, "queue.json"))
	if strings.Count(string(b), `"track"`) != 3 {
		t.Fatalf("queue %s", b)
	}
	// Started again, offline: it waits, then goes on by itself.
	f.set(func() { delete(f.hold, "1") })
	f.set(func() { f.offline = true })
	r.start(f)
	r.ask("cancel", "3")
	waitFor(t, "the stall", 5*time.Second, func() bool {
		_, rows := r.ask("menu")
		return rows[0][2] == "Deezer can't be reached"
	})
	f.set(func() { f.offline = false })
	waitFor(t, "the rest", 10*time.Second, func() bool { return len(r.downloaded()) == 2 })
	if _, err := os.Stat(filepath.Join(r.dir, "queue.json")); !os.IsNotExist(err) {
		waitFor(t, "the queue gone", 5*time.Second, func() bool {
			_, err := os.Stat(filepath.Join(r.dir, "queue.json"))
			return os.IsNotExist(err)
		})
	}
	waitFor(t, "no hold", 3*time.Second, func() bool { return r.state()["hold"] == "0" })
	close(gate)
}

// A track under way when q2-deezer dies (killed, out of memory) is counted on the card first, as a
// stop is not: after maxTries such deaths the queue moves it to the failed ones instead of starting
// it again first after every restart, which kept the add-on down. The temp files an interrupted
// download left are swept at the start, the folder's other files kept.
func TestQueueAfterDeaths(t *testing.T) {
	f := newFakeDeezer(t)
	gate := make(chan struct{})
	f.hold["1"] = gate
	r := newRig(t, goodARL, f)
	queue := filepath.Join(r.dir, "queue.json")
	r.ask("download", "playlist:99#1")
	waitFor(t, "counted under way", 5*time.Second, func() bool {
		b, _ := os.ReadFile(queue)
		return strings.Contains(string(b), `"tries":1`)
	})
	r.stop()
	if b, _ := os.ReadFile(queue); !strings.Contains(string(b), `"track"`) || strings.Contains(string(b), `"tries"`) {
		t.Fatalf("a stop counted as a death: %s", b)
	}
	// It died under way maxTries times, the last leaving its temp files.
	var kept []item
	b, _ := os.ReadFile(queue)
	if err := json.Unmarshal(b, &kept); err != nil || len(kept) != 1 {
		t.Fatalf("queue %s: %v", b, err)
	}
	kept[0].Tries = maxTries
	b, _ = json.Marshal(kept)
	os.WriteFile(queue, b, 0o644)
	part := "The Be Colony - Broadcast [1]-4242.flac.part"
	for _, n := range []string{part, part + ".tag-77", "notes.txt"} {
		os.WriteFile(filepath.Join(r.folder, n), []byte("x"), 0o644)
	}
	f.set(func() { delete(f.hold, "1") })
	r.start(f)
	waitFor(t, "given up", 5*time.Second, func() bool {
		_, rows := r.ask("downloads")
		for _, row := range rows {
			if row[2] == "The Be Colony" && strings.Contains(row[3], "it stopped Deezer each time") {
				return true
			}
		}
		return false
	})
	if got := strings.Join(r.downloaded(), ","); got != "notes.txt" {
		t.Fatalf("folder %s", got)
	}
	// Tried again by hand, it starts afresh and comes.
	r.ask("retry", "1")
	waitFor(t, "the retry", 5*time.Second, func() bool {
		return strings.Join(r.downloaded(), ",") == "The Be Colony - Broadcast [1].flac,notes.txt"
	})
	close(gate)
}

// Nothing to download and its page unused: it stops by itself.
func TestIdle(t *testing.T) {
	idleFor = 300 * time.Millisecond
	defer func() { idleFor = 15 * time.Minute }()
	r := newRig(t, "", newFakeDeezer(t))
	select {
	case <-r.done:
	case <-time.After(5 * time.Second):
		t.Fatal("not stopped when idle")
	}
}
