// seed.go: the ops-only apprise config upload (N.7, N.8). Never invoked by
// the tick loop.
package notify

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"strings"

	"github.com/thiscantbeserious/ai-ops-nanny/internal/config"
)

// SeedConfig uploads APPRISE_CONFIG_FILE verbatim via POST /add/{key}. Ops
// one-shot: no substitution, no shell, no expansion, the file arrives
// already rendered.
func SeedConfig(ctx context.Context, cfg *config.Config) (int, error) {
	data, err := os.ReadFile(cfg.AppriseConfigFile)
	if err != nil {
		return 0, fmt.Errorf("read %s: %w", cfg.AppriseConfigFile, err)
	}

	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	if err := w.WriteField("format", "text"); err != nil {
		return 0, err
	}
	fw, err := w.CreateFormFile("config", "sentinel.cfg")
	if err != nil {
		return 0, err
	}
	if _, err := fw.Write(data); err != nil {
		return 0, err
	}
	if err := w.Close(); err != nil {
		return 0, err
	}

	endpoint := strings.TrimRight(cfg.AppriseURL, "/") + "/add/" + cfg.AppriseKey
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, &buf)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", w.FormDataContentType())

	client := &http.Client{Timeout: cfg.NotifyTimeout}
	resp, err := client.Do(req)
	if err != nil {
		// unwrapURLErr: a *url.Error's Error() embeds the full request
		// URL, whose last path segment is APPRISE_KEY, never let it into
		// a returned/logged error (C7, same fix as notify.go's postApprise).
		return 0, fmt.Errorf("transport: %w", unwrapURLErr(err))
	}
	defer resp.Body.Close()
	// N.3.1's 204 rule applies here too: a 204 means apprise did not
	// register the key, which is precisely the failure SeedConfig exists
	// to prevent, the one caller that must never mistake it for success.
	if resp.StatusCode == http.StatusNoContent {
		return 0, fmt.Errorf("http 204: apprise did not accept the config")
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 200))
		return 0, fmt.Errorf("http %d: %s", resp.StatusCode, body)
	}

	return countConfigURLs(data), nil
}

// countConfigURLs is N.7's "reported URL count is the number of non-empty,
// non-# lines".
func countConfigURLs(data []byte) int {
	n := 0
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		n++
	}
	return n
}
