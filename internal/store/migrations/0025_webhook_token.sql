-- The per-app webhook used one value as both its URL path segment and its HMAC
-- key: a sender configured with just the URL was always refused, and anyone who
-- saw the URL could sign. webhook_token now routes the URL; webhook_secret
-- verifies the delivery. Existing apps keep their current value as the secret
-- too, so senders already configured with it keep working; the app page can
-- regenerate the secret to separate them.
ALTER TABLE apps RENAME COLUMN webhook_secret TO webhook_token;
ALTER TABLE apps ADD COLUMN webhook_secret TEXT NOT NULL DEFAULT '';
UPDATE apps SET webhook_secret = webhook_token;

-- Preview children copied their parent's value; left in place they would also
-- answer the parent's webhook URL. Previews never use the generic webhook.
UPDATE apps SET webhook_token = '', webhook_secret = '' WHERE ephemeral = 1;
