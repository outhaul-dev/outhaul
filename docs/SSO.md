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

## Who can sign in

Everyone the provider lets sign in gets an Outhaul account the first time they sign
in to Outhaul. The account has no role (see below). Everyone the provider lets sign in
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

To turn SSO off, press "Remove SSO" in the panel. The button disappears from the login
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
