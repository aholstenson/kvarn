-- Previews can now come into being by being asked for: a request for a
-- hostname an auto-start pattern claims resolves to a pull request and boots
-- it. Both columns are about that origin — which pull request the preview is
-- of, and which hostname first asked for it.
ALTER TABLE previews ADD COLUMN pr TEXT NOT NULL DEFAULT '';
ALTER TABLE previews ADD COLUMN auto_start_host TEXT NOT NULL DEFAULT '';
