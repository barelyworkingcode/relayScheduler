package main

import (
	"errors"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"
	"testing"
	"time"
)

func TestBuildMatches(t *testing.T) {
	const head = "0123456789abcdef0123456789abcdef01234567"
	s := func(rev, mod string) []debug.BuildSetting {
		return []debug.BuildSetting{{Key: "vcs.revision", Value: rev}, {Key: "vcs.modified", Value: mod}}
	}
	cases := []struct {
		name string
		in   []debug.BuildSetting
		ok   bool
	}{
		{"match", s(head, "false"), true},
		{"revision differs", s("fedcba9876543210fedcba9876543210fedcba98", "false"), false},
		{"modified tree", s(head, "true"), false},
		{"no revision", nil, false},
	}
	for _, c := range cases {
		if err := buildMatches(c.in, head); (err == nil) != c.ok {
			t.Errorf("%s: err=%v", c.name, err)
		}
	}
}

func TestStatusState(t *testing.T) {
	r := func(s ...state) []result {
		var rs []result
		for _, x := range s {
			rs = append(rs, result{ID: "j", State: x})
		}
		return rs
	}
	cases := []struct {
		in   []result
		want string
	}{
		{r(statePass), "success"},
		{r(statePass, stateBlocked), "error"},
		{r(stateBlocked, stateFail, statePass), "failure"},
	}
	for _, c := range cases {
		if got := statusState(c.in); got != c.want {
			t.Errorf("%v: got %s want %s", c.in, got, c.want)
		}
	}
}

func TestRenderCommentScrubsHomeAndEscapesPipes(t *testing.T) {
	out := renderComment(evidence{
		PR: 1, Commit: "abc", ToolCommit: "def", Home: "/home/testuser", RunTime: 3 * time.Second,
		Results: []result{{ID: "j1", State: stateFail, Detail: "saw /home/testuser/x a|b"}},
	})
	if strings.Contains(out, "/home/testuser") {
		t.Errorf("home path leaked:\n%s", out)
	}
	if !strings.Contains(out, `a\|b`) {
		t.Errorf("pipe not escaped:\n%s", out)
	}
	if !strings.HasPrefix(out, "### devbox/verify: failure") {
		t.Errorf("heading wrong:\n%s", out)
	}
}

func TestReadMarkerRefusals(t *testing.T) {
	dir := t.TempDir()
	vm := func(v bool) func() (bool, error) { return func() (bool, error) { return v, nil } }
	if _, err := readMarker(filepath.Join(dir, "missing.json"), vm(true)); err == nil || !strings.Contains(err.Error(), "not a test machine") {
		t.Errorf("missing marker: %v", err)
	}
	p := filepath.Join(dir, "machine.json")
	doc := `{"schema":1,"world_checkout":"/w","world_root":"/r","world_version":1,"written_at":"x"}`
	if err := os.WriteFile(p, []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := readMarker(p, vm(false)); err == nil || !strings.Contains(err.Error(), "not a VM") {
		t.Errorf("non-VM host: %v", err)
	}
	if _, err := readMarker(p, func() (bool, error) { return false, errors.New("x") }); err == nil {
		t.Error("VM check error accepted")
	}
	if _, err := readMarker(p, vm(true)); err != nil {
		t.Errorf("valid marker on VM refused: %v", err)
	}
}
