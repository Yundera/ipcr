package main

import "testing"

func TestIPNSPath(t *testing.T) {
	cases := []struct {
		path, name, kind, ref string // name "" = no match
	}{
		{"/v2/ipns/k51abc/manifests/1.0.0", "k51abc", "manifests", "1.0.0"},
		{"/v2/ipns/example.com/blobs/sha256:00ff", "example.com", "blobs", "sha256:00ff"},
		{"/v2/ipns/aptero.eth/ipcr-hello/manifests/1.1.2", "aptero.eth/ipcr-hello", "manifests", "1.1.2"},
		{"/v2/ipns/aptero.eth/team/app_x/blobs/sha256:00ff", "aptero.eth/team/app_x", "blobs", "sha256:00ff"},
		{"/v2/ipns/k51abc/app/manifests/sha256:00ff", "k51abc/app", "manifests", "sha256:00ff"},
		// An image may itself be called "manifests": the reference never holds a "/".
		{"/v2/ipns/k51abc/manifests/manifests/latest", "k51abc/manifests", "manifests", "latest"},
		{"/v2/ipns/aptero.eth/App/manifests/1", "", "", ""},  // uppercase
		{"/v2/ipns/aptero.eth//manifests/1", "", "", ""},     // empty segment
		{"/v2/ipns/aptero.eth/-app/manifests/1", "", "", ""}, // bad first character
	}
	for _, c := range cases {
		m := ipnsPath.FindStringSubmatch(c.path)
		if c.name == "" {
			if m != nil {
				t.Errorf("%s: matched %q, want no match", c.path, m[1])
			}
			continue
		}
		if m == nil || m[1] != c.name || m[2] != c.kind || m[3] != c.ref {
			t.Errorf("%s: got %q, want [%s %s %s]", c.path, m, c.name, c.kind, c.ref)
		}
	}
}
