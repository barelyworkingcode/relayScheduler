package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"unicode/utf8"
)

const (
	markerAbsent     = "not a test machine: run devboxWorld bootstrap on a VM"
	markerUnreadable = "marker is not readable"
	maxMarkerBytes   = 1 << 20
	maxWorldBytes    = 8 << 20
)

type worldMarker struct {
	Schema        int
	WorldCheckout string
	WorldRoot     string
	WorldVersion  int
	WrittenAt     string
}

var intToken = regexp.MustCompile(`^-?(0|[1-9][0-9]*)$`)

func markerRefusal(reason string) error {
	return errors.New("not a test machine: " + reason + "; run devboxWorld bootstrap on a VM")
}

// markerPath reads HOME at call time, so a test that isolates HOME never
// reaches the machine's real marker.
func markerPath() string {
	if p := os.Getenv("DEVBOXWORLD_MARKER"); p != "" {
		return p
	}
	h, _ := os.UserHomeDir()
	return filepath.Join(h, ".config", "devboxWorld", "machine.json")
}

// readMarker follows devboxWorld's reference reader check for check; the
// first failure wins and no reason carries OS error text.
func readMarker(path string, isVM func() (bool, error)) (worldMarker, error) {
	fi, lstatErr := os.Lstat(path)
	if errors.Is(lstatErr, fs.ErrNotExist) {
		return worldMarker{}, errors.New(markerAbsent)
	}
	if vm, err := isVM(); err != nil || !vm {
		return worldMarker{}, markerRefusal("not a VM: kern.hv_vmm_present is not 1")
	}
	if lstatErr != nil {
		return worldMarker{}, markerRefusal(markerUnreadable)
	}
	if !fi.Mode().IsRegular() {
		return worldMarker{}, markerRefusal("marker is not a regular file")
	}
	if mode := permBits(fi); mode&0o077 != 0 {
		return worldMarker{}, markerRefusal(fmt.Sprintf("marker is open to group or others (mode %04o)", mode))
	}
	// O_NOFOLLOW: a symlink swapped in after the lstat is refused, not followed.
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return worldMarker{}, markerRefusal(markerUnreadable)
	}
	raw, err := io.ReadAll(io.LimitReader(f, maxMarkerBytes))
	_ = f.Close()
	if err != nil {
		return worldMarker{}, markerRefusal(markerUnreadable)
	}
	if !utf8.Valid(raw) || !json.Valid(raw) {
		return worldMarker{}, markerRefusal("marker is not valid JSON")
	}
	var doc map[string]json.RawMessage
	if !bytes.HasPrefix(bytes.TrimSpace(raw), []byte("{")) || json.Unmarshal(raw, &doc) != nil {
		return worldMarker{}, markerRefusal("marker is not a JSON object")
	}
	if s, ok := doc["schema"]; !ok || string(bytes.TrimSpace(s)) != "1" {
		return worldMarker{}, markerRefusal("marker schema is not 1")
	}
	for _, field := range []string{"world_checkout", "world_root", "world_version", "written_at"} {
		if _, ok := doc[field]; !ok {
			return worldMarker{}, markerRefusal("marker lacks " + field)
		}
	}
	m := worldMarker{Schema: 1}
	for _, f := range []struct {
		name string
		dst  *string
	}{{"world_checkout", &m.WorldCheckout}, {"world_root", &m.WorldRoot}} {
		s, ok := jsonString(doc[f.name])
		if !ok || !filepath.IsAbs(s) {
			return worldMarker{}, markerRefusal("marker " + f.name + " is not an absolute path")
		}
		*f.dst = s
	}
	v, ok := positiveInt(doc["world_version"])
	if !ok {
		return worldMarker{}, markerRefusal("marker world_version is not a positive integer")
	}
	m.WorldVersion = v
	m.WrittenAt, _ = jsonString(doc["written_at"])
	return m, nil
}

// permBits is the stat mode's permission bits including setuid, setgid and
// sticky, which os.FileMode.Perm drops; the refusal prints them.
func permBits(fi os.FileInfo) uint32 {
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		return uint32(st.Mode) & 0o7777
	}
	return uint32(fi.Mode().Perm())
}

func jsonString(raw json.RawMessage) (string, bool) {
	raw = bytes.TrimSpace(raw)
	var s string
	if !bytes.HasPrefix(raw, []byte(`"`)) || json.Unmarshal(raw, &s) != nil {
		return "", false
	}
	return s, true
}

// positiveInt accepts only an integer token: 1.0, "1" and true are refused.
func positiveInt(raw json.RawMessage) (int, bool) {
	tok := string(bytes.TrimSpace(raw))
	if !intToken.MatchString(tok) {
		return 0, false
	}
	n, err := strconv.Atoi(tok)
	if err != nil || n < 1 {
		return 0, false
	}
	return n, true
}

// worldProjectName reads the display name of one project key from the world
// checkout's data/world.json. The name is how relay grant lists the project.
func worldProjectName(m worldMarker, key string) (string, error) {
	f, err := os.Open(filepath.Join(m.WorldCheckout, "data", "world.json"))
	if err != nil {
		return "", errors.New("world data is not readable")
	}
	raw, err := io.ReadAll(io.LimitReader(f, maxWorldBytes))
	_ = f.Close()
	if err != nil {
		return "", errors.New("world data is not readable")
	}
	var doc struct {
		Projects []struct {
			Key  string `json:"key"`
			Name string `json:"name"`
		} `json:"projects"`
	}
	if !utf8.Valid(raw) || json.Unmarshal(raw, &doc) != nil {
		return "", errors.New("world data is not valid JSON")
	}
	for _, p := range doc.Projects {
		if p.Key == key && strings.TrimSpace(p.Name) != "" {
			return p.Name, nil
		}
	}
	return "", fmt.Errorf("world data has no project %q", key)
}
