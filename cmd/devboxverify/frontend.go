package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"
)

const frontendRequestTimeout = 10 * time.Second

// frontendResponse is one answer from relay's front door. Status 0 means the
// request never got an HTTP answer. Error is the JSON "error" field, or the
// trimmed body when there is none.
type frontendResponse struct {
	Status int
	Body   []byte
	Error  string
}

// frontendDo sends one JSON request over relay's frontend unix socket with
// the proxy-class bearer. Each request gets its own connection and 10 s.
func frontendDo(ctx context.Context, e env, method, path string, body any) frontendResponse {
	ctx, cancel := context.WithTimeout(ctx, frontendRequestTimeout)
	defer cancel()
	var rd io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return frontendResponse{}
		}
		rd = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://relay"+path, rd)
	if err != nil {
		return frontendResponse{}
	}
	req.Header.Set("Authorization", "Bearer "+e.Credential)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	client := &http.Client{Transport: &http.Transport{DisableKeepAlives: true, DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", e.FrontendSocket)
	}}}
	resp, err := client.Do(req)
	if err != nil {
		return frontendResponse{}
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	msg := strings.TrimSpace(string(raw))
	var b struct {
		Error string `json:"error"`
	}
	if json.Unmarshal(raw, &b) == nil && b.Error != "" {
		msg = b.Error
	}
	return frontendResponse{Status: resp.StatusCode, Body: raw, Error: msg}
}

var errCredentialMissing = errors.New("credential file missing")

// readCredential reads the bearer. The file must be mode 0600 so a token
// another account can read is refused.
func readCredential(path string) (string, error) {
	fi, err := os.Stat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return "", errCredentialMissing
	}
	if err != nil {
		return "", errors.New("credential file empty or unreadable")
	}
	if fi.Mode().Perm()&0o077 != 0 {
		return "", errors.New("credential file not mode 0600")
	}
	raw, err := os.ReadFile(path)
	token := strings.TrimSpace(string(raw))
	if err != nil || token == "" {
		return "", errors.New("credential file empty or unreadable")
	}
	return token, nil
}

type grantRecord struct {
	ID, Name, Kind string
}

func grantRecords(ctx context.Context, relayBin string) ([]grantRecord, error) {
	out, err := exec.CommandContext(ctx, relayBin, "grant", "--json").Output()
	if err != nil {
		return nil, errors.New("relay grant failed")
	}
	var rs []grantRecord
	if err := json.Unmarshal(out, &rs); err != nil {
		return nil, errors.New("relay grant printed unreadable JSON")
	}
	return rs, nil
}

func findGrant(rs []grantRecord, name string) *grantRecord {
	for i := range rs {
		if rs[i].Name == name {
			return &rs[i]
		}
	}
	return nil
}
