-- The alerts still firing, found without reading every older firing.
CREATE INDEX firings_open ON firings (start_at) WHERE end_at IS NULL;
