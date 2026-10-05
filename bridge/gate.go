package main

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
)

// The gate is the staging registry's front door (`ipcr-forge-bridge gate`). CI jobs push to
// localhost:5000 and IPCR imports from it; before the gate, anyone who could run a job could push
// any image name, and IPCR published what it found. Now:
//
//   - a job logs in as its repository: user <owner>/<repo>, password the IPCR_PUSH_TOKEN secret the
//     bridge set on that repository (pushcreds.go). It may read and write that repository's
//     manifests, blobs and tags, and nothing else: no other name, no catalog, no delete, no
//     cross-repository blob mount;
//   - IPCR logs in as the importer (the credential it generated itself): read everything, list the
//     catalog, delete manifests it has published.
//
// It runs in the CI daemon's network namespace, where jobs see it as localhost:5000. The registry
// behind it listens on a unix socket shared with the gate only, so a job cannot go around it.
//
// Environment: GATE_LISTEN (default :5000), GATE_UPSTREAM (default
// unix:///run/staging/registry.sock), GATE_CREDENTIALS (default /gate/credentials.json).

// gateCreds is credentials.json, written by the bridge: SHA-256 (hex) of each password.
type gateCreds struct {
	Push     map[string]string `json:"push"`     // "<owner>/<repo>" (lowercase) → hash
	Importer map[string]string `json:"importer"` // user → hash
}

type gate struct {
	credFile string
	upstream http.Handler

	mu    sync.Mutex
	mtime time.Time
	creds gateCreds
}

func gateMain() error {
	g := &gate{credFile: env("GATE_CREDENTIALS", "/gate/credentials.json")}
	up, err := upstreamProxy(env("GATE_UPSTREAM", "unix:///run/staging/registry.sock"))
	if err != nil {
		return err
	}
	g.upstream = up
	addr := env("GATE_LISTEN", ":5000")
	log.Printf("gate: %s → %s, credentials %s", addr, env("GATE_UPSTREAM", "unix:///run/staging/registry.sock"), g.credFile)
	return http.ListenAndServe(addr, g)
}

// upstreamProxy forwards to the registry, keeping the client's Host (the registry builds upload
// URLs from it) and dropping the client's credentials (the registry has none of its own).
func upstreamProxy(target string) (http.Handler, error) {
	u, err := url.Parse(target)
	if err != nil {
		return nil, err
	}
	tr := http.DefaultTransport.(*http.Transport).Clone()
	dest := &url.URL{Scheme: u.Scheme, Host: u.Host}
	if u.Scheme == "unix" {
		sock := u.Path
		tr.DialContext = func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", sock)
		}
		dest = &url.URL{Scheme: "http", Host: "registry"}
	}
	return &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(dest)
			pr.Out.Host = pr.In.Host
			pr.Out.Header.Del("Authorization")
		},
		Transport:     tr,
		FlushInterval: -1,
	}, nil
}

func (g *gate) load() gateCreds {
	g.mu.Lock()
	defer g.mu.Unlock()
	st, err := os.Stat(g.credFile)
	if err != nil {
		return g.creds
	}
	if !st.ModTime().Equal(g.mtime) {
		var c gateCreds
		if b, err := os.ReadFile(g.credFile); err == nil && json.Unmarshal(b, &c) == nil {
			g.creds, g.mtime = c, st.ModTime()
		}
	}
	return g.creds
}

func hashSecret(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

func matches(stored, pass string) bool {
	return stored != "" && subtle.ConstantTimeCompare([]byte(stored), []byte(hashSecret(pass))) == 1
}

func registryError(w http.ResponseWriter, status int, code, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Docker-Distribution-API-Version", "registry/2.0")
	if status == http.StatusUnauthorized {
		w.Header().Set("WWW-Authenticate", `Basic realm="ipcr-staging"`)
	}
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]any{"errors": []map[string]string{{"code": code, "message": msg}}})
}

func (g *gate) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p := r.URL.Path
	if r.URL.RawPath != "" || strings.Contains(p, "..") || strings.Contains(p, "//") || !strings.HasPrefix(p, "/v2") {
		registryError(w, http.StatusBadRequest, "NAME_INVALID", "bad path")
		return
	}
	user, pass, _ := r.BasicAuth()
	user = strings.ToLower(user)
	creds := g.load()
	role := ""
	switch {
	case matches(creds.Importer[user], pass):
		role = "importer"
	case matches(creds.Push[user], pass):
		role = "push"
	}
	if role == "" {
		registryError(w, http.StatusUnauthorized, "UNAUTHORIZED", "log in as <owner>/<repo> with the repository's IPCR_PUSH_TOKEN secret")
		return
	}
	if !allowed(role, user, r) {
		log.Printf("gate: %s refused %s %s", user, r.Method, p)
		registryError(w, http.StatusForbidden, "DENIED", user+" may only push "+user)
		return
	}
	if role == "push" {
		// A cross-repository mount would copy a blob from a repository this user may not read.
		// Without it the client simply uploads the blob.
		q := r.URL.Query()
		if q.Has("mount") || q.Has("from") {
			q.Del("mount")
			q.Del("from")
			r.URL.RawQuery = q.Encode()
		}
	}
	g.upstream.ServeHTTP(w, r)
}

// allowed applies the two roles to a request.
func allowed(role, user string, r *http.Request) bool {
	p, m := r.URL.Path, r.Method
	read := m == "GET" || m == "HEAD"
	if p == "/v2/" || p == "/v2" {
		return read
	}
	if role == "importer" {
		if read {
			return true
		}
		return m == "DELETE" && strings.Contains(p, "/manifests/")
	}
	rest, ok := strings.CutPrefix(p, "/v2/"+user+"/")
	if !ok || user == "" {
		return false
	}
	if !strings.HasPrefix(rest, "blobs/") && !strings.HasPrefix(rest, "manifests/") && !strings.HasPrefix(rest, "tags/") {
		return false
	}
	switch m {
	case "GET", "HEAD", "POST", "PUT", "PATCH":
		return true
	}
	return false
}
