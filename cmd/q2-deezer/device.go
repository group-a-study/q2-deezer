package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/group-a-study/q2-deezer/internal/deezer"
	"github.com/group-a-study/q2-deezer/internal/q2spot"
)

// The Q2's Deezer page (Q2 Pod's card app page, q2-pod's patch/addon.c): the
// account's music and Deezer's catalogue, each list opened by a row (kind list: its uri the op, its
// context the argument), a song downloaded by a press (kind action: download <list>#<id>), or in
// Stream mode played from it (stream <list>#<id>, stream.go), a whole list by its Download All row
// or Play/Pause held on its row (the row's position: the op held sends). A song on the card is a local row: a press plays it from the card, as local music (its
// uri a file; a folder's ends "/", which lists its songs; context "album", in track order, as an
// organized album's folder holds them). Lists whose rows change as downloads finish say "refresh",
// asked again every 2 s while on top.
//
// Lists ("contexts") are named liked, flow, chart, playlist:<id>, album:<id>, artistmix:<id>,
// artisttop:<id> and search:<term>; fetched whole (OpenDeezer's paging) and kept two minutes, so
// a download takes the tracks as the page showed them. Cursors are offsets.
//
// A big list (an account's 1,376 liked songs are 7 of Deezer's pages, a 2,320-song playlist 12)
// can take longer than the Q2 waits (15 s, q2-pod's patch/addon.c), so a request waits at most answerIn for
// its fetch, which carries on: the page says Loading and asks again ("refresh") until it's there.
// Once kept, a list past keepFor is still shown while it is fetched again.

const keepFor = 2 * time.Minute

var answerIn = 8 * time.Second // tests shorten it

// errLoading is a list still being fetched after answerIn.
var errLoading = errors.New("still loading")

type cache struct {
	mu    sync.Mutex
	m     map[string]*entry
	slots chan struct{} // the fetches under way, at most its capacity: the Q2's memory is small
}

// entry is a key's listing (fetched at at; at zero before the first), the fetch under way (busy,
// closed as it ends), and the last fetch's failure while there is no listing to show.
type entry struct {
	l    listing
	at   time.Time
	err  error
	busy chan struct{}
}

type listing struct {
	rows   []q2spot.Row
	tracks []deezer.Track
	search *deezer.SearchResults
}

func newCache() *cache { return &cache{m: map[string]*entry{}, slots: make(chan struct{}, 2)} }

func (c *cache) clear() {
	c.mu.Lock()
	c.m = map[string]*entry{}
	c.mu.Unlock()
}

// get is key's listing: a kept one younger than keepFor; else fetch's, waited for at most answerIn
// (errLoading after, the fetch carrying on); else, while fetch runs again, the one kept.
func (c *cache) get(key string, fetch func() (listing, error)) (listing, error) {
	c.mu.Lock()
	e := c.m[key]
	if e == nil {
		if len(c.m) >= 32 {
			for k, old := range c.m {
				if old.busy == nil {
					delete(c.m, k)
				}
			}
		}
		e = &entry{}
		c.m[key] = e
	}
	if !e.at.IsZero() && time.Since(e.at) < keepFor {
		defer c.mu.Unlock()
		return e.l, nil
	}
	if e.busy == nil {
		if err := e.err; err != nil { // it failed after its request stopped waiting: this one says so, the next tries again
			delete(c.m, key)
			c.mu.Unlock()
			return listing{}, err
		}
		e.busy = make(chan struct{})
		go c.run(e, fetch)
	}
	busy, kept, l := e.busy, !e.at.IsZero(), e.l
	c.mu.Unlock()
	if kept {
		return l, nil
	}
	select {
	case <-busy:
	case <-time.After(answerIn):
		return listing{}, errLoading
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if e.at.IsZero() {
		if c.m[key] == e {
			delete(c.m, key) // told: the next request tries again
		}
		return listing{}, e.err
	}
	return e.l, nil
}

// run is e's fetch, when a slot is free; a failure keeps a listing e already has. A fetch never
// gets from the cache itself, which could leave every slot waiting on one more.
func (c *cache) run(e *entry, fetch func() (listing, error)) {
	c.slots <- struct{}{}
	l, err := fetch()
	<-c.slots
	c.mu.Lock()
	if err == nil {
		e.l, e.at, e.err = l, time.Now(), nil
	} else if e.at.IsZero() {
		e.err = err
	}
	close(e.busy)
	e.busy = nil
	c.mu.Unlock()
}

// page is rows from cursor on, as many as fit, with the cursor for the rest; msg the reply's.
func page(rows []q2spot.Row, cursor, msg string) q2spot.Reply {
	from, _ := strconv.Atoi(cursor)
	if from < 0 || from > len(rows) {
		from = len(rows)
	}
	r := q2spot.Reply{Message: msg, Rows: rows[from:]}
	if _, kept := r.Encode("0"); from+kept < len(rows) {
		r.Rows = rows[from : from+kept]
		r.Cursor = strconv.Itoa(from + kept)
	}
	return r
}

func info(title, sub string) q2spot.Row { return q2spot.Row{Kind: "info", Title: title, Subtitle: sub} }

func artistNames(as []deezer.Artist) string {
	names := make([]string, 0, len(as))
	for _, a := range as {
		if a.Name != "" {
			names = append(names, a.Name)
		}
	}
	return strings.Join(names, ", ")
}

// open is a row opening a list: its tracks (op tracks), held Play/Pause downloading them.
func open(ctx, title, sub string) q2spot.Row {
	return q2spot.Row{Kind: "list", URI: "tracks", Context: ctx, Title: title, Subtitle: sub, Position: "download"}
}

func albumRow(a deezer.Album) q2spot.Row {
	return open("album:"+a.ID, a.Name, artistNames(a.Artists))
}

func artistRow(a deezer.ArtistInfo, sub string) q2spot.Row {
	return q2spot.Row{Kind: "list", URI: "artist", Context: a.ID, Title: a.Name, Subtitle: sub}
}

func playlistRow(p deezer.Playlist) q2spot.Row {
	sub := p.Owner
	if sub == "" && p.TrackCount > 0 {
		sub = plural(p.TrackCount, "song")
	}
	return open("playlist:"+p.ID, p.Name, sub)
}

// trackRow is a song of the list ctx: on the card, a local row that plays it; else a press
// downloads it, or in Stream mode plays it (Play/Pause held still downloads it), its state beside
// its artists.
func (d *daemon) trackRow(t deezer.Track, ctx string) q2spot.Row {
	artists := t.ArtistLine()
	op := "download"
	if d.streaming() {
		op = "stream"
		if d.st.loading(t.ID) {
			artists += " · loading"
		}
	}
	switch st, extra := d.dl.state(t.ID); st {
	case "done":
		return q2spot.Row{Kind: "local", URI: extra, Context: d.dl.order(filepath.Dir(extra)), Title: t.Name,
			Subtitle: artists + " · on the card"}
	case "downloading":
		artists += " · downloading"
	case "waiting":
		artists += " · waiting"
	case "failed":
		artists += " · failed: press to try again"
	}
	return q2spot.Row{Kind: "action", URI: op, Context: ctx + "#" + t.ID, Title: t.Name, Subtitle: artists,
		Position: "download"}
}

// fail is an error as the page shows it; a list still loading, a row saying so while the page
// asks again.
func fail(err error) q2spot.Reply {
	switch {
	case errors.Is(err, errLoading):
		return q2spot.Reply{Message: "refresh", Rows: []q2spot.Row{info("Loading…", "Deezer is sending a long list")}}
	case errors.Is(err, errNoARL):
		return q2spot.Fail(q2spot.Login, "Sign in to Deezer from your phone")
	case deezer.IsARLExpired(err):
		return q2spot.Fail(q2spot.Login, "Your Deezer sign-in has expired: sign in again from your phone")
	case deezer.IsNoNetwork(err):
		return q2spot.Fail(q2spot.Offline, "Deezer can't be reached")
	}
	return q2spot.Fail(q2spot.Error, "Deezer didn't answer")
}

func (d *daemon) device(req q2spot.Request) q2spot.Reply {
	d.touch()
	ok := func(msg string) q2spot.Reply { return q2spot.Reply{Message: msg} }
	switch req.Op {
	case "caps":
		var rows []q2spot.Row
		for _, op := range []string{"caps", "menu", "playlists", "albums", "artists", "artist", "tracks", "search",
			"found", "downloads", "account", "download", "cancel", "cancelall", "retry", "clearfailed", "quality", "organize",
			"mode", "stream", "cardartists", "cardalbums", "signout", "stop"} {
			rows = append(rows, q2spot.Row{Kind: "op", URI: op})
		}
		return q2spot.Reply{Message: version, Rows: rows}
	case "menu":
		return d.menu()
	case "downloads":
		return d.downloadsPage(cursor(req, 0))
	case "cancel":
		if !d.dl.remove(req.Arg(0)) {
			return q2spot.Fail(q2spot.Unavailable, "It isn't waiting any more")
		}
		return ok("Removed")
	case "cancelall":
		return ok(plural(d.dl.clear(), "song") + " removed")
	case "retry":
		if d.dl.retry(req.Arg(0)) == 0 {
			return ok("Nothing to try again")
		}
		return ok("Trying again")
	case "clearfailed":
		d.dl.clearFailed()
		return ok("Cleared")
	case "quality":
		next := qualities[0]
		for i, q := range qualities {
			if q == d.quality() {
				next = qualities[(i+1)%len(qualities)]
			}
		}
		if err := d.setQuality(next); err != nil {
			return q2spot.Fail(q2spot.Error, "Couldn't save the setting")
		}
		return ok("Downloads in " + qualityNames[next])
	case "organize":
		on := !d.organized()
		if err := d.setOrganize(on); err != nil {
			return q2spot.Fail(q2spot.Error, "Couldn't save the setting")
		}
		if on {
			return ok("New downloads go in Artist/Year - Album folders")
		}
		return ok("New downloads go straight in Deezer/")
	case "mode":
		stream := !d.streaming()
		if err := d.setMode(stream); err != nil {
			return q2spot.Fail(q2spot.Error, "Couldn't save the setting")
		}
		if stream {
			return ok("A press plays the song; nothing is kept")
		}
		return ok("A press downloads the song")
	case "cardartists":
		return d.cardArtists(cursor(req, 0))
	case "cardalbums":
		return d.cardAlbums(req.Arg(0), cursor(req, 1))
	case "signout":
		if err := d.signOut(); err != nil {
			return q2spot.Fail(q2spot.Error, "Couldn't remove .deezer/arl")
		}
		return ok("Signed out; your downloads stay")
	case "stop":
		go func() { time.Sleep(300 * time.Millisecond); d.shutdown() }() // the reply first
		return ok("Deezer stopped")
	}
	dz, err := d.session()
	if err != nil {
		return fail(err)
	}
	list := func(key, cursor, msg string, fetch func() (listing, error)) q2spot.Reply {
		l, err := d.cache.get(key, fetch)
		if err != nil {
			return fail(err)
		}
		return page(l.rows, cursor, msg)
	}
	switch req.Op {
	case "account":
		a := dz.Account()
		plan := a.Offer
		if !a.Premium {
			plan += ": downloads need a paid plan"
		}
		return q2spot.Reply{Rows: []q2spot.Row{info(a.Name, plan),
			{Kind: "action", URI: "signout", Title: "Sign Out", Subtitle: "Removes your ARL; your downloads stay"}}}
	case "playlists":
		return list("playlists", cursor(req, 0), "", func() (listing, error) {
			ps, err := dz.Playlists()
			l := listing{rows: []q2spot.Row{open("flow", "Flow", "Your mix of the moment"),
				open("chart", "Top Charts", "Deezer's most played")}}
			for _, p := range ps {
				l.rows = append(l.rows, playlistRow(p))
			}
			return l, err
		})
	case "albums":
		return list("albums", cursor(req, 0), "", func() (listing, error) {
			as, err := dz.FavoriteAlbums()
			var l listing
			for _, a := range as {
				l.rows = append(l.rows, albumRow(a))
			}
			return l, err
		})
	case "artists":
		return list("artists", cursor(req, 0), "", func() (listing, error) {
			as, err := dz.FavoriteArtists()
			var l listing
			for _, a := range as {
				l.rows = append(l.rows, artistRow(a, ""))
			}
			return l, err
		})
	case "artist":
		id := req.Arg(0)
		if id == "" {
			return q2spot.Fail(q2spot.Bad, "Bad request")
		}
		return list("artist:"+id, cursor(req, 1), "", func() (listing, error) {
			pg, err := dz.ArtistProfile(id)
			if err != nil {
				return listing{}, err
			}
			l := listing{rows: []q2spot.Row{
				open("artisttop:"+id, "Top Songs", plural(len(pg.Top), "song")),
				open("artistmix:"+id, "Artist Mix", "Songs like "+pg.Artist.Name)}}
			for _, a := range pg.Albums {
				l.rows = append(l.rows, albumRow(a))
			}
			for _, a := range pg.Related {
				l.rows = append(l.rows, artistRow(a, "Similar artist"))
			}
			return l, nil
		})
	case "tracks":
		ctx := req.Arg(0)
		if !listCtx(ctx) {
			return q2spot.Fail(q2spot.Bad, "Bad request")
		}
		return d.tracksPage(dz, ctx, cursor(req, 1))
	case "search":
		term := strings.TrimSpace(req.Arg(0))
		if term == "" {
			return q2spot.Fail(q2spot.Bad, "Search for what?")
		}
		sr, err := d.search(dz, term)
		if err != nil {
			return fail(err)
		}
		return q2spot.Reply{Rows: []q2spot.Row{
			{Kind: "list", URI: "found", Context: "tracks:" + term, Title: "Songs", Subtitle: plural(len(sr.Tracks), "song")},
			{Kind: "list", URI: "found", Context: "albums:" + term, Title: "Albums", Subtitle: plural(len(sr.Albums), "album")},
			{Kind: "list", URI: "found", Context: "artists:" + term, Title: "Artists", Subtitle: plural(len(sr.Artists), "artist")},
			{Kind: "list", URI: "found", Context: "playlists:" + term, Title: "Playlists",
				Subtitle: plural(len(sr.Playlists), "playlist")},
		}}
	case "found":
		kind, term, _ := strings.Cut(req.Arg(0), ":")
		if kind == "tracks" {
			return d.tracksPage(dz, "search:"+term, cursor(req, 1))
		}
		sr, err := d.search(dz, term)
		if err != nil {
			return fail(err)
		}
		var rows []q2spot.Row
		switch kind {
		case "albums":
			for _, a := range sr.Albums {
				rows = append(rows, albumRow(a))
			}
		case "artists":
			for _, a := range sr.Artists {
				rows = append(rows, artistRow(a, ""))
			}
		case "playlists":
			for _, p := range sr.Playlists {
				rows = append(rows, playlistRow(p))
			}
		default:
			return q2spot.Fail(q2spot.Bad, "Bad request")
		}
		return page(rows, cursor(req, 1), "")
	case "download":
		return d.download(dz, req.Arg(0))
	case "stream":
		return d.stream(dz, req.Arg(0))
	}
	return q2spot.Fail(q2spot.Unsupported, "Update Deezer on the card")
}

// cursor is a list request's cursor, its argument after the n that name the list (Q2SPOT/1: the
// op's last argument). Q2 Pod's card app page up to 1.4.1 sent each More…'s cursor after the one
// before ("albums 30 60"), which read as the first showed the second page again: the last is the one.
func cursor(req q2spot.Request, n int) string {
	if len(req.Args) <= n {
		return ""
	}
	return req.Args[len(req.Args)-1]
}

// listCtx is a list of songs the page opens.
func listCtx(ctx string) bool {
	switch ctx {
	case "liked", "flow", "chart":
		return true
	}
	kind, rest, ok := strings.Cut(ctx, ":")
	switch kind {
	case "playlist", "album", "artistmix", "artisttop":
		return ok && rest != ""
	case "search":
		return ok && strings.TrimSpace(rest) != ""
	}
	return false
}

// search is a term's results, the four kinds at once (OpenDeezer's Search), kept two minutes.
func (d *daemon) search(dz *deezer.Client, term string) (*deezer.SearchResults, error) {
	l, err := d.cache.get("search\t"+term, func() (listing, error) {
		sr, err := dz.Search(term)
		return listing{search: sr}, err
	})
	if err != nil {
		return nil, err
	}
	return l.search, nil
}

// tracks is a list's songs, as the page shows them and a download takes them: an album's with its
// title, cover and year, which its track list leaves out but the files' tags want.
func (d *daemon) tracks(dz *deezer.Client, ctx string) ([]deezer.Track, error) {
	if term, ok := strings.CutPrefix(ctx, "search:"); ok { // the search's own, as its page counted them
		sr, err := d.search(dz, term)
		if err != nil {
			return nil, err
		}
		return sr.Tracks, nil
	}
	l, err := d.cache.get(ctx, func() (listing, error) {
		var ts []deezer.Track
		var err error
		kind, id, _ := strings.Cut(ctx, ":")
		switch kind {
		case "liked":
			ts, err = dz.Favorites()
		case "flow":
			ts, err = dz.Flow()
		case "chart":
			var ch *deezer.Chart
			if ch, err = dz.Charts("0"); err == nil {
				ts = ch.Tracks
			}
		case "playlist":
			ts, err = dz.PlaylistTracks(id)
		case "artistmix":
			ts, err = dz.ArtistMix(id)
		case "artisttop":
			ts, err = dz.ArtistTop(id)
		case "album":
			if ts, err = dz.AlbumTracks(id); err != nil {
				break
			}
			a, year, aerr := dz.AlbumInfo(id)
			if aerr != nil {
				return listing{}, aerr
			}
			for i := range ts {
				if ts[i].AlbumName == "" {
					ts[i].AlbumName = a.Name
				}
				if ts[i].ArtworkURL == "" {
					ts[i].ArtworkURL = a.ArtworkURL
				}
				if ts[i].Year == "" {
					ts[i].Year = year
				}
			}
		default:
			err = errors.New("unknown list " + ctx)
		}
		return listing{tracks: ts}, err
	})
	return l.tracks, err
}

// tracksPage is a list's songs: Download All first (on its first page), then each song.
func (d *daemon) tracksPage(dz *deezer.Client, ctx, cursor string) q2spot.Reply {
	ts, err := d.tracks(dz, ctx)
	if err != nil {
		return fail(err)
	}
	if len(ts) == 0 {
		return q2spot.Reply{Rows: []q2spot.Row{info("Nothing here", "")}}
	}
	d.dl.fresh()
	on := 0
	rows := make([]q2spot.Row, 0, len(ts)+1)
	rows = append(rows, q2spot.Row{})
	for _, t := range ts {
		r := d.trackRow(t, ctx)
		if r.Kind == "local" {
			on++
		}
		rows = append(rows, r)
	}
	all := q2spot.Row{Kind: "action", URI: "download", Context: ctx, Title: "Download All",
		Subtitle: plural(len(ts), "song")}
	if on > 0 {
		all.Subtitle += fmt.Sprintf(" · %d on the card", on)
	}
	if on == len(ts) {
		all = d.allOnCard(rows[1:])
	} else if d.streaming() {
		rows = append([]q2spot.Row{{Kind: "action", URI: "stream", Context: ctx, Title: "Play All",
			Subtitle: "Stream it, keeping none"}}, rows...)
		rows[1] = all
		return page(rows, cursor, "refresh")
	}
	rows[0] = all
	return page(rows, cursor, "refresh")
}

// stream plays a list's songs in Stream mode (stream.go), from one of them (<list>#<id>) or its first.
func (d *daemon) stream(dz *deezer.Client, arg string) q2spot.Reply {
	if !dz.Account().Premium {
		return q2spot.Fail(q2spot.Premium, "Streaming needs a paid Deezer plan")
	}
	ctx, one := arg, ""
	if i := strings.LastIndexByte(arg, '#'); i >= 0 {
		ctx, one = arg[:i], arg[i+1:]
	}
	if !listCtx(ctx) {
		return q2spot.Fail(q2spot.Bad, "Bad request")
	}
	ts, err := d.tracks(dz, ctx)
	if errors.Is(err, errLoading) {
		return q2spot.Fail(q2spot.Unavailable, "Still loading: try again in a moment")
	}
	if err != nil {
		return fail(err)
	}
	from := 0
	if one != "" {
		from = -1
		for i, t := range ts {
			if t.ID == one {
				from = i
				break
			}
		}
		if from < 0 {
			return q2spot.Fail(q2spot.Unavailable, "The list changed: open it again")
		}
	}
	if from >= len(ts) {
		return q2spot.Fail(q2spot.Unavailable, "Nothing to play")
	}
	d.dl.fresh()
	d.st.start(append([]deezer.Track(nil), ts[from:]...))
	return q2spot.Reply{Message: "Playing in a moment"}
}

// download queues a list's songs, or one of them (<list>#<id>).
func (d *daemon) download(dz *deezer.Client, arg string) q2spot.Reply {
	if !dz.Account().Premium {
		return q2spot.Fail(q2spot.Premium, "Downloads need a paid Deezer plan")
	}
	ctx, one := arg, ""
	if i := strings.LastIndexByte(arg, '#'); i >= 0 {
		ctx, one = arg[:i], arg[i+1:]
	}
	if !listCtx(ctx) {
		return q2spot.Fail(q2spot.Bad, "Bad request")
	}
	ts, err := d.tracks(dz, ctx)
	if errors.Is(err, errLoading) { // a toast: the list's page isn't open to say Loading
		return q2spot.Fail(q2spot.Unavailable, "Still loading: try again in a moment")
	}
	if err != nil {
		return fail(err)
	}
	if one != "" {
		var pick []deezer.Track
		for _, t := range ts {
			if t.ID == one {
				pick = append(pick, t)
				break
			}
		}
		if pick == nil {
			return q2spot.Fail(q2spot.Unavailable, "The list changed: open it again")
		}
		ts = pick
	}
	d.dl.fresh()
	if st, _ := d.dl.state(one); one != "" && st == "failed" {
		d.dl.retry(one)
		return q2spot.Reply{Message: "Trying again"}
	}
	n := d.dl.add(ts)
	switch {
	case n == 0 && one != "":
		if st, _ := d.dl.state(one); st == "done" {
			return q2spot.Reply{Message: "Already on the card"}
		}
		return q2spot.Reply{Message: "Already in Downloads"}
	case n == 0:
		return q2spot.Reply{Message: "Already on the card or in Downloads"}
	case one != "":
		return q2spot.Reply{Message: "Downloading"}
	}
	return q2spot.Reply{Message: "Downloading " + plural(n, "song")}
}

// allOnCard is a list's first row when all its songs (rows, local each) are on the card: their
// folder, played from the card (an album's in track order), or, spread over several, Downloaded
// Songs' artists.
func (d *daemon) allOnCard(rows []q2spot.Row) q2spot.Row {
	dirs := map[string]bool{}
	for _, r := range rows {
		dirs[filepath.Dir(r.URI)] = true
	}
	if len(dirs) > 1 {
		return q2spot.Row{Kind: "list", URI: "cardartists", Title: "All on the card", Subtitle: "Browse your Deezer downloads"}
	}
	dir := filepath.Dir(rows[0].URI)
	return q2spot.Row{Kind: "local", URI: dir + "/", Context: d.dl.order(dir), Title: "All on the card",
		Subtitle: "Play your Deezer downloads"}
}

// songsIn is how many downloaded songs dir holds, and its folders but hidden ones.
func songsIn(dir string) (int, []string) {
	des, _ := os.ReadDir(dir)
	n, dirs := 0, []string(nil)
	for _, de := range des {
		switch {
		case strings.HasPrefix(de.Name(), "."):
		case de.IsDir():
			dirs = append(dirs, de.Name())
		case named.MatchString(de.Name()):
			n++
		}
	}
	sort.Slice(dirs, func(i, j int) bool { return strings.ToLower(dirs[i]) < strings.ToLower(dirs[j]) })
	return n, dirs
}

// cardArtists is Downloaded Songs with Artist/Album folders on the card: the songs in Deezer/
// itself (downloaded with Organize Files off), newest first, then each artist's folder.
func (d *daemon) cardArtists(cursor string) q2spot.Reply {
	loose, artists := songsIn(d.dl.folder)
	var rows []q2spot.Row
	if loose > 0 {
		rows = append(rows, q2spot.Row{Kind: "local", URI: d.dl.folder + "/", Title: "Unsorted Songs",
			Subtitle: plural(loose, "song") + " in Deezer/, newest first"})
	}
	for _, a := range artists {
		_, albums := songsIn(filepath.Join(d.dl.folder, a))
		rows = append(rows, q2spot.Row{Kind: "list", URI: "cardalbums", Context: a, Title: a, Subtitle: plural(len(albums), "album")})
	}
	if len(rows) == 0 {
		rows = append(rows, info("Nothing on the card yet", "Downloaded songs show here"))
	}
	return page(rows, cursor, "")
}

// albumYear is an album folder's title and year: "2005 - Tender Buttons", and v3.2's "Tender Buttons
// (2005)", are Tender Buttons and 2005; without a year, the name and 9999, so those come after the
// dated ones.
func albumYear(folder string) (string, int) {
	m := yearPrefix.FindStringSubmatch(folder)
	if m == nil {
		if m = yearSuffix.FindStringSubmatch(folder); m == nil {
			return folder, 9999
		}
		m[1], m[2] = m[2], m[1]
	}
	y, _ := strconv.Atoi(m[1])
	return m[2], y
}

var (
	yearPrefix = regexp.MustCompile(`^([0-9]{4}) - (.+)$`)
	yearSuffix = regexp.MustCompile(`^(.+) \(([0-9]{4})\)$`)
)

// cardAlbums is an artist's folder: each album's, oldest first (then by name), its songs in track
// order.
func (d *daemon) cardAlbums(artist, cursor string) q2spot.Reply {
	if artist == "" || artist == "." || artist == ".." || strings.ContainsAny(artist, "/\\") {
		return q2spot.Fail(q2spot.Bad, "Bad request")
	}
	dir := filepath.Join(d.dl.folder, artist)
	if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
		return q2spot.Fail(q2spot.Unavailable, "It isn't on the card any more")
	}
	loose, albums := songsIn(dir)
	sort.SliceStable(albums, func(i, j int) bool {
		_, a := albumYear(albums[i])
		_, b := albumYear(albums[j])
		return a < b
	})
	var rows []q2spot.Row
	for _, a := range albums {
		n, _ := songsIn(filepath.Join(dir, a))
		title, year := albumYear(a)
		sub := plural(n, "song")
		if year != 9999 {
			sub = strconv.Itoa(year) + " · " + sub
		}
		rows = append(rows, q2spot.Row{Kind: "local", URI: filepath.Join(dir, a) + "/", Context: "album", Title: title,
			Subtitle: sub})
	}
	if loose > 0 {
		rows = append(rows, q2spot.Row{Kind: "local", URI: dir + "/", Title: "Other Songs", Subtitle: plural(loose, "song")})
	}
	if len(rows) == 0 {
		rows = append(rows, info("Nothing here", ""))
	}
	return page(rows, cursor, "")
}

// menu is the page's first level, from the sign-in as last found (never waiting on Deezer).
func (d *daemon) menu() q2spot.Reply {
	d.dl.fresh()
	have := d.dl.count()
	songs := q2spot.Row{Kind: "local", URI: d.dl.folder + "/", Title: "Downloaded Songs", Subtitle: plural(have, "song") + " on the card"}
	if _, dirs := songsIn(d.dl.folder); len(dirs) > 0 { // Artist/Album folders: browsed by them
		songs.Kind, songs.URI = "list", "cardartists"
	}
	org := q2spot.Row{Kind: "action", URI: "organize", Title: "Organize Files: On", Subtitle: "Artist / Year - Album / 01 - Song"}
	if !d.organized() {
		org.Title, org.Subtitle = "Organize Files: Off", "Every song in one folder, Deezer/"
	}
	mode := q2spot.Row{Kind: "action", URI: "mode", Title: "Mode: Download", Subtitle: "A press saves the song in Deezer/"}
	if d.streaming() {
		mode.Title, mode.Subtitle = "Mode: Stream", "A press plays the song; nothing is kept"
	}
	stop := q2spot.Row{Kind: "action", URI: "stop", Title: "Stop Deezer", Subtitle: "Downloads carry on next time"}
	queue := d.queueRow()
	var rows []q2spot.Row
	switch d.authState() {
	case authNoARL:
		rows = []q2spot.Row{{Kind: "remote", Title: "Sign In from Your Phone", Subtitle: "Press to show the code"},
			info("A paid Deezer plan is needed", "Songs download to the card, then play from it")}
	case authExpired:
		rows = []q2spot.Row{{Kind: "remote", Title: "Sign In Again from Your Phone", Subtitle: "Your sign-in has expired"}}
	case authFree:
		rows = []q2spot.Row{info("This account has no paid plan", "Deezer downloads need Premium or HiFi"),
			{Kind: "remote", Title: "Sign In with Another Account", Subtitle: "Press to show the code"},
			{Kind: "action", URI: "signout", Title: "Sign Out", Subtitle: ""}}
	case authOffline:
		rows = []q2spot.Row{info("Deezer can't be reached", "Connect the Q2 to Wi-Fi")}
	case authUnknown:
		rows = []q2spot.Row{info("Signing in…", "")}
	default:
		rows = []q2spot.Row{
			open("liked", "Liked Songs", ""),
			{Kind: "list", URI: "playlists", Title: "Playlists", Subtitle: "Flow, Top Charts and yours"},
			{Kind: "list", URI: "albums", Title: "Albums"},
			{Kind: "list", URI: "artists", Title: "Artists"},
			{Kind: "search", Title: "Search Deezer"},
			queue,
			mode,
			{Kind: "action", URI: "quality", Title: "Quality: " + qualityNames[d.quality()],
				Subtitle: "Each song in the best there is, up to this"},
			org,
			{Kind: "list", URI: "account", Title: "Account"},
		}
		return q2spot.Reply{Message: "refresh", Rows: append(append([]q2spot.Row{songs}, rows...), stop)}
	}
	if have > 0 {
		rows = append(rows, songs)
	}
	if d.dl.pending() {
		rows = append(rows, queue)
	}
	return q2spot.Reply{Message: "refresh", Rows: append(rows, stop)}
}

// queueRow is the menu's Downloads row: what is under way.
func (d *daemon) queueRow() q2spot.Row {
	cur, waiting, failed, done, stalled := d.dl.snapshot()
	sub := "Nothing waiting"
	switch {
	case stalled != "" && len(waiting) > 0:
		sub = fmt.Sprintf("%d waiting · %s", len(waiting), stalled)
	case cur != nil:
		sub = fmt.Sprintf("Downloading %d of %d", len(done)+1, len(done)+1+len(waiting))
	case len(failed) > 0:
		sub = plural(len(failed), "song") + " failed"
	case len(done) > 0:
		sub = plural(len(done), "song") + " downloaded"
	}
	return q2spot.Row{Kind: "list", URI: "downloads", Title: "Downloads", Subtitle: sub}
}

// downloadsPage is the queue: what downloads now, what waits (a press removes it), what failed (a
// press tries it again), and what came (a press plays it).
func (d *daemon) downloadsPage(cursor string) q2spot.Reply {
	cur, waiting, failed, done, stalled := d.dl.snapshot()
	var rows []q2spot.Row
	if stalled != "" && len(waiting) > 0 {
		rows = append(rows, info("Waiting: "+stalled, "It tries again by itself"))
	}
	if cur != nil {
		rows = append(rows, info("Downloading "+cur.Track.Name, cur.Track.ArtistLine()+" · "+plural(len(waiting), "song")+" after it"))
	}
	if len(waiting) > 1 {
		rows = append(rows, q2spot.Row{Kind: "action", URI: "cancelall", Title: "Remove All Waiting",
			Subtitle: plural(len(waiting), "song")})
	}
	if len(failed) > 0 {
		rows = append(rows, q2spot.Row{Kind: "action", URI: "retry", Title: "Try the Failed Again",
			Subtitle: plural(len(failed), "song")},
			q2spot.Row{Kind: "action", URI: "clearfailed", Title: "Clear the Failed"})
	}
	for _, it := range waiting {
		rows = append(rows, q2spot.Row{Kind: "action", URI: "cancel", Context: it.Track.ID, Title: it.Track.Name,
			Subtitle: it.Track.ArtistLine() + " · waiting: press to remove"})
	}
	for _, it := range failed {
		rows = append(rows, q2spot.Row{Kind: "action", URI: "retry", Context: it.Track.ID, Title: it.Track.Name,
			Subtitle: "Failed (" + it.Err + "): press to try again"})
	}
	for _, it := range done {
		rows = append(rows, q2spot.Row{Kind: "local", URI: it.Path, Context: d.dl.order(filepath.Dir(it.Path)), Title: it.Track.Name,
			Subtitle: it.Track.ArtistLine() + " · downloaded"})
	}
	if len(rows) == 0 {
		rows = append(rows, info("Nothing to download", "Press a song, or hold Play/Pause on a list"))
	}
	return page(rows, cursor, "refresh")
}
