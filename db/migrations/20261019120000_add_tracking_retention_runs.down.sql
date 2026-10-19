-- The receipts go with the table; the run's own work (deleted events, closed
-- tokens, emptied links) is not undone, and could not be.
DROP TABLE tracking_retention_runs;
