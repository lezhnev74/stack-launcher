// Package manifest acquires and saves Compose sources.
package manifest

import (
	"context"
	"database/sql"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"stack-launcher.local/app/internal/config"
	"stack-launcher.local/app/internal/ledger"
)

type store interface {
	Get(context.Context, string) (ledger.Stack, error)
	GetManifest(context.Context, string) (ledger.Manifest, error)
	SaveManifest(context.Context, string, ledger.Manifest) error
}

// Input identifies exactly one source. An empty Reference and nil InlineYAML
// are absent. A non-nil empty InlineYAML slice is an explicitly supplied source.
type Input struct {
	Reference  string
	InlineYAML []byte
}

// Acquire saves one source for a launching stack, retaining Git checkouts.
func Acquire(ctx context.Context, cfg config.Config, db store, id string, source Input) (ledger.Manifest, error) {
	return acquire(ctx, cfg, db, id, source, gitRunner{stateDir: cfg.StateDir})
}

func acquire(ctx context.Context, cfg config.Config, db store, id string, input Input, run runner) (ledger.Manifest, error) {
	if (input.Reference != "") == (input.InlineYAML != nil) {
		return ledger.Manifest{}, errors.New("provide exactly one manifest reference or inline Compose YAML")
	}
	result := ledger.Manifest{Content: input.InlineYAML}
	if input.Reference != "" {
		if err := checkStack(ctx, db, id); err != nil {
			return ledger.Manifest{}, err
		}
		var err error
		result, err = resolveReference(ctx, cfg.StateDir, input.Reference, run)
		if err != nil {
			return ledger.Manifest{}, err
		}
	}
	if err := db.SaveManifest(ctx, id, result); err != nil {
		return ledger.Manifest{}, errors.Join(err, discardUnownedCheckout(cfg.StateDir, db, id, result.Checkout))
	}
	return result, nil
}

type sourceURL struct {
	repo  string
	parts []string
}

var repoPattern = regexp.MustCompile(`^[A-Za-z0-9-]+/[A-Za-z0-9_.-]+$`)
var commitPattern = regexp.MustCompile(`^[0-9a-fA-F]{40}$`)

func validParts(parts []string) bool {
	for _, part := range parts {
		if part == "" || part == "." || part == ".." || strings.ContainsAny(part, "\\\x00\n\r") {
			return false
		}
	}
	return true
}
func parseGitHub(link *url.URL) (sourceURL, error) {
	if link.User != nil || link.RawPath != "" {
		return sourceURL{}, errors.New("invalid GitHub reference")
	}
	parts := strings.Split(strings.TrimPrefix(link.Path, "/"), "/")
	if link.Scheme == "github" {
		parts = append([]string{link.Host}, parts...)
	} else {
		parts = slices.Delete(parts, 2, 3) // The HTTPS blob link includes a routing segment.
	}
	if len(parts) < 4 {
		return sourceURL{}, errors.New("expected GitHub owner, repository, ref, and path")
	}
	repo := strings.ToLower(parts[0] + "/" + parts[1])
	if !repoPattern.MatchString(repo) || !validParts(parts[1:]) {
		return sourceURL{}, errors.New("invalid repository, ref, or path")
	}
	return sourceURL{repo, parts[2:]}, nil
}

type runner interface {
	run(context.Context, string, ...string) ([]byte, error)
}

func resolveGitHub(ctx context.Context, stateDir string, link *url.URL, run runner) (result ledger.Manifest, err error) {
	source, err := parseGitHub(link)
	if err != nil {
		return result, err
	}
	remote := "ssh://git@github.com/" + source.repo + ".git"
	oid, file, err := resolve(ctx, run, remote, source.parts)
	if err != nil {
		return result, err
	}
	checkout, err := newCheckout(stateDir)
	if err != nil {
		return result, err
	}
	defer func() {
		if err != nil {
			result = ledger.Manifest{}
			err = errors.Join(err, os.RemoveAll(checkout))
		}
	}()
	return checkoutManifest(ctx, run, remote, oid, file, checkout, source.repo)
}

const checkoutDirectory = "checkouts"

func discardUnownedCheckout(stateDir string, db store, id, checkout string) error {
	if checkout == "" {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	recorded, err := db.GetManifest(ctx, id)
	if recorded.Checkout == checkout || (err != nil && !errors.Is(err, sql.ErrNoRows)) {
		return nil
	}
	return discardCheckout(stateDir, checkout)
}

func discardCheckout(stateDir, checkout string) error {
	if checkout == "" {
		return nil
	}
	return os.RemoveAll(filepath.Join(stateDir, checkoutDirectory, checkout))
}

func checkStack(ctx context.Context, db store, id string) error {
	stack, err := db.Get(ctx, id)
	if err != nil {
		return err
	}
	if stack.State != "launching" {
		return errors.New("stack is not launching")
	}
	return nil
}

func resolve(ctx context.Context, run runner, remote string, parts []string) (string, string, error) {
	if commitPattern.MatchString(parts[0]) {
		return strings.ToLower(parts[0]), strings.Join(parts[1:], "/"), nil
	}
	output, err := run.run(ctx, "", "ls-remote", "--refs", "--heads", "--tags", remote)
	if err != nil {
		return "", "", err
	}
	refs := advertisedRefs(output)
	var oid, file string
	matches := 0
	for i := 1; i < len(parts); i++ {
		if value, ok := refs[strings.Join(parts[:i], "/")]; ok {
			matches++
			oid = value
			file = strings.Join(parts[i:], "/")
		}
	}
	if matches != 1 || oid == "" {
		return "", "", errors.New("ref is missing or ambiguous. Use a full commit permalink")
	}
	return oid, file, nil
}

func advertisedRefs(output []byte) map[string]string {
	refs := map[string]string{}
	for _, line := range strings.Split(string(output), "\n") {
		oid, ref, ok := strings.Cut(line, "\t")
		if !ok || !commitPattern.MatchString(oid) {
			continue
		}
		name, isBranch := strings.CutPrefix(ref, "refs/heads/")
		if !isBranch {
			name = strings.TrimPrefix(ref, "refs/tags/")
		}
		if prev, ok := refs[name]; ok && prev != oid {
			refs[name] = ""
		} else {
			refs[name] = oid
		}
	}
	return refs
}

func newCheckout(state string) (string, error) {
	dir := filepath.Join(state, checkoutDirectory)
	if err := os.Mkdir(dir, 0700); err != nil && !errors.Is(err, os.ErrExist) {
		return "", err
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return "", err
	}
	if !info.IsDir() || info.Mode()&(os.ModePerm|os.ModeSetuid|os.ModeSetgid|os.ModeSticky) != 0700 {
		return "", errors.New("checkouts must be a mode 0700 directory")
	}
	return os.MkdirTemp(dir, "checkout-")
}

func checkoutManifest(ctx context.Context, run runner, remote, oid, file, checkout, repo string) (ledger.Manifest, error) {
	commit, err := checkoutGit(ctx, run, checkout, remote, oid)
	if err != nil {
		return ledger.Manifest{}, err
	}
	if err := checkBlob(ctx, run, checkout, file); err != nil {
		return ledger.Manifest{}, err
	}
	content, err := run.run(ctx, checkout, "cat-file", "blob", "HEAD:"+file)
	if err != nil {
		return ledger.Manifest{}, err
	}
	return ledger.Manifest{Content: content, Repository: repo, Commit: commit, Path: file, Checkout: filepath.Base(checkout)}, nil
}

func checkBlob(ctx context.Context, run runner, checkout, file string) error {
	listing, err := run.run(ctx, checkout, "ls-tree", "-z", "HEAD", "--", ":(literal)"+file)
	if err != nil {
		return err
	}
	if !strings.HasPrefix(string(listing), "100644 blob ") && !strings.HasPrefix(string(listing), "100755 blob ") {
		return errors.New("manifest must be a regular Git blob")
	}
	if !strings.HasSuffix(string(listing), "\t"+file+"\x00") {
		return errors.New("manifest must be an exact Git blob")
	}
	return nil
}
