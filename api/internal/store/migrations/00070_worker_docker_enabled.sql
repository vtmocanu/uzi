-- +goose Up

-- Docker-capable hosted workers (PRD #83 M3). A hosted worker may opt into a
-- rootless-DinD sidecar so its agent can run `docker`/`docker compose`. This is a
-- new pod-shape DIMENSION, orthogonal to template (which stays base/jvm for image
-- selection) — Decision 1 keeps docker off the template axis, so it is a boolean on
-- the worker rather than a new template.
--
-- Nullable, not NOT NULL DEFAULT false, and that is deliberate: only HOSTED rows
-- carry a pod shape at all (an external worker the user runs by hand has no sidecar
-- for us to render), so leaving it NULL on external rows keeps "this column is
-- meaningless for this kind" expressible, exactly as hosted_size does. The
-- controller reads it as false-when-absent (COALESCE at the wire), so a NULL and an
-- explicit false render identically: no sidecar. No CHECK ties it to kind — an
-- external row with a stray false costs nothing, and the provision path is the only
-- writer that ever sets it.
ALTER TABLE workers ADD COLUMN docker_enabled boolean;

-- +goose Down
ALTER TABLE workers DROP COLUMN docker_enabled;
