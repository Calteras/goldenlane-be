-- Customers can sign in with Google, Apple or email and password as well as a phone number,
-- so an account may have an email (verified by the provider) and no phone.
ALTER TABLE customers ADD COLUMN email text;
CREATE INDEX customers_email_idx ON customers (lower(email)) WHERE email IS NOT NULL;
