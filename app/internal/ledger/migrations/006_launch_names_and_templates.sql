CREATE TABLE stack_launch_names (
    id TEXT PRIMARY KEY NOT NULL
);

INSERT INTO stack_launch_names (id) SELECT id FROM stacks WHERE api_owned = 1;

CREATE TABLE manifest_templates (
    name TEXT PRIMARY KEY NOT NULL,
    content BLOB NOT NULL
);
