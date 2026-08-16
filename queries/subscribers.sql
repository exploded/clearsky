-- name: GetSubscriberByEmail :one
SELECT * FROM subscribers WHERE email = ?;

-- name: GetSubscriberByToken :one
SELECT * FROM subscribers WHERE token = ?;

-- name: CreateSubscriber :exec
INSERT INTO subscribers (email, discord_webhook, token, last_sent_at, created_at, updated_at)
VALUES (?, ?, ?, ?, ?, ?);

-- name: RefreshPendingSubscriber :exec
-- A re-submission for an address that has not confirmed yet: take the newest webhook,
-- rotate the token so the older confirmation link stops working, and restamp last_sent_at.
UPDATE subscribers
SET discord_webhook = ?, token = ?, last_sent_at = ?, updated_at = ?
WHERE email = ? AND confirmed_at IS NULL;

-- name: TouchSubscriberSent :exec
UPDATE subscribers SET last_sent_at = ?, updated_at = ? WHERE id = ?;

-- name: ConfirmSubscriber :execresult
UPDATE subscribers SET confirmed_at = ?, updated_at = ?
WHERE token = ? AND confirmed_at IS NULL;

-- name: DeleteSubscriberByToken :execresult
DELETE FROM subscribers WHERE token = ?;

-- name: ListConfirmedSubscribers :many
SELECT * FROM subscribers WHERE confirmed_at IS NOT NULL ORDER BY id;

-- name: CountSubscribers :one
SELECT COUNT(*) FROM subscribers;

-- name: PurgeStalePending :exec
-- Sign-ups that never confirmed. Purged so an abused form cannot fill the table.
-- updated_at, not created_at: a re-submission rotates the token and restarts the clock.
DELETE FROM subscribers WHERE confirmed_at IS NULL AND updated_at < ?;

-- name: CountConfirmedSubscribers :one
SELECT COUNT(*) FROM subscribers WHERE confirmed_at IS NOT NULL;
