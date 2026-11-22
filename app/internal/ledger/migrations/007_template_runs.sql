ALTER TABLE manifest_templates ADD COLUMN environment BLOB;

ALTER TABLE stacks ADD COLUMN template_name TEXT NOT NULL DEFAULT '';
ALTER TABLE stacks ADD COLUMN ref_overrides BLOB;
