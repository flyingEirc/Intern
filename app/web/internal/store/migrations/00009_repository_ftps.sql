-- +goose Up
-- PostgreSQL alters the constraints atomically without rebuilding referenced rows.
ALTER TABLE repositories DROP CONSTRAINT repositories_tls_mode_check;
ALTER TABLE repositories DROP CONSTRAINT repositories_shape_check;
ALTER TABLE repositories ADD CONSTRAINT repositories_tls_mode_check
    CHECK (tls_mode IN ('none', 'plain', 'explicit', 'implicit'));
ALTER TABLE repositories ADD CONSTRAINT repositories_shape_check CHECK (
    (kind = 'local' AND tls_mode = 'none' AND secret_ciphertext IS NULL)
    OR (kind = 'ftp' AND tls_mode IN ('plain', 'explicit', 'implicit')
        AND secret_ciphertext IS NOT NULL AND octet_length(secret_ciphertext) > 0)
);

-- +goose Down
-- Existing FTPS rows refuse downgrade rather than losing TLS protection.
ALTER TABLE repositories DROP CONSTRAINT repositories_tls_mode_check;
ALTER TABLE repositories DROP CONSTRAINT repositories_shape_check;
ALTER TABLE repositories ADD CONSTRAINT repositories_tls_mode_check
    CHECK (tls_mode IN ('none', 'plain'));
ALTER TABLE repositories ADD CONSTRAINT repositories_shape_check CHECK (
    (kind = 'local' AND tls_mode = 'none' AND secret_ciphertext IS NULL)
    OR (kind = 'ftp' AND tls_mode = 'plain'
        AND secret_ciphertext IS NOT NULL AND octet_length(secret_ciphertext) > 0)
);
