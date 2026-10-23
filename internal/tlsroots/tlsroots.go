// Package tlsroots verifies TLS for the card add-ons: the Q2's firmware has no CA bundle, so the
// add-ons carry Mozilla's, as curl converts it (cacert.pem, MPL-2.0, from curl.se/docs/caextract.html).
// A Q2 whose clock was never set reads 1970 or 2000; certificates are then checked at the time a
// plain HTTP Date header gives, so they aren't refused as not yet valid.
package tlsroots

import (
	"crypto/tls"
	"crypto/x509"
	_ "embed"
	"net/http"
	"sync"
	"time"
)

//go:embed cacert.pem
var bundle []byte

var (
	once  sync.Once
	pool  *x509.CertPool
	mu    sync.Mutex
	skew  time.Duration // what to add to the clock, once a Date header said so
	asked time.Time
)

// Pool is the bundle's roots.
func Pool() *x509.CertPool {
	once.Do(func() {
		pool = x509.NewCertPool()
		pool.AppendCertsFromPEM(bundle)
	})
	return pool
}

// clockSet is whether the system clock looks set: no certificate in use was issued before 2025.
func clockSet() bool { return time.Now().Year() >= 2025 }

// Now is the time to check certificates at.
func Now() time.Time {
	if clockSet() {
		return time.Now()
	}
	mu.Lock()
	defer mu.Unlock()
	if skew == 0 && time.Since(asked) > time.Minute {
		asked = time.Now()
		c := &http.Client{Timeout: 10 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		}}
		if resp, err := c.Head(DateURL); err == nil {
			resp.Body.Close()
			if d, err := http.ParseTime(resp.Header.Get("Date")); err == nil {
				skew = time.Until(d)
			}
		}
	}
	return time.Now().Add(skew)
}

// DateURL answers plain HTTP with a Date header; the add-on sets its own service's.
var DateURL = "http://www.deezer.com/"

// Config is a client TLS configuration with the bundle's roots and Now's clock.
func Config() *tls.Config {
	return &tls.Config{RootCAs: Pool(), Time: Now, MinVersion: tls.VersionTLS12}
}

// Client is an HTTP client using Config, with a total timeout for each request.
func Client(timeout time.Duration) *http.Client {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.TLSClientConfig = Config()
	t.MaxIdleConnsPerHost = 4
	t.IdleConnTimeout = 60 * time.Second
	return &http.Client{Transport: t, Timeout: timeout}
}
