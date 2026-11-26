# Bildirimler (`POST /v1/notifications`) ve Bildirim türleri

Hub bileti iletişim-servisi 03; ADR 0070 (core ne oldu ve kime, SkyNotify
hangi kanaldan ve hangi metinle). Kanallar e-posta ve uygulama içi kutu
(bilet 05, [`inbox.md`](inbox.md)); push (12) ve SMS (13) kendi biletleriyle
gelir.

## Bildirim türü (`notification_types`)

| Alan | Anlamı |
|---|---|
| `key` | Template key'in alfabesi (`event.cancelled`). Türün e-posta varyantı aynı anahtarlı şablondur. |
| `category` | Tercih kategorisi; `null` kategorisiz. |
| `required` | Zorunlu tür tercihe bakmaz (giriş kodu, bilet, sertifika). |
| `default_channels` | Türün kanalları: `email`, `inbox` (veritabanında `CHECK`). |
| `lane` | Kuyruk şeridi: `system` (toplu gönderimi hiç beklemez) ya da `bulk` (işçilerin en çok %70'i). |
| `archived_at` | Doluysa tür kullanımda değildir. |
| `inbox` | Türün kutu metni (`title`, `summary`, `link`); kanallarda `inbox` yoksa `null`. `notification_variants`'ta tutulur. |

- Template seed yazar: `PUT /v1/notification_types/{key}` (`skynotify:templates:write`),
  gövde `{"category", "required", "default_channels", "lane", "inbox"}`. Var
  olan tür, kutu metniyle birlikte üzerine yazılır, arşivi olduğu gibi kalır.
  `inbox` kanallarda `inbox` varken zorunlu, yokken reddedilir
  ([`inbox.md`](inbox.md#türün-kutu-metni)). Kategori küçük harf, rakam, nokta,
  tire ve alt çizgi; 2–64 karakter.
- Okuma: `GET /v1/notification_types`, `GET /v1/notification_types/{key}`
  (`skynotify:templates:read`).
- Operatörün türü değiştireceği bir uç yok (konsol, bilet 10). O uç geldiğinde
  ADR-0047'nin kuralı (seed operatörün değişikliğini sessizce ezmez) buraya da
  uygulanır.
- `templates.channel` her şablonun hangi kanal varyantı olduğunu söyler; bugün
  hepsi `email`. Tekillik ikinci kanal gelene kadar `key` üzerinde kalır (eski
  imajın seed'i `ON CONFLICT (key)` yazar).

## `POST /v1/notifications`

Yalnız bir servis hesabının token'ı (client credentials: `client_id` = `azp`)
ve `skynotify:notifications:send` (bilet 04, core'un servis hesabına) girer;
`/v1`'in her ucu gibi `skynotify:access` de gerekir. Kişi token'ı rolü olsa da
`403 auth.service_account_required` alır.

```http
POST /v1/notifications
Idempotency-Key: core-outbox-7f3a…
Content-Type: application/json

{
  "type": "event.cancelled",
  "recipients": [
    {"sub": "31ef736f-…", "allowed_channels": ["email"]},
    {"sub": "5d0c…", "email": "ayse@example.com", "name": "Ayşe Kaya", "allowed_channels": []}
  ],
  "variables": {"EventName": "Kodlama Gecesi"},
  "source": {"kind": "event", "ref": "6f1c…"}
}
```

- `recipients`: 1–1000 kişi, Keycloak `sub`'ıyla. Aynı `sub` ikinci kez
  geçerse sayılır (`duplicates`) ve atlanır.
- `email`, `name` isteğe bağlı. `variables` her alıcıya aynıdır; alıcının adı ve
  adresi şablonda `.FullName` ve `.Email`.
- `allowed_channels`: core'un kişinin tercihine göre izin verdiği kanallar
  (`email`, `inbox`, `push`, `sms`). İsteğe bağlı türde her alıcıda zorunludur
  (`[]` hiçbir kanal; alan yoksa `400 notification.allowed_channels_required`,
  `params.recipient` sırası). Zorunlu türde yok sayılır.
- `unsubscribe_url` (isteğe bağlı): kişinin core'un imzaladığı tek tık çıkış
  bağlantısı (`https://api.yildizskylab.com/v1/communication-preferences/unsubscribe?t=…`;
  core'un kitle ucunun `unsubscribeUrl`'i). Yalnız **isteğe bağlı** ve
  **`bulk` şeritli** türde kabul edilir; zorunlu ya da `system` şeritli türde
  `400 notification.unsubscribe_url_not_allowed` (giriş kodu, bilet, onay
  postası çıkış bağlantısı taşımaz). Mutlak `https`, host'lu, kullanıcısız, en
  çok 960 karakter görünür ASCII, `<`, `>`, virgül ve tırnak yok; değilse
  `400 notification.unsubscribe_url_invalid` (ikisi de `params.recipient`
  sırası). `SKYNOTIFY_BULK_PREFERENCE=on` iken isteğe bağlı ve `bulk` şeritli
  türde e-postası gidecek her alıcı onu taşımalıdır, yoksa
  `400 notification.unsubscribe_url_required` (`params.recipient` sırası):
  dağıtıcı bağlantısız duyuru postasını göndermez (README, "Toplu postada
  tercih"). Bağlantı satırda (`mail_queue.list_unsubscribe_url`) saklanır,
  şablona `.UnsubscribeUrl` olarak girer ve `SKYNOTIFY_LIST_UNSUBSCRIBE=on`
  iken duyuru kutusundan `List-Unsubscribe` + `List-Unsubscribe-Post`
  başlıklarıyla gider (README, "Tek tık çıkış"). Boşsa istekte yazılmaz; eski
  bir isteğin `Idempotency-Key` özeti değişmez.
- `source`: nereden geldiği (`kind` en çok 64, `ref` en çok 200 karakter);
  bildirimle saklanır.
- `expires_at` (isteğe bağlı, gelecekte): konusu bitince kutu öğeleri o an
  gider (en çok 90 gün). Geçmiş tarih `400 notification.expires_at_past`.
- `attachments` (isteğe bağlı, ADR 0082 Mail attachment): yalnız **tek
  alıcılı** istekte (`recipients` uzunluğu 1; değilse
  `400 attachment.single_recipient_only`). En çok 2 dosya, `application/pdf`
  ya da `image/png`, çözülmüş toplam 512 KB, standart base64:
  `[{"filename": "bilet-gecekodu-2026.pdf", "content_type": "application/pdf", "content_base64": "JVBERi0x…"}]`.
  Yalnız e-posta taşır, kutu yok sayar; kişi atlanırsa (ör. `not_allowed`)
  dosya saklanmaz. Dosya postanın satırıyla yalnız gönderilene kadar durur.
  Kurallar ve hata kodları README "Posta eki". Ek isteğin özetine girmez:
  aynı anahtarla yeniden üretilmiş PDF'li tekrar ilk yanıtı alır, ikinci dosya
  yazılmaz.

### Kanal seçimi (kişi başına)

tür varsayılanı ∩ `allowed_channels` (yalnız isteğe bağlı türde) ∩ kişinin o
kanaldaki adresi.

Kutu (`inbox`) tercihe bakmaz: türün kanallarında varsa isteğe bağlı türde de
her alıcıya öğe yazılır, adres gerekmez ve Keycloak'a sorulmaz
([`inbox.md`](inbox.md)).

E-posta adresi: istekteki `email`; yoksa Keycloak'tan `sub` ile Primary e-mail
(servis hesabının `view-users` rolü). Atlanma nedenleri:

| Neden | Ne zaman |
|---|---|
| `not_allowed` | İsteğe bağlı tür, kanal `allowed_channels`'ta yok |
| `unknown_person` | Keycloak'ta bu `sub`'la kullanıcı yok |
| `disabled` | Hesap devre dışı |
| `no_address` | İstekte de Keycloak'ta da adres yok |
| `unverified` | Keycloak'taki adres doğrulanmamış |

İstekte adres verilmişse Keycloak'a sorulmaz: adresi veren çağıran onun
doğruluğundan sorumludur. Keycloak yanıt vermezse hiçbir şey kuyruğa girmez ve
saklanmaz: `503 notification.directory_unavailable`, aynı anahtarla yeniden
denenir.

E-posta türün anahtarındaki şablonla, türün şeridinde, kişinin `sub`'ı
`mail_queue.recipient_sub`'a yazılarak kuyruğa girer (bir gönderim,
`mail_tasks.notification_id`). Gönderim bir listeye ya da onaylı isteğe
gitmediği için içerik kuralında tekil sayılır (`docs/data-lifecycle.md`).
Takip yalnız `SKYNOTIFY_TRACKING_SYSTEM=on` (ve `SKYNOTIFY_TRACKING=on`) iken
vardır: core gönderimden önce (2 saniye içinde) sorulur, piksel konur, tıklama
adresi yalnız şablonun olduğu gibi yazdığı bağlantılara, hassas şablonda hiçbir
bağlantıya konmaz (README, "Sistem postaları"). Varsayılan: takip yok.

### Yanıt (`201`)

```json
{
  "id": "…",
  "type": "event.cancelled",
  "recipients": 2,
  "duplicates": 0,
  "channels": {
    "email": {"task_id": "…", "queued": 1, "failed": 0, "skipped": {"not_allowed": 1}, "attachments": 1},
    "inbox": {"task_id": null, "queued": 2, "failed": 0, "skipped": {}}
  }
}
```

`task_id` e-posta gönderimidir (`GET /v1/mail_tasks/{id}`); kimse kuyruğa
girmediyse ve kutuda her zaman `null`. `failed`: metni render edilemeyen
alıcılar (e-postada gönderimde `failed` satır olarak görünür; kutuda öğe
yazılmaz). Kutuda `queued` yazılan öğe sayısıdır. `attachments` (yalnız
e-postada): postayla giden ek sayısı; ek yoksa ya da satır kuyruğa girerken
düştüyse yazılmaz.

### Idempotency (`Idempotency-Key`)

- Başlık zorunlu: 1–255 görünür ASCII karakter, bir bildirimin her denemesinde
  aynı. Yoksa `400 notification.idempotency_key_required`.
- Anahtar çağırana özeldir: `(sent_by, idempotency_key)` tekil
  (`notifications`).
- Aynı çağıran, aynı anahtar, aynı istek (isteğin SHA-256'sı): ilk yanıt yine
  `201` ile döner, `Idempotent-Replayed: true`; hiçbir şey yeniden kuyruğa
  girmez, Keycloak'a sorulmaz.
- Aynı anahtar, başka istek: `422 notification.idempotency_key_reused`.
- Aynı anda gelen iki istek: ikincisi birincinin işlemini bekler, sonra onun
  yanıtını döner.
- Anahtar, bildirim satırı durdukça (silinmez) geçerlidir; yeni bildirim yeni
  anahtar ister (core'un giden kutusu satırının kimliği gibi).

### Hatalar

| Durum | Kod |
|---|---|
| 400 | `validation.error`, `notification.idempotency_key_required`, `notification.allowed_channels_required`, `notification.expires_at_past`, `notification.unsubscribe_url_not_allowed`, `notification.unsubscribe_url_invalid`, `notification.unsubscribe_url_required`, `attachment.single_recipient_only`, `attachment.too_many`, `attachment.type_not_allowed`, `attachment.content_invalid`, `attachment.content_type_mismatch`, `attachment.too_large` |
| 403 | `server.forbidden` (rol yok), `auth.service_account_required` (kişi token'ı) |
| 409 | `notification.email_variant_missing` (tür e-postayla gider, anahtarında kullanımda şablon yok), `notification.inbox_variant_missing` (tür kutuya gider, kutu metni yok) |
| 422 | `notification.type_unknown` (yok ya da arşivli), `notification.idempotency_key_reused` |
| 503 | `notification.directory_unavailable` |

### Gözlem

Her kabul edilen bildirim kanal başına bir log satırı yazar (kişi, adres,
değişken yok):

```json
{"level":"info","event":"notification_queued","notification_id":"…","type":"event.cancelled","lane":"bulk","channel":"email","queued":12,"failed":0,"skipped_not_allowed":3}
```

SkyNotify'da bugün metrik ucu yok; tür ve kanal başına kuyruk derinliği ve
gönderim sayısı metrikleri duyuru 05'in metrik maddesiyle birlikte gelir.
O zamana kadar sayım bu satırdan (production sunucusunda, salt okunur; `SERVIS`
production SkyNotify uygulamasının Swarm servis adıdır, `docker service ls`
gösterir):

```bash
docker service logs --raw --since 24h "$SERVIS" 2>&1 | grep -F '"event":"notification_queued"' | grep -o '"type":"[^"]*","lane":"[^"]*","channel":"[^"]*","queued":[0-9]*' | sort | uniq -c
```

## `POST /v1/mail_tasks/single` aynen kalır

İstek ve yanıt değişmez; Keycloak SPI, Forms ve core çağırmaya devam eder.
`template_key` o anahtarın e-posta varyantıdır ve yalnız e-postayla gider;
`notification_types`'ta satırı olması gerekmez. `Idempotency-Key` `/single`'da
henüz okunmaz (horizontal-scale 28'in `/single` kısmı ayrı iş).
