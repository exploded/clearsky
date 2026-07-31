-- The GO/NO-GO gate moved from whole-night aggregates (mean + PEAK cloud) to the
-- longest contiguous run of individually-imageable hours. A peak gate made every
-- night hostage to its worst hour, so a clear evening followed by an overcast 3am
-- scored as a washout. window_json snapshots the run the decision was made on so the
-- log page, notifications and /api/tonight can show the session you would actually get.
--
-- Existing rows keep '{}' — they decode to an empty window and render as "—".

ALTER TABLE nights ADD COLUMN window_json TEXT NOT NULL DEFAULT '{}';
