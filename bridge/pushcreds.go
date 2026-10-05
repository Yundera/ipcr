package main

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

// pushSecret is the Actions secret each publishing repository gets: its password at the gate.
const pushSecret = "IPCR_PUSH_TOKEN"

// pushCreds hands out the gate's credentials. Every repository allowed to publish gets a random
// token, set as its IPCR_PUSH_TOKEN Actions secret (Gitea gives secrets to the repository's own
// workflows, never to pull requests from forks); the gate only learns the token's hash. A
// repository that may no longer publish loses both. The importer's credential is the one IPCR
// generated (IMPORT_AUTH_GENERATE), read from its state folder.
type pushCreds struct {
	file        string // the gate's credentials.json
	stagingAuth string // IPCR's "user:password"
	gitea       *gitea // admin-scoped (write:repository): sets and deletes secrets

	mu sync.Mutex
}

func (p *pushCreds) read() gateCreds {
	var c gateCreds
	if b, err := os.ReadFile(p.file); err == nil {
		json.Unmarshal(b, &c)
	}
	if c.Push == nil {
		c.Push = map[string]string{}
	}
	return c
}

func (p *pushCreds) has(repo string) bool {
	_, ok := p.read().Push[strings.ToLower(repo)]
	return ok
}

// sync makes the gate's credentials match repos (owner/repo, as Gitea names them): new ones get a
// token, gone ones lose it. rotate, when set, gets a new token even if it has one.
func (p *pushCreds) sync(repos []string, rotate string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	cur := p.read()
	next := gateCreds{Push: map[string]string{}, Importer: map[string]string{}}
	var errs []string
	for _, repo := range repos {
		key := strings.ToLower(repo)
		if h, ok := cur.Push[key]; ok && !strings.EqualFold(repo, rotate) {
			next.Push[key] = h
			continue
		}
		tok := randomString(32)
		if _, err := p.gitea.call("PUT", "/repos/"+repo+"/actions/secrets/"+pushSecret, map[string]string{"data": tok}, nil); err != nil {
			errs = append(errs, repo+": "+err.Error())
			if h, ok := cur.Push[key]; ok {
				next.Push[key] = h // keep the old one working
			}
			continue
		}
		next.Push[key] = hashSecret(tok)
		log.Printf("push credentials: %s gets a new %s", repo, pushSecret)
	}
	for key := range cur.Push {
		if _, ok := next.Push[key]; !ok {
			// Best effort: without its hash at the gate the secret is useless anyway.
			p.gitea.call("DELETE", "/repos/"+key+"/actions/secrets/"+pushSecret, nil, nil)
			log.Printf("push credentials: %s may no longer publish", key)
		}
	}
	if b, err := os.ReadFile(p.stagingAuth); err == nil {
		if user, pass, ok := strings.Cut(strings.TrimSpace(string(b)), ":"); ok && user != "" && pass != "" {
			next.Importer[strings.ToLower(user)] = hashSecret(pass)
		}
	}
	if err := writeCreds(p.file, next); err != nil {
		return err
	}
	if len(errs) > 0 {
		sort.Strings(errs)
		return fmt.Errorf("could not set %s: %s", pushSecret, strings.Join(errs, "; "))
	}
	return nil
}

func writeCreds(path string, c gateCreds) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	b, _ := json.MarshalIndent(c, "", "  ")
	// Hashes only: readable by the gate (same uid), useless to anyone else.
	if err := os.WriteFile(path+".new", b, 0o644); err != nil {
		return err
	}
	return os.Rename(path+".new", path)
}
