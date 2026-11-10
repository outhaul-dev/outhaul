package server

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"sort"
	"strings"

	"github.com/outhaul-dev/outhaul/internal/compose"
	"github.com/outhaul-dev/outhaul/internal/core"
	"github.com/outhaul-dev/outhaul/internal/docker"
	"github.com/outhaul-dev/outhaul/internal/store"
	"github.com/outhaul-dev/outhaul/internal/traefik"
)

// The "Require sign-in" switch on a domain row, and how a change to it is
// applied. Traefik learns a row's protection only from the labels a deploy
// puts on the app's containers, so a change on a running app queues a deploy
// at once, and Restart, which would bring a container back with its old
// labels, deploys instead whenever the labels and the rows disagree.
//
// The same holds for an open row on a host that requires sign-in (an open
// path under a protected host, on this app or another): when it is removed,
// or its host or path changes, the old container keeps answering its old
// route with no gate until a deploy replaces it.

// Why the switch is unavailable.
const (
	protectNeedsPublicURL = "Set OUTHAUL_PUBLIC_URL so people can be sent to sign in."
	protectNeedsHTTPS     = "Protected routes need HTTPS. Turn on HTTPS for the server, or use a tunnel."
	protectTunnelUnknown  = "Outhaul couldn't read its tunnel setting, so it can't tell whether visitors would reach this address over HTTPS."
)

// protectionAvailability reports whether d's switch can be turned on, and why
// not. Browsers are sent to the public URL to sign in, which Traefik publishes
// over HTTPS or through the tunnel, and visitors need HTTPS: on a TLS server a
// protected row is served over HTTPS whatever its own TLS flag
// (core.Domain.WantsTLS), and a tunnel's edge terminates TLS. A row on
// Outhaul's own host can't be protected (it can't be saved at all).
func (s *Server) protectionAvailability(ctx context.Context, d core.Domain) (ok bool, reason string) {
	if d.Host != "" && s.isOwnHost(d.Host) {
		return false, ownHostRefusal
	}
	if !s.publicURLSet() {
		return false, protectNeedsPublicURL
	}
	if s.tlsEnabled {
		return true, ""
	}
	tunnel, err := s.store.TunnelEnabled(ctx)
	if err != nil {
		log.Printf("protected routes: read the tunnel setting: %v", err)
		return false, protectTunnelUnknown
	}
	if !tunnel {
		return false, protectNeedsHTTPS
	}
	return true, ""
}

// appContainers returns the app's containers, stopped ones included: those
// labelled outhaul.app with its name. That is its routed containers, and a
// deploy's health-check container, which has no routers.
func (s *Server) appContainers(ctx context.Context, app core.App) ([]docker.Container, error) {
	return s.runtime.ListContainers(ctx, map[string]string{"outhaul.app": app.Name})
}

// rowGate reports whether any of containers defines router (has its rule
// label), and whether every one that does puts it behind its gate
// (traefik.RowHasGate). During a cutover two containers can define it, and a
// request may reach either.
func rowGate(containers []docker.Container, router string) (defined, gated bool) {
	rule := "traefik.http.routers." + router + ".rule"
	gated = true
	for _, c := range containers {
		if _, ok := c.Labels[rule]; !ok {
			continue
		}
		defined = true
		gated = gated && traefik.RowHasGate(c.Labels, router)
	}
	return defined, defined && gated
}

// protectionMismatch reports whether containers' labels disagree with any of
// the app's rows: a row is protected and some container serves it without the
// gate, or it isn't defined (an open row on its host may be serving its path);
// or a row is open and its router still has the gate; or a container serves,
// without the gate, a route the rows no longer have, on one of protectedHosts
// (staleOpenRoute). An open row no container defines yet isn't a mismatch:
// nothing was protected there.
func protectionMismatch(domains []core.Domain, appName string, containers []docker.Container, protectedHosts map[string]bool) bool {
	for _, d := range domains {
		defined, gated := rowGate(containers, traefik.RouterName(appName, d.ID))
		if !d.Protected && !defined {
			continue
		}
		if d.Protected != gated {
			return true
		}
	}
	return staleOpenRoute(domains, appName, containers, protectedHosts)
}

// staleOpenRoute reports whether any of containers routes, without the gate,
// a host and path that none of the app's rows has any more, on a host in
// protectedHosts (lower-cased): see staleOpenHosts.
func staleOpenRoute(domains []core.Domain, appName string, containers []docker.Container, protectedHosts map[string]bool) bool {
	for h := range staleOpenHosts(domains, appName, containers) {
		if protectedHosts[h] {
			return true
		}
	}
	return false
}

// staleOpenHosts returns the hosts, lower-cased, on which any of containers
// routes, without the gate, a host and path that none of the app's rows has
// any more (its row was removed, or its host or path changed). Only the app's
// own row routers count, and only when their host can be read from their
// rule.
func staleOpenHosts(domains []core.Domain, appName string, containers []docker.Container) map[string]bool {
	current := make(map[string]string, len(domains)) // router -> rule
	for _, d := range domains {
		current[traefik.RouterName(appName, d.ID)] = traefik.Rule(d.Host, d.Path)
	}
	hosts := map[string]bool{}
	const prefix, suffix = "traefik.http.routers.", ".rule"
	ours := strings.TrimSuffix(traefik.RouterName(appName, 0), "0") // a row's router, less its ID
	for _, c := range containers {
		for k, rule := range c.Labels {
			if !strings.HasPrefix(k, prefix) || !strings.HasSuffix(k, suffix) {
				continue
			}
			router := strings.TrimSuffix(strings.TrimSuffix(strings.TrimPrefix(k, prefix), suffix), "-tls")
			if !strings.HasPrefix(router, ours) {
				continue
			}
			if want, ok := current[router]; ok && want == rule {
				continue
			}
			if traefik.RowHasGate(c.Labels, router) {
				continue
			}
			if h := strings.ToLower(traefik.RuleHost(rule)); h != "" {
				hosts[h] = true
			}
		}
	}
	return hosts
}

// otherRedeploy is another app that a save requiring sign-in on Host
// redeploys: its running containers still serve an open route on Host that
// its rows no longer have, which would keep answering there with no gate.
type otherRedeploy struct {
	Host  string // lower-cased
	App   core.App
	Ships string // what its deploy builds (shipsWhat), or "" when it reuses its image
}

// Sentence is the warning the domain dialog shows for o.
func (o otherRedeploy) Sentence() string {
	s := "Requiring sign-in on " + o.Host + " also redeploys " + o.App.Name + ", which still serves an old route there."
	if o.Ships != "" {
		s += " It will deploy " + o.Ships + "."
	}
	return s
}

// key is how a form names o in others_was: its host and its app's name.
func (o otherRedeploy) key() string { return o.Host + " " + o.App.Name }

// leftoverRoutes lists the managed containers once, and returns every app
// other than app whose running containers serve an open route that its rows
// no longer have, once per host, sorted by host then app. A container whose
// app no longer exists is left out: there is no app to redeploy.
func (s *Server) leftoverRoutes(ctx context.Context, app core.App) ([]otherRedeploy, error) {
	cs, err := s.runtime.ListContainers(ctx, map[string]string{"outhaul.managed": "true"})
	if err != nil {
		return nil, fmt.Errorf("list the containers: %w", err)
	}
	running := map[string][]docker.Container{}
	for _, c := range cs {
		if name := c.Labels["outhaul.app"]; name != "" && name != app.Name && c.Running() {
			running[name] = append(running[name], c)
		}
	}
	if len(running) == 0 {
		return nil, nil
	}
	all, err := s.store.ListAllDomains(ctx)
	if err != nil {
		return nil, fmt.Errorf("read the domains: %w", err)
	}
	rows := map[string][]core.Domain{}
	for _, l := range all {
		rows[l.AppName] = append(rows[l.AppName], l.Domain)
	}
	var out []otherRedeploy
	for name, rc := range running {
		hosts := staleOpenHosts(rows[name], name, rc)
		if len(hosts) == 0 {
			continue
		}
		other, err := s.store.GetAppByName(ctx, name)
		if errors.Is(err, sql.ErrNoRows) {
			log.Printf("protected routes: a container of %q, which no longer exists, serves an old route", name)
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("read app %q: %w", name, err)
		}
		deps, err := s.store.ListDeploymentsForApp(ctx, other.ID)
		if err != nil {
			return nil, fmt.Errorf("read the deployments of %q: %w", name, err)
		}
		ships := ""
		if protectionWarningFor(other, deps) != "" {
			ships = shipsWhat(other)
		}
		for h := range hosts {
			out = append(out, otherRedeploy{Host: h, App: other, Ships: ships})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Host != out[j].Host {
			return out[i].Host < out[j].Host
		}
		return out[i].App.Name < out[j].App.Name
	})
	return out, nil
}

// openOnProtectedHost reports whether d is an open row on a host in
// protectedHosts (lower-cased): removing it, or changing its host or path,
// leaves its old route open on that host until the app redeploys.
func openOnProtectedHost(d core.Domain, protectedHosts map[string]bool) bool {
	return !d.Protected && protectedHosts[strings.ToLower(d.Host)]
}

// leavesOpenRoute is openOnProtectedHost for d, against the hosts the store
// has protected rows on now.
func (s *Server) leavesOpenRoute(ctx context.Context, d core.Domain) (bool, error) {
	if d.Protected {
		return false, nil
	}
	hosts, err := s.store.ProtectedHosts(ctx)
	if err != nil {
		return false, err
	}
	return openOnProtectedHost(d, hosts), nil
}

// chooseProtectionDeploy picks how to deploy app so its labels follow its
// rows, from its deployments, newest first. It returns the image to reuse and
// the deployment that built it, or "" for a normal deploy:
//
//  1. A Compose app has no per-deployment image: a normal deploy.
//  2. When the newest queued, building or deploying row reuses an image, the
//     same image, so that rollback isn't undone.
//  3. When it is a normal deploy, another normal deploy. Deployments run in
//     order, so reusing the live image would put the old code back after it.
//  4. The live (newest running) deployment's image, when it has one that
//     hasn't been pruned, and no newer deployment failed after building its
//     image: that one may have replaced the container before it failed (the
//     cutover's own health check fails after the swap), so the running row's
//     image isn't known to be the live one.
//  5. Otherwise a normal deploy.
func chooseProtectionDeploy(app core.App, newestFirst []core.Deployment) (image string, builtBy int64) {
	if app.Kind == core.KindCompose {
		return "", 0
	}
	for _, d := range newestFirst {
		switch d.Status {
		case core.StatusQueued, core.StatusBuilding, core.StatusDeploying:
			if d.RollbackOf != 0 && d.Image != "" {
				return d.Image, d.RollbackOf
			}
			return "", 0
		}
	}
	for _, d := range newestFirst {
		if d.Status == core.StatusFailed && d.Image != "" {
			return "", 0
		}
		if d.Status != core.StatusRunning {
			continue
		}
		if d.Image == "" || d.ImagePruned {
			return "", 0
		}
		if d.RollbackOf != 0 {
			return d.Image, d.RollbackOf
		}
		return d.Image, d.ID
	}
	return "", 0
}

// queueProtectionDeploy queues the deploy chooseProtectionDeploy picks, in
// one store transaction so no other deployment of the app can be queued
// between the choice and the insert, and wakes the worker.
func (s *Server) queueProtectionDeploy(ctx context.Context, app core.App) (core.Deployment, error) {
	dep, _, err := s.enqueueProtectionDeploy(ctx, app, true)
	return dep, err
}

// enqueueProtectionDeploy is queueProtectionDeploy, except that when running
// is false it queues only if a deployment of the app is queued, building or
// deploying, decided in the same transaction. It reports whether it queued.
func (s *Server) enqueueProtectionDeploy(ctx context.Context, app core.App, running bool) (core.Deployment, bool, error) {
	dep, queued, err := s.store.EnqueueDeployment(ctx, app.ID, func(newestFirst []core.Deployment) store.DeployChoice {
		if !running && !deployInFlight(newestFirst) {
			return store.DeployChoice{Skip: true}
		}
		image, builtBy := chooseProtectionDeploy(app, newestFirst)
		return store.DeployChoice{Image: image, RollbackOf: builtBy}
	})
	if err != nil || !queued {
		return core.Deployment{}, false, err
	}
	s.deployer.Notify()
	return dep, true, nil
}

// deployInFlight reports whether any of deps is queued, building or deploying.
func deployInFlight(deps []core.Deployment) bool {
	for _, d := range deps {
		switch d.Status {
		case core.StatusQueued, core.StatusBuilding, core.StatusDeploying:
			return true
		}
	}
	return false
}

// applyProtection queues the deploy that applies a protection change, when
// the app is running or a deployment of it is in flight: that deployment read
// the rows when it started, so the change needs another behind it (rule 3 in
// chooseProtectionDeploy). A stopped app with nothing in flight, or one never
// deployed, queues nothing: its next deploy carries the change, and Restart
// deploys instead of starting a container whose labels disagree
// (handleRestartApp).
func (s *Server) applyProtection(ctx context.Context, app core.App) (*core.Deployment, error) {
	running, err := s.appRunning(ctx, app)
	if err != nil {
		return nil, err
	}
	dep, queued, err := s.enqueueProtectionDeploy(ctx, app, running)
	if err != nil || !queued {
		return nil, err
	}
	return &dep, nil
}

// appRunning reports whether any of the app's containers is running. A
// Compose stack counts by its project, so a stack with no routes yet counts.
func (s *Server) appRunning(ctx context.Context, app core.App) (bool, error) {
	cs, err := s.appContainers(ctx, app)
	if err != nil {
		return false, err
	}
	if app.Kind == core.KindCompose {
		stack, err := s.runtime.ListContainers(ctx, map[string]string{"com.docker.compose.project": compose.ProjectName(app.Name)})
		if err != nil {
			return false, err
		}
		cs = append(cs, stack...)
	}
	for _, c := range cs {
		if c.Running() {
			return true, nil
		}
	}
	return false, nil
}

// protectionWarning names the code a protection deploy of app would ship when
// it builds rather than reusing the live image, or is "" when it reuses one.
// A template app redeploys its own snapshot, so it has no warning. When the
// deployments can't be read, it warns.
func (s *Server) protectionWarning(ctx context.Context, app core.App) string {
	deps, err := s.store.ListDeploymentsForApp(ctx, app.ID)
	if err != nil {
		log.Printf("protected routes: list deployments of %q: %v", app.Name, err)
		return buildWarning(app)
	}
	return protectionWarningFor(app, deps)
}

// protectionWarningFor is protectionWarning for app's deployments, newest first.
func protectionWarningFor(app core.App, newestFirst []core.Deployment) string {
	if app.Source == core.SourceTemplate {
		return ""
	}
	if image, _ := chooseProtectionDeploy(app, newestFirst); image != "" {
		return ""
	}
	return buildWarning(app)
}

// buildWarning names what a normal deploy of app builds (shipsWhat).
func buildWarning(app core.App) string {
	return "This will deploy " + shipsWhat(app) + "."
}

// shipsWhat is what a normal deploy of app builds: the newest matching tag in
// tag mode, otherwise its branch's head.
func shipsWhat(app core.App) string {
	if app.DeployTrigger == core.TriggerTag {
		pattern := app.TagPattern
		if pattern == "" {
			pattern = core.DefaultTagPattern
		}
		return "the newest tag matching " + pattern
	}
	branch := app.Branch
	if branch == "" {
		branch = "main"
	}
	return "the latest code from " + branch
}

// What a domain row shows in its Access column.
const (
	accessSignIn   = "Sign-in required"
	accessOpen     = "Open"
	accessNotYet   = "Not protected yet"
	accessUnknown  = "Unknown"
	accessNextNote = "applies on the next deploy"
)

// accessState is one row's Access cell. DeploymentID, when set, is the
// deployment the cell's note is about, to link to.
type accessState struct {
	Label, Class, Note string
	DeploymentID       int64
}

// unknownAccess is the cell when Docker can't be asked: it is never
// "Sign-in required" or "Open", because neither is known.
func unknownAccess() accessState {
	return accessState{Label: accessUnknown, Class: "state-queued", Note: "Docker can't be reached"}
}

// accessStateFor says what Traefik is doing for d, from the labels on the
// app's running containers, not from the stored setting, so the page can't
// disagree with the proxy. A stopped container serves nothing and doesn't
// count. A row no running container defines counts as lacking the gate when
// it is protected: an open row above it may already be serving that path.
// latest is the app's newest deployment, named in the note when a protected
// row lacks the gate; it may be nil.
func accessStateFor(d core.Domain, appName string, containers []docker.Container, latest *core.Deployment) accessState {
	var running []docker.Container
	for _, c := range containers {
		if c.Running() {
			running = append(running, c)
		}
	}
	if len(running) == 0 {
		if d.Protected {
			return accessState{Label: accessSignIn, Class: "state-running", Note: accessNextNote}
		}
		return accessState{Label: accessOpen, Class: "state-stopped", Note: accessNextNote}
	}
	_, gated := rowGate(running, traefik.RouterName(appName, d.ID))
	switch {
	case d.Protected && gated:
		return accessState{Label: accessSignIn, Class: "state-running"}
	case d.Protected:
		st := accessState{Label: accessNotYet, Class: "state-building"}
		if latest != nil {
			switch latest.Status {
			case core.StatusQueued, core.StatusBuilding, core.StatusDeploying:
				st.Note, st.DeploymentID = "redeploying", latest.ID
			case core.StatusFailed:
				st.Class, st.Note, st.DeploymentID = "state-failed", "the last deploy failed", latest.ID
			}
		}
		return st
	case gated:
		return accessState{Label: accessSignIn, Class: "state-running", Note: "until the redeploy finishes"}
	}
	return accessState{Label: accessOpen, Class: "state-stopped"}
}
