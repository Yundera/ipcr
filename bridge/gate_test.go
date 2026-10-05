package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func newTestGate(t *testing.T) (*gate, *[]*http.Request) {
	t.Helper()
	var seen []*http.Request
	var mu sync.Mutex
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.Clone(r.Context()))
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(up.Close)
	proxy, err := upstreamProxy(up.URL)
	if err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(t.TempDir(), "credentials.json")
	writeCreds(file, gateCreds{
		Push:     map[string]string{"metadec/app": hashSecret("app-token"), "alice/tool": hashSecret("alice-token")},
		Importer: map[string]string{"ipcr": hashSecret("importer-pass")},
	})
	return &gate{credFile: file, upstream: proxy}, &seen
}

func TestGateAuthorization(t *testing.T) {
	g, _ := newTestGate(t)
	for _, c := range []struct {
		name, method, path, user, pass string
		want                           int
	}{
		{"no credentials", "GET", "/v2/", "", "", 401},
		{"wrong password", "GET", "/v2/", "metadec/app", "nope", 401},
		{"login check", "GET", "/v2/", "metadec/app", "app-token", 200},
		{"login, uppercase user", "GET", "/v2/", "Metadec/App", "app-token", 200},
		{"own manifest", "PUT", "/v2/metadec/app/manifests/1.0.0", "metadec/app", "app-token", 200},
		{"own blob upload", "POST", "/v2/metadec/app/blobs/uploads/", "metadec/app", "app-token", 200},
		{"own upload chunk", "PATCH", "/v2/metadec/app/blobs/uploads/abc", "metadec/app", "app-token", 200},
		{"own blob check", "HEAD", "/v2/metadec/app/blobs/sha256:00", "metadec/app", "app-token", 200},
		{"other repository", "PUT", "/v2/alice/tool/manifests/1.0.0", "metadec/app", "app-token", 403},
		{"prefix of another name", "PUT", "/v2/metadec/app2/manifests/1", "metadec/app", "app-token", 403},
		{"nested name", "PUT", "/v2/metadec/app/sub/manifests/1", "metadec/app", "app-token", 403},
		{"catalog as pusher", "GET", "/v2/_catalog", "metadec/app", "app-token", 403},
		{"delete as pusher", "DELETE", "/v2/metadec/app/manifests/sha256:00", "metadec/app", "app-token", 403},
		{"dot-dot", "GET", "/v2/metadec/app/../alice/tool/tags/list", "metadec/app", "app-token", 400},
		{"importer catalog", "GET", "/v2/_catalog", "ipcr", "importer-pass", 200},
		{"importer pull", "GET", "/v2/alice/tool/manifests/1.0.0", "ipcr", "importer-pass", 200},
		{"importer delete", "DELETE", "/v2/alice/tool/manifests/sha256:00", "ipcr", "importer-pass", 200},
		{"importer push", "PUT", "/v2/alice/tool/manifests/1.0.0", "ipcr", "importer-pass", 403},
	} {
		r := httptest.NewRequest(c.method, "http://localhost:5000"+c.path, nil)
		if c.user != "" {
			r.SetBasicAuth(c.user, c.pass)
		}
		w := httptest.NewRecorder()
		g.ServeHTTP(w, r)
		if w.Code != c.want {
			t.Errorf("%s: %d, want %d", c.name, w.Code, c.want)
		}
		if w.Code == 401 && !strings.HasPrefix(w.Header().Get("WWW-Authenticate"), "Basic ") {
			t.Errorf("%s: no Basic challenge", c.name)
		}
	}
}

func TestGateForwarding(t *testing.T) {
	g, seen := newTestGate(t)
	r := httptest.NewRequest("POST", "http://localhost:5000/v2/metadec/app/blobs/uploads/?mount=sha256:00&from=alice/tool", nil)
	r.Host = "localhost:5000"
	r.SetBasicAuth("metadec/app", "app-token")
	w := httptest.NewRecorder()
	g.ServeHTTP(w, r)
	if w.Code != 200 || len(*seen) != 1 {
		t.Fatalf("%d, %d upstream requests", w.Code, len(*seen))
	}
	up := (*seen)[0]
	if up.URL.RawQuery != "" {
		t.Errorf("cross-repository mount forwarded: %q", up.URL.RawQuery)
	}
	if up.Host != "localhost:5000" {
		t.Errorf("Host %q: the registry builds upload URLs from it", up.Host)
	}
	if up.Header.Get("Authorization") != "" {
		t.Error("credentials forwarded to the registry")
	}
}

func TestGateReloadsCredentials(t *testing.T) {
	g, _ := newTestGate(t)
	try := func() int {
		r := httptest.NewRequest("GET", "http://localhost:5000/v2/", nil)
		r.SetBasicAuth("new/repo", "t")
		w := httptest.NewRecorder()
		g.ServeHTTP(w, r)
		return w.Code
	}
	if try() != 401 {
		t.Fatal("unknown repository accepted")
	}
	c := gateCreds{Push: map[string]string{"new/repo": hashSecret("t")}}
	writeCreds(g.credFile, c)
	// A different modification time, as a later write would have.
	st, _ := os.Stat(g.credFile)
	os.Chtimes(g.credFile, st.ModTime().Add(1e9), st.ModTime().Add(1e9))
	if try() != 200 {
		t.Error("new credentials not picked up")
	}
}

// sync against a fake Gitea: secrets set for new repositories, kept for known ones, deleted for
// removed ones, the importer taken from IPCR's file.
func TestPushCredsSync(t *testing.T) {
	var mu sync.Mutex
	secrets := map[string]string{}
	var deleted []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		repo := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/api/v1/repos/"), "/actions/secrets/"+pushSecret)
		switch r.Method {
		case "PUT":
			var b struct{ Data string }
			json.NewDecoder(r.Body).Decode(&b)
			secrets[repo] = b.Data
			w.WriteHeader(201)
		case "DELETE":
			deleted = append(deleted, repo)
			w.WriteHeader(204)
		}
	}))
	defer srv.Close()
	dir := t.TempDir()
	tok := filepath.Join(dir, "token")
	os.WriteFile(tok, []byte("admin-token"), 0o600)
	auth := filepath.Join(dir, "staging-auth")
	os.WriteFile(auth, []byte("ipcr:importer-pass\n"), 0o644)
	p := &pushCreds{file: filepath.Join(dir, "gate", "credentials.json"), stagingAuth: auth, gitea: newGitea(srv.URL, "gitea_admin", tok)}

	if err := p.sync([]string{"Metadec/App", "alice/tool"}, ""); err != nil {
		t.Fatal(err)
	}
	c := p.read()
	if !matches(c.Push["metadec/app"], secrets["Metadec/App"]) || !matches(c.Push["alice/tool"], secrets["alice/tool"]) {
		t.Fatalf("credentials do not match the secrets: %v %v", c.Push, secrets)
	}
	if !matches(c.Importer["ipcr"], "importer-pass") {
		t.Error("importer credential missing")
	}
	first := secrets["alice/tool"]

	// alice/tool keeps its token; Metadec/App is rotated; a removed repository loses it.
	if err := p.sync([]string{"Metadec/App", "alice/tool"}, "metadec/app"); err != nil {
		t.Fatal(err)
	}
	if secrets["alice/tool"] != first || !matches(p.read().Push["metadec/app"], secrets["Metadec/App"]) {
		t.Error("rotation")
	}
	if err := p.sync([]string{"Metadec/App"}, ""); err != nil {
		t.Fatal(err)
	}
	if _, ok := p.read().Push["alice/tool"]; ok || len(deleted) != 1 || deleted[0] != "alice/tool" {
		t.Errorf("removal: %v, deleted %v", p.read().Push, deleted)
	}
}
