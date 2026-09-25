-- Down drops the proof of every erasure SkyMail finished. Only for a
-- controlled rollback: core keeps its own record of each request, but
-- repeating a command after this does its (by then empty) work again.
DROP TABLE account_erasure_receipts;
