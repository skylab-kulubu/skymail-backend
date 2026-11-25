-- The record of who read a history goes; an image from before writes none.
SET lock_timeout = '5s';

DROP TABLE IF EXISTS recipient_history_views;

RESET lock_timeout;
