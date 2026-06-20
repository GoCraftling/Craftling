-- Marketplace → server creation: per-server template provenance, the resolved
-- environment, and the authoritative OCI image ref.
--
-- A server launched from a marketplace template carries the template's resolved
-- env (operator answers substituted into the manifest's env) and the exact image
-- the VM must boot. The agent prefers image_ref over its FC_IMAGE_REF default and
-- merges env over the image's baked-in environment. All three are NULL for
-- servers created the direct way (name + version), so the add is non-disruptive.

-- +goose Up
ALTER TABLE game_servers ADD COLUMN IF NOT EXISTS image_ref   TEXT;
ALTER TABLE game_servers ADD COLUMN IF NOT EXISTS env         JSONB;
ALTER TABLE game_servers ADD COLUMN IF NOT EXISTS template_id TEXT;

-- +goose Down
ALTER TABLE game_servers DROP COLUMN IF EXISTS template_id;
ALTER TABLE game_servers DROP COLUMN IF EXISTS env;
ALTER TABLE game_servers DROP COLUMN IF EXISTS image_ref;
