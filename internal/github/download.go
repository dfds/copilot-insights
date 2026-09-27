package github

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"

	"go.uber.org/zap"
)

// maxDownload caps one report or CSV download.
const maxDownload = 256 << 20

// errLinkExpired is a 403 from blob storage: the signed URL has expired and
// the caller should fetch fresh links.
var errLinkExpired = errors.New("signed download link expired")

// fetchSigned downloads a pre-signed report or CSV URL. No Authorization
// header is sent. Errors name the URL without its query string, which holds
// the signature.
func (c *Client) fetchSigned(ctx context.Context, rawURL string) ([]byte, error) {
	safe := redactURL(rawURL)
	for attempt := 0; ; attempt++ {
		last := attempt == maxRetries
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
		if err != nil {
			return nil, fmt.Errorf("download %s: invalid URL", safe)
		}
		req.Header.Set("User-Agent", userAgent)
		resp, err := c.download.Do(req)
		if err != nil {
			c.metrics.GitHubRequests.WithLabelValues(familyDownload, "none", "error").Inc()
			var ue *url.Error
			if errors.As(err, &ue) {
				err = ue.Err
			}
			if ctx.Err() != nil || last {
				return nil, fmt.Errorf("download %s: %w", safe, err)
			}
			c.logger.Warn("report download failed, retrying", zap.String("url", safe), zap.Error(err))
			if err := c.wait(ctx, "transport", backoff(attempt)); err != nil {
				return nil, err
			}
			continue
		}
		c.metrics.GitHubRequests.WithLabelValues(familyDownload, "none", strconv.Itoa(resp.StatusCode)).Inc()
		switch {
		case resp.StatusCode == http.StatusOK:
			defer resp.Body.Close()
			return readBody(resp.Body, safe)
		case resp.StatusCode >= 500 && !last:
			io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
			resp.Body.Close()
			if err := c.wait(ctx, "server_error", backoff(attempt)); err != nil {
				return nil, err
			}
			continue
		}
		io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		resp.Body.Close()
		if resp.StatusCode == http.StatusForbidden {
			return nil, fmt.Errorf("download %s: %w", safe, errLinkExpired)
		}
		return nil, fmt.Errorf("download %s: status %d", safe, resp.StatusCode)
	}
}

// readBody reads at most maxDownload bytes and gunzips gzip bodies. Blob
// storage serves the files with Content-Encoding unset, so net/http does not
// decompress them.
func readBody(r io.Reader, safe string) ([]byte, error) {
	b, err := io.ReadAll(io.LimitReader(r, maxDownload+1))
	if err != nil {
		return nil, fmt.Errorf("download %s: %w", safe, err)
	}
	if len(b) > maxDownload {
		return nil, fmt.Errorf("download %s: body exceeds %d bytes", safe, maxDownload)
	}
	if len(b) < 2 || b[0] != 0x1f || b[1] != 0x8b {
		return b, nil
	}
	zr, err := gzip.NewReader(bytes.NewReader(b))
	if err != nil {
		return nil, fmt.Errorf("download %s: gzip: %w", safe, err)
	}
	out, err := io.ReadAll(io.LimitReader(zr, maxDownload+1))
	if err != nil {
		return nil, fmt.Errorf("download %s: gzip: %w", safe, err)
	}
	if len(out) > maxDownload {
		return nil, fmt.Errorf("download %s: decompressed body exceeds %d bytes", safe, maxDownload)
	}
	return out, nil
}

func redactURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return "<unparseable url>"
	}
	return u.Scheme + "://" + u.Host + u.Path
}
