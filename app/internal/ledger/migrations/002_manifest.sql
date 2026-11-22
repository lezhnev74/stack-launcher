ALTER TABLE stacks ADD COLUMN manifest BLOB;
ALTER TABLE stacks ADD COLUMN repository TEXT;
ALTER TABLE stacks ADD COLUMN commit_hash TEXT;
ALTER TABLE stacks ADD COLUMN manifest_path TEXT;
ALTER TABLE stacks ADD COLUMN checkout TEXT;
