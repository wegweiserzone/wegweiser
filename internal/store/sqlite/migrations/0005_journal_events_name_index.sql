-- The history of one name: which commits added or removed a record owned by
-- it. Without this, finding them reads every event the journal holds, and the
-- journal keeps everything by default (docs/decisions/d08-journal-retention.md),
-- so a name that changed once is the most expensive one to ask about.
CREATE INDEX journal_events_name_idx ON journal_events(name, commit_id);
