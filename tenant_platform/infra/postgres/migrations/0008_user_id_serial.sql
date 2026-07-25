-- Fix: users.id was BIGINT PRIMARY KEY without a default, causing
-- INSERT INTO users (username, status) to fail with a not-null violation.
-- Add a sequence so the admin CreateUser endpoint can create non-dev users.

CREATE TABLE IF NOT EXISTS migration_0008_user_id_serial_marker ();

CREATE SEQUENCE IF NOT EXISTS users_id_seq AS BIGINT START 1000;

SELECT setval('users_id_seq', COALESCE((SELECT MAX(id) FROM users), 999) + 1, false);

ALTER TABLE users
    ALTER COLUMN id SET DEFAULT nextval('users_id_seq');
