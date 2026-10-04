package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func sign(secret, body string) string {
	m := hmac.New(sha256.New, []byte(secret))
	m.Write([]byte(body))
	return hex.EncodeToString(m.Sum(nil))
}

func TestValidSignature(t *testing.T) {
	body := `{"repository":{"id":7}}`
	if !validSignature([]byte("s3cret"), []byte(body), sign("s3cret", body)) {
		t.Error("good signature rejected")
	}
	for _, sig := range []string{"", "zz", sign("other", body), sign("s3cret", body+" ")} {
		if validSignature([]byte("s3cret"), []byte(body), sig) {
			t.Errorf("bad signature %q accepted", sig)
		}
	}
}

func TestHook(t *testing.T) {
	dir := t.TempDir()
	secretFile := filepath.Join(dir, "hook-secret")
	var got []int64
	h := &hookHandler{secretFile: secretFile, enqueue: func(id int64) { got = append(got, id) }}
	post := func(body, sig string) int {
		r := httptest.NewRequest("POST", "/hooks/gitea", strings.NewReader(body))
		r.Header.Set("X-Gitea-Signature", sig)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w.Code
	}
	body := `{"ref":"refs/tags/v1.0.0","repository":{"id":42,"full_name":"a/b"}}`
	if c := post(body, sign("k", body)); c != http.StatusServiceUnavailable {
		t.Errorf("no secret yet: %d", c)
	}
	os.WriteFile(secretFile, []byte("k\n"), 0o600)
	if c := post(body, sign("x", body)); c != http.StatusUnauthorized {
		t.Errorf("bad signature: %d", c)
	}
	if c := post(body, sign("k", body)); c != http.StatusAccepted {
		t.Errorf("good delivery: %d", c)
	}
	if c := post(`{"sender":{}}`, sign("k", `{"sender":{}}`)); c != http.StatusNoContent {
		t.Errorf("no repository: %d", c)
	}
	if len(got) != 1 || got[0] != 42 {
		t.Errorf("enqueued %v, want [42]", got)
	}
}

func TestQueueCoalesces(t *testing.T) {
	q := newQueue()
	for _, id := range []int64{3, 1, 3, 3, 2, 1} {
		q.add(id)
	}
	var order []int64
	for {
		id, ok := q.next()
		if !ok {
			break
		}
		order = append(order, id)
	}
	if len(order) != 3 || order[0] != 3 || order[1] != 1 || order[2] != 2 {
		t.Errorf("order %v, want [3 1 2]", order)
	}
	q.add(3) // popped, so it may be queued again
	if id, ok := q.next(); !ok || id != 3 {
		t.Error("re-adding a popped repository")
	}
}

func TestPublic(t *testing.T) {
	r := repo{}
	if !r.public() {
		t.Error("plain repository")
	}
	for _, f := range []func(*repo){
		func(r *repo) { r.Private = true },
		func(r *repo) { r.Internal = true },
		func(r *repo) { r.Owner.Visibility = "limited" },
		func(r *repo) { r.Owner.Visibility = "private" },
	} {
		r := repo{}
		f(&r)
		if r.public() {
			t.Errorf("%+v counted as public", r)
		}
	}
}

func TestRIDFromURL(t *testing.T) {
	for in, want := range map[string]string{
		"rad://z2pnYhouQzbiTEmXW9bydgXFqjP1o\n":                                                "rad:z2pnYhouQzbiTEmXW9bydgXFqjP1o",
		"rad://z2pnYhouQzbiTEmXW9bydgXFqjP1o/z6MkppQoVh2QKyQGkPzGgEaqZcXmbeBVhZseQ2cgxuMzA5gR": "rad:z2pnYhouQzbiTEmXW9bydgXFqjP1o",
		"https://example.com/x.git":                                                            "",
		"":                                                                                     "",
	} {
		if got := ridFromURL(in); got != want {
			t.Errorf("ridFromURL(%q) = %q, want %q", in, got, want)
		}
	}
	if got := ridRe.FindString("Your Repository ID (RID) is rad:z2pnYhouQzbiTEmXW9bydgXFqjP1o\nYou can"); got != "rad:z2pnYhouQzbiTEmXW9bydgXFqjP1o" {
		t.Errorf("ridRe: %q", got)
	}
}

func TestPushedRefs(t *testing.T) {
	out := "To rad://z2pn/z6Mk\n" +
		"=\trefs/heads/main:refs/heads/main\t[up to date]\n" +
		"*\trefs/tags/v1.0.0:refs/tags/v1.0.0\t[new tag]\n" +
		"-\t:refs/heads/old\t[deleted]\n" +
		"+\trefs/heads/dev:refs/heads/dev\t1111111...2222222 (forced update)\n" +
		"Done\n"
	got := strings.Join(pushedRefs(out), "; ")
	want := "tags/v1.0.0 [new tag]; old [deleted]; dev 1111111...2222222 (forced update)"
	if got != want {
		t.Errorf("got %q\nwant %q", got, want)
	}
}

func TestStateRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "repos.json")
	s, err := loadState(path)
	if err != nil {
		t.Fatal(err)
	}
	if s.known(5) || s.get(5).ID != 5 {
		t.Fatal("empty state")
	}
	s.put(repoState{ID: 5, FullName: "b/two", RID: "rad:z2", State: stSynced, Refs: "abc", Crefs: true})
	s.put(repoState{ID: 9, FullName: "a/one", State: stWaiting})
	s2, err := loadState(path)
	if err != nil {
		t.Fatal(err)
	}
	if r := s2.get(5); r.RID != "rad:z2" || r.Refs != "abc" || !r.Crefs || r.State != stSynced {
		t.Errorf("reloaded %+v", r)
	}
	l := s2.list()
	if len(l) != 2 || l[0].FullName != "a/one" || l[1].Refs != "" {
		t.Errorf("list %+v", l)
	}
	if ids := s2.ids(); len(ids) != 2 || ids[0] != 5 || ids[1] != 9 {
		t.Errorf("ids %v", ids)
	}
}

func TestExampleEmbedded(t *testing.T) {
	for _, p := range []string{"example/Dockerfile", "example/.gitea/workflows/build.yml", "example/README.md"} {
		if _, err := exampleFiles.ReadFile(p); err != nil {
			t.Errorf("%s: %v", p, err)
		}
	}
	b, _ := exampleFiles.ReadFile("example/.gitea/workflows/build.yml")
	if !strings.Contains(string(b), "localhost:5000/${{ github.repository }}") || !strings.Contains(string(b), "network=host") {
		t.Error("example workflow lost its staging registry lines")
	}
}

func TestWeb(t *testing.T) {
	dir := t.TempDir()
	st, _ := loadState(filepath.Join(dir, "repos.json"))
	h := webHandler(st, filepath.Join(dir, "published.json"))
	get := func(p string) (int, string) {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("GET", p, nil))
		return w.Code, w.Body.String()
	}
	if c, b := get("/"); c != 200 || !strings.Contains(b, "IPCR Forge") {
		t.Errorf("/: %d", c)
	}
	if c, _ := get("/images.json"); c != 404 {
		t.Errorf("/images.json before anything is published: %d", c)
	}
	os.WriteFile(filepath.Join(dir, "published.json"), []byte(`{"images":{}}`), 0o644)
	if c, b := get("/images.json"); c != 200 || b != `{"images":{}}` {
		t.Errorf("/images.json: %d %q", c, b)
	}
	if c, b := get("/repos.json"); c != 200 || strings.TrimSpace(b) != "[]" {
		t.Errorf("/repos.json: %d %q", c, b)
	}
	if c, _ := get("/nothing"); c != 404 {
		t.Errorf("/nothing: %d", c)
	}
}
