-- Development only (the migrate command refuses down migrations elsewhere).
ALTER TABLE mpesa_settings DROP COLUMN IF EXISTS callback_token_ciphertext;
