package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// What is copied, both ways of the bare mirror: branches and tags. Not refs/pull/* (Gitea's PR
// heads), not notes or anything else. Branches and tags are forced: Gitea is the truth, and a
// force push or a moved tag there is mirrored as such. --prune makes deletions follow too.
var refspecs = []string{"+refs/heads/*:refs/heads/*", "+refs/tags/*:refs/tags/*"}

type mirror struct {
	gitea   *gitea
	state   *state
	repoDir string // one bare repository per Gitea repository: <id>.git
	radHome string
}

// sync brings one Gitea repository's Radicle copy up to date, and records the outcome.
func (m *mirror) sync(id int64) {
	prev := m.state.get(id)
	r, err := m.gitea.repoByID(id)
	if err != nil {
		log.Printf("sync %d: %v", id, err)
		return
	}
	if r == nil || !r.public() {
		// Private now, or deleted. Only a repository that was mirrored has anything to record.
		if m.state.known(id) && prev.State != stFrozen {
			prev.State = stFrozen
			prev.Error = "no longer public in Gitea: not synced any more (what was published stays on the Radicle network)"
			m.save(prev)
			log.Printf("sync %s: frozen, no longer public", prev.FullName)
		}
		return
	}
	if r.Fork || r.Mirror {
		// Did not originate here: a fork's upstream or a mirror's source is the one to publish.
		return
	}
	cur := prev
	cur.FullName, cur.HTMLURL = r.FullName, r.HTMLURL
	if r.Empty {
		if cur.RID == "" {
			cur.State, cur.Error = stWaiting, ""
			m.save(cur)
		}
		return
	}
	if err := m.publish(r, &cur); err != nil {
		cur.State, cur.Error = stError, err.Error()
		log.Printf("sync %s: %v", r.FullName, err)
	} else {
		cur.State, cur.Error, cur.LastSync = stSynced, "", time.Now().UTC()
	}
	m.save(cur)
}

func (m *mirror) save(r repoState) {
	if err := m.state.put(r); err != nil {
		log.Printf("state: %v", err)
	}
}

// publish fetches the repository from Gitea into its bare mirror, creates its Radicle repository on
// first sight, and pushes what changed.
func (m *mirror) publish(r *repo, cur *repoState) error {
	dir := filepath.Join(m.repoDir, fmt.Sprintf("%d.git", r.ID))
	if _, err := os.Stat(dir); err != nil {
		if _, err := m.git("", "init", "--quiet", "--bare", dir); err != nil {
			return err
		}
	}
	src := m.gitea.base + "/" + r.FullName + ".git"
	args := append([]string{"-c", "http.extraHeader=" + m.gitea.gitAuth(), "fetch", "--quiet", "--prune", "--no-tags", src}, refspecs...)
	if _, err := m.git(dir, args...); err != nil {
		return fmt.Errorf("fetch from Gitea: %w", err)
	}
	// rad init and the project's default branch read HEAD.
	if _, err := m.git(dir, "symbolic-ref", "HEAD", "refs/heads/"+r.DefaultBranch); err != nil {
		return err
	}
	digest, err := m.refsDigest(dir)
	if err != nil {
		return err
	}

	if cur.RID == "" {
		// A previous attempt may have created it and failed afterwards: rad init leaves the remote.
		if rid := m.remoteRID(dir); rid != "" {
			cur.RID = rid
		} else {
			rid, err := m.radInit(dir, r)
			if err != nil {
				return fmt.Errorf("rad init: %w", err)
			}
			cur.RID = rid
			log.Printf("sync %s: created %s", r.FullName, rid)
		}
		cur.DefaultBranch = r.DefaultBranch
		m.save(*cur) // before anything else can fail: never init the same repository twice
	} else if m.remoteRID(dir) == "" {
		// The mirror was deleted (it is disposable): point a fresh one at the existing repository.
		if err := m.addRemote(dir, cur.RID); err != nil {
			return err
		}
	}
	if !cur.Crefs {
		if err := m.radCanonicalTags(cur.RID); err != nil {
			log.Printf("sync %s: canonical tags rule: %v (tags stay under the node's namespace)", r.FullName, err)
		} else {
			cur.Crefs = true
		}
	}

	if digest == cur.Refs && cur.State == stSynced && cur.DefaultBranch == r.DefaultBranch {
		return nil
	}
	out, err := m.git(dir, append([]string{"push", "--prune", "--porcelain", "-o", "no-sync", "rad"}, refspecs...)...)
	if err != nil {
		return fmt.Errorf("push to Radicle: %w", err)
	}
	if changed := pushedRefs(out); len(changed) > 0 {
		log.Printf("sync %s → %s: %s", r.FullName, cur.RID, strings.Join(changed, ", "))
	}
	if cur.DefaultBranch != r.DefaultBranch {
		if err := m.radDefaultBranch(cur.RID, r.DefaultBranch); err != nil {
			return fmt.Errorf("default branch: %w", err)
		}
		cur.DefaultBranch = r.DefaultBranch
	}
	cur.Refs = digest
	m.announce(cur.RID)
	return nil
}

// refsDigest summarises the mirror's branches and tags; equal digests mean nothing to push.
func (m *mirror) refsDigest(dir string) (string, error) {
	out, err := m.git(dir, "for-each-ref", "--format=%(objectname) %(refname)", "refs/heads", "refs/tags")
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256([]byte(out))
	return hex.EncodeToString(sum[:]), nil
}

// pushedRefs reads `git push --porcelain` output and returns the refs that changed, for the log.
// Porcelain lines are "<flag>\t<from>:<to>\t<summary>"; '=' is up to date.
func pushedRefs(out string) []string {
	var changed []string
	for _, line := range strings.Split(out, "\n") {
		f := strings.Split(line, "\t")
		if len(f) < 3 || len(f[0]) != 1 || f[0] == "=" {
			continue
		}
		to := f[1]
		if i := strings.LastIndexByte(to, ':'); i >= 0 {
			to = to[i+1:]
		}
		changed = append(changed, strings.TrimPrefix(strings.TrimPrefix(to, "refs/heads/"), "refs/")+" "+f[2])
	}
	return changed
}
