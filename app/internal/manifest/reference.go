package manifest

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"stack-launcher.local/app/internal/ledger"
)

const maxReferenceSize = 16 << 20

var referenceHTTPClient = http.Client{
	Timeout:       30 * time.Second,
	CheckRedirect: checkReferenceRedirect,
}

// resolveReference reads a source without changing the ledger. A successful Git
// resolution transfers ownership of its checkout to the caller.
func resolveReference(ctx context.Context, stateDir, reference string, run runner) (ledger.Manifest, error) {
	first, _, _ := strings.Cut(reference, "/")
	if !strings.Contains(first, ":") {
		content, err := readLocalFile(reference)
		return ledger.Manifest{Content: content}, err
	}
	link, err := url.Parse(reference)
	if err != nil {
		return ledger.Manifest{}, errors.New("invalid manifest reference URL")
	}
	if link.Scheme == "github" || isGitHubBlob(link) {
		return resolveGitHub(ctx, stateDir, link, run)
	}
	content, err := readReferenceURL(ctx, link, &referenceHTTPClient)
	return ledger.Manifest{Content: content}, err
}

func isGitHubBlob(link *url.URL) bool {
	if link.Scheme != "https" || !strings.EqualFold(link.Host, "github.com") {
		return false
	}
	parts := strings.SplitN(strings.TrimPrefix(link.Path, "/"), "/", 4)
	return len(parts) >= 3 && parts[2] == "blob"
}

func readReferenceURL(ctx context.Context, link *url.URL, client *http.Client) ([]byte, error) {
	switch link.Scheme {
	case "http", "https":
		return readHTTP(ctx, client, link)
	case "file":
		return readFileURL(link)
	default:
		return nil, fmt.Errorf("unsupported manifest scheme %q. Use HTTP(S), a local path or file URL, or a GitHub reference", link.Scheme)
	}
}

func checkReferenceRedirect(request *http.Request, via []*http.Request) error {
	if len(via) >= 10 {
		return errors.New("stopped after 10 redirects")
	}
	previous := via[len(via)-1].URL
	if request.URL.Scheme != previous.Scheme || !strings.EqualFold(request.URL.Host, previous.Host) {
		request.Header.Del("Referer") // The preceding URL may contain a signature.
	}
	return nil
}

func readFileURL(link *url.URL) ([]byte, error) {
	if link.User != nil || (link.Host != "" && !strings.EqualFold(link.Host, "localhost")) || !filepath.IsAbs(link.Path) {
		return nil, errors.New("file URL must name an absolute path on the launcher host")
	}
	return readLocalFile(link.Path)
}

func readHTTP(ctx context.Context, client *http.Client, link *url.URL) ([]byte, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, link.String(), nil)
	if err != nil {
		return nil, errors.New("invalid HTTP manifest URL")
	}
	response, err := client.Do(request)
	if err != nil {
		return nil, httpFetchError(err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode/100 != 2 {
		return nil, fmt.Errorf("fetch HTTP manifest: HTTP %d", response.StatusCode)
	}
	return readReferenceContent(response.Body)
}

func httpFetchError(err error) error {
	// Request and redirect errors may contain the full URL, including a signature.
	for _, cause := range []error{context.Canceled, context.DeadlineExceeded} {
		if errors.Is(err, cause) {
			return fmt.Errorf("fetch HTTP manifest: %w", cause)
		}
	}
	return errors.New("fetch HTTP manifest: request or redirect failed")
}

func readLocalFile(path string) ([]byte, error) {
	// Nonblocking open prevents a named pipe from waiting for a writer before Stat.
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0) // #nosec G304 G703 -- the trusted caller chooses a launcher-host path
	if err != nil {
		return nil, fmt.Errorf("open local manifest: %w", err)
	}
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil {
		return nil, fmt.Errorf("stat local manifest: %w", err)
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("local manifest must be a regular file")
	}
	return readReferenceContent(file)
}

func readReferenceContent(source io.Reader) ([]byte, error) {
	content, err := io.ReadAll(io.LimitReader(source, maxReferenceSize+1))
	if err != nil {
		return nil, fmt.Errorf("read manifest: %w", err)
	}
	if len(content) > maxReferenceSize {
		return nil, fmt.Errorf("manifest exceeds %d MiB", maxReferenceSize>>20)
	}
	return content, nil
}
