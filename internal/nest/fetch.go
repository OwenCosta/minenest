package nest

import (
	"context"
	"crypto/md5"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"hash"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

var httpClient = &http.Client{Timeout: 0} // downloads can be long; requests get their own deadlines

// getBytes fetches a small document (JSON, mostly). Responses are cached in cache/ for maxAge,
// and an old copy is used if the network is down.
func getBytes(ctx context.Context, url string, maxAge time.Duration) ([]byte, error) {
	cache := ""
	if maxAge > 0 {
		sum := sha1.Sum([]byte(url))
		cache = homePath("cache", hex.EncodeToString(sum[:8])+".json")
		if st, err := os.Stat(cache); err == nil && time.Since(st.ModTime()) < maxAge {
			if b, err := os.ReadFile(cache); err == nil {
				return b, nil
			}
		}
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", agent())
	req.Header.Set("Accept", "application/json")
	resp, err := httpClient.Do(req)
	if err == nil {
		defer resp.Body.Close()
		if resp.StatusCode == 200 {
			b, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
			if err == nil {
				if cache != "" {
					writeFile(cache, b, 0o644)
				}
				return b, nil
			}
		} else {
			err = fmt.Errorf("%s answered %s", hostOf(url), resp.Status)
			if resp.StatusCode == 404 {
				return nil, err
			}
		}
	}
	if cache != "" {
		if b, err2 := os.ReadFile(cache); err2 == nil {
			return b, nil
		}
	}
	return nil, friendlyNetErr(url, err)
}

func getJSON(ctx context.Context, url string, maxAge time.Duration, v any) error {
	b, err := getBytes(ctx, url, maxAge)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(b, v); err != nil {
		return fmt.Errorf("%s sent something MineNest couldn't read: %v", hostOf(url), err)
	}
	return nil
}

func hostOf(url string) string {
	s := strings.TrimPrefix(strings.TrimPrefix(url, "https://"), "http://")
	if i := strings.IndexAny(s, "/?"); i >= 0 {
		s = s[:i]
	}
	return s
}

func friendlyNetErr(url string, err error) error {
	if err == nil {
		return nil
	}
	if strings.Contains(err.Error(), "no such host") || strings.Contains(err.Error(), "dial tcp") {
		return fmt.Errorf("couldn't reach %s. Is this computer online?", hostOf(url))
	}
	return err
}

// Hash names a checksum to check a download against: "sha1", "sha256", "sha512" or "md5".
type Hash struct {
	Algo, Hex string
}

func newHasher(algo string) hash.Hash {
	switch algo {
	case "sha1":
		return sha1.New()
	case "sha256":
		return sha256.New()
	case "sha512":
		return sha512.New()
	case "md5":
		return md5.New()
	}
	return nil
}

// download fetches url to dest (through dest.part), checking the hash when there is one, and
// reports progress to job (which may be nil).
func download(ctx context.Context, url, dest string, want Hash, job *Job, what string) error {
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", agent())
	resp, err := httpClient.Do(req)
	if err != nil {
		return friendlyNetErr(url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("downloading %s: %s answered %s", what, hostOf(url), resp.Status)
	}
	part := dest + ".part"
	f, err := os.Create(part)
	if err != nil {
		return err
	}
	h := newHasher(want.Algo)
	var w io.Writer = f
	if h != nil {
		w = io.MultiWriter(f, h)
	}
	total := resp.ContentLength
	var done int64
	buf := make([]byte, 128<<10)
	last := time.Time{}
	for {
		n, rerr := resp.Body.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				f.Close()
				os.Remove(part)
				return werr
			}
			done += int64(n)
			if job != nil && time.Since(last) > 200*time.Millisecond {
				last = time.Now()
				job.Progress(what, done, total)
			}
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			f.Close()
			os.Remove(part)
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return fmt.Errorf("downloading %s: %v", what, rerr)
		}
	}
	if err := f.Close(); err != nil {
		os.Remove(part)
		return err
	}
	if h != nil && want.Hex != "" {
		if got := hex.EncodeToString(h.Sum(nil)); !strings.EqualFold(got, want.Hex) {
			os.Remove(part)
			return fmt.Errorf("%s didn't match its %s checksum, so it wasn't kept (try again)", what, want.Algo)
		}
	}
	if job != nil {
		job.Progress(what, done, done)
	}
	os.Remove(dest)
	return os.Rename(part, dest)
}

func fileHash(path, algo string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := newHasher(algo)
	if h == nil {
		return "", fmt.Errorf("unknown hash %s", algo)
	}
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
