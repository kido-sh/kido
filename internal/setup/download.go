package setup

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"sync/atomic"
	"time"
)

const (
	maxArchiveBytes = 1 << 30
	stallTimeout    = 60 * time.Second
)

var httpClient = &http.Client{
	Transport: &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		TLSHandshakeTimeout:   30 * time.Second,
		ResponseHeaderTimeout: 60 * time.Second,
		IdleConnTimeout:       30 * time.Second,
		ForceAttemptHTTP2:     true,
	},
	CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) >= 10 {
			return errors.New("too many redirects")
		}
		if req.URL.Scheme != "https" {
			return fmt.Errorf("refusing a redirect to %s: not https", req.URL.Redacted())
		}
		return nil
	},
}

// download streams an https URL into dst (created 0600; it must not exist), enforcing the size
// and checking the SHA-256 on the way. dst is removed on any error.
func download(ctx context.Context, rawURL, dst, wantSHA string, wantSize int64, log io.Writer) (err error) {
	u, err := url.Parse(rawURL)
	if err != nil || u.Scheme != "https" || u.Host == "" {
		return fmt.Errorf("%q is not an https URL", rawURL)
	}
	limit := int64(maxArchiveBytes)
	if wantSize > 0 {
		limit = wantSize
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "kido-setup")
	resp, err := httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s: %s", u.Redacted(), resp.Status)
	}
	if resp.ContentLength > limit {
		return fmt.Errorf("GET %s: %d bytes, expected at most %d", u.Redacted(), resp.ContentLength, limit)
	}

	f, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	defer func() {
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			os.Remove(dst)
		}
	}()

	total := resp.ContentLength
	if total <= 0 {
		total = wantSize
	}
	var stalled atomic.Bool
	timer := time.AfterFunc(stallTimeout, func() { stalled.Store(true); cancel() })
	defer timer.Stop()
	body := &watchdogReader{r: io.LimitReader(resp.Body, limit+1), timer: timer}
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(f, h, &progressWriter{log: log, total: total}), body)
	if err != nil {
		if stalled.Load() {
			return fmt.Errorf("GET %s: no data for %v", u.Redacted(), stallTimeout)
		}
		return fmt.Errorf("GET %s: %w", u.Redacted(), err)
	}
	if n > limit {
		return fmt.Errorf("GET %s: more than %d bytes", u.Redacted(), limit)
	}
	if wantSize > 0 && n != wantSize {
		return fmt.Errorf("GET %s: got %d bytes, want %d", u.Redacted(), n, wantSize)
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != wantSHA {
		return fmt.Errorf("GET %s: sha256 %s, want %s", u.Redacted(), got, wantSHA)
	}
	return nil
}

// verifyFile checks the SHA-256 (and the size, when known) of a local archive.
func verifyFile(path, wantSHA string, wantSize int64) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, io.LimitReader(f, maxArchiveBytes+1))
	if err != nil {
		return err
	}
	if n > maxArchiveBytes {
		return fmt.Errorf("%s is larger than %d MiB", path, maxArchiveBytes>>20)
	}
	if wantSize > 0 && n != wantSize {
		return fmt.Errorf("%s has %d bytes, want %d", path, n, wantSize)
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != wantSHA {
		return fmt.Errorf("%s: sha256 %s, want %s", path, got, wantSHA)
	}
	return nil
}

// watchdogReader pushes back a timer that cancels the request when no byte arrives for
// stallTimeout.
type watchdogReader struct {
	r     io.Reader
	timer *time.Timer
}

func (w *watchdogReader) Read(p []byte) (int, error) {
	n, err := w.r.Read(p)
	if n > 0 {
		w.timer.Reset(stallTimeout)
	}
	return n, err
}

// progressWriter prints a line at every quarter of the download.
type progressWriter struct {
	log   io.Writer
	total int64
	done  int64
	step  int64
}

func (p *progressWriter) Write(b []byte) (int, error) {
	p.done += int64(len(b))
	if p.log != nil && p.total > 0 {
		if step := p.done * 4 / p.total; step > p.step && step < 4 {
			p.step = step
			fmt.Fprintf(p.log, "  %.1f of %.1f MB\n", float64(p.done)/1e6, float64(p.total)/1e6)
		}
	}
	return len(b), nil
}
