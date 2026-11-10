package server

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"sort"
	"strconv"
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

// protectedOverHTTPS reports whether a protected row is served over HTTPS
// whatever its own TLS flag: on a TLS server (core.Domain.WantsTLS), or
// through a tunnel, whose edge terminates TLS. With neither, nothing serves
// it over HTTPS. Pages use it for a protected row's link and badge.
func (s *Server) protectedOverHTTPS(ctx context.Context) bool {
	if s.tlsEnabled {
		return true
	}
	tunnel, err := s.store.TunnelEnabled(ctx)
	if err != nil {
		log.Printf("protected routes: read the tunnel setting: %v", err)
		return false
	}
	return tunnel
}

// answers reports whether c serves its routes, or is about to: it is running,
// or Docker is restarting it, after which it serves what its labels say.
func answers(c docker.Container) bool {
	return c.Running() || c.State == "restarting"
}

// appContainers returns the app's containers, stopped ones included: those
// labelled outhaul.app with its name. That is its routed containers, and a
// deploy's health-check container, which has no routers.
func (s *Server) appContainers(ctx context.Context, app core.App) ([]docker.Container, error) {
	return s.runtime.ListContainers(ctx, map[string]string{"outhaul.app": app.Name})
}

// sharedHostRefusal answers someone who isn't a system admin putting a route
// on, or requiring sign-in on, an address another project uses.
const sharedHostRefusal = "Another project already uses this address, so a system admin has to set this up."

// sharedHostRefused reports whether the person making r may not, for an app
// in projectID, put a route on host (onto) or require sign-in there
// (protecting). A sign-in holds for a whole host, so an app with a route on a
// host where another project's app requires sign-in receives that sign-in's
// cookie from everyone who visits. Only a system admin may bring two projects
// together on such a host; anyone else is refused a route on a host where an
// app in another project has a protected row, and sign-in on a host where an
// app in another project has any row.
func (s *Server) sharedHostRefused(r *http.Request, projectID int64, host string, onto, protecting bool) (bool, error) {
	if !onto && !protecting {
		return false, nil
	}
	if u, ok := userFrom(r); ok && u.Role == core.RoleSystemAdmin {
		return false, nil
	}
	anyRow, protected, err := s.store.OtherProjectsOnHost(r.Context(), host, projectID)
	if err != nil {
		return false, err
	}
	return onto && protected || protecting && anyRow, nil
}

// takenText is err's text for the person making r. A route another app
// already serves (store.DomainTakenError) names that app only when they may
// see its project; otherwise it is "another app".
func (s *Server) takenText(r *http.Request, err error) string {
	var taken *store.DomainTakenError
	if !errors.As(err, &taken) {
		return err.Error()
	}
	if scope, serr := s.scopeFor(r); serr == nil && scope.allows(taken.ProjectID) {
		return err.Error()
	}
	return fmt.Sprintf("%v: %s%s is served by another app", store.ErrDomainTaken, taken.Host, taken.Path)
}

// rowGate reports whether any of containers defines d's router, the row's
// router on the row's address (its rule names d's host and path, in either
// spelling traefik.RuleAddress reads), and whether every one that does puts it
// behind its gate (traefik.RowHasGate). After d's host or path is edited, a
// container deployed before has the router on the old address: it doesn't
// serve d's address, so it doesn't define d. During a cutover two containers
// can define it, and a request may reach either.
func rowGate(containers []docker.Container, router string, d core.Domain) (defined, gated bool) {
	rule := "traefik.http.routers." + router + ".rule"
	gated = true
	for _, c := range containers {
		if r, ok := c.Labels[rule]; !ok || !traefik.SameAddress(r, d.Host, d.Path) {
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
		defined, gated := rowGate(containers, traefik.RouterName(appName, d.ID), d)
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
	current := make(map[string]core.Domain, len(domains)) // by router
	for _, d := range domains {
		current[traefik.RouterName(appName, d.ID)] = d
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
			if d, ok := current[router]; ok && traefik.SameAddress(rule, d.Host, d.Path) {
				continue
			}
			if traefik.RowHasGate(c.Labels, router) {
				continue
			}
			if h, _, ok := traefik.RuleAddress(rule); ok {
				hosts[strings.ToLower(h)] = true
			}
		}
	}
	return hosts
}

// otherRedeploy is another app that a save requiring sign-in redeploys. Either
// it is another app whose running containers still serve an open route on
// Host that its rows no longer have, which would keep answering there with no
// gate; or (Preview) it is a preview of the app being saved, which gets the
// same protection (protectPreviews) and redeploys if it is running.
//
// An app in a project the person saving can't see is Hidden: the dialog says
// only that an app in a project they don't share redeploys, and its form
// names it by an opaque value (hiddenAppKey), not its name or ID.
type otherRedeploy struct {
	Host    string // lower-cased; "" for a Preview
	App     core.App
	Ships   string // what its deploy builds (shipsWhat), or "" when it reuses its image
	Preview bool
	Hidden  bool
	opaque  string // its key when Hidden
}

// notShared is how the dialog and errors refer to a Hidden app.
const notShared = "an app in a project you don't share"

// Sentence is the warning the domain dialog shows for o.
func (o otherRedeploy) Sentence() string {
	if o.Hidden {
		return "Requiring sign-in on " + o.Host + " also redeploys " + notShared + "."
	}
	s := "Requiring sign-in on " + o.Host + " also redeploys " + o.App.Name + ", which still serves an old route there."
	if o.Preview {
		s = fmt.Sprintf("Requiring sign-in also requires it on this app's preview for pull request #%d (%s), which redeploys if it is running.", o.App.PRNumber, o.App.Name)
	}
	if o.Ships != "" {
		s += " It will deploy " + o.Ships + "."
	}
	return s
}

// label names o's app in an error: its name, unless o is Hidden.
func (o otherRedeploy) label() string {
	if o.Hidden {
		return notShared
	}
	return o.App.Name
}

// key is how a form names o in others_was: its host and its app's name; for a
// Hidden app, its host, "~" and its opaque value (an app name has no "~");
// or, for a preview, "preview:" and its name (a host has no colon).
func (o otherRedeploy) key() string {
	switch {
	case o.Preview:
		return "preview:" + o.App.Name
	case o.Hidden:
		return o.Host + " ~" + o.opaque
	}
	return o.Host + " " + o.App.Name
}

// hiddenAppKey is the opaque value a form names a Hidden app by: the first 16
// hex digits of an HMAC-SHA256 of its ID under a key this process made, so a
// page's value matches a save's, and says nothing of which app it is.
func (s *Server) hiddenAppKey(id int64) string {
	mac := hmac.New(sha256.New, s.formKey)
	mac.Write([]byte(strconv.FormatInt(id, 10)))
	return hex.EncodeToString(mac.Sum(nil))[:16]
}

// alsoLine is one line of the domain dialog's warning of the other apps a
// save redeploys, shown when the save requires sign-in on Host.
type alsoLine struct{ Host, Sentence string }

// alsoLines words others for the dialog, in order: each one's Sentence,
// except that the Hidden apps on a host make one line, which counts them.
func alsoLines(others []otherRedeploy) []alsoLine {
	var out []alsoLine
	hidden := map[string]int{}
	var hosts []string
	for _, o := range others {
		if !o.Hidden {
			out = append(out, alsoLine{o.Host, o.Sentence()})
			continue
		}
		if hidden[o.Host] == 0 {
			hosts = append(hosts, o.Host)
		}
		hidden[o.Host]++
	}
	for _, h := range hosts {
		s := "Requiring sign-in on " + h + " also redeploys " + notShared + "."
		if n := hidden[h]; n > 1 {
			s = fmt.Sprintf("Requiring sign-in on %s also redeploys %d apps in projects you don't share.", h, n)
		}
		out = append(out, alsoLine{h, s})
	}
	return out
}

// leftoverRoutes lists the managed containers once, and returns every app
// other than app whose running containers serve, on one of hosts
// (lower-cased), an open route that its rows no longer have: once per host,
// in the order of their keys (by host). Only those apps are read. A container whose app no
// longer exists is left out: there is no app to redeploy. An app in a project
// outside scope is Hidden, and errors don't name it.
func (s *Server) leftoverRoutes(ctx context.Context, app core.App, hosts map[string]bool, scope projectScope) ([]otherRedeploy, error) {
	if len(hosts) == 0 {
		return nil, nil
	}
	cs, err := s.runtime.ListContainers(ctx, map[string]string{"outhaul.managed": "true"})
	if err != nil {
		return nil, fmt.Errorf("list the containers: %w", err)
	}
	running := map[string][]docker.Container{}
	for _, c := range cs {
		if name := c.Labels["outhaul.app"]; name != "" && name != app.Name && answers(c) {
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
		var on []string
		for h := range staleOpenHosts(rows[name], name, rc) {
			if hosts[h] {
				on = append(on, h)
			}
		}
		if len(on) == 0 {
			continue
		}
		other, err := s.store.GetAppByName(ctx, name)
		if errors.Is(err, sql.ErrNoRows) {
			log.Printf("protected routes: a container of %q, which no longer exists, serves an old route", name)
			continue
		}
		if err != nil {
			log.Printf("protected routes: read app %q: %v", name, err)
			return nil, fmt.Errorf("read an app that still serves an old route there: %w", err)
		}
		o := otherRedeploy{App: other, Hidden: !scope.allows(other.ProjectID)}
		if o.Hidden {
			o.opaque = s.hiddenAppKey(other.ID)
		}
		deps, err := s.store.ListDeploymentsForApp(ctx, other.ID)
		if err != nil {
			log.Printf("protected routes: read the deployments of %q: %v", name, err)
			return nil, fmt.Errorf("read the deployments of %s: %w", o.label(), err)
		}
		if protectionWarningFor(other, deps, canonicalImage(other, rc)) != "" {
			o.Ships = shipsWhat(other)
		}
		for _, h := range on {
			o.Host = h
			out = append(out, o)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].key() < out[j].key() })
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

// previewProtection is what giving preview the protection of parentRows (its
// parent's rows) changes. Every open preview row with the service and path of
// a protected parent row is turned on, whatever its host: the row the preview
// manager minted from it, and any added by hand. So is the row on the host
// the preview manager minted for that service (mintedPreviewHost) and that
// path, whatever its service; when the preview has none there, one is added,
// protected, as the preview manager would mint it, so the route isn't left to
// an open row above it. A host added by hand never gains a path. Protection
// is only ever added: a parent row turned off leaves its previews as they
// are. Rows that collapse into one preview row are protected once.
func (s *Server) previewProtection(cfg core.PreviewConfig, parent core.App, parentRows []core.Domain, preview core.App, previewRows []core.Domain) (turnOn, add []core.Domain) {
	type route struct{ service, path string }
	type at struct{ host, path string } // host lower-cased
	protect := map[route]bool{}
	have := map[at]bool{}
	for _, r := range previewRows {
		have[at{strings.ToLower(r.Host), r.Path}] = true
	}
	minted := map[at]bool{}
	for _, p := range parentRows {
		if !p.Protected || protect[route{p.Service, p.Path}] {
			continue
		}
		protect[route{p.Service, p.Path}] = true
		host := s.mintedPreviewHost(cfg, parent, preview, p.Service, previewRows)
		k := at{strings.ToLower(host), p.Path}
		if minted[k] {
			continue
		}
		minted[k] = true
		if !have[k] {
			add = append(add, core.Domain{AppID: preview.ID, Host: host, Service: p.Service, Port: p.Port,
				Path: p.Path, InternalPath: p.InternalPath, TLS: p.TLS, Protected: true})
		}
	}
	for _, r := range previewRows {
		if !r.Protected && (protect[route{r.Service, r.Path}] || minted[at{strings.ToLower(r.Host), r.Path}]) {
			turnOn = append(turnOn, r)
		}
	}
	return turnOn, add
}

// mintedPreviewHost is the host the preview manager minted preview's rows of
// service on. That is the host it would mint now (core.PreviewHost) when one
// of rows has it; otherwise the first of rows whose first DNS label is that
// host's, which it minted under a base domain changed since, so the
// service's rows aren't split over two; otherwise the host it would mint now.
func (s *Server) mintedPreviewHost(cfg core.PreviewConfig, parent, preview core.App, service string, rows []core.Domain) string {
	now := core.PreviewHost(cfg, parent.Name, preview.PRNumber, service, s.serverIP)
	label, _, _ := strings.Cut(now, ".")
	found := ""
	for _, r := range rows {
		if strings.EqualFold(r.Host, now) {
			return r.Host
		}
		if l, _, _ := strings.Cut(r.Host, "."); found == "" && strings.EqualFold(l, label) {
			found = r.Host
		}
	}
	if found != "" {
		return found
	}
	return now
}

// previewRedeploy names preview in the domain dialog of its parent: a save
// requiring sign-in on the parent also requires it on the preview, and
// redeploys it by the same rules as any app. Its deploy builds the pull
// request's latest code unless it reuses its image.
func (s *Server) previewRedeploy(ctx context.Context, preview core.App) (otherRedeploy, error) {
	deps, err := s.store.ListDeploymentsForApp(ctx, preview.ID)
	if err != nil {
		return otherRedeploy{}, fmt.Errorf("read the deployments of %q: %w", preview.Name, err)
	}
	o := otherRedeploy{App: preview, Preview: true}
	if protectionWarningFor(preview, deps, s.servingImage(ctx, preview)) != "" {
		o.Ships = fmt.Sprintf("the latest code from pull request #%d", preview.PRNumber)
	}
	return o, nil
}

// previewRedeploys names every preview of app, for its domain dialog: which
// ones a save changes depends on the row being saved.
func (s *Server) previewRedeploys(ctx context.Context, app core.App) ([]otherRedeploy, error) {
	kids, err := s.store.ListPreviewsForParent(ctx, app.ID)
	if err != nil {
		return nil, fmt.Errorf("read the previews: %w", err)
	}
	out := make([]otherRedeploy, 0, len(kids))
	for _, k := range kids {
		o, err := s.previewRedeploy(ctx, k)
		if err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, nil
}

// previewsToProtect returns the previews of app that protectPreviews would
// change or redeploy once app's rows are rows: those a save leaving rows
// protected also protects, and redeploys. A preview whose containers can't be
// read is counted in, so the save names it rather than not.
func (s *Server) previewsToProtect(ctx context.Context, app core.App, rows []core.Domain) ([]otherRedeploy, error) {
	kids, err := s.store.ListPreviewsForParent(ctx, app.ID)
	if err != nil || len(kids) == 0 {
		return nil, err
	}
	cfg, err := s.store.GetPreviewConfig(ctx, app.ID)
	if err != nil {
		return nil, err
	}
	var out []otherRedeploy
	for _, k := range kids {
		have, err := s.store.ListDomains(ctx, k.ID)
		if err != nil {
			return nil, err
		}
		if turnOn, add := s.previewProtection(cfg, app, rows, k, have); len(turnOn)+len(add) == 0 {
			stale, err := s.previewMismatch(ctx, k, have)
			if err != nil {
				log.Printf("protected routes: check the containers of %q: %v", k.Name, err)
				stale = true
			}
			if !stale {
				continue
			}
		}
		o, err := s.previewRedeploy(ctx, k)
		if err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, nil
}

// protectPreviews gives each preview of parent the protection of parent's
// rows (previewProtection), fork previews included, and redeploys each one
// whose rows changed, or whose containers still disagree with its rows, by
// the same rules as any app (applyProtection). It carries on past a preview
// it can't change, and returns what failed, naming each preview.
func (s *Server) protectPreviews(ctx context.Context, parent core.App) error {
	kids, err := s.store.ListPreviewsForParent(ctx, parent.ID)
	if err != nil {
		return fmt.Errorf("read the previews: %w", err)
	}
	if len(kids) == 0 {
		return nil
	}
	rows, err := s.store.ListDomains(ctx, parent.ID)
	if err != nil {
		return fmt.Errorf("read the domains: %w", err)
	}
	cfg, err := s.store.GetPreviewConfig(ctx, parent.ID)
	if err != nil {
		return fmt.Errorf("read the preview settings: %w", err)
	}
	var errs []error
	for _, k := range kids {
		if err := s.protectPreview(ctx, cfg, parent, rows, k); err != nil {
			errs = append(errs, fmt.Errorf("%s (%w)", k.Name, err))
		}
	}
	return errors.Join(errs...)
}

// protectPreview is protectPreviews for one preview. When its rows already
// have the protection, it is still redeployed if its containers serve them
// otherwise (previewMismatch): the deploy an earlier save asked for may not
// have been queued.
func (s *Server) protectPreview(ctx context.Context, cfg core.PreviewConfig, parent core.App, parentRows []core.Domain, preview core.App) error {
	have, err := s.store.ListDomains(ctx, preview.ID)
	if err != nil {
		return fmt.Errorf("read its domains: %w", err)
	}
	turnOn, add := s.previewProtection(cfg, parent, parentRows, preview, have)
	if len(turnOn)+len(add) == 0 {
		stale, err := s.previewMismatch(ctx, preview, have)
		if err != nil {
			return fmt.Errorf("check whether its containers have the protection: %w", err)
		}
		if !stale {
			return nil
		}
		return s.applyPreviewProtection(ctx, preview)
	}
	var errs []error
	changed := false
	for _, d := range turnOn {
		if err := s.store.SetDomainProtected(ctx, preview.ID, d.ID, true); err != nil {
			errs = append(errs, fmt.Errorf("require sign-in on %s%s: %w", d.Host, d.Path, err))
			continue
		}
		changed = true
	}
	for _, d := range add {
		if err := s.addPreviewRow(ctx, d); err != nil {
			errs = append(errs, fmt.Errorf("add %s%s, requiring sign-in: %w", d.Host, d.Path, err))
			continue
		}
		changed = true
	}
	if changed {
		s.refreshPreviewURL(ctx, preview)
		if err := s.applyPreviewProtection(ctx, preview); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// addPreviewRow adds d, a protected row, to its preview. The preview manager
// can have minted a row on d's host and path since the preview's rows were
// read, while it was making the preview: then that row is turned on instead.
func (s *Server) addPreviewRow(ctx context.Context, d core.Domain) error {
	_, addErr := s.store.AddDomain(ctx, d)
	if addErr == nil {
		return nil
	}
	have, err := s.store.ListDomains(ctx, d.AppID)
	if err != nil {
		return addErr
	}
	for _, r := range have {
		if strings.EqualFold(r.Host, d.Host) && r.Path == d.Path {
			if r.Protected {
				return nil
			}
			return s.store.SetDomainProtected(ctx, d.AppID, r.ID, true)
		}
	}
	return addErr
}

// applyPreviewProtection is applyProtection for a preview, whose failure says
// to deploy it.
func (s *Server) applyPreviewProtection(ctx context.Context, preview core.App) error {
	if _, err := s.applyProtection(ctx, preview); err != nil {
		return fmt.Errorf("its deploy couldn't be queued, so deploy the preview to apply it: %w", err)
	}
	return nil
}

// previewMismatch reports whether preview's containers serve its rows other
// than they say (protectionMismatch). One with no containers has nothing to
// redeploy: its next deploy reads the rows.
func (s *Server) previewMismatch(ctx context.Context, preview core.App, rows []core.Domain) (bool, error) {
	cs, err := s.appContainers(ctx, preview)
	if err != nil || len(cs) == 0 {
		return false, err
	}
	hosts, err := s.store.ProtectedHosts(ctx)
	if err != nil {
		return false, err
	}
	return protectionMismatch(rows, preview.Name, cs, hosts), nil
}

// refreshPreviewURL moves the preview's OUTHAUL_PREVIEW_URL to https when the
// rule the pages link with says so for the row it names: a row that now
// requires sign-in may be served over HTTPS. Only the scheme changes, of a
// URL whose host is one of the preview's rows (the first of them on that
// host, as the preview manager links it); one naming none is left alone, and
// one already on https stays there. Its next deploy carries it. A failure is
// only logged: the route is protected either way.
func (s *Server) refreshPreviewURL(ctx context.Context, preview core.App) {
	rows, err := s.store.ListDomains(ctx, preview.ID)
	if err != nil {
		log.Printf("protected routes: read the domains of %q: %v", preview.Name, err)
		return
	}
	vars, err := s.store.ListEnv(ctx, preview.ID)
	if err != nil {
		log.Printf("protected routes: read the variables of %q: %v", preview.Name, err)
		return
	}
	for _, v := range vars {
		if v.Key != "OUTHAUL_PREVIEW_URL" {
			continue
		}
		rest, ok := strings.CutPrefix(v.Value, "http://")
		if !ok {
			continue
		}
		u, err := url.Parse(v.Value)
		if err != nil {
			continue
		}
		for _, d := range rows {
			if !strings.EqualFold(d.Host, u.Hostname()) {
				continue
			}
			if d.TLS || d.Protected && s.protectedOverHTTPS(ctx) {
				if err := s.store.SetEnvScoped(ctx, preview.ID, v.Key, "https://"+rest, v.IsSecret, v.Scope); err != nil {
					log.Printf("protected routes: set OUTHAUL_PREVIEW_URL of %q: %v", preview.Name, err)
				}
			}
			break
		}
	}
}

// protectionDeploy is how a change to what sign-in covers is applied
// (chooseProtectionDeploy).
type protectionDeploy struct {
	// wait: waitFor, a deployment that is queued and hasn't started, applies
	// it, so nothing more is queued.
	wait    bool
	waitFor core.Deployment
	image   string // the image to reuse, or "" for a normal deploy
	builtBy int64  // the deployment that built image
	builds  bool   // the deployment that applies it builds code (shipsWhat)
}

// chooseProtectionDeploy picks how to deploy app so its labels follow its
// rows, from its deployments, newest first, and serving, the image its
// canonical container was made from (canonicalImage):
//
//  1. When the newest queued, building or deploying deployment is queued, it
//     reads the rows when it starts, so it applies the change: wait for it.
//     Not one a tag push queued: it can skip itself when it starts.
//  2. A Compose app has no per-deployment image: a normal deploy.
//  3. When the newest deployment in flight has started and reuses an image,
//     the same image, so that rollback isn't undone. When it is a normal
//     deploy, another normal deploy: deployments run in order, so reusing the
//     live image would put the old code back after it.
//  4. The newest running deployment's image, when it hasn't been pruned and
//     it is the one serving. A deploy that failed since may have cut over
//     before it failed (the cutover's own health check fails after the swap),
//     so the running row alone doesn't say what serves; when it failed before
//     the cutover the old container still serves its image, whatever failed.
//  5. Otherwise a normal deploy.
func chooseProtectionDeploy(app core.App, newestFirst []core.Deployment, serving string) protectionDeploy {
	normal := protectionDeploy{builds: true}
	for _, d := range newestFirst {
		switch d.Status {
		case core.StatusQueued, core.StatusBuilding, core.StatusDeploying:
			if d.Status == core.StatusQueued && !d.FromTagEvent {
				return protectionDeploy{wait: true, waitFor: d, builds: d.Image == ""}
			}
			if app.Kind != core.KindCompose && d.RollbackOf != 0 && d.Image != "" {
				return protectionDeploy{image: d.Image, builtBy: d.RollbackOf}
			}
			return normal
		}
	}
	if app.Kind == core.KindCompose {
		return normal
	}
	for _, d := range newestFirst {
		if d.Status != core.StatusRunning {
			continue
		}
		if d.Image == "" || d.ImagePruned || d.Image != serving {
			return normal
		}
		if d.RollbackOf != 0 {
			return protectionDeploy{image: d.Image, builtBy: d.RollbackOf}
		}
		return protectionDeploy{image: d.Image, builtBy: d.ID}
	}
	return normal
}

// canonicalImage is the image app's canonical container was made from, among
// cs, running or not, or "" when it has none. Docker lists the reference a
// container was created from (the deploy's tag, outhaul/<app>:<deployment>,
// as the deployment row stores it) while that reference still names the same
// image, and the image's ID once it doesn't (the tag was removed or moved),
// which matches no row.
func canonicalImage(app core.App, cs []docker.Container) string {
	name := core.AppContainerName(app.Name)
	for _, c := range cs {
		if c.Name == name {
			return c.Image
		}
	}
	return ""
}

// servingImage is canonicalImage for app's containers, or "" (which reuses no
// image, so the dialog warns of a build) when they can't be listed.
func (s *Server) servingImage(ctx context.Context, app core.App) string {
	cs, err := s.appContainers(ctx, app)
	if err != nil {
		log.Printf("protected routes: list the containers of %q: %v", app.Name, err)
		return ""
	}
	return canonicalImage(app, cs)
}

// queueProtectionDeploy queues the deploy chooseProtectionDeploy picks, in
// one store transaction so no other deployment of the app can be queued
// between the choice and the insert, and wakes the worker. When a queued
// deployment will apply the change instead, it queues nothing and returns
// that one.
func (s *Server) queueProtectionDeploy(ctx context.Context, app core.App) (core.Deployment, error) {
	_, serving, err := s.liveState(ctx, app)
	if err != nil {
		return core.Deployment{}, err
	}
	dep, _, err := s.enqueueProtectionDeploy(ctx, app, true, serving)
	return dep, err
}

// enqueueProtectionDeploy is queueProtectionDeploy, given serving
// (canonicalImage), except that when running is false it queues only if a
// deployment of the app is queued, building or deploying, decided in the same
// transaction. It reports whether it queued.
func (s *Server) enqueueProtectionDeploy(ctx context.Context, app core.App, running bool, serving string) (core.Deployment, bool, error) {
	var waiting core.Deployment
	dep, queued, err := s.store.EnqueueDeployment(ctx, app.ID, func(newestFirst []core.Deployment) store.DeployChoice {
		if !running && !deployInFlight(newestFirst) {
			return store.DeployChoice{Skip: true}
		}
		c := chooseProtectionDeploy(app, newestFirst, serving)
		if c.wait {
			waiting = c.waitFor
			return store.DeployChoice{Skip: true}
		}
		return store.DeployChoice{Image: c.image, RollbackOf: c.builtBy}
	})
	if err != nil {
		return core.Deployment{}, false, err
	}
	if !queued {
		return waiting, false, nil
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
// the app is running or a deployment of it is in flight (rule 1 in
// chooseProtectionDeploy says when that deployment applies it itself). A
// stopped app with nothing in flight, or one never deployed, queues nothing:
// its next deploy carries the change, and Restart deploys instead of starting
// a container whose labels disagree (handleRestartApp).
func (s *Server) applyProtection(ctx context.Context, app core.App) (*core.Deployment, error) {
	running, serving, err := s.liveState(ctx, app)
	if err != nil {
		return nil, err
	}
	dep, queued, err := s.enqueueProtectionDeploy(ctx, app, running, serving)
	if err != nil || !queued {
		return nil, err
	}
	return &dep, nil
}

// liveState reads app's containers: whether any of them is running, or
// restarting (answers), a Compose stack counting by its project, so a stack
// with no routes yet counts; and the image its canonical container was made
// from (canonicalImage).
func (s *Server) liveState(ctx context.Context, app core.App) (running bool, serving string, err error) {
	cs, err := s.appContainers(ctx, app)
	if err != nil {
		return false, "", err
	}
	serving = canonicalImage(app, cs)
	if app.Kind == core.KindCompose {
		stack, err := s.runtime.ListContainers(ctx, map[string]string{"com.docker.compose.project": compose.ProjectName(app.Name)})
		if err != nil {
			return false, "", err
		}
		cs = append(cs, stack...)
	}
	for _, c := range cs {
		if answers(c) {
			return true, serving, nil
		}
	}
	return false, serving, nil
}

// protectionWarning names the code the deployment that applies a protection
// change to app would ship when it builds rather than reusing the image that
// serves, or is "" when it reuses one. A template app redeploys its own
// snapshot, so it has no warning. When the deployments or containers can't be
// read, it warns.
func (s *Server) protectionWarning(ctx context.Context, app core.App) string {
	deps, err := s.store.ListDeploymentsForApp(ctx, app.ID)
	if err != nil {
		log.Printf("protected routes: list deployments of %q: %v", app.Name, err)
		return buildWarning(app)
	}
	return protectionWarningFor(app, deps, s.servingImage(ctx, app))
}

// protectionWarningFor is protectionWarning for app's deployments, newest
// first, and serving (canonicalImage).
func protectionWarningFor(app core.App, newestFirst []core.Deployment, serving string) string {
	if app.Source == core.SourceTemplate || !chooseProtectionDeploy(app, newestFirst, serving).builds {
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
// app's running containers (restarting ones included: answers), not from the
// stored setting, so the page can't disagree with the proxy. A stopped
// container serves nothing and doesn't count. A row no running container
// defines counts as lacking the gate when it is protected: an open row above
// it may already be serving that path. An open row no running container
// defines is "Open", applying on the next deploy: it isn't served yet, or an
// old gated router on its former address still answers there. latest is the
// app's newest deployment, named in the note when a protected row lacks the
// gate; it may be nil.
func accessStateFor(d core.Domain, appName string, containers []docker.Container, latest *core.Deployment) accessState {
	var running []docker.Container
	for _, c := range containers {
		if answers(c) {
			running = append(running, c)
		}
	}
	if len(running) == 0 {
		if d.Protected {
			return accessState{Label: accessSignIn, Class: "state-running", Note: accessNextNote}
		}
		return accessState{Label: accessOpen, Class: "state-stopped", Note: accessNextNote}
	}
	defined, gated := rowGate(running, traefik.RouterName(appName, d.ID), d)
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
	case !defined:
		return accessState{Label: accessOpen, Class: "state-stopped", Note: accessNextNote}
	}
	return accessState{Label: accessOpen, Class: "state-stopped"}
}

// strayRoute looks among running, every running (or restarting: answers)
// container Outhaul manages,
// for a router that answers part of d's address without the gate: one that
// Traefik routes (its container has traefik.enable on), whose rule names d's
// host (in any case) and d's path or a path under it (PathPrefix compares
// paths as written, as plain prefixes), that isn't gated
// (traefik.RowHasGate), and that is neither d's own router on d's address
// (rowGate answers for that) nor a current open row's router on that row's
// address (rows: every app's rows, by router name). A rule it can't read
// counts when it names d's host. It returns the outhaul.app label of the
// container whose router it is, appName's own first.
func strayRoute(d core.Domain, appName string, running []docker.Container, rows map[string]core.Domain) (owner string, found bool) {
	const prefix, suffix = "traefik.http.routers.", ".rule"
	own := traefik.RouterName(appName, d.ID)
	names := strings.ToLower("Host(`" + d.Host + "`)")
	for _, c := range running {
		if !answers(c) || !routed(c.Labels) {
			continue
		}
		app := c.Labels["outhaul.app"]
		if found && app != appName {
			continue // one is found; only appName's own could be named first
		}
		for k, rule := range c.Labels {
			if !strings.HasPrefix(k, prefix) || !strings.HasSuffix(k, suffix) {
				continue
			}
			router := strings.TrimSuffix(strings.TrimSuffix(strings.TrimPrefix(k, prefix), suffix), "-tls")
			host, urlPath, ok := traefik.RuleAddress(rule)
			switch {
			case !ok && !strings.Contains(strings.ToLower(rule), names):
				continue
			case ok && (!strings.EqualFold(host, d.Host) || !strings.HasPrefix(urlPath, d.Path)):
				continue
			case ok && router == own && traefik.SameAddress(rule, d.Host, d.Path):
				continue
			}
			if r, current := rows[router]; current && !r.Protected && traefik.SameAddress(rule, r.Host, r.Path) {
				continue
			}
			if traefik.RowHasGate(c.Labels, router) {
				continue
			}
			owner, found = app, true
			if app == appName {
				return owner, true
			}
			break
		}
	}
	return owner, found
}

// routed reports whether Traefik routes a container with labels: Outhaul's
// Traefik exposes only containers whose traefik.enable is on. A value Go
// can't read as a boolean counts as on.
func routed(labels map[string]string) bool {
	v, ok := labels["traefik.enable"]
	if !ok {
		return false
	}
	on, err := strconv.ParseBool(v)
	return err != nil || on
}

// rowsByRouter is every app's rows, by router name, for strayRoute.
func rowsByRouter(all []core.DomainListing) map[string]core.Domain {
	m := make(map[string]core.Domain, len(all))
	for _, l := range all {
		m[traefik.RouterName(l.AppName, l.ID)] = l.Domain
	}
	return m
}

// Notes on a protected row another route answers part of (strayRoute).
const (
	strayOwnNote     = "an old route of this app still answers part of this address"
	strayAnotherNote = "another app still answers part of this address"
)

// strayState is the Access cell of a protected row whose address owner's
// container also answers without the gate (strayRoute), named as far as scope
// lets the viewer see owner's project. An owner that no longer exists is
// "another app".
func (s *Server) strayState(ctx context.Context, owner, appName string, scope projectScope) accessState {
	st := accessState{Label: accessNotYet, Class: "state-building", Note: strayAnotherNote}
	switch a, err := s.store.GetAppByName(ctx, owner); {
	case owner == appName:
		st.Note = strayOwnNote
	case err == nil && scope.allows(a.ProjectID):
		st.Note = owner + " still answers part of this address"
	}
	return st
}

// strayAccess replaces the Access cell of each of rows that is protected and
// reads "Sign-in required" in states (by row ID) when another route answers
// part of its address (strayRoute). It lists the managed containers and every
// app's rows only when there is such a row; when it can't, those rows are
// Unknown.
func (s *Server) strayAccess(r *http.Request, rows []core.Domain, appName string, states map[int64]accessState) {
	var check []core.Domain
	for _, d := range rows {
		if d.Protected && states[d.ID].Label == accessSignIn {
			check = append(check, d)
		}
	}
	if len(check) == 0 {
		return
	}
	ctx := r.Context()
	cs, err := s.runtime.ListContainers(ctx, map[string]string{"outhaul.managed": "true"})
	var all []core.DomainListing
	if err == nil {
		all, err = s.store.ListAllDomains(ctx)
	}
	if err != nil {
		log.Printf("protected routes: other routes on %q's addresses: %v", appName, err)
		for _, d := range check {
			states[d.ID] = unknownAccess()
		}
		return
	}
	byRouter := rowsByRouter(all)
	var scope *projectScope
	for _, d := range check {
		owner, found := strayRoute(d, appName, cs, byRouter)
		if !found {
			continue
		}
		if scope == nil {
			sc, err := s.scopeFor(r)
			if err != nil {
				log.Printf("protected routes: read the person's projects: %v", err)
				sc = projectScope{} // names no other app
			}
			scope = &sc
		}
		states[d.ID] = s.strayState(ctx, owner, appName, *scope)
	}
}
