-- Drops the schema and metadata table used by pgfs.
--
-- Dependent objects are not dropped: this fails if a foreign key
-- references pgfs.metadata, or if the pgfs schema contains other objects.
-- Large objects are not removed.

DROP TABLE pgfs.metadata;
DROP SCHEMA pgfs;
