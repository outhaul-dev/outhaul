package server

import (
	"context"
	"database/sql"
	"errors"
	"log"
	"sort"

	"github.com/outhaul-dev/outhaul/internal/docker"
)

// RedeployStale queues a deploy for every running app that has a routed
// container labelled under other routing settings, as a protection change
// would (applyProtection), and returns the apps it queued for. It returns an
// error, having queued nothing, when the containers can't be listed.
//
// Settings change only at a restart, so this runs once at startup. A stopped
// app is left alone: its next deploy carries the change. A container whose app
// no longer exists is named in a warning and left alone.
func (s *Server) RedeployStale(ctx context.Context) (apps []string, err error) {
	cs, err := s.runtime.ListContainers(ctx, map[string]string{"outhaul.managed": "true"})
	if err != nil {
		return nil, err
	}
	byApp := map[string][]docker.Container{}
	for _, c := range cs {
		if s.routing.Stale(c.Labels) {
			name := c.Labels["outhaul.app"]
			byApp[name] = append(byApp[name], c)
		}
	}
	for name, stale := range byApp {
		app, err := s.store.GetAppByName(ctx, name)
		if errors.Is(err, sql.ErrNoRows) {
			for _, c := range stale {
				log.Printf("ingress: container %s has routes from earlier settings and no app; remove it by hand", c.Name)
			}
			continue
		}
		if err != nil {
			return nil, err
		}
		dep, err := s.applyProtection(ctx, app)
		if err != nil {
			return nil, err
		}
		if dep == nil {
			continue
		}
		if dep.Image != "" {
			log.Printf("ingress: the routing settings changed; redeploying %s (deployment #%d, reusing image %s)", app.Name, dep.ID, dep.Image)
		} else {
			log.Printf("ingress: the routing settings changed; redeploying %s (deployment #%d, building the latest code)", app.Name, dep.ID)
		}
		apps = append(apps, app.Name)
	}
	sort.Strings(apps)
	log.Printf("ingress: %d app(s) redeployed for the routing settings", len(apps))
	return apps, nil
}

// anyStale reports whether any of cs was labelled under other routing
// settings.
func (s *Server) anyStale(cs []docker.Container) bool {
	for _, c := range cs {
		if s.routing.Stale(c.Labels) {
			return true
		}
	}
	return false
}

// served returns the containers Traefik isn't ignoring under the current
// constraint. With no header required, every container is served. Callers
// that answer "what does Traefik route" use this; liveState and the
// protection-mismatch checks keep seeing every container, so a stale one is
// still found and redeployed.
func (s *Server) served(cs []docker.Container) []docker.Container {
	if !s.routing.HeaderRequired() {
		return cs
	}
	out := make([]docker.Container, 0, len(cs))
	for _, c := range cs {
		if !s.routing.Unserved(c.Labels) {
			out = append(out, c)
		}
	}
	return out
}
