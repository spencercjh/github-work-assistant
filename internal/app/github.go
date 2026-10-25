package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os/exec"
	"regexp"
	"strings"
)

var shaRE = regexp.MustCompile(`^[a-f0-9]{40}$`)
var ErrNotPublic = errors.New("repository is not confirmed public")

type GitHub interface {
	CheckIdentity(context.Context) error
	Collect(context.Context, Watch) ([]byte, error)
}
type GH struct {
	Login string
}

// gh controls GitHub credentials. No token is copied into this App or sent to Raft.
func (g GH) api(ctx context.Context, path string, pages bool) (json.RawMessage, error) {
	if !validGitHubReadPath(path) {
		return nil, errors.New("GitHub endpoint outside read-only allowlist")
	}
	args := []string{"api", "--hostname", "github.com", "--method", "GET", path}
	if pages {
		args = append(args, "--paginate", "--slurp")
	}
	cmd := exec.CommandContext(ctx, "gh", args...)
	var out limitedBuffer
	cmd.Stdout = &out
	cmd.Stderr = &limitedBuffer{}
	if e := cmd.Run(); e != nil {
		return nil, errors.New("GitHub query failed (authentication, permissions or response size); inspect gh separately")
	}
	return json.RawMessage(out.Bytes()), nil
}

var githubReadPathRE = regexp.MustCompile(`^(user|notifications|repos/[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+(?:/(?:issues/[0-9]+(?:/(?:comments|timeline))?|pulls/[0-9]+(?:/(?:reviews|comments))?|commits/[a-f0-9]{40}/(?:check-runs|statuses)))?)(?:\?[^\s#]*)?$`)

func validGitHubReadPath(path string) bool {
	return !strings.Contains(path, "..") && githubReadPathRE.MatchString(path)
}

type limitedBuffer struct{ bytes.Buffer }

func (b *limitedBuffer) Write(p []byte) (int, error) {
	if b.Len()+len(p) > 16<<20 {
		return 0, errors.New("response exceeds 16 MiB")
	}
	return b.Buffer.Write(p)
}
func (g GH) CheckIdentity(ctx context.Context) error {
	b, e := g.api(ctx, "user", false)
	if e != nil {
		return e
	}
	var u struct {
		Login string `json:"login"`
	}
	if json.Unmarshal(b, &u) != nil || !strings.EqualFold(u.Login, g.Login) {
		return errors.New("gh login does not match configured github_login")
	}
	return nil
}
