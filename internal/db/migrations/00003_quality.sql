-- +goose Up
-- Video quality caps the smaller side of the frame; 'best' is uncapped (the
-- only quality before this migration). Each quality is its own stored file,
-- so a video can now have one row per quality. Audio has no quality.
CREATE TYPE video_quality AS ENUM ('best', '1080', '720', '480', '360');

ALTER TABLE videos ADD COLUMN quality video_quality NOT NULL DEFAULT 'best';
ALTER TABLE videos DROP CONSTRAINT videos_source_id_key;
ALTER TABLE videos ADD CONSTRAINT videos_source_id_quality_key UNIQUE (source_id, quality);

ALTER TABLE tasks ADD COLUMN quality video_quality NOT NULL DEFAULT 'best';

-- +goose Down
ALTER TABLE tasks DROP COLUMN quality;

-- Only one row per video fits the old constraint: keep 'best'. Files of the
-- dropped rows stay in storage untracked.
DELETE FROM videos WHERE quality <> 'best';
ALTER TABLE videos DROP CONSTRAINT videos_source_id_quality_key;
ALTER TABLE videos ADD CONSTRAINT videos_source_id_key UNIQUE (source_id);
ALTER TABLE videos DROP COLUMN quality;

DROP TYPE video_quality;
