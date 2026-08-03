-- The decision is made on a pessimistic merge of every weather source, which cannot be
-- more optimistic than its gloomiest member. That is correct, but it is also lossy: it
-- cannot distinguish "all three models agree it is clear" from "two said overcast and
-- were overruled by nothing, because both sources were secretly the same model".
--
-- That last case shipped a wrong GO on 2026-08-03, when the two configured sources
-- (Open-Meteo best_match and yr.no) returned cloud within ~4% of each other on every
-- hour while GFS and ICON put the same window at 85-100%. sources_json records what
-- each model said on its own, so a split decision is visible on the log page instead
-- of being averaged away.
--
-- Existing rows keep '{}' — an Agreement with no members, which decodes to no
-- breakdown and renders as the bare source name, exactly as before.

ALTER TABLE nights ADD COLUMN sources_json TEXT NOT NULL DEFAULT '{}';
