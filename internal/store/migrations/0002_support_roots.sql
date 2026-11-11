-- Versioned OpenCode turns may persist a bounded JSON array of constituent
-- source roots in refs.root; legacy roots remain plain strings. The migration
-- version fences older binaries that would treat the array as one opaque root.
INSERT INTO meta (key, value) VALUES ('support_root_format', '2');

-- Determine whether a turn has stable message identities without scanning its
-- historical versions. Legacy turns retain their original scalar support root.
CREATE INDEX events_message_support ON events (turn_id)
WHERE coalesce(json_extract(flags, '$.message_key'), '') != '';

CREATE INDEX events_message_identity ON events (session_id, json_extract(flags, '$.message_key'))
WHERE coalesce(json_extract(flags, '$.message_key'), '') != '';
