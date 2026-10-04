package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// gitea is a minimal Gitea API client. The daemon authenticates with the token in tokenFile (read
// on every call: the setup step may write it after the service starts); the setup step with the
// admin's password instead (Gitea's token API accepts nothing else).
type gitea struct {
	base      string
	owner     string
	tokenFile string
	password  string // setup only
	http      *http.Client
}

func newGitea(base, owner, tokenFile string) *gitea {
	return &gitea{base: strings.TrimRight(base, "/"), owner: owner, tokenFile: tokenFile,
		http: &http.Client{Timeout: 30 * time.Second}}
}

func (g *gitea) token() string {
	b, _ := os.ReadFile(g.tokenFile)
	return strings.TrimSpace(string(b))
}

func (g *gitea) hasToken() bool { return g.token() != "" }

// gitAuth is the header git sends to Gitea's HTTP endpoint: basic auth with the token as password,
// passed with http.extraHeader so it never appears in a URL, `ps` or a log.
func (g *gitea) gitAuth() string {
	return "Authorization: Basic " + base64.StdEncoding.EncodeToString([]byte(g.owner+":"+g.token()))
}

// call sends a request and decodes a JSON answer into out (when non-nil and the status is 2xx).
// It returns the status; a transport error is returned as an error with status 0.
func (g *gitea) call(method, path string, body, out any) (int, error) {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return 0, err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, g.base+"/api/v1"+path, rd)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	if g.password != "" {
		req.SetBasicAuth(g.owner, g.password)
	} else {
		req.Header.Set("Authorization", "token "+g.token())
	}
	resp, err := g.http.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if resp.StatusCode/100 != 2 {
		msg := strings.TrimSpace(string(data))
		if len(msg) > 300 {
			msg = msg[:300]
		}
		return resp.StatusCode, fmt.Errorf("%s %s: %s %s", method, path, resp.Status, msg)
	}
	if out != nil && len(data) > 0 {
		if err := json.Unmarshal(data, out); err != nil {
			return resp.StatusCode, fmt.Errorf("%s %s: %w", method, path, err)
		}
	}
	return resp.StatusCode, nil
}

type repo struct {
	ID            int64  `json:"id"`
	Name          string `json:"name"`
	FullName      string `json:"full_name"`
	Description   string `json:"description"`
	Private       bool   `json:"private"`
	Internal      bool   `json:"internal"`
	Fork          bool   `json:"fork"`
	Mirror        bool   `json:"mirror"`
	Empty         bool   `json:"empty"`
	DefaultBranch string `json:"default_branch"`
	Owner         struct {
		Login string `json:"login"`
		// public | limited | private. Only a public owner's repositories are public.
		Visibility string `json:"visibility"`
	} `json:"owner"`
}

// public reports whether the repository may leave this server. The token is `public-only`, so
// Gitea already hides everything else; this is the second lock on the same door.
func (r *repo) public() bool {
	return !r.Private && !r.Internal && (r.Owner.Visibility == "" || r.Owner.Visibility == "public")
}

// publicRepos lists every repository the token can see.
func (g *gitea) publicRepos() ([]repo, error) {
	var all []repo
	for page := 1; ; page++ {
		var res struct {
			OK   bool   `json:"ok"`
			Data []repo `json:"data"`
		}
		q := url.Values{"limit": {"50"}, "page": {fmt.Sprint(page)}}
		if _, err := g.call("GET", "/repos/search?"+q.Encode(), nil, &res); err != nil {
			return nil, err
		}
		all = append(all, res.Data...)
		if len(res.Data) < 50 {
			return all, nil
		}
	}
}

// repoByID returns the repository, or nil when the token cannot see it (private, deleted).
func (g *gitea) repoByID(id int64) (*repo, error) {
	var r repo
	code, err := g.call("GET", fmt.Sprintf("/repositories/%d", id), nil, &r)
	if code == http.StatusNotFound || code == http.StatusForbidden {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &r, nil
}
