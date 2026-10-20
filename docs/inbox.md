# Uygulama içi kutu ("Bildirimlerim"): `inbox` kanalı ve `/v1/me/inbox`

Hub bileti iletişim-servisi 05; ADR 0070 (iletişim tek servistir). Hesap
Merkezi'nin "Bildirimlerim"i (bilet 07) ve sky-app (bilet 08, Mobile Lab) bu
sözleşmeyi okur.

## Kutuya ne düşer

- Bir Bildirim türünün `default_channels`'ında `inbox` varsa
  `POST /v1/notifications` her alıcı için bir kutu öğesi yazar.
- **Kutu tercihten bağımsızdır** (S4): isteğe bağlı türde de kişinin
  `allowed_channels`'ına bakılmaz, `[]` olan kişi de öğesini alır. E-posta
  tercihe bakmaya devam eder. Türün kanallarında `inbox` yoksa (ör.
  Keycloak'ın hesap postaları, `/single`) kutuya hiçbir şey düşmez.
- Kutu adres istemez: kişinin `sub`'ı kutusudur, Keycloak'a sorulmaz.
- Öğe asıl kaydın (bilet, sertifika, duyuru) kopyası değildir: türün kutu
  metninden render edilen kısa bir başlık, özet ve uygulama içi bağlantı.
- Öğe bildirimin kendi işleminde yazılır, kuyruğa (`mail_queue`) girmez: veri
  veritabanından çıkmadığı için deneme, şerit ve gönderim bütçesinin
  yapacağı bir şey yoktur. Aynı `Idempotency-Key` ile tekrar ikinci öğe
  yazmaz; bir bildirim bir kişiye en çok bir öğe yazar.

## Türün kutu metni

`PUT /v1/notification_types/{key}` gövdesinde `inbox` (Template seed yazar):

```json
{
  "category": "events",
  "required": false,
  "default_channels": ["email", "inbox"],
  "lane": "bulk",
  "inbox": {
    "title": "{{.EventName}} yarın",
    "summary": "Saat {{.Time}}",
    "link": "/events/{{.EventID}}"
  }
}
```

- `inbox` kanallarda `inbox` varken zorunlu
  (`400 notification_type.inbox_variant_required`), yokken reddedilir
  (`400 notification_type.inbox_variant_unused`). `inbox`'sız `PUT` kayıtlı
  metni siler.
- Üç alan Go `text/template`'tir; yalnız bildirimin `variables`'ını okur
  (kişinin adı ve adresi yoktur, `.FullName`/`.Email` kullanılmaz). Bildirimde
  olmayan bir değişken render hatasıdır (`missingkey=error`), boş yazılmaz.
- `link` boş (bağlantı yok) ya da tek `/` ile başlayan uygulama içi yoldur
  (`/events/…`, `/news/…`); şema, host, `//` ve değişkenle başlayan bağlantı
  reddedilir (`400 notification_type.inbox_variant_invalid`, `params.part`).
- Metin düz metindir; istemciler HTML olarak yorumlamaz.
- Okuma: `GET /v1/notification_types[/{key}]` her türde `inbox` döner (`null`:
  kutuya gitmez).

## Gönderim (`POST /v1/notifications`)

- Kutu metni bildirim başına bir kez render edilir. Render edilmezse ya da
  sınırı aşarsa (başlık boş ya da 200 karakterden uzun, özet 1000'den uzun,
  bağlantı geçersiz) hiçbir öğe yazılmaz, bütün alıcılar `inbox.failed`'da
  sayılır, e-posta yine gider. Log satırı yalnız tür ve parçayı yazar.
- `expires_at` (isteğe bağlı, RFC 3339, gelecekte): bildirimin konusu daha
  erken biterse (etkinlik bitti) öğeler o an gider. 90 günden ileri bir tarih
  90 güne kısalır. Geçmiş tarih `400 notification.expires_at_past`.
- Yanıtta `channels.inbox`: `queued` yazılan öğeler, `failed` render
  edilemeyenler, `task_id` her zaman `null`.

## Kişinin kendi uçları

Yalnız kişinin kendi token'ı: Keycloak `userinfo`'su kabul etmeli, `aud`'unda
`skymail` olmalı (`V1_TOKEN_AUDIENCE_MODE`'dan bağımsız; yoksa `401`) ve kişi
token'ı olmalı (servis hesabı token'ı, `client_id` = `azp`:
`403 auth.person_required`). **Rol istemez**; `skymail:access` konsolun
kapısıdır, kutunun değil. Hesap erişim kapısı (`ACCOUNT_ACCESS_GATE_MODE`)
`/v1`'deki gibi uygulanır. Hesap Merkezi BFF'inin ve sky-app'in token'larına
`aud=skymail` Keycloak'ta eklenir (bilet 04).

| Uç | Ne yapar | Yanıt |
|---|---|---|
| `GET /v1/me/inbox?cursor=&limit=&archived=` | Kişinin öğeleri, yeniden eskiye | `200 {"items": [...], "next_cursor": "…" \| null}` |
| `GET /v1/me/inbox/counts` | Rozet | `200 {"unseen": n, "unread": n}` |
| `POST /v1/me/inbox/{id}/seen` | Görüldü | `200` öğe |
| `POST /v1/me/inbox/{id}/read` | Okundu (görüldü de olur) | `200` öğe |
| `POST /v1/me/inbox/{id}/unread` | Okunmadı (görüldü kalır) | `200` öğe |
| `POST /v1/me/inbox/{id}/archive` | Arşivle | `200` öğe |
| `POST /v1/me/inbox/read-all` | Arşivsiz bütün öğeleri okundu (ve görüldü) | `200 {"updated": n}` |
| `POST /v1/me/inbox/seen-all` | Arşivsiz bütün öğeleri görüldü | `200 {"updated": n}` |

Öğe:

```json
{
  "id": "…",
  "type": "event.reminder",
  "title": "Kodlama Gecesi yarın",
  "summary": "Saat 19.00",
  "link": "/events/evt-9",
  "created_at": "2026-10-21T09:00:00Z",
  "seen_at": null,
  "read_at": null,
  "archived_at": null
}
```

- **Sayfalama:** `limit` 1–100, varsayılan 50. Sıra `created_at`, eşitlikte
  `id`, ikisi de azalan; `next_cursor` sonraki sayfanın `cursor`'ıdır (opak),
  son sayfada `null`. Aynı anda yazılan öğeler de kaybolmaz, iki kez gelmez.
  Geçersiz `limit`/`archived` `400 inbox.invalid_query`, tanınmayan `cursor`
  `400 inbox.invalid_cursor`.
- **Arşiv:** `archived=exclude` (varsayılan), `include`, `only`. Arşivlenen
  öğe sayılara girmez; geri alma ucu yoktur.
- **İşaretler** sunucudadır ve kişinin bütün cihazlarında ortaktır. Var olan
  işaret ilk zamanını korur (`read` iki kez çağrılırsa `read_at` değişmez).
- **Sayılar:** `unseen` görülmemiş, `unread` okunmamış; ikisi de arşivliler
  ve süresi dolanlar hariç. Rozet için önerilen `unseen` (Knock'un
  varsayılanı); sky-app#45'in okunmamış sayısı `unread`.
- **Başkasının öğesi** — ve var olmayan, süresi dolmuş, biçimsiz kimlik —
  `404 server.not_found`: öğenin varlığı sahibinden başkasına söylenmez.
- Yanıtlar `Cache-Control: no-store`.
- **Gerçek zamanlı yok** (ilk sürüm): uygulama açılışta ve push gelince,
  Hesap Merkezi sayfa açılışında çeker.

sky-app#45'teki istekler: liste → `GET /v1/me/inbox`, okunmamış sayısı →
`GET /v1/me/inbox/counts` (`unread`), okundu → `POST /v1/me/inbox/{id}/read`,
hepsini okundu → `POST /v1/me/inbox/read-all`. Cihaz kaydı (`/v1/me/devices`)
push biletinde (12) gelir.

## Saklama ve silme

- Öğe en çok **90 gün** tutulur (`expires_at` = yazıldığı an + 90 gün, ya da
  bildirimin `expires_at`'i daha erkense o; veritabanında `CHECK`). Süresi
  dolan öğe o andan itibaren okunmaz, sayılmaz, işaretlenmez; kutu süpürmesi
  (`sweepInbox`, saatte bir, açılışta da) siler ve yalnız sayıyı loglar
  (`"event":"inbox_sweep"`).
- Hesap silme (`PUT /internal/v1/account-erasures/{id}`) kişinin bütün
  öğelerini yaşına bakmadan siler (`inbox_items_deleted`) ve metni kişinin
  adını ya da adresini geçen başkalarının öğelerini de siler
  (`inbox_items_naming_deleted`). Ayrıntı:
  [`data-lifecycle.md`](data-lifecycle.md#account-erasure).
- Öğelerin içeriği hiçbir operatör ucunda görünmez; loglarda kişi, metin ve
  bağlantı yazılmaz.

## Geri dönüş

`20261021120000` expand'dir: eski imaj yeni tabloları okumaz. Ama eski imaj
`default_channels`'ındaki her kanalı e-posta sanar: `inbox`'lı bir tür varken
bu sürümden önceki bir imaja dönülürse o türün alıcıları kanal başına bir
e-posta alır, yalnız `inbox`'lı türün isteği hata verir. Dönmeden önce `inbox`'ı bütün türlerden çıkarın (`PUT` ile ya da
down migration, `docs/database-migrations.md`).
