# Signing in through an identity provider (SSO)

Outhaul can let people sign in through an OpenID Connect (OIDC) provider, next to
passwords. It follows the OIDC standard, so it is meant to work with providers such
as Okta, Keycloak, Authentik or Microsoft Entra ID, but it hasn't been tried with a
real provider yet: see [Testing so far](#testing-so-far). The UI calls it "SSO" and
never names a provider. When one is connected, the login page shows a "Sign in with
SSO" button above the password form.

A system admin sets it up on the Settings page, in the "Single sign-on (SSO)"
panel. Passwords keep working the whole time.

The same sign-in can also stand in front of a deployed app: see
[PROTECTED-ROUTES.md](PROTECTED-ROUTES.md). This page covers signing in to Outhaul
itself, and the roles described below.

## Before you start

SSO needs `OUTHAUL_PUBLIC_URL`, the public URL of the admin UI. The provider has to
send people back to a known address, and Outhaul builds it from this setting. Without
it the panel says so, and SSO counts as not connected: the button isn't shown, and
saving settings is refused. See [README.md](../README.md#configuration) for how to set it.

Register Outhaul at the provider with these values:

| Setting       | Value                                               |
| ------------- | --------------------------------------------------- |
| Redirect URI  | `OUTHAUL_PUBLIC_URL` + `/auth/sso/callback`, for example `https://outhaul.example.com/auth/sso/callback` |
| Application type | A web application with a client secret (confidential client) |
| Grant         | Authorization code, with PKCE                       |
| Scopes        | `openid email profile`                              |

The Settings panel shows the exact redirect URI for your install. The provider must
match it exactly.

The issuer URL must start with `https://`. `http://` is accepted only for
`localhost` and other loopback addresses, for trying it out on one machine.

## Worked example: Okta

These steps are written from Okta's documentation; they haven't been followed against
a real Okta organization yet. The names of the menus change now and then; the idea is
the same in any provider.

1. In the Okta admin console, go to Applications, then Create App Integration.
2. Choose "OIDC - OpenID Connect", then "Web Application".
3. Name it (for example "Outhaul"). Under Grant type keep "Authorization Code".
4. Set "Sign-in redirect URIs" to `https://outhaul.example.com/auth/sso/callback`
   (your own public URL).
5. Under Assignments, choose "Limited access to selected groups" and pick the group
   of people who may use Outhaul. See "Who can sign in" below before skipping this.
6. Save. Copy the Client ID and the Client secret from the application's page.
7. In Outhaul, open Settings and fill in the SSO panel:
   - Issuer URL: your Okta URL, for example `https://your-company.okta.com`. If you use
     a custom authorization server, use its issuer, for example
     `https://your-company.okta.com/oauth2/default`.
   - Client ID and Client secret: from step 6.
8. Press Connect.

On save, Outhaul contacts the provider (OIDC discovery) and refuses settings that
fail, with the reason. The client secret is stored encrypted and is never shown again.
To keep it when you edit other fields, leave the secret box empty.

## Connecting SSO from the environment

Instead of the Settings page, the provider can be connected from Outhaul's
environment, for a host that is rebuilt from a script. Three variables, all or none,
and `OUTHAUL_PUBLIC_URL` must be set too:

| Variable | Value |
| --- | --- |
| `OUTHAUL_SSO_ISSUER` | The issuer URL, held to the same rule as on Settings |
| `OUTHAUL_SSO_CLIENT_ID` | The client ID |
| `OUTHAUL_SSO_CLIENT_SECRET_FILE` | An absolute path to a file holding the client secret, or the name of a systemd credential |

Outhaul refuses to start if only some of the three are set, or if the issuer is set
without `OUTHAUL_PUBLIC_URL`.

While they are set, the environment wins. The Settings panel shows the issuer, the
client ID and the redirect URI as text, with no form and no Remove button, and saving
or removing SSO there is refused (HTTP 409). Settings stored earlier are kept, unused,
and are used again if you remove the variables.

The service runs as the `outhaul` user, which can't read a file only root can. systemd
can: it reads a credential as root when the unit starts, and gives the service a copy
only that unit's user can read. Keep the secret in a file owned by root with mode 0600,
and add a drop-in:

```ini
# /etc/systemd/system/outhaul.service.d/sso.conf
[Service]
LoadCredential=sso-client-secret:/etc/outhaul/sso-client-secret
```

```sh
# /etc/outhaul.env
OUTHAUL_SSO_ISSUER=https://idp.example.com
OUTHAUL_SSO_CLIENT_ID=<client id>
OUTHAUL_SSO_CLIENT_SECRET_FILE=sso-client-secret
```

A value that isn't an absolute path is the name of a credential, and has no `/`; `.` and
`..` are refused.
systemd tells the service where its credentials are with `CREDENTIALS_DIRECTORY`; if
that isn't set, Outhaul stops at startup and says so. The drop-in goes in
`/etc/systemd/system/outhaul.service.d/`, not in the unit file, because the installer
and the updater replace the unit file. An absolute path is read as it is, for a file the
`outhaul` user can read some other way.

- The secret is read once, at startup. It never enters the environment or the database,
  and it isn't logged or shown on a page.
- One trailing newline is dropped. An empty file, or one over 4096 bytes, stops startup.
- Replacing the file takes effect at the next restart.
- Outhaul doesn't wait for the provider at startup. A provider that can't be reached is
  logged, and Outhaul starts anyway; the first sign-in tries it again.

Outhaul remembers the issuer and client ID it last ran with. If either differs at
startup, from the environment or from Settings, every SSO sign-in is ended, protected
routes included, exactly as saving a new issuer on Settings does, and the log says so.
Password sessions are not affected. Nobody can be asked to confirm this when it comes
from the environment, so remember that a changed issuer makes everyone a new account
with no role (see [Accounts are not linked by email](#accounts-are-not-linked-by-email)),
unless the groups below give them one.

## Roles from groups

By default a new SSO account has no role and waits for a system admin. If the provider
sends the person's groups, Outhaul can give the role itself, at each sign-in to Outhaul.
This is off until you set `OUTHAUL_SSO_ROLE_GROUPS`.

| Variable | Meaning |
| --- | --- |
| `OUTHAUL_SSO_ROLE_GROUPS` | `role:group` pairs, separated by commas, such as `system_admin:outhaul-admins,project_member:engineering`. The role is `system_admin`, `project_admin`, `project_member` or `project_viewer`. The first colon splits the pair, so a group name may contain colons but not a comma. Spaces around each part are trimmed. A group maps to one role |
| `OUTHAUL_SSO_GROUPS_CLAIM` | The claim of the ID token that holds the groups. `groups` unless you name another. Needs `OUTHAUL_SSO_ROLE_GROUPS` |
| `OUTHAUL_SSO_SCOPES` | Extra scopes to request, separated by commas or spaces. None by default |

Some providers send the groups claim only when a scope such as `groups` is asked for,
and some refuse a scope they don't define, so Outhaul adds none of its own: the scopes
are `openid email profile` and whatever you list.

The rule:

- The highest role any of the person's groups maps to wins. Group names are compared
  exactly, including case.
- The groups come from the ID token only. The claim may be a list of names or one name.
  If there is no claim, or it is anything else, the person has no groups. A claim that
  only points at the groups, because there are too many to send, counts as no groups.
- It runs only when someone signs in to Outhaul itself. Signing in to a protected route
  creates no account and changes no role.

| The person's groups | The account's role came from | The role after sign-in |
| --- | --- | --- |
| Map to a role | Anything | That role. It is marked as from a group |
| Map to nothing | A group | No role. The mark is removed |
| Map to nothing | A system admin's hand, or the account is new | Unchanged |

Things to know:

- **A role set by hand lasts until the next sign-in** of someone in a mapped group, which
  sets the mapped role again, whether the hand-set one was higher or lower. The Users
  page says so beside the role of an account that came from a group. For these people the
  provider decides: to change their role, change their groups.
- **Removal shows at the next sign-in.** SSO sessions last 8 hours, so someone taken out
  of a group keeps their role until then. To cut them off at once, also delete the
  account on the Users page, which ends its sessions. Then, if the provider still lets
  them in, they get a new account at their next sign-in, with the role their groups map to.
- **`project_member` shares no project.** A role says what a person may do; which
  projects they see is set separately, on each project. A project member on no project
  sees none and can't create one. A project admin can create projects, and sees the ones
  they create, so map a group to `project_admin` to let people start on their own.
- **A `system_admin` mapping means whoever can change that group at the provider controls
  Outhaul's admins.** Keep that group small and protected.
- **Password accounts are never affected.** Only the SSO account for that issuer and
  subject changes, and the rule that the last password system admin can't be demoted or
  removed stands.
- One log line is written when a sign-in changes a role, with the account and the old and
  new role. Group names aren't logged.

Settings lists the mapping, the claim's name and the extra scopes, as text.

### Okta: sending groups

Written from Okta's documentation, and not followed against a real organization yet.

1. In the Okta admin console, open the Outhaul application, then Sign On, and edit
   the OpenID Connect ID Token section. (On a custom authorization server, add the claim
   under Security, API, the server, Claims instead.)
2. Add a claim named `groups`, included in the ID token always, with a "Matches regex"
   or "Starts with" filter that selects the groups Outhaul should see, for example
   `outhaul-`. A filter that matches only the Outhaul groups keeps the token small.
3. On a custom authorization server, make sure the `groups` scope exists, under Scopes.
4. In Outhaul, set `OUTHAUL_SSO_SCOPES=groups` and the mapping, for example
   `OUTHAUL_SSO_ROLE_GROUPS=system_admin:outhaul-admins,project_member:outhaul-staff`,
   and restart.

## Who can sign in

Everyone the provider lets sign in gets an Outhaul account the first time they sign
in to Outhaul. The account has no role, unless a group maps to one (see below). Everyone the provider lets sign in
also gets through every protected route, whether or not they have an account: signing
in to a protected route doesn't create one. See
[PROTECTED-ROUTES.md](PROTECTED-ROUTES.md#who-gets-through).

So limit who is assigned to the application at the provider, as in step 5 above. A
provider where anyone can sign in, such as public Google accounts, is not safe to use.

## First sign-in and roles

1. Someone presses "Sign in with SSO" and signs in at the provider.
2. Outhaul creates an account for them with no role. It can sign in, but it can't see
   any project.
3. A system admin opens the Users page, where accounts waiting for a role are listed
   first, and gives the account a role.

You can't give someone a role before their first sign-in, because the account doesn't
exist until then. The new role applies from their next request.

With `OUTHAUL_SSO_ROLE_GROUPS` set (see [Roles from groups](#roles-from-groups)), a
person in a mapped group arrives with that role and doesn't wait on the Users page. Anyone
in no mapped group still arrives with no role.

An SSO account never has an Outhaul password and can't sign in through the password
form. A password sign-in can't end up in an SSO account either.

### Accounts are not linked by email

An account is identified by the provider's issuer URL and the person's subject
(the provider's own permanent ID for them). Email is never used to match or link
accounts, so an SSO account and a password account with the same email stay separate.

Because the issuer is part of the identity, changing it makes everyone a new account
with no role. The old accounts keep their roles and push keys until you remove them
on the Users page. Outhaul asks you to confirm an issuer change for this reason.

### The name Outhaul shows

The name shown for an SSO account is the `email` claim the provider sends. If there
is no email, or the provider marks it unverified (`email_verified: false`), Outhaul
uses `preferred_username` instead. If the provider sends neither, the sign-in is
refused with a message saying so. The name is refreshed at each sign-in. Apps behind
a protected route receive the same name in the `X-Outhaul-User` header.

This is whatever the provider says. Don't let people change their own email or username
at the provider, or one person could appear under another's name in Outhaul.

## Keeping a password system admin

At least one system admin must be able to sign in with a password, and Outhaul won't let
you demote, delete or reset the last one. There is no command-line way to recover access,
and if the provider has an outage or is misconfigured, SSO accounts can't sign in at all.
A password system admin is how you get back in.

To make someone an SSO system admin, they sign in with SSO once, then a password
system admin changes their role on the Users page.

If the provider can't be reached, the login page says that SSO isn't available right
now, and the password form still works. Outhaul tries the provider again after a few
seconds, not on every attempt.

## Sessions and removing people

- An SSO sign-in lasts 8 hours. A password sign-in lasts 7 days.
- Removing someone at the provider stops them signing in again, but a session they
  already have lasts up to 8 hours.
- Removing them on the Users page in Outhaul ends their sessions at once and deletes their
  push keys. Removing them at the provider does neither of these, so the account and its
  keys stay in Outhaul until you remove it there.
- Removing them on the Users page also ends their sign-ins to protected routes, but while
  the provider still lets them sign in, they can sign in to a protected route again. To
  stop that, remove them at the provider.
- Changing the issuer or the client ID, or removing the SSO settings, ends every SSO
  sign-in, protected routes included, and ones in progress. Password sessions are not
  affected.
- Replacing only the client secret does not end any sign-in.
- The same holds for the environment: a changed issuer or client ID at startup ends every
  SSO sign-in. See [Connecting SSO from the environment](#connecting-sso-from-the-environment).

To turn SSO off, press "Remove SSO" in the panel. If SSO is connected from the
environment, remove the three variables instead and restart; any settings stored earlier
apply again, so press "Remove SSO" then too if you want SSO off. The button disappears from the login
page, and the SSO accounts remain on the Users page, but nobody can use them to sign in
until a provider is connected again.

## If it doesn't work

- The redirect URI at the provider must match the one on the Settings page character for
  character, including `https://` and no trailing slash.
- "Single sign-on needs `OUTHAUL_PUBLIC_URL`": set it in `/etc/outhaul.env` and restart
  Outhaul.
- Saving fails at discovery: the issuer URL must be the one the provider lists in its
  `/.well-known/openid-configuration` document, and Outhaul's server must be able to
  reach it. Each request to the provider times out after 10 seconds.
- "Your SSO account has no email address or username": the provider sent neither claim.
  Add the `email` or `preferred_username` claim to the application's profile or scopes
  at the provider.
- The provider's own error text is written to Outhaul's log, not shown on the login page.

## Testing so far

SSO has been tested against a test provider built into Outhaul's test suite, and
end to end through `deploy/test/integration_proxy.sh`: a real Outhaul and Traefik, a
stand-in CDN and a stand-in OIDC provider. It hasn't been run against a real
provider, a real CDN, or a real browser yet. The Okta steps above are written from
Okta's documentation. If your provider behaves differently from what's described
here, open an issue.
