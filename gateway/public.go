package main

// The public front door. With PUBLIC_POLICY=<state dir>, this ipcrd serves the registry API to
// anyone, read-only, for one publisher's own images only: IPCR Forge runs it as a second process
// (ipcr-public) behind its web proxy, so `docker pull ipcr-<domain>/ipns/<name>/<image>:<tag>`
// works on any machine, with no IPCR installed.
//
// What it serves comes from the main gateway's state, mounted read-only:
//
//	published.json  the publisher's IPNS name, its configured DNSLink/ENS name while verified, and
//	                every published image root
//	config.json     "public": false turns it all off (the admin page's switch)
//
// Everything else gets 404: another IPNS name or CID would make this node fetch, and serve,
// content nobody here published. Run it with AUTO_PIN=false so a stranger's pull pins nothing.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

type publicPolicy struct {
	dir string

	mu    sync.Mutex
	at    time.Time
	on    bool
	names map[string]bool // first path segment of an allowed /ipns/ name
	roots map[string]bool // allowed /ipfs/ roots
}

var public *publicPolicy

func init() {
	if d := os.Getenv("PUBLIC_POLICY"); d != "" {
		public = &publicPolicy{dir: d}
	}
}

// load re-reads the state at most every 10 seconds.
func (p *publicPolicy) load() {
	if time.Since(p.at) < 10*time.Second && p.names != nil {
		return
	}
	p.at = time.Now()
	p.names, p.roots = map[string]bool{}, map[string]bool{}
	p.on = loadConfig(p.dir).publicOn()
	var pub published
	if b, err := os.ReadFile(filepath.Join(p.dir, "published.json")); err == nil {
		json.Unmarshal(b, &pub)
	}
	if pub.Publisher != "" {
		p.names[pub.Publisher] = true
	}
	if pub.Name != "" { // set only while the name checks out (admin.go, checkName)
		p.names[pub.Name] = true
	}
	for _, r := range pub.Images {
		for _, t := range r.Tags {
			p.roots[t.CID] = true
		}
	}
}

// allows reports whether the public door serves kind ("ipns" or "ipfs") for name (the path after
// /v2/<kind>/ up to the manifests/blobs part), and why not.
func (p *publicPolicy) allows(kind, name string) (bool, string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.load()
	if !p.on {
		return false, "public pulls are off on this server"
	}
	switch kind {
	case "ipns":
		first, _, _ := strings.Cut(name, "/")
		if p.names[first] {
			return true, ""
		}
	case "ipfs":
		if p.roots[name] {
			return true, ""
		}
	}
	return false, "this server only serves its own images"
}
