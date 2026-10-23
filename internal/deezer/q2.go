package deezer

// q2-deezer's additions to OpenDeezer's client (the rest of this package, kept as it is there so it
// can be updated from it): the app's TLS roots, the account's favourite albums and artists, which
// the Q2's menu lists, an album's own details for its tracks' tags, a download of a track already
// looked up, and where a track goes in the organized layout (Artist/Album/07 - Title [id]).

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
)

// transport carries every request: the session's (New), the media service's and the CDN's. nil is
// http.DefaultTransport.
var transport http.RoundTripper

// UseTransport sends every request of the clients made after it, and of every CDN fetch (a
// download, its cover), through rt. The Q2's firmware has no CA bundle, so q2-deezer passes one with its own roots (tlsroots);
// tests pass one that answers for Deezer. An *http.Transport keeps the CDN fetch's own timeouts.
func UseTransport(rt http.RoundTripper) {
	transport = rt
	if t, ok := rt.(*http.Transport); ok {
		d := downloadClient.Transport.(*http.Transport).Clone()
		d.TLSClientConfig = t.TLSClientConfig.Clone()
		d.DialContext, d.DialTLSContext, d.Proxy = t.DialContext, t.DialTLSContext, t.Proxy
		downloadClient.Transport = d
		return
	}
	downloadClient.Transport = rt
}

// SaveTrackInfo is SaveTrack for a track already looked up (its album, cover, number and year
// filled in by the caller, as an album's track list leaves them out): downloaded, decrypted,
// tagged and renamed into place in dir. Downloads need a paid plan, as SaveTrack's.
func (c *Client) SaveTrackInfo(ctx context.Context, t Track, dir string) (string, error) {
	if !c.LoggedIn() {
		return "", fmt.Errorf("not logged in")
	}
	if !c.Account().Premium {
		return "", ErrPremiumRequired
	}
	return c.saveTrack(ctx, t, dir, nil, nil)
}

// SaveTrackInfoAs is SaveTrackInfo with the file's name, less its extension, given: the organized
// layout's OrganizedName.
func (c *Client) SaveTrackInfoAs(ctx context.Context, t Track, dir, name string) (string, error) {
	if !c.LoggedIn() {
		return "", fmt.Errorf("not logged in")
	}
	if !c.Account().Premium {
		return "", ErrPremiumRequired
	}
	return c.saveTrackAs(ctx, t, dir, name, nil, nil)
}

// Placement is where a track sits on its album, for the organized layout and the tags a playlist's
// or a search's track lacks: the album's artist, title and year, how many discs and tracks (the
// highest number on a disc) it has, and the track's disc and number.
type Placement struct {
	Artist, Album, Year string
	Disc, Number        int
	Discs, Tracks       int
}

type albumPlace struct {
	artist, title, year string
	discs, tracks       int
}

// albums keeps the albums TrackPlacement looked up, so an album's tracks ask for it once. They are
// public, the same for every account.
var albums = struct {
	sync.Mutex
	m map[string]albumPlace
}{m: map[string]albumPlace{}}

// TrackPlacement looks t up on the public API: its album, disc and number (/track/<id>), then the
// album's artist, title and year (/album/<id>) and its discs (/album/<id>/tracks), kept for the
// album's other tracks.
func (c *Client) TrackPlacement(t Track) (Placement, error) {
	b, err := c.restGet("/track/" + t.ID)
	if err != nil {
		return Placement{}, err
	}
	var r struct {
		TrackPosition json.Number `json:"track_position"`
		DiskNumber    json.Number `json:"disk_number"`
		ReleaseDate   string      `json:"release_date"`
		Artist        restArtist  `json:"artist"`
		Album         struct {
			ID    json.Number `json:"id"`
			Title string      `json:"title"`
		} `json:"album"`
	}
	if err := json.Unmarshal(b, &r); err != nil {
		return Placement{}, err
	}
	pos, _ := r.TrackPosition.Int64()
	disc, _ := r.DiskNumber.Int64()
	p := Placement{Artist: r.Artist.Name, Album: r.Album.Title, Year: yearFromDate(r.ReleaseDate),
		Disc: int(disc), Number: int(pos)}
	id := r.Album.ID.String()
	if id == "" || id == "0" {
		return p, nil
	}
	albums.Lock()
	a, ok := albums.m[id]
	albums.Unlock()
	if !ok {
		if a, err = c.albumPlace(id); err != nil {
			return Placement{}, err
		}
		albums.Lock()
		if len(albums.m) >= 64 {
			albums.m = map[string]albumPlace{}
		}
		albums.m[id] = a
		albums.Unlock()
	}
	if a.artist != "" {
		p.Artist = a.artist
	}
	if a.title != "" {
		p.Album = a.title
	}
	if a.year != "" {
		p.Year = a.year
	}
	p.Discs, p.Tracks = a.discs, a.tracks
	return p, nil
}

func (c *Client) albumPlace(id string) (albumPlace, error) {
	al, year, err := c.AlbumInfo(id)
	if err != nil {
		return albumPlace{}, err
	}
	a := albumPlace{title: al.Name, year: year}
	if len(al.Artists) > 0 {
		a.artist = al.Artists[0].Name
	}
	for index := 0; index < maxTracks; index += albumPageSize {
		b, err := c.restGet(fmt.Sprintf("/album/%s/tracks?limit=%d&index=%d", id, albumPageSize, index))
		if err != nil {
			return albumPlace{}, err
		}
		var r struct {
			Data []struct {
				TrackPosition json.Number `json:"track_position"`
				DiskNumber    json.Number `json:"disk_number"`
			} `json:"data"`
			Next string `json:"next"`
		}
		if err := json.Unmarshal(b, &r); err != nil {
			return albumPlace{}, err
		}
		for _, t := range r.Data {
			pos, _ := t.TrackPosition.Int64()
			disc, _ := t.DiskNumber.Int64()
			a.tracks, a.discs = max(a.tracks, int(pos)), max(a.discs, int(disc))
		}
		if len(r.Data) < albumPageSize || r.Next == "" {
			break
		}
	}
	return a, nil
}

// OrganizedName is a track's file name, less its extension, in its album's folder: its number
// first, so the folder lists in track order ("07 - Title [id]"; "2-07 - …" on an album of more
// than one disc, three digits on one of 100 tracks or more), and the [id] OpenDeezer's names end
// with, which tells q2-deezer it is on the card. Without a number, "Title [id]".
func OrganizedName(t Track, p Placement) string {
	prefix := ""
	if p.Number > 0 {
		width := 2
		if p.Tracks >= 100 || p.Number >= 100 {
			width = 3
		}
		prefix = fmt.Sprintf("%0*d - ", width, p.Number)
		if p.Discs > 1 && p.Disc > 0 {
			prefix = fmt.Sprintf("%d-%0*d - ", p.Disc, width, p.Number)
		}
	}
	suffix := " [" + t.ID + "]"
	return sanitizeFilename(truncateSafe(prefix+t.Name, 180-len(suffix)) + suffix)
}

// FolderName is s as a card folder's name (an artist's, an album's): the characters FAT refuses
// replaced, at most 100 bytes, ending in neither a dot nor a space; or, without one, unknown.
func FolderName(s, unknown string) string {
	if strings.Trim(s, " .") == "" { // nothing sanitizeFilename would keep
		return unknown
	}
	return sanitizeFilename(truncateSafe(strings.TrimSpace(s), 100))
}

// FavoriteAlbums lists the user's saved albums (gw pageProfile, as Playlists).
func (c *Client) FavoriteAlbums() ([]Album, error) {
	return profileTab(c, "albums", func(a struct {
		AlbID      json.Number `json:"ALB_ID"`
		AlbTitle   string      `json:"ALB_TITLE"`
		ArtID      json.Number `json:"ART_ID"`
		ArtName    string      `json:"ART_NAME"`
		AlbPicture string      `json:"ALB_PICTURE"`
	}) (string, Album) {
		return a.AlbID.String(), Album{ID: a.AlbID.String(), Name: a.AlbTitle,
			Artists: []Artist{{ID: a.ArtID.String(), Name: a.ArtName}}, ArtworkURL: gwCover(a.AlbPicture)}
	})
}

// FavoriteArtists lists the artists the user follows (gw pageProfile, as Playlists).
func (c *Client) FavoriteArtists() ([]ArtistInfo, error) {
	return profileTab(c, "artists", func(a struct {
		ArtID   json.Number `json:"ART_ID"`
		ArtName string      `json:"ART_NAME"`
		NbFan   json.Number `json:"NB_FAN"`
	}) (string, ArtistInfo) {
		fans, _ := a.NbFan.Int64()
		return a.ArtID.String(), ArtistInfo{ID: a.ArtID.String(), Name: a.ArtName, NbFans: int(fans)}
	})
}

// profileTab is one tab of the user's profile (gw pageProfile): each item decoded as D, made an
// id and an R by conv, in Deezer's order, each id once. Deezer answers with the whole tab whatever
// nb and index ask (all 239 of an account's albums for nb 100, index 100: checked 2026-10-22), so a
// full page doesn't mean there is more: paging by it fetched the whole tab 50 times, past the Q2's
// 15 s. A page is asked for again only while the items are short of the tab's total and the last
// page brought new ones, so a Deezer that does page is followed too.
func profileTab[D, R any](c *Client, tab string, conv func(D) (string, R)) ([]R, error) {
	const nb = 1000
	var out []R
	seen := map[string]bool{}
	for index := 0; index < maxTracks; {
		body := fmt.Sprintf(`{"user_id":%s,"tab":%s,"nb":%d,"index":%d}`, jsonEsc(c.uid()), jsonEsc(tab), nb, index)
		b, err := c.gw("deezer.pageProfile", body)
		if err != nil {
			return out, err
		}
		var r struct {
			Results struct {
				Tab json.RawMessage `json:"TAB"`
			} `json:"results"`
		}
		if err := json.Unmarshal(b, &r); err != nil {
			return out, err
		}
		var tabs map[string]struct {
			Data  json.RawMessage `json:"data"`
			Total json.RawMessage `json:"total"`
		}
		if json.Unmarshal(r.Results.Tab, &tabs) != nil { // an empty profile's TAB may be [] (PHP's empty map)
			return out, nil
		}
		var data []D
		if t := tabs[tab].Data; len(t) > 0 && t[0] == '[' {
			if err := json.Unmarshal(t, &data); err != nil {
				return out, err
			}
		}
		fresh := 0
		for _, d := range data {
			if id, v := conv(d); !seen[id] {
				seen[id] = true
				out = append(out, v)
				fresh++
			}
		}
		total, _ := strconv.Atoi(strings.Trim(string(tabs[tab].Total), `"`))
		if fresh == 0 || total > 0 && len(out) >= total {
			return out, nil
		}
		index += len(data)
	}
	return out, nil
}

// AlbumInfo is an album's title, artist, cover and release year (public REST): the album's track
// list (AlbumTracks) has none of them, and a download's tags want them.
func (c *Client) AlbumInfo(id string) (Album, string, error) {
	b, err := c.restGet("/album/" + id)
	if err != nil {
		return Album{}, "", err
	}
	var a struct {
		restAlbumDTO
		ReleaseDate string `json:"release_date"`
	}
	if err := json.Unmarshal(b, &a); err != nil {
		return Album{}, "", err
	}
	return a.toAlbum(), yearFromDate(a.ReleaseDate), nil
}
