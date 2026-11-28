DROP INDEX IF EXISTS customers_email_idx;
ALTER TABLE customers DROP COLUMN IF EXISTS email;
