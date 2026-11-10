-- Protected routes: a domain row can require sign-in through Outhaul's gate.
-- Every existing row stays open, and nothing redeploys on upgrade; the labels
-- that strip the identity header arrive with each app's next deploy.
ALTER TABLE domains ADD COLUMN protected INTEGER NOT NULL DEFAULT 0;

-- A gateway session is a sign-in on Outhaul's own host, made through /login or
-- the gate. Tokens are stored as SHA-256 hex only. subject names who signed in
-- (an account, or an SSO identity with no account) so their sessions can be
-- ended together; kind is the sign-in kind; display is what apps receive in
-- the identity header.
CREATE TABLE gate_sessions (
    token_hash TEXT PRIMARY KEY,
    subject    TEXT NOT NULL,
    kind       TEXT NOT NULL,
    display    TEXT NOT NULL,
    created_at TEXT NOT NULL,
    expires_at TEXT NOT NULL
);

-- A route session is the cookie for one protected host. It belongs to the
-- gateway session it was minted from, and ends with it.
CREATE TABLE route_sessions (
    token_hash      TEXT PRIMARY KEY,
    gate_token_hash TEXT NOT NULL REFERENCES gate_sessions(token_hash) ON DELETE CASCADE,
    host            TEXT NOT NULL,
    display         TEXT NOT NULL,
    expires_at      TEXT NOT NULL
);

-- A one-time code carries a gateway session to one protected host, once.
CREATE TABLE gate_codes (
    code_hash       TEXT PRIMARY KEY,
    gate_token_hash TEXT NOT NULL REFERENCES gate_sessions(token_hash) ON DELETE CASCADE,
    host            TEXT NOT NULL,
    expires_at      TEXT NOT NULL
);

CREATE INDEX idx_gate_sessions_subject ON gate_sessions(subject);
CREATE INDEX idx_route_sessions_gate ON route_sessions(gate_token_hash);
CREATE INDEX idx_gate_codes_gate ON gate_codes(gate_token_hash);
