package server

import (
	"net/http"
	"strings"
	"time"

	"github.com/outhaul-dev/outhaul/internal/githttp"
)

// gitPlainHTTP reports whether r is known to have reached the edge over plain
// HTTP. Only when Traefik serves TLS itself and the tunnel is off does
// X-Forwarded-Proto: http mean that: Traefik never writes http there of its
// own accord then, so a trusted proxy in front said so. Anywhere else (the
// tunnel says http for people on HTTPS) it can't be told, and a setting that
// can't be read counts as can't be told. A chain of proxies may send a list;
// its first entry is the hop nearest the client, and only http there counts.
func (s *Server) gitPlainHTTP(r *http.Request) bool {
	if !s.tlsEnabled {
		return false
	}
	first, _, _ := strings.Cut(r.Header.Get("X-Forwarded-Proto"), ",")
	if !strings.EqualFold(strings.TrimSpace(first), "http") {
		return false
	}
	tunnel, err := s.store.TunnelEnabled(r.Context())
	return err == nil && !tunnel
}

// gitHandler is the git over HTTPS transport, built once.
func (s *Server) gitHandler() *githttp.Handler {
	if s.git != nil {
		return s.git
	}
	repos := s.repos
	bridgeReady := s.gitHTTP.BridgeReady
	s.git = githttp.New(githttp.Options{
		Store:         s.store,
		Repos:         repos,
		PublicURL:     s.publicURL,
		MaxDays:       s.gitHTTP.MaxDays,
		SSOMaxAge:     s.gitHTTP.SSOMaxAge,
		Limiter:       githttp.NewLimiter(time.Now),
		ClientAddr:    s.gitHTTP.ClientAddr,
		OnPublicHost:  s.onPublicHost,
		PlainHTTP:     s.gitPlainHTTP,
		StreamContext: s.streamContext,
		Ready:         func() bool { return repos != nil && (bridgeReady == nil || bridgeReady()) },
	})
	return s.git
}

// gitRoutes are the three routes of git over HTTPS. Each request signs itself
// in with a push token, so they are public.
func (s *Server) gitRoutes() []route {
	g := s.gitHandler()
	return []route{
		{"GET /git/{repo}/info/refs", levelPublic, nil, g.InfoRefs},
		{"POST /git/{repo}/git-receive-pack", levelPublic, nil, g.Service},
		{"POST /git/{repo}/git-upload-pack", levelPublic, nil, g.Service},
	}
}

// isGitPost reports whether r is a POST to /git/{repo}/git-receive-pack or
// /git-upload-pack: a push, fetch or clone, whose body has no size limit.
func isGitPost(r *http.Request) bool {
	if r.Method != http.MethodPost {
		return false
	}
	rest, ok := strings.CutPrefix(r.URL.Path, "/git/")
	if !ok {
		return false
	}
	repo, service, ok := strings.Cut(rest, "/")
	return ok && repo != "" && (service == "git-receive-pack" || service == "git-upload-pack")
}
