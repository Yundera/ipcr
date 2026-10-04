package main

// GitHub's `on:` trigger filters, which act does not apply (it runs every workflow that names the
// event, whatever its branches/tags filters say). Gitea and GitHub apply them server-side; this is
// the same job for the Radicle side. Covered: push (branches, branches-ignore, tags, tags-ignore)
// and pull_request (branches, branches-ignore on the base branch). Path filters are not applied:
// a workflow with only `paths:` runs on every matching event.
//
// Pattern syntax: https://docs.github.com/actions/reference/workflows-and-actions/workflow-syntax#filter-pattern-cheat-sheet

import (
	"fmt"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

// event is what a workflow is matched against.
type event struct {
	Name string // "push" or "pull_request"
	Ref  string // refs/heads/<b> or refs/tags/<t> for push; the base branch name for pull_request
}

// triggers reports whether a workflow file's `on:` fires for ev.
func triggers(workflow []byte, ev event) (bool, error) {
	var wf struct {
		On yaml.Node `yaml:"on"`
	}
	if err := yaml.Unmarshal(workflow, &wf); err != nil {
		return false, err
	}
	on := &wf.On
	switch on.Kind {
	case 0:
		return false, fmt.Errorf("no `on:`")
	case yaml.ScalarNode:
		return on.Value == ev.Name, nil
	case yaml.SequenceNode:
		for _, n := range on.Content {
			if n.Value == ev.Name {
				return true, nil
			}
		}
		return false, nil
	case yaml.MappingNode:
		for i := 0; i+1 < len(on.Content); i += 2 {
			if on.Content[i].Value == ev.Name {
				return matchFilters(on.Content[i+1], ev)
			}
		}
		return false, nil
	}
	return false, fmt.Errorf("unreadable `on:`")
}

func matchFilters(n *yaml.Node, ev event) (bool, error) {
	f := map[string][]string{}
	if n.Kind == yaml.MappingNode {
		for i := 0; i+1 < len(n.Content); i += 2 {
			var list []string
			v := n.Content[i+1]
			switch v.Kind {
			case yaml.SequenceNode:
				if err := v.Decode(&list); err != nil {
					return false, err
				}
			case yaml.ScalarNode:
				list = []string{v.Value}
			}
			f[n.Content[i].Value] = list
		}
	}
	has := func(k string) bool { _, ok := f[k]; return ok }
	pass := func(include, exclude string, name string) bool {
		switch {
		case has(include):
			return matchList(f[include], name)
		case has(exclude):
			return !matchList(f[exclude], name)
		}
		return true
	}

	if ev.Name == "pull_request" {
		return pass("branches", "branches-ignore", ev.Ref), nil
	}
	branchFilter := has("branches") || has("branches-ignore")
	tagFilter := has("tags") || has("tags-ignore")
	if b, ok := strings.CutPrefix(ev.Ref, "refs/heads/"); ok {
		// A workflow that filters tags only never runs for a branch, and the other way round.
		if tagFilter && !branchFilter {
			return false, nil
		}
		return pass("branches", "branches-ignore", b), nil
	}
	if t, ok := strings.CutPrefix(ev.Ref, "refs/tags/"); ok {
		if branchFilter && !tagFilter {
			return false, nil
		}
		return pass("tags", "tags-ignore", t), nil
	}
	return false, nil
}

// matchList applies patterns in order; a later `!pattern` can exclude what an earlier one matched.
func matchList(patterns []string, name string) bool {
	matched := false
	for _, p := range patterns {
		neg := strings.HasPrefix(p, "!")
		if neg {
			p = p[1:]
		}
		if globMatch(p, name) {
			matched = !neg
		}
	}
	return matched
}

// globMatch: `*` any run of characters but `/`, `**` any run of characters, `?` zero or one of the
// preceding character, `+` one or more of it, `[...]` a character class, `\` escapes.
func globMatch(pattern, name string) bool {
	var b strings.Builder
	b.WriteString("^")
	for i := 0; i < len(pattern); i++ {
		switch c := pattern[i]; c {
		case '*':
			if i+1 < len(pattern) && pattern[i+1] == '*' {
				b.WriteString(".*")
				i++
			} else {
				b.WriteString("[^/]*")
			}
		case '?', '+':
			b.WriteByte(c)
		case '[':
			if j := strings.IndexByte(pattern[i:], ']'); j > 0 {
				b.WriteString(pattern[i : i+j+1])
				i += j
			} else {
				b.WriteString(`\[`)
			}
		case '\\':
			if i+1 < len(pattern) {
				i++
				b.WriteString(regexp.QuoteMeta(pattern[i : i+1]))
			}
		default:
			b.WriteString(regexp.QuoteMeta(string(c)))
		}
	}
	b.WriteString("$")
	re, err := regexp.Compile(b.String())
	return err == nil && re.MatchString(name)
}
