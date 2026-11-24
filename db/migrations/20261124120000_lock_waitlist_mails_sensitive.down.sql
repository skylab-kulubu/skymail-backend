-- Restores 20261013140000's list, without the waiting list mails.
SET lock_timeout = '5s';

CREATE OR REPLACE FUNCTION template_key_content_locked(k TEXT) RETURNS BOOLEAN
    LANGUAGE sql
    IMMUTABLE
    SET search_path = pg_catalog, public
AS
$$
WITH list AS (SELECT ARRAY ['login', 'signin', 'code', 'otp', 'passcode', 'password', 'reset', 'verify',
                            'verification', 'confirm', 'confirmation', 'token', 'magic', 'invite', 'activation',
                            'activate', '2fa', 'mfa', 'ticket', 'link',
                            'giris', 'kod', 'kodu', 'dogrulama', 'sifre', 'parola', 'baglanti', 'baglantisi', 'davet',
                            'onay'] AS words)
SELECT k IS NOT NULL
           AND (k ~ '^(keycloak|account|auth|login|otp)\.'
        OR k = ANY (ARRAY ['core.contact-consent-confirm', 'event.ticket-created', 'event.reminder'])
        OR EXISTS (SELECT 1
                   FROM (SELECT w, lead(w) OVER (ORDER BY n) AS next_w
                         FROM regexp_split_to_table(k, '[.-]') WITH ORDINALITY AS s(w, n)
                         WHERE w <> '') parts,
                        list
                   WHERE parts.w = ANY (list.words)
                      OR parts.w || COALESCE(parts.next_w, '') = ANY (list.words)))
$$;
