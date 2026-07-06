// Package githook is the deploy bridge behind a local unix socket. A per-app
// post-receive hook (installed by internal/gitrepo) relays each push to this
// socket; the bridge resolves-or-cold-creates the app, enqueues an ordinary
// deployment on the worker, streams the build/deploy log back to the hook (and
// thus to the `git push` terminal), and returns the terminal exit status.
package githook

import (
	"bufio"
	"fmt"
	"io"
	"strings"
)

// RefUpdate is one pushed ref as reported by git's post-receive stdin.
type RefUpdate struct {
	Old, New, Ref string
}

// request is what the hook sends: the app name and the pushed refs.
type request struct {
	App  string
	Refs []RefUpdate
}

// readRequest parses the hook's request framing from r.
func readRequest(r *bufio.Reader) (request, error) {
	var req request
	for {
		line, err := r.ReadString('\n')
		if err != nil && line == "" {
			return req, err
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			return req, nil
		}
		switch {
		case strings.HasPrefix(line, "app "):
			req.App = strings.TrimPrefix(line, "app ")
		case strings.HasPrefix(line, "ref "):
			f := strings.Fields(strings.TrimPrefix(line, "ref "))
			if len(f) != 3 {
				return req, fmt.Errorf("bad ref line %q", line)
			}
			req.Refs = append(req.Refs, RefUpdate{Old: f[0], New: f[1], Ref: f[2]})
		default:
			return req, fmt.Errorf("bad request line %q", line)
		}
	}
}

// writeRequest is the client side of readRequest (used by RunHook and tests).
func writeRequest(w io.Writer, req request) error {
	if _, err := fmt.Fprintf(w, "app %s\n", req.App); err != nil {
		return err
	}
	for _, u := range req.Refs {
		if _, err := fmt.Fprintf(w, "ref %s %s %s\n", u.Old, u.New, u.Ref); err != nil {
			return err
		}
	}
	_, err := io.WriteString(w, "\n")
	return err
}
