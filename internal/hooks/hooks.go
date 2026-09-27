// Package hooks connects git's server-side hooks to the running onegit
// server. git runs `onegit hook <name>`, which forwards the ref updates to an
// internal HTTP endpoint so all policy lives in the server process.
package hooks

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

// Environment variables set by the server on every git process it spawns.
const (
	EnvInternalURL   = "ONEGIT_INTERNAL_URL"
	EnvInternalToken = "ONEGIT_INTERNAL_TOKEN"
	EnvPusherID      = "ONEGIT_PUSHER_ID"
	EnvPusherName    = "ONEGIT_PUSHER_NAME"
)

type RefUpdate struct {
	OldSHA string `json:"old"`
	NewSHA string `json:"new"`
	Ref    string `json:"ref"`
}

type Request struct {
	PusherID   int64       `json:"pusher_id"`
	PusherName string      `json:"pusher_name"`
	Updates    []RefUpdate `json:"updates"`
	// GitEnv carries quarantine object dirs so the server can inspect
	// objects that are not yet part of the repository during pre-receive.
	GitEnv      []string `json:"git_env"`
	PushOptions []string `json:"push_options"`
}

type Response struct {
	// Message is printed to the pusher (shown as "remote: ...").
	Message string `json:"message"`
	// Reject aborts the whole push (pre-receive only).
	Reject bool `json:"reject"`
}

// passthroughEnv is what the server needs from the hook's environment.
var passthroughEnv = []string{"GIT_OBJECT_DIRECTORY", "GIT_ALTERNATE_OBJECT_DIRECTORIES", "GIT_QUARANTINE_PATH"}

// RunHook is the entry point for `onegit hook <name>`.
func RunHook(name string, stdin io.Reader, stdout, stderr io.Writer) int {
	url, token := os.Getenv(EnvInternalURL), os.Getenv(EnvInternalToken)
	if url == "" || token == "" {
		// Push that did not come through onegit (e.g. an admin on the host).
		return 0
	}
	req := Request{PusherName: os.Getenv(EnvPusherName)}
	req.PusherID, _ = strconv.ParseInt(os.Getenv(EnvPusherID), 10, 64)
	for _, k := range passthroughEnv {
		if v, ok := os.LookupEnv(k); ok {
			req.GitEnv = append(req.GitEnv, k+"="+v)
		}
	}
	if n, _ := strconv.Atoi(os.Getenv("GIT_PUSH_OPTION_COUNT")); n > 0 {
		for i := 0; i < n; i++ {
			req.PushOptions = append(req.PushOptions, os.Getenv(fmt.Sprintf("GIT_PUSH_OPTION_%d", i)))
		}
	}
	sc := bufio.NewScanner(stdin)
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) == 3 {
			req.Updates = append(req.Updates, RefUpdate{OldSHA: f[0], NewSHA: f[1], Ref: f[2]})
		}
	}

	body, _ := json.Marshal(req)
	hreq, _ := http.NewRequest(http.MethodPost, url+"/internal/hook/"+name, bytes.NewReader(body))
	hreq.Header.Set("Authorization", "Bearer "+token)
	hreq.Header.Set("Content-Type", "application/json")
	client := &http.Client{Timeout: 5 * time.Minute}
	resp, err := client.Do(hreq)
	if err != nil {
		fmt.Fprintf(stderr, "onegit: hook %s failed: %v\n", name, err)
		return failCode(name)
	}
	defer resp.Body.Close()
	var r Response
	if resp.StatusCode != http.StatusOK || json.NewDecoder(resp.Body).Decode(&r) != nil {
		fmt.Fprintf(stderr, "onegit: hook %s: unexpected response %s\n", name, resp.Status)
		return failCode(name)
	}
	if r.Message != "" {
		fmt.Fprintln(stderr, strings.TrimRight(r.Message, "\n"))
	}
	if r.Reject {
		return 1
	}
	return 0
}

// A broken pre-receive must block the push; post-receive failures must not.
func failCode(name string) int {
	if name == "pre-receive" {
		return 1
	}
	return 0
}
