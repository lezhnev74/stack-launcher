CREATE TABLE stack_launch_activity (
    sequence INTEGER PRIMARY KEY,
    stack_id TEXT NOT NULL REFERENCES stacks(id) ON DELETE CASCADE,
    recorded_at TEXT NOT NULL,
    message TEXT NOT NULL
);

CREATE INDEX stack_launch_activity_by_stack ON stack_launch_activity(stack_id, sequence);
