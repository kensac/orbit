-- +goose Up

-- Uploads are served in slices (store.Blob). Postgres reads a slice of an uncompressed value from only the TOAST chunks it covers, but decompresses a compressed one from its start for every slice. Uploads are mostly media that does not compress, so storing them uncompressed costs little disk. It applies to rows written from now on.
ALTER TABLE uploads ALTER COLUMN content SET STORAGE EXTERNAL;

-- +goose Down
ALTER TABLE uploads ALTER COLUMN content SET STORAGE EXTENDED;
