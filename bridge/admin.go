package main

import (
	"bytes"
	"encoding/base32"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// adminAPI is what the admin page calls, behind auth.requireAdmin:
//
//	/admin/api/me                     who is signed in
//	/admin/api/ipcr/<path>            ipcrd's admin API (gateway/admin.go), with its token
//	POST /admin/api/key/export        the publisher key, encrypted with the given passphrase
//	GET  /admin/api/repos             Gitea's public repositories and whether each may publish images
//	PUT  /admin/api/repos             {full_name, publish}
//
// It also keeps ipcrd's allowlist equal to "public Gitea repositories, minus those switched off":
// a build can push any image name to the staging registry, and only those names are published.
type adminAPI struct {
	ipcr      string // ipcrd's admin API, e.g. http://ipcr-gateway:4769
	tokenFile string // ipcrd's admin-token, read through the read-only ipcr/state mount
	keystore  string // Kubo's keystore folder, read-only
	publisher string // the publisher key's name (IMPORT_PUBLISHER)
	settings  string // admin.json: the repositories switched off, the root organisation
	gitea     *gitea // public-only, read: lists the repositories
	admin     *gitea // admin-scoped: name collisions (any user or org), push secrets
	creds     *pushCreds
	rootOrg   string // default root organisation (IPCR_ROOT_ORG), until set in the admin page
	au        *auth
	http      *http.Client

	mu sync.Mutex
}

type adminSettings struct {
	// Repositories (owner/name, lowercase) whose images are not published.
	Disabled []string `json:"disabled"`
	// The organisation whose repositories are also published at the root of the forge's name
	// (metadec/app → /ipns/<forge>/app). nil: IPCR_ROOT_ORG; "": none.
	RootOrg *string `json:"rootOrg,omitempty"`
}

func (a *adminAPI) root() string {
	if s := a.load(); s.RootOrg != nil {
		return strings.ToLower(*s.RootOrg)
	}
	return strings.ToLower(a.rootOrg)
}

func (a *adminAPI) save(s adminSettings) error {
	b, _ := json.MarshalIndent(s, "", "  ")
	if err := os.WriteFile(a.settings+".new", b, 0o644); err != nil {
		return err
	}
	return os.Rename(a.settings+".new", a.settings)
}

func (a *adminAPI) load() adminSettings {
	var s adminSettings
	if b, err := os.ReadFile(a.settings); err == nil {
		json.Unmarshal(b, &s)
	}
	return s
}

func (a *adminAPI) token() string {
	b, _ := os.ReadFile(a.tokenFile)
	return strings.TrimSpace(string(b))
}

func (a *adminAPI) routes(mux *http.ServeMux, au *auth) {
	a.au = au
	guard := au.requireAdmin
	mux.Handle("GET /admin/api/me", guard(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u, _ := au.user(r)
		writeJSONResponse(w, map[string]string{"user": u, "publisher": a.publisher})
	})))
	mux.Handle("/admin/api/ipcr/{path...}", guard(http.HandlerFunc(a.proxy)))
	mux.Handle("POST /admin/api/key/export", guard(http.HandlerFunc(a.export)))
	mux.Handle("GET /admin/api/repos", guard(http.HandlerFunc(a.listRepos)))
	mux.Handle("PUT /admin/api/repos", guard(http.HandlerFunc(a.setRepo)))
	mux.Handle("POST /admin/api/repos/rotate", guard(http.HandlerFunc(a.rotate)))
	mux.Handle("GET /admin/api/settings", guard(http.HandlerFunc(a.getSettings)))
	mux.Handle("PUT /admin/api/settings", guard(http.HandlerFunc(a.putSettings)))
}

func writeJSONResponse(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	json.NewEncoder(w).Encode(v)
}

// proxy forwards to ipcrd's admin API. Only its own paths: /admin/<path>.
func (a *adminAPI) proxy(w http.ResponseWriter, r *http.Request) {
	p := r.PathValue("path")
	if strings.Contains(p, "..") {
		http.Error(w, "bad path", http.StatusBadRequest)
		return
	}
	u := a.ipcr + "/admin/" + p
	if r.URL.RawQuery != "" {
		u += "?" + r.URL.RawQuery
	}
	req, err := http.NewRequestWithContext(r.Context(), r.Method, u, io.LimitReader(r.Body, 1<<20))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+a.token())
	resp, err := a.http.Do(req)
	if err != nil {
		// Not 502: a proxy in front (Cloudflare) replaces a 502's body with its own page.
		http.Error(w, "IPCR unreachable: "+err.Error(), http.StatusServiceUnavailable)
		return
	}
	defer resp.Body.Close()
	w.Header().Set("Content-Type", resp.Header.Get("Content-Type"))
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(resp.StatusCode)
	io.Copy(w, resp.Body)
}

// keystoreFile is where Kubo keeps the key <name>: "key_" + lowercase unpadded base32 of the name.
func keystoreFile(dir, name string) string {
	return filepath.Join(dir, "key_"+strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString([]byte(name))))
}

// export reads the publisher key from Kubo's keystore and returns it sealed with the passphrase.
// The cleartext key exists only in this process's memory, for the length of the request.
func (a *adminAPI) export(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Passphrase string `json:"passphrase"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&req); err != nil {
		http.Error(w, "{passphrase}?", http.StatusBadRequest)
		return
	}
	key, err := os.ReadFile(keystoreFile(a.keystore, a.publisher))
	if err != nil {
		log.Printf("admin: key export: %v", err)
		http.Error(w, "cannot read the publisher key "+a.publisher+" (has anything been published yet?)", http.StatusNotFound)
		return
	}
	b, err := sealKey(a.publisher, key, req.Passphrase)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	u, _ := a.au.user(r)
	log.Printf("admin: publisher key %s (%s) exported by %s", a.publisher, b.ID, u)
	name := fmt.Sprintf("%s-%s-%s.ipcrkey.json", a.publisher, b.ID[len(b.ID)-8:], time.Now().UTC().Format("20060102"))
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Disposition", `attachment; filename="`+name+`"`)
	w.Header().Set("Cache-Control", "no-store")
	json.NewEncoder(w).Encode(b)
}

type repoToggle struct {
	FullName string `json:"full_name"`
	Publish  bool   `json:"publish"`
}

// repoView is a row of the admin page's Repositories tab.
type repoView struct {
	repoToggle
	PublishedAs []string `json:"published_as,omitempty"` // short path first
	Credential  bool     `json:"credential"`             // the gate knows its IPCR_PUSH_TOKEN
	Collision   string   `json:"collision,omitempty"`    // why its short path is not used
}

// pubPlan is what IPCR is told to publish, and why some short paths are not used.
type pubPlan struct {
	Repos      []string            // owner/repo, lowercase
	Aliases    map[string][]string // owner/repo → short paths
	Collisions map[string]string   // owner/repo → reason
}

// plan computes the allowlist: the eligible public repositories not switched off, and for the root
// organisation's, their short path, unless it would collide with an owner's folder: the name of
// any Gitea user or organisation (exists), or the owner of another allowed repository.
func plan(repos []repo, disabled []string, root string, exists func(string) bool) pubPlan {
	p := pubPlan{Aliases: map[string][]string{}, Collisions: map[string]string{}}
	off := map[string]bool{}
	for _, d := range disabled {
		off[d] = true
	}
	owners := map[string]bool{}
	for i := range repos {
		n := strings.ToLower(repos[i].FullName)
		if eligible(&repos[i]) && !off[n] {
			p.Repos = append(p.Repos, n)
			owners[strings.SplitN(n, "/", 2)[0]] = true
		}
	}
	sort.Strings(p.Repos)
	if root == "" {
		return p
	}
	for _, n := range p.Repos {
		owner, name, _ := strings.Cut(n, "/")
		if owner != root {
			continue
		}
		switch {
		case owners[name]:
			p.Collisions[n] = "short path /" + name + " is the folder of " + name + "'s own repositories"
		case exists != nil && exists(name):
			p.Collisions[n] = "a Gitea user or organisation is named " + name + ": its repositories would share /" + name
		default:
			p.Aliases[n] = []string{name}
		}
	}
	return p
}

func (a *adminAPI) currentPlan() (pubPlan, []repo, error) {
	repos, err := a.gitea.publicRepos()
	if err != nil {
		return pubPlan{}, nil, err
	}
	return plan(repos, a.load().Disabled, a.root(), a.admin.exists), repos, nil
}

func (a *adminAPI) listRepos(w http.ResponseWriter, r *http.Request) {
	p, repos, err := a.currentPlan()
	if err != nil {
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}
	disabled := map[string]bool{}
	for _, d := range a.load().Disabled {
		disabled[d] = true
	}
	out := []repoView{}
	for _, rp := range repos {
		if !eligible(&rp) {
			continue
		}
		n := strings.ToLower(rp.FullName)
		v := repoView{repoToggle: repoToggle{FullName: n, Publish: !disabled[n]}, Collision: p.Collisions[n]}
		if v.Publish {
			v.PublishedAs = append(append([]string{}, p.Aliases[n]...), n)
			v.Credential = a.creds != nil && a.creds.has(n)
		}
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].FullName < out[j].FullName })
	writeJSONResponse(w, out)
}

func (a *adminAPI) setRepo(w http.ResponseWriter, r *http.Request) {
	var req repoToggle
	if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&req); err != nil || req.FullName == "" {
		http.Error(w, "{full_name, publish}?", http.StatusBadRequest)
		return
	}
	n := strings.ToLower(req.FullName)
	a.mu.Lock()
	s := a.load()
	var keep []string
	for _, d := range s.Disabled {
		if d != n {
			keep = append(keep, d)
		}
	}
	if !req.Publish {
		keep = append(keep, n)
	}
	sort.Strings(keep)
	s.Disabled = keep
	err := a.save(s)
	a.mu.Unlock()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if err := a.pushAllowlist(); err != nil {
		http.Error(w, "saved, but not fully applied: "+err.Error(), http.StatusServiceUnavailable)
		return
	}
	writeJSONResponse(w, req)
}

// rotate gives a repository a new IPCR_PUSH_TOKEN: when it changed hands, or the old one leaked.
func (a *adminAPI) rotate(w http.ResponseWriter, r *http.Request) {
	var req repoToggle
	if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&req); err != nil || req.FullName == "" {
		http.Error(w, "{full_name}?", http.StatusBadRequest)
		return
	}
	p, _, err := a.currentPlan()
	if err == nil {
		err = a.creds.sync(p.Repos, req.FullName)
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}
	writeJSONResponse(w, map[string]string{"rotated": req.FullName})
}

func (a *adminAPI) getSettings(w http.ResponseWriter, r *http.Request) {
	writeJSONResponse(w, map[string]string{"rootOrg": a.root(), "rootOrgDefault": strings.ToLower(a.rootOrg)})
}

func (a *adminAPI) putSettings(w http.ResponseWriter, r *http.Request) {
	var req struct {
		RootOrg string `json:"rootOrg"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&req); err != nil {
		http.Error(w, "{rootOrg}?", http.StatusBadRequest)
		return
	}
	org := strings.ToLower(strings.TrimSpace(req.RootOrg))
	if org != "" && !a.admin.exists(org) {
		http.Error(w, "no Gitea organisation named "+org, http.StatusBadRequest)
		return
	}
	a.mu.Lock()
	s := a.load()
	s.RootOrg = &org
	err := a.save(s)
	a.mu.Unlock()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if err := a.pushAllowlist(); err != nil {
		http.Error(w, "saved, but not fully applied: "+err.Error(), http.StatusServiceUnavailable)
		return
	}
	writeJSONResponse(w, map[string]string{"rootOrg": org})
}

// eligible: the repositories whose images may be published — the same that are mirrored.
func eligible(r *repo) bool { return r.public() && !r.Fork && !r.Mirror }

// pushAllowlist sends ipcrd the current plan and gives the gate the matching credentials. Called
// on every reconcile, toggle and root-organisation change.
func (a *adminAPI) pushAllowlist() error {
	p, _, err := a.currentPlan()
	if err != nil {
		return err
	}
	if err := a.sendAllowlist(p); err != nil {
		return err
	}
	if a.creds != nil {
		return a.creds.sync(p.Repos, "")
	}
	return nil
}

func (a *adminAPI) sendAllowlist(p pubPlan) error {
	list := p.Repos
	if list == nil {
		list = []string{}
	}
	body, _ := json.Marshal(map[string]any{"allow": map[string]any{"required": true, "repos": list, "aliases": p.Aliases}})
	req, _ := http.NewRequest("PATCH", a.ipcr+"/admin/config", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+a.token())
	resp, err := a.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 300))
		return errors.New(resp.Status + " " + strings.TrimSpace(string(b)))
	}
	return nil
}
