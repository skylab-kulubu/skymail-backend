-- Down drops the record of who opened sensitive content: it is kept nowhere
-- else.
SET lock_timeout = '5s';

DROP TABLE IF EXISTS sent_content_views;

RESET lock_timeout;
