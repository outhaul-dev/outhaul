package githook

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"time"
)

// RunHook is the client the `outhaul git-hook` subcommand runs inside a
// post-receive hook. It reads ref updates from stdin (git's post-receive
// format: "<old> <new> <ref>" lines), relays them to the bridge socket along
// with the app name, copies streamed progress to stdout, and returns the exit
// code the bridge sends. A dial/IO failure returns a non-nil error.
func RunHook(sockPath, app string, stdin io.Reader, stdout io.Writer) (int, error) {
	refs, err := parseStdinRefs(stdin)
	if err != nil {
		return 1, err
	}
	conn, err := net.DialTimeout("unix", sockPath, 5*time.Second)
	if err != nil {
		return 1, fmt.Errorf("connect to outhaul: %w", err)
	}
	defer conn.Close()

	if err := writeRequest(conn, request{App: app, Refs: refs}); err != nil {
		return 1, err
	}

	sc := bufio.NewScanner(conn)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	exit := 1
	sawExit := false
	for sc.Scan() {
		line := sc.Text()
		if code, ok := strings.CutPrefix(line, "\x00exit "); ok {
			if n, err := strconv.Atoi(strings.TrimSpace(code)); err == nil {
				exit = n
			}
			sawExit = true
			break
		}
		fmt.Fprintln(stdout, line)
	}
	if err := sc.Err(); err != nil {
		return exit, err
	}
	if !sawExit {
		return 1, fmt.Errorf("deploy stream ended without a status (outhaul may have restarted)")
	}
	return exit, nil
}

// parseStdinRefs reads git post-receive stdin ("<old> <new> <ref>" per line).
func parseStdinRefs(r io.Reader) ([]RefUpdate, error) {
	var out []RefUpdate
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) != 3 {
			continue
		}
		out = append(out, RefUpdate{Old: f[0], New: f[1], Ref: f[2]})
	}
	return out, sc.Err()
}
