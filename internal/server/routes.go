package server

import (
	"io/fs"
	"net/http"
)

// route is one line of the route table.
type route struct {
	pattern string
	level   accessLevel
	project projectResolver // set exactly when level.projectScoped()
	handler http.HandlerFunc
}

// routes is every route the server answers, and who may use it. Handler
// registers each one behind guard.
func (s *Server) routes() []route {
	sub, _ := fs.Sub(staticFS, "static")
	static := http.StripPrefix("/static/", http.FileServer(http.FS(sub)))

	routes := []route{
		{"GET /healthz", levelPublic, nil, s.handleHealthz},

		// Unauthenticated by design: the root certificate is public material and
		// devices need it before they can trust anything (including the login page).
		{"GET /ca.pem", levelPublic, nil, s.handleCARoot},

		{"GET /static/", levelPublic, nil, static.ServeHTTP},

		{"GET /setup", levelPublic, nil, s.handleSetupForm},
		{"POST /setup", levelPublic, nil, s.handleSetupSubmit},
		{"GET /account/setup", levelPublic, nil, s.handleAccountSetupForm},
		{"POST /account/setup", levelPublic, nil, s.handleAccountSetupSubmit},
		{"GET /account", levelAccount, nil, s.accountPage(s.handleAccount)},
		{"POST /account/password", levelAccount, nil, s.accountPage(s.handleChangePassword)},
		{"POST /account/push-keys", levelAccount, nil, s.accountPage(s.handleAddPushKey)},
		{"POST /account/push-keys/{id}/delete", levelAccount, nil, s.accountPage(s.handleDeletePushKey)},
		{"GET /login", levelPublic, nil, s.handleLoginForm},
		{"POST /login", levelPublic, nil, s.handleLoginSubmit},
		{"POST /logout", levelPublic, nil, s.handleLogout},
		// The browser comes back from the SSO provider with no Outhaul session;
		// the callback is tied to the browser that started it by the flow's
		// own cookie and state.
		{"GET /auth/sso/start", levelPublic, nil, s.handleSSOStart},
		{"GET /auth/sso/callback", levelPublic, nil, s.handleSSOCallback},

		// Traefik's forward auth asks this about every request to a protected
		// route, over plain HTTP on the admin port, with the visitor's cookies
		// for the app's host. It checks a route cookie or one-time code itself.
		// It is a GET, so cross-origin protection lets it by.
		{"GET /gate/check", levelPublic, nil, s.handleGateCheck},
		// Signing in and out for protected routes, on the public URL. A route
		// sign-in makes a gateway session, not an Outhaul session, so these
		// are open to anyone; every address and nonce is checked inside, and
		// the POSTs are covered by cross-origin protection.
		{"GET /gate/start", levelPublic, nil, s.handleGateStart},
		{"POST /gate/sign-in", levelPublic, nil, s.handleGateSignIn},
		{"GET /gate/sign-out", levelPublic, nil, s.handleGateSignOutPage},
		{"POST /gate/sign-out", levelPublic, nil, s.handleGateSignOut},

		{"GET /{$}", levelAnyRole, nil, s.handleOverview},

		{"GET /projects", levelAnyRole, nil, s.handleProjectsList},
		{"POST /projects", levelProjectAdminRole, nil, s.handleCreateProject},
		{"GET /projects/{id}", levelViewer, s.projectOfPath, s.handleProjectDetail},
		{"POST /projects/{id}/settings", levelMember, s.projectOfPath, s.handleProjectSettings},
		{"POST /projects/{id}/env", levelMember, s.projectOfPath, s.handleSetProjectEnv},
		{"POST /projects/{id}/env/delete", levelMember, s.projectOfPath, s.handleDeleteProjectEnv},
		{"POST /projects/{id}/delete", levelProjectAdmin, s.projectOfPath, s.handleDeleteProject},
		{"POST /projects/{id}/members", levelProjectAdmin, s.projectOfPath, s.handleAddProjectMember},
		{"POST /projects/{id}/members/{userID}/delete", levelProjectAdmin, s.projectOfPath, s.handleRemoveProjectMember},
		{"POST /projects/{id}/databases", levelMember, s.projectOfPath, s.handleCreateDatabase},

		{"GET /databases", levelAnyRole, nil, s.handleDatabasesList},
		{"GET /domains", levelAnyRole, nil, s.handleDomainsList},
		{"GET /volumes", levelSystemAdmin, nil, s.handleVolumesList},
		{"POST /volumes/reclaim", levelSystemAdmin, nil, s.handleReclaimVolume},

		{"GET /databases/{id}", levelViewer, s.projectOfDatabase, s.handleDatabaseDetail},
		{"GET /databases/{id}/logs", levelViewer, s.projectOfDatabase, s.handleDatabaseLogsSSE},
		{"POST /databases/{id}/start", levelMember, s.projectOfDatabase, s.handleStartDatabase},
		{"POST /databases/{id}/stop", levelMember, s.projectOfDatabase, s.handleStopDatabase},
		{"POST /databases/{id}/settings", levelMember, s.projectOfDatabase, s.handleDatabaseSettings},
		{"POST /databases/{id}/delete", levelMember, s.projectOfDatabase, s.handleDeleteDatabase},

		{"GET /templates", levelMemberRole, nil, s.handleTemplatesList},
		{"POST /templates/{id}", levelMember, s.projectOfForm, s.handleDeployTemplate},

		{"GET /apps", levelAnyRole, nil, s.handleAppsList},
		{"POST /apps", levelMember, s.projectOfForm, s.handleCreateApp},
		{"GET /apps/{id}", levelViewer, s.projectOfApp, s.handleAppDetail},
		{"GET /apps/{id}/logs", levelViewer, s.projectOfApp, s.handleRuntimeLogsSSE},
		{"GET /apps/{id}/stats", levelViewer, s.projectOfApp, s.handleAppStats},

		{"GET /metrics/sample", levelSystemAdmin, nil, s.handleMetricsSample},
		{"GET /metrics", levelSystemAdmin, nil, s.handleMetrics},

		{"POST /apps/{id}/deploy", levelMember, s.projectOfApp, s.handleDeploy},
		{"POST /apps/{id}/settings", levelMember, s.projectOfApp, s.handleAppSettings},
		{"POST /apps/{id}/limits", levelSystemAdmin, nil, s.handleAppLimits},
		{"POST /apps/{id}/kind", levelMember, s.projectOfApp, s.handleUpdateAppKind},
		{"POST /apps/{id}/source", levelMember, s.projectOfApp, s.handleUpdateAppSource},
		{"POST /apps/{id}/domains", levelMember, s.projectOfApp, s.handleAddDomain},
		{"POST /apps/{id}/domains/{domainID}", levelMember, s.projectOfApp, s.handleUpdateDomain},
		{"POST /apps/{id}/domains/{domainID}/delete", levelMember, s.projectOfApp, s.handleDeleteDomain},
		{"POST /apps/{id}/volumes", levelMember, s.projectOfApp, s.handleAddVolume},
		{"POST /apps/{id}/volumes/{volumeID}", levelMember, s.projectOfApp, s.handleUpdateVolume},
		{"POST /apps/{id}/volumes/{volumeID}/delete", levelMember, s.projectOfApp, s.handleDeleteVolume},
		{"POST /apps/{id}/env", levelMember, s.projectOfApp, s.handleSetEnv},
		{"POST /apps/{id}/env/delete", levelMember, s.projectOfApp, s.handleDeleteEnv},
		{"POST /apps/{id}/attachments", levelMember, s.projectOfApp, s.handleAttachDatabase},
		{"POST /apps/{id}/attachments/{attachmentID}/delete", levelMember, s.projectOfApp, s.handleDetachDatabase},
		{"POST /apps/{id}/webhook/secret", levelMember, s.projectOfApp, s.handleRegenerateWebhookSecret},
		{"POST /apps/{id}/previews", levelMember, s.projectOfApp, s.handleSavePreviewConfig},
		{"POST /apps/{id}/previews/{previewID}/destroy", levelMember, s.projectOfApp, s.handleDestroyPreview},
		{"POST /apps/{id}/stop", levelMember, s.projectOfApp, s.handleStopApp},
		{"POST /apps/{id}/restart", levelMember, s.projectOfApp, s.handleRestartApp},
		{"POST /apps/{id}/delete", levelMember, s.projectOfApp, s.handleDeleteApp},

		{"GET /deployments", levelPublic, nil, s.handleDeploymentsRedirect},
		{"GET /containers", levelSystemAdmin, nil, s.handleContainers},
		{"GET /deployments/{id}", levelViewer, s.projectOfDeployment, s.handleDeploymentDetail},
		{"GET /deployments/{id}/logs", levelViewer, s.projectOfDeployment, s.handleLogsSSE},
		{"POST /deployments/{id}/cancel", levelMember, s.projectOfDeployment, s.handleCancel},
		{"POST /deployments/{id}/rollback", levelMember, s.projectOfDeployment, s.handleRollback},

		{"POST /backups", levelMember, s.projectOfBackupForm, s.handleCreateBackup},
		{"POST /backups/{id}/run", levelMember, s.projectOfBackup, s.handleRunBackup},
		{"POST /backups/{id}/toggle", levelMember, s.projectOfBackup, s.handleToggleBackup},
		{"POST /backups/{id}/delete", levelMember, s.projectOfBackup, s.handleDeleteBackup},
		{"GET /backups/{id}/restore", levelMember, s.projectOfBackup, s.handleRestorePage},
		{"POST /backups/{id}/restore", levelMember, s.projectOfBackup, s.handleRestoreBackup},

		{"GET /users", levelSystemAdmin, nil, s.handleUsersList},
		{"POST /users", levelSystemAdmin, nil, s.handleCreateUser},
		{"POST /users/{id}/role", levelSystemAdmin, nil, s.handleSetUserRole},
		{"POST /users/{id}/reset", levelSystemAdmin, nil, s.handleResetUserPassword},
		{"POST /users/{id}/delete", levelSystemAdmin, nil, s.handleDeleteUser},

		{"GET /settings", levelSystemAdmin, nil, s.handleSettings},
		{"POST /settings/destinations", levelSystemAdmin, nil, s.handleCreateDestination},
		{"POST /settings/destinations/{id}/test", levelSystemAdmin, nil, s.handleTestDestination},
		{"POST /settings/destinations/{id}/delete", levelSystemAdmin, nil, s.handleDeleteDestination},
		{"POST /settings/state-backup/run", levelSystemAdmin, nil, s.handleRunStateBackup},
		{"POST /settings/restore/dismiss", levelSystemAdmin, nil, s.handleDismissRestoreNotice},
		{"POST /settings/ssh", levelSystemAdmin, nil, s.handleSetSSHAddr},
		{"POST /settings/tunnel/enable", levelSystemAdmin, nil, s.handleEnableTunnel},
		{"POST /settings/tunnel/disable", levelSystemAdmin, nil, s.handleDisableTunnel},
		{"POST /settings/git-sources/{id}/delete", levelSystemAdmin, nil, s.handleDeleteGitSource},
		{"POST /settings/sso", levelSystemAdmin, nil, s.handleSaveSSO},
		{"POST /settings/sso/delete", levelSystemAdmin, nil, s.handleDeleteSSO},
		{"POST /settings/update", levelSystemAdmin, nil, s.handleRequestUpdate},
		{"POST /settings/update/check", levelSystemAdmin, nil, s.handleCheckForUpdate},
		{"GET /settings/update/progress", levelSystemAdmin, nil, s.handleUpdateProgress},

		{"GET /github/connect", levelSystemAdmin, nil, s.handleGithubConnect},
		// callback and setup are called by GitHub directly (no session cookie); the
		// callback is guarded by the one-time CSRF state instead.
		{"GET /github/callback", levelPublic, nil, s.handleGithubCallback},
		{"GET /github/setup", levelPublic, nil, s.handleGithubSetup},

		// Webhook endpoints are called by GitHub/Gitea/GitLab directly (no session
		// cookie); each verifies its own HMAC signature or token instead.
		{"POST /webhooks/github", levelPublic, nil, s.handleGithubWebhook},
		{"POST /webhooks/app/{token}", levelPublic, nil, s.handleAppWebhook},
	}

	for _, p := range placeholderPages {
		routes = append(routes, route{"GET /" + p.active, levelSystemAdmin, nil, s.handlePlaceholder(p)})
	}
	if s.gitHTTPOn() {
		routes = append(routes,
			route{"POST /account/push-tokens", levelAccount, nil, s.accountPage(s.handleAddPushToken)},
			route{"POST /account/push-tokens/{id}/delete", levelAccount, nil, s.accountPage(s.handleDeletePushToken)},
			route{"POST /users/{id}/push-tokens/{tokenID}/delete", levelSystemAdmin, nil, s.handleAdminDeletePushToken},
		)
	}
	return routes
}

func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	// The commit is for the updater, which asks on 127.0.0.1. Traefik and
	// cloudflared come from the Docker bridge, so the public URL never
	// tells it.
	if s.build.Known() && fromLoopback(r) {
		w.Header().Set("X-Outhaul-Commit", s.build.Commit)
	}
	w.WriteHeader(http.StatusOK)
	w.Write([]byte("ok"))
}
