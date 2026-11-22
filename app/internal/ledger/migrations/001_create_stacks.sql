CREATE TABLE IF NOT EXISTS stacks (
    id TEXT PRIMARY KEY NOT NULL CHECK (id <> ''),
    hostname TEXT NOT NULL COLLATE NOCASE UNIQUE CHECK (hostname <> ''),
    duration_ns INTEGER NOT NULL CHECK (duration_ns > 0),
    state TEXT NOT NULL
);
