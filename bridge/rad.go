package main

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"time"
)

// Everything Radicle is done with the stock `rad` CLI and `git-remote-rad`, against the node's own
// home: the repository is created and signed by the node's key (its only delegate), written into
// the node's storage, and announced through the node's control socket. The key has no passphrase
// (the install step creates it that way), so nothing prompts.

var ridRe = regexp.MustCompile(`rad:z[1-9A-HJ-NP-Za-km-z]+`)

func (m *mirror) env() []string {
	return append(os.Environ(),
		"RAD_HOME="+m.radHome,
		"GIT_TERMINAL_PROMPT=0",
		// rad's output is read by people in the container log, not by a terminal.
		"NO_COLOR=1",
		// Identity documents are git commits; git wants a name even when rad signs them.
		"GIT_AUTHOR_NAME=ipcr-forge", "GIT_AUTHOR_EMAIL=ipcr-forge@localhost",
		"GIT_COMMITTER_NAME=ipcr-forge", "GIT_COMMITTER_EMAIL=ipcr-forge@localhost",
	)
}

// run executes a command in dir (when set) and returns its stdout; on failure the error carries
// the end of stderr, which is where git and rad explain themselves.
func (m *mirror) run(timeout time.Duration, dir, name string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	cmd.Env = m.env()
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			// rad reports its errors on stdout.
			msg = strings.TrimSpace(stdout.String())
		}
		if len(msg) > 500 {
			msg = "…" + msg[len(msg)-500:]
		}
		// Never echo an argument list: a fetch carries the Gitea token in one.
		return stdout.String(), fmt.Errorf("%s %s: %v: %s", name, args[0], err, msg)
	}
	return stdout.String(), nil
}

func (m *mirror) git(dir string, args ...string) (string, error) {
	return m.run(10*time.Minute, dir, "git", args...)
}

func (m *mirror) rad(dir string, args ...string) (string, error) {
	return m.run(2*time.Minute, dir, "rad", args...)
}

// radInit creates the Radicle repository for r from its bare mirror and returns its RID. It also
// makes this node seed it (the node's policy is `block`: nothing is kept unless asked) and adds
// the `rad` remote that pushes go to.
func (m *mirror) radInit(dir string, r *repo) (string, error) {
	desc := r.Description
	if desc == "" {
		desc = "Mirrored from Gitea: " + r.FullName
	}
	out, err := m.rad(dir, "init", dir, "--name", r.Name, "--description", desc,
		"--default-branch", r.DefaultBranch, "--public", "--no-confirm")
	if err != nil {
		return "", err
	}
	if rid := m.remoteRID(dir); rid != "" {
		return rid, nil
	}
	if rid := ridRe.FindString(out); rid != "" {
		return rid, nil
	}
	return "", fmt.Errorf("no repository ID in rad init's output")
}

// remoteRID reads the RID from the mirror's `rad` remote (rad://z…), or "" when there is none.
func (m *mirror) remoteRID(dir string) string {
	out, err := m.git(dir, "config", "--get", "remote.rad.url")
	if err != nil {
		return ""
	}
	return ridFromURL(out)
}

func ridFromURL(u string) string {
	u = strings.TrimSpace(u)
	if !strings.HasPrefix(u, "rad://") {
		return ""
	}
	z, _, _ := strings.Cut(strings.TrimPrefix(u, "rad://"), "/")
	return "rad:" + z
}

// addRemote recreates what rad init sets up: fetch from the repository, push into this node's
// namespace of it.
func (m *mirror) addRemote(dir, rid string) error {
	nid, err := m.rad("", "self", "--nid")
	if err != nil {
		return err
	}
	z := strings.TrimPrefix(rid, "rad:")
	if _, err := m.git(dir, "remote", "add", "rad", "rad://"+z); err != nil {
		return err
	}
	_, err = m.git(dir, "config", "remote.rad.pushurl", "rad://"+z+"/"+strings.TrimSpace(nid))
	return err
}

// radCanonicalTags adds the rule that makes a delegate's tags the repository's refs/tags/*. Without
// it a tag exists only under the node's namespace, where `rad clone` and the explorer do not show
// it. One delegate (this node), so a threshold of 1.
func (m *mirror) radCanonicalTags(rid string) error {
	_, err := m.rad("", "id", "update", "--repo", rid, "--no-confirm", "--title", "Canonical tags",
		"--description", "Tags pushed by the forge are the repository's tags.",
		"--payload", "xyz.radicle.crefs", "rules", `{"refs/tags/*":{"threshold":1,"allow":"delegates"}}`)
	return err
}

func (m *mirror) radDefaultBranch(rid, branch string) error {
	_, err := m.rad("", "id", "update", "--repo", rid, "--no-confirm", "--title", "Default branch",
		"--description", "Follows the Gitea repository.",
		"--payload", "xyz.radicle.project", "defaultBranch", fmt.Sprintf("%q", branch))
	return err
}

// announce tells the network about the new refs. Best effort: a node without peers is not an error,
// and the node announces again on its own schedule.
func (m *mirror) announce(rid string) {
	if _, err := m.rad("", "sync", rid, "--announce", "--timeout", "30s"); err != nil {
		log.Printf("announce %s: %v", rid, err)
	}
}
