-- Add password hash to users for username/password login.
CREATE TABLE IF NOT EXISTS migration_0010_user_password_hash_marker ();

ALTER TABLE users ADD COLUMN IF NOT EXISTS password_hash TEXT;
