package main

import "testing"

func TestTriggers(t *testing.T) {
	tag := event{"push", "refs/tags/v1.2.3"}
	main := event{"push", "refs/heads/main"}
	feat := event{"push", "refs/heads/feature/x"}
	pr := event{"pull_request", "main"}
	cases := []struct {
		on   string
		ev   event
		want bool
	}{
		{"on: push", tag, true},
		{"on: push", main, true},
		{"on: push", pr, false},
		{"on: [push, pull_request]", pr, true},
		{"on:\n  push:\n    tags: ['v*']", tag, true},
		{"on:\n  push:\n    tags: ['v*']", main, false},
		{"on:\n  push:\n    tags: ['release-*']", tag, false},
		{"on:\n  push:\n    branches: [main]", main, true},
		{"on:\n  push:\n    branches: [main]", feat, false},
		{"on:\n  push:\n    branches: [main]", tag, false},
		{"on:\n  push:\n    branches: ['**']", feat, true},
		{"on:\n  push:\n    branches: ['*']", feat, false},
		{"on:\n  push:\n    branches-ignore: [main]", feat, true},
		{"on:\n  push:\n    branches-ignore: [main]", main, false},
		{"on:\n  push:\n    branches: [main]\n    tags: ['v*']", tag, true},
		{"on:\n  push:\n    branches: [main]\n    tags: ['v*']", main, true},
		{"on:\n  push:\n    tags: ['v*', '!v*-rc*']", event{"push", "refs/tags/v2.0.0-rc1"}, false},
		{"on:\n  push:\n    tags: ['v[0-9]+.[0-9]+.[0-9]+']", tag, true},
		{"on:\n  push:\n    paths: ['src/**']", tag, true},
		{"on:\n  push:\n  workflow_dispatch:", main, true},
		{"on:\n  pull_request:\n    branches: [main]", pr, true},
		{"on:\n  pull_request:\n    branches: [develop]", pr, false},
		{"on:\n  workflow_dispatch:", main, false},
	}
	for _, c := range cases {
		got, err := triggers([]byte(c.on+"\njobs: {}\n"), c.ev)
		if err != nil || got != c.want {
			t.Errorf("%q on %v: got %v (%v), want %v", c.on, c.ev, got, err, c.want)
		}
	}
}
