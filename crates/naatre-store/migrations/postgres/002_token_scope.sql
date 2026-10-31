-- PG twin of SQLite migration 2: per-token path-prefix scope
-- (`None` = whole repo). Existing tokens get NULL, which the scope
-- matcher treats as unrestricted.
ALTER TABLE tokens ADD COLUMN scope_path TEXT;
