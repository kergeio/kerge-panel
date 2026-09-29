-- The charts let the operator read a summarized window as averages or as
-- maxima, so every metric a chart draws needs its maximum in the summarized
-- tables, not only the bursty ones. Rows summarized before this migration
-- keep NULL here: the raw samples they came from are long pruned, so there
-- is nothing to compute them from.
ALTER TABLE metrics_1m ADD COLUMN mem_used_max INTEGER;
ALTER TABLE metrics_1m ADD COLUMN load1_max REAL;
ALTER TABLE metrics_1m ADD COLUMN disk_used_max INTEGER;

ALTER TABLE metrics_1h ADD COLUMN mem_used_max INTEGER;
ALTER TABLE metrics_1h ADD COLUMN load1_max REAL;
ALTER TABLE metrics_1h ADD COLUMN disk_used_max INTEGER;
