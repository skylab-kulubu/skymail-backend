-- Drops the unsubscribe record. Not for production once the one-click link
-- has been on: every unsubscribe would be lost and the duyuru stream would
-- mail those addresses again. Roll back with SKYMAIL_UNSUBSCRIBE_LINK=off, which
-- keeps the records and keeps applying them (README, "Tek tık çıkış ucu").
SET lock_timeout = '5s';

DROP TABLE mail_unsubscribe_reactivations;
DROP TABLE mail_unsubscribes;

RESET lock_timeout;
