-- The server an update-test copy was made from. The copy is deleted by hand or by applying
-- the test; if the source goes first, the copy stays as an ordinary server.
ALTER TABLE instances ADD COLUMN test_of TEXT REFERENCES instances (id) ON DELETE SET NULL;
