# Long-running log streams

Outhaul's three log views — a deployment's build log, an app's runtime log, and
a database's runtime log — are Server-Sent Events. This page covers how they
stay open through a CDN, and what a reconnect does.

## Streams

Every stream sends the comment `: ping` and flushes it every 20 seconds. A
browser raises no event for a comment, so the page ignores it; it exists so a
CDN does not close a connection that has been quiet. A build step that prints
nothing for minutes therefore keeps the same `/logs` request open.

A reconnect clears the page and takes what the server replays:

- a build still running replays the log held so far, then live lines;
- a build that finished meanwhile replays the log, then `done`, and the page
  reloads to show the final status;
- a build whose log is gone — Outhaul restarted, or too many newer builds
  finished — gets `done` at once, and the page says the log is no longer kept.

Build logs replay at most 10,000 lines. Runtime logs replay the last 100 to
5,000 lines, as the page's tail choice asks. No line appears twice, and there
are no event ids.

When a stream breaks, a page that had a log keeps it on screen under the
`reconnecting…` label. If the browser keeps retrying by itself, the page lets
it. When the reply is not a stream at all — a 502, or the sign-in page once a
session has ended — the page retries with a 2-second to 30-second backoff. It
stops after five minutes with `disconnected. Reload the page to try again.`
A `done` or `err` event closes the stream for good.

## WebSockets

Outhaul's own pages use no WebSockets. An app's WebSockets pass through
Traefik, and a CDN that forwards WebSockets needs nothing from Outhaul.

## What only the real host can show

Items 1–3 of the shared list — the CDN's forwarded headers and a protected
app's sign-in — belong to the R1–R4 proxy-ingress document, which is not in
this tree yet. For streams:

4. A build step that prints nothing for four minutes: the log page stays
   `streaming…`, and the browser's network panel shows one `/logs` request for
   the whole build.
5. `systemctl restart outhaul` while a build log and a runtime log are open:
   both pages say `reconnecting…`, then show the log once, with no line twice.
6. An app that uses a WebSocket works through the CDN.
7. End the session in a second tab while a log page is open, then restart
   Outhaul: the page keeps its log under `reconnecting…`, retries at 2s→30s,
   ends with `disconnected. Reload the page to try again.` after five minutes,
   and reopens after signing in again.
8. Change an app's runtime tail or compose service and a database's tail: the
   box clears and shows the newly requested tail. Let a build finish with its
   page open: the page reloads and does not reconnect.
