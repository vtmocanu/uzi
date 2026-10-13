-- +goose Up
ALTER TABLE repos
 ADD COLUMN code_cross_check_guidance text,
 ADD COLUMN cross_check_repo_instructions_enabled boolean NOT NULL DEFAULT false,
 ADD CONSTRAINT repos_code_cross_check_guidance_bound CHECK (
   code_cross_check_guidance IS NULL OR octet_length(code_cross_check_guidance) <= 8192);
ALTER TABLE cross_checks ADD CONSTRAINT cross_checks_code_input_bounds CHECK (
 stage <> 'code' OR (
   octet_length(guidance_snapshot) <= 8192
   AND octet_length(guidance_text) <= 8192
   AND guidance_snapshot = guidance_text
   AND octet_length(repo_instructions_text) <= 65536
   AND COALESCE(octet_length(code_context->>'issue_title') <= 4096, true)
   AND COALESCE(octet_length(code_context->>'issue_description') <= 262144, true)
   AND (head_commit IS NOT NULL OR (
     guidance_text = '' AND NOT repo_instructions_enabled
     AND repo_instructions_text = ''))
 ));
-- The existing enforce_code_cross_check_snapshot trigger retains its explicit
-- IS DISTINCT FROM comparisons for every frozen input.

-- +goose Down
ALTER TABLE cross_checks DROP CONSTRAINT cross_checks_code_input_bounds;
ALTER TABLE repos DROP CONSTRAINT repos_code_cross_check_guidance_bound,
 DROP COLUMN code_cross_check_guidance,
 DROP COLUMN cross_check_repo_instructions_enabled;
