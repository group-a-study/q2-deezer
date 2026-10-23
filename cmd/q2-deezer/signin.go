package main

import (
	"crypto/subtle"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/group-a-study/q2-deezer/internal/deezer"
	"github.com/group-a-study/q2-deezer/internal/q2"
	"github.com/group-a-study/q2-deezer/internal/qr"
)

//go:embed signin.html
var signinHTML []byte

const signinCookie = "q2dz"

// signin is the sign-in page (contrib/shanlingq2/README, Sign-in): a page on the Wi-Fi where the account's arl
// is pasted, checked with Deezer (a paid plan, for downloads) and saved to the card. While the
// add-on isn't signed in, its status file carries the page's address, a PIN and the QR code that
// opens it signed in, which the Q2's sign-in row shows (q2-pod's patch/addon.c qr_open). The key and PIN are
// new each start.
type signin struct {
	d    *daemon
	port int
	key  string
	pin  string

	mu      sync.Mutex
	fails   int
	lockout time.Time
	qrFor   string
	qrHex   string
	qrSize  int
}

func (d *daemon) startSignin(port int) {
	s := &signin{d: d, key: q2.Key(12), pin: q2.PIN(6)}
	var l net.Listener
	for p := port; p < port+10; p++ {
		if ln, err := net.Listen("tcp4", fmt.Sprintf(":%d", p)); err == nil {
			l, s.port = ln, p
			break
		}
	}
	d.signin = s
	if l == nil {
		return
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.index)
	mux.HandleFunc("/api/", s.api)
	go (&http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}).Serve(l)
}

// addr is where the page is, "" without Wi-Fi.
func (s *signin) addr() string {
	ip := q2.LocalIP()
	if ip == "" || s.port == 0 {
		return ""
	}
	return fmt.Sprintf("%s:%d", ip, s.port)
}

// fields are what the Q2's QR page reads from the status file.
func (s *signin) fields(kv map[string]string) {
	addr := s.addr()
	url := ""
	if addr != "" {
		url = "http://" + addr + "/?k=" + s.key
	}
	s.mu.Lock()
	if url != s.qrFor {
		s.qrFor, s.qrHex, s.qrSize = url, "", 0
		if c, err := qr.Encode([]byte(url)); url != "" && err == nil {
			s.qrHex, s.qrSize = c.Hex(), c.Size
		}
	}
	kv["addr"], kv["pin"], kv["qr"], kv["qrsize"] = addr, s.pin, s.qrHex, fmt.Sprint(s.qrSize)
	s.mu.Unlock()
	kv["note"] = "Paste your Deezer ARL there"
	if addr == "" {
		kv["note"] = "Connect the Q2 to Wi-Fi"
	}
}

func (s *signin) authed(r *http.Request) bool {
	c, err := r.Cookie(signinCookie)
	return err == nil && subtle.ConstantTimeCompare([]byte(c.Value), []byte(s.key)) == 1
}

func (s *signin) setCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{Name: signinCookie, Value: s.key, Path: "/", HttpOnly: true,
		SameSite: http.SameSiteStrictMode})
}

func (s *signin) index(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	if k := r.URL.Query().Get("k"); k != "" {
		if subtle.ConstantTimeCompare([]byte(k), []byte(s.key)) == 1 {
			s.setCookie(w)
		}
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Write(signinHTML)
}

func send(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func refuse(w http.ResponseWriter, code int, msg string) {
	send(w, code, map[string]string{"error": msg})
}

// account is the page's view of a signed-in account: its name and plan.
func account(a deezer.Account) map[string]any {
	return map[string]any{"signed_in": true, "name": a.Name, "plan": a.Offer}
}

// arlShape is what Deezer's arl cookie looks like: a long run of hex digits.
var arlShape = regexp.MustCompile(`^[0-9a-fA-F]{64,512}$`)

func (s *signin) api(w http.ResponseWriter, r *http.Request) {
	route := strings.TrimPrefix(r.URL.Path, "/api/")
	if r.Method == http.MethodPost && r.Header.Get("X-Q2") != "1" {
		refuse(w, http.StatusBadRequest, "bad request")
		return
	}
	var body struct{ PIN, ARL string }
	if r.Method == http.MethodPost {
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<10)).Decode(&body); err != nil && err != io.EOF {
			refuse(w, http.StatusBadRequest, "bad body")
			return
		}
	}
	if route == "login" && r.Method == http.MethodPost {
		s.mu.Lock()
		locked := time.Now().Before(s.lockout)
		ok := !locked && subtle.ConstantTimeCompare([]byte(strings.TrimSpace(body.PIN)), []byte(s.pin)) == 1
		if !ok && !locked {
			if s.fails++; s.fails >= 10 {
				s.fails, s.lockout = 0, time.Now().Add(time.Minute)
			}
		}
		s.mu.Unlock()
		switch {
		case locked:
			refuse(w, http.StatusTooManyRequests, "Too many tries. Wait a minute.")
		case !ok:
			refuse(w, http.StatusForbidden, "That's not the PIN on the Q2's screen.")
		default:
			s.setCookie(w)
			send(w, http.StatusOK, map[string]bool{"ok": true})
		}
		return
	}
	if !s.authed(r) {
		refuse(w, http.StatusUnauthorized, "sign in")
		return
	}
	switch {
	case route == "state" && r.Method == http.MethodGet:
		st := map[string]any{"signed_in": false}
		if dz, err := s.d.session(); err == nil && dz.Account().Premium {
			st = account(dz.Account())
		} else if err == nil {
			st["error"] = "This account has no paid plan; Deezer downloads need Premium or HiFi."
		} else if deezer.IsNoNetwork(err) {
			st["error"] = "Deezer can't be reached right now."
		} else if !errors.Is(err, errNoARL) && !deezer.IsARLExpired(err) {
			st["error"] = "Deezer didn't answer. Try again in a moment."
		}
		send(w, http.StatusOK, st)
	case route == "arl" && r.Method == http.MethodPost:
		arl := strings.TrimSpace(body.ARL)
		if !arlShape.MatchString(arl) {
			refuse(w, http.StatusBadRequest, "That doesn't look like an ARL: it is a long run of letters a-f and digits.")
			return
		}
		dz := deezer.New(arl)
		switch err := dz.Login(); {
		case deezer.IsARLExpired(err):
			refuse(w, http.StatusForbidden, "Deezer didn't accept that ARL. Copy it again after signing in on deezer.com.")
			return
		case err != nil:
			refuse(w, http.StatusBadGateway, "Deezer can't be reached right now. Try again in a moment.")
			return
		case !dz.Account().Premium:
			refuse(w, http.StatusForbidden, "This Deezer account has no paid plan; downloads on the Q2 need Premium or HiFi.")
			return
		}
		if err := q2.WriteFile(filepath.Join(s.d.dir, "arl"), []byte(arl+"\n")); err != nil {
			refuse(w, http.StatusInternalServerError, "Couldn't save to the card: "+err.Error())
			return
		}
		s.d.signedIn()
		st := account(dz.Account())
		st["ok"] = true
		send(w, http.StatusOK, st)
	case route == "signout" && r.Method == http.MethodPost:
		if err := s.d.signOut(); err != nil {
			refuse(w, http.StatusInternalServerError, "Couldn't remove the ARL from the card.")
			return
		}
		send(w, http.StatusOK, map[string]bool{"ok": true})
	default:
		http.NotFound(w, r)
	}
}
