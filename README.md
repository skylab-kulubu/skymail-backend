<div align="center">
  <a href="https://github.com/skylab-kulubu/skymail-backend">
    <img src="https://avatars.githubusercontent.com/u/96308083?s=200&v=4" alt="Repo Logo" height="100">
  </a>
</div>

<h2 align="center">Skymail</h3>

<div align="center">
  <img src="https://img.shields.io/badge/license-MIT-blue.svg?labelColor=003694&color=ffffff" alt="License">
  <img src="https://img.shields.io/github/contributors/skylab-kulubu/skymail-backend?labelColor=003694&color=ffffff" alt="GitHub contributors" >
  <img src="https://img.shields.io/github/stars/skylab-kulubu/skymail-backend.svg?labelColor=003694&color=ffffff" alt="Stars">
  <img src="https://img.shields.io/github/forks/skylab-kulubu/skymail-backend.svg?labelColor=003694&color=ffffff" alt="Forks">
  <img src="https://img.shields.io/github/issues/skylab-kulubu/skymail-backend.svg?labelColor=003694&color=ffffff" alt="Issues">
</div>

## Özellikler ✨

* **Şablonlar:** React ve Tailwind ile email şablonları oluşturun.
* **Mail Listeleri:** Kolayca toplu mail gönderin.

## Health endpoints

`GET /health` is process-only liveness (always `204`). `GET /ready` answers
`503` while the task is shutting down or its database does not answer, and
additionally verifies the exact shared account-access contract when the access
gate is in `enforce` mode; `GET /ready?gate=skip` leaves the gate out and is
what the container's health check (`skymail-backend healthcheck`, the image's
`HEALTHCHECK`) asks. On SIGTERM the service drains HTTP, gives back the queue
rows it took and has not begun, lets the sends in progress finish and exits
within 25 s: run it with a stop grace period of 30 s. Details and the Swarm
values: [`docs/health-and-shutdown.md`](docs/health-and-shutdown.md). API
documentation under `/docs` remains public. See
[`docs/account-access-gate.md`](docs/account-access-gate.md) for the deployment
contract and required configuration.

## Ters proxy ve istemci IP'si

Skymail bir ters proxy'nin arkasında çalışır. Proxy, çağıranın gönderdiği
`X-Forwarded-For` başlığını atar ve kendi başlığını yazar; bu yüzden başlık
yalnızca bağlantı o proxy'lerden birinden geldiğinde okunur. Diğer tüm
çağıranlar için `ctx.IP()` soket adresini döndürür.

- `TRUSTED_PROXY_RANGES` — virgülle ayrılmış CIDR aralıkları; tek bir adres o
  tek makine anlamına gelir. Varsayılan:
  `10.0.0.0/8,172.16.0.0/12,192.168.0.0/16,127.0.0.0/8,::1/128,fc00::/7`.

IP adresi kişisel veridir: loglar istemcinin adresini ve User-Agent'ını
yazmaz. Gerekirse yalnız sınıfını yazar (`"client"`: `loopback`, `private` —
Docker ağları, RFC 1918, `fc00::/7` —, `link_local`, `public`, `unknown`);
bugün bunu yazan tek satır veritabanında bulunamayan kaydın 404'üdür.

Liste servis trafiğe açılmadan önce doğrulanır: CIDR aralığı ya da adres
olmayan bir girdi atlanmak yerine başlatmayı durdurur. Proxy bir konteyner
olduğu ve ağ içindeki adresi her yeniden oluşturulduğunda değiştiği için tek
bir adres yazılmaz, paylaşılan ağın aralığı yazılır.

## Keycloak

SkyMail'in izinleri `KEYCLOAK_CLIENT_ID` istemcisinin (varsayılan `skymail`)
rolleridir: `skymail:access` giriş kapısı, `templates:*`, `lists:*`,
`mails:*` kaynak rolleri, `skymail:mails:approve` mail onayı. Mail onayına
sunmak, sunulanı okumayı ister: `skymail:templates:read`, listeye sunmak için
ayrıca `skymail:lists:read`. Eksik olana 403 döner ve `params.missing_roles`
eksik rolleri sayar.

`KEYCLOAK_SERVICE_CLIENT_ID` (`skymail-backend`) istemcisinin service
account'u Keycloak'tan okur: gönderim ve listeler için grupları ve üyelerini,
mail onayında da `skymail:mails:approve` rolünü kimin taşıdığını (doğrudan ya
da bir grup üzerinden). Bunun için `realm-management` istemcisinin şu rolleri
gerekir:

- `view-users` — kullanıcıları, grupları ve üyelerini okumak;
- `view-clients` — istemciyi clientId ile bulup rolünün kullanıcılarını ve
  gruplarını okumak.

`view-clients` yoksa mail onayı yine çalışır, ama onaycılar bulunamaz ve
sunuşun yanıtı `notification.problem: approver_lookup_failed` der.

Bu Admin REST çağrıları (`/admin/realms/<realm>/…`) ve service account'un
onlar için aldığı token, `KEYCLOAK_ADMIN_URL` verilmişse oraya gider (ör.
Docker ağının içinden `http://<keycloak servisi>:8080`); verilmemişse
bugünkü gibi `KEYCLOAK_REALM_URL`'in köküne. Değer `KEYCLOAK_REALM_URL`'in
kökü gibi bir taban adrestir: sondaki `/` ve `/realms/<realm>` atılır;
mutlak `http`/`https` olmayan, kimlik bilgisi, sorgu ya da `/admin`,
`/realms` taşıyan değer açılışı durdurur (hata değeri değil değişkeni
söyler). Açılışta `keycloak admin REST: KEYCLOAK_ADMIN_URL` satırı yazılır.
Token denetimleri değişmez: `userinfo`, silme ucunun issuer'ı ve JWKS'i,
erişim kapısının issuer eşitliği `KEYCLOAK_REALM_URL`'de kalır. Keycloak
token'ı hangi adresten verirse versin `iss`'e public adını (`KC_HOSTNAME`)
yazar. Amaç, Keycloak'ın public adında `/admin`'i kenarda kapatabilmek
(Keycloak'ın reverse proxy kılavuzu `/admin/`'i dışarı açmaz).

Mail onayı bildirimlerindeki linkler `SKYMAIL_UI_URL` altına kurulur
(varsayılan `https://mail.yildizskylab.com`).

## `/v1` token audience'ı (`V1_TOKEN_AUDIENCE_MODE`)

`/v1` token'ı Keycloak `userinfo`'suna sorar. `userinfo` token'ın canlı
olduğunu kanıtlar ama kimin için verildiğini söylemez. Bu değişken `aud`'unda
`KEYCLOAK_CLIENT_ID` (varsayılan `skymail`) olmayan token'a ne yapılacağını
seçer (RFC 9068 §4):

- `off` (varsayılan, yoksa da): `aud` okunmaz, davranış öncekiyle aynı.
- `log`: istek kabul edilir. `userinfo`'dan ve rol denetiminden geçen her
  istek için bir uyarı satırı yazılır:
  `"event":"v1_token_audience_missing","azp":"<istemci>","mode":"log"`.
  Satırda yalnız çağıran istemcinin `azp`'si ve mod vardır. Token, `sub`, ad,
  e-posta ve IP yazılmaz. `azp`'siz ya da JWT olmayan token `"azp":"none"`
  olarak yazılır.
- `enforce`: aynı token'a `401` döner,
  `WWW-Authenticate: Bearer error="invalid_token"` ile. Satır `"mode":"enforce"`
  ile yazılır. Rol denetiminden önce çalışır: `aud`'u ve rolü olmayan token
  `403` değil `401` alır.

Başka bir değer açılışı durdurur. Açılışta `v1 token audience check` satırı
modu yazar. `aud`, `userinfo` kabul ettikten sonra token'dan okunur: imzayı,
issuer'ı ve süreyi Keycloak denetlemiştir. Silme ucu (`/internal`) bundan
etkilenmez, kendi yerel JWKS denetimiyle `aud`'u zaten şart koşar.

Bir haftalık sayım (production sunucusunda, salt okunur):

```bash
docker service logs --raw --since 168h sky-lab-production-skymail-tvrjzs 2>&1 | grep -F '"event":"v1_token_audience_missing"' | grep -o '"azp":"[^"]*","mode":"[^"]*"' | sort | uniq -c | sort -rn
```

## Hesap silme (Account erasure)

`PUT /internal/v1/account-erasures/{request_id}` core'un silme komutudur
(ADR-0051). `/v1` dışındadır ve `userinfo`'dan geçmez: token realm'in JWKS'i
ile yerelde doğrulanır ve `azp` = `core-erasure`, `aud` ∋ `skymail`,
`resource_access.skymail.roles` ∋ `skymail:account:erase` ister. Yalnız iç
Docker ağından çağrılır; `X-Forwarded-*`, `Forwarded` ya da `X-Real-Ip`
taşıyan istek `404` alır. Silinecek kişinin marker'ı account-access
Redis'te olmalıdır; kapı `off` iken uç `503` döner. Ayrıntı:
[`docs/data-lifecycle.md`](docs/data-lifecycle.md#account-erasure).

## Mail göndericisi (`MAIL_SENDER`)

`MAIL_SENDER` `on` (varsayılan, yoksa da açık) ya da `paused` olur; başka bir
değer başlatmayı durdurur. `paused` iken mail kuyruğa yazılır ama gönderilmez:
dağıtıcı ve işçiler başlamaz, açılışta bir uyarı logu çıkar, API ve `/ready`
normal çalışır. `GET /v1/mail_tasks/summary` `sender_paused: true` döner.
Yedekten geri yükleme bu değerle başlar. Sonra dump'ın getirdiği kuyruk
göndermeden kapatılır: `skymail-backend queue-close-restored --before <RFC3339>`
önce kuru çalışır, `--apply` ile geri yükleme anından önceki `pending` ve
`processing` satırları `failed` (`restore: gönderilmedi`) yapar. `--apply`
yalnız `MAIL_SENDER=paused` iken çalışır. Prosedür ve `docker exec` satırları:
[`docs/data-lifecycle.md`](docs/data-lifecycle.md#backup-and-restore).

## Kanallar (`mail_queue.channel`, `mail_queue.recipient_sub`)

Bir kuyruk satırı bir kişiye bir kanaldan bir teslimdir. Motor (kira, deneme,
şerit, bütçe) kanaldan bağımsızdır: işçi aldığı satırı satırın kanalının
göndericisine verir (`internal/mailer/channel.go`, `ChannelSender`). Her işçi
açılışta her kanalın kendi göndericisini kurar; e-postada bu işçinin SMTP
istemcisidir (go-mail, STARTTLS zorunlu, `Message-ID`, gönderim başına 1
dakika); hangi kutudan ve nasıl gideceğini gönderen şeridi seçer (aşağıda,
"Gönderen şeritleri"). Göndericinin hatası sonucu söyler: hata yok gönderildi, kalıcı hata
hemen düşer (`failed`), ötekiler yeniden denenir (en çok 5 deneme; 30 sn,
1, 2, 4 dakika arayla).

E-postada kalıcı hata yalnız alıcının adresine özgü olandır: go-mail'in
ayrıştıramadığı adres ve relay'in adresi reddi. Gönderene, hesaba, kiracıya ya
da relay politikasına ilişkin bir 5xx (kota, gönderen reddi, relay erişim
reddi) bütün toplu gönderimi bir anda düşürmesin diye geçici sayılır (`internal/mailer/smtp_refusal.go`):

| Yanıt | Adım | Sonuç |
|---|---|---|
| 5xx, ek kod 5.1.0–5.1.6 ya da 5.1.10 (alıcı adresinin durumu; 5.1.10 Exchange Online'da `RecipientNotFound`) | `RCPT TO`, `DATA` sonu | kalıcı (`failed`, ilk denemede) |
| ek kodsuz 550 / 551 / 553 | `RCPT TO` | kalıcı |
| 5xx, ek kod 5.1.7 / 5.1.8 (gönderen adresi ve sistemi; Postfix bunu `RCPT TO`'da verir, Exchange Online engelli hesaba 5.1.8 verir), 5.1.9, 5.1.90 (Exchange Online günlük alıcı sınırı) ve listede olmayan başka 5.1.x | `RCPT TO`, `DATA` sonu | geçici |
| 5xx, ek kod 5.2.x / 5.3.x / 5.7.x (ör. relay erişim reddi) ya da 5.1.x dışı başka sınıf | `RCPT TO`, `DATA` sonu | geçici |
| ek kodsuz başka 5xx (ör. 554, 501) | `RCPT TO`, `DATA` sonu | geçici |
| 5xx (ör. `554 5.2.0` SubmissionQuotaExceeded, 5.7.x gönderen reddi) | `MAIL FROM` | geçici |
| 4xx (greylisting, dolu kutu, kısma) | her adım | geçici |
| yanıtsız hata (bağlantı kopması, 1 dakikalık sınır) | her adım | geçici |

Geçici hata en çok 5 denemeye kadar yeniden denenir, sonra `failed` olur.
Satırın `error`'ı yalnız adım ve kodlardır, ör. `smtp_rejected: RCPT TO 550
5.1.1` (kalıcı) ya da `smtp_deferred: MAIL FROM 554 5.2.0` (geçici). Relay'in
metni yazılmaz, çünkü adresi tekrarlayabilir; bu hata hem loglanır hem
gönderimin sayfasında görünür. Bir alıcının kalıcı reddi öteki alıcıları
beklemez, bütçeden de yalnız o tek denemeyi yer. Kalıcı ret adresi bastırma
listesine de alır: aynı adrese sonraki gönderim relay'e gitmez (Geri dönen
posta ve bastırma listesi).

RFC 2606/6761'in ayrılmış alan adlarındaki alıcı (`example.com`, `example.net`,
`example.org` ve altları; `.example`, `.test`, `.invalid`, `.localhost`;
sandbox'taki seed hesapları gibi) gönderim kuyruğa alınırken `failed` yazılır
(`reserved_domain: …`, deneme 0). Relay'e hiç gitmez, bütçeye sayılmaz.
Gönderimin öteki alıcıları kuyruğa girer. Bu alıcı atlanmış sayılır,
gönderimin durumunu "Başarısız" yapmaz: gönderim öteki alıcıları neyse onu
gösterir (`mail_task_status()`, `20261020150000`). Bütün alıcıları atlanan
gönderim (hiçbir posta gitmedi) "Başarısız"dır. Alıcı sayımlarında satır yine
`failed` sayılır. Tek posta (`/single`) da reddedilmez,
kabul edilir ve alıcısı `failed` olur. Bu kuralı `main.go`
(`WithReservedDomainsRefused`) açar. Testler sahte relay ile `example.com`'a
gönderdiği için kural varsayılan olarak kapalıdır.

Bugün tek kanal `email`'dir: `recipient_email` kanalın adresidir. Var olan ve
eski imajın yazdığı satırlar varsayılanla `email` olur; `CHECK` başka kanalı
reddeder, yeni bir kanal kendi migration'ında eklenir. Bu sürecin göndericisi
olmayan bir kanalın satırı (daha yeni bir imajın yazdığı, geri dönülmüşse)
kalıcı hatayla düşer, işçi sıradakine geçer.

`recipient_sub` alıcının Keycloak `sub`'ıdır, gönderim biliyorsa: Keycloak
grubuna (doğrudan ya da onaylı) gönderimde her üyenin id'si. Yalnız adres
bilinen gönderimlerde (liste, adla yazılan kişiler, `POST /mail_tasks/single`)
boştur. Hesap silme kişinin satırlarını adresleriyle ve `sub`'ıyla bulur;
silinen satırda `recipient_sub` da boşalır. `GET /v1/mail_tasks/{id}/queue`
her satırın `channel`'ını döner.

## Gönderen şeritleri (`SKYMAIL_SEND_HESAP`, `SKYMAIL_SEND_BILGI`, `SKYMAIL_SEND_DUYURU`)

Her posta türüne göre üç paylaşılan kutudan birinden çıkar (hub bileti
skymail-bounce-isleme 10; Yusuf, 2026-10-20). Kural `internal/mailer/lanes.go`
`LaneOf`'tadır, gönderimin şablonuna ve satırın şeridine bakar:

| Şerit | Ne | Kural | Kararlaştırılan yol |
|---|---|---|---|
| `hesap` | giriş/kayıt kodu, e-posta doğrulama, şifre sıfırlama | şablon **sistem** şablonu ve anahtarı `keycloak.*` ya da `account.*` (`AccountMail`, bastırma muafiyetiyle aynı kural); hangi öncelik şeridinde olursa olsun | `graph` |
| `duyuru` | operatörün kitleye postası | toplu şerit (`priority = 0`): liste, Keycloak grubu, Forms formunun yanıtlayanları, onaylı gönderim, toplu bildirim türü, takip isteyen tekil posta | `xoauth2` |
| `bilgi` | geri kalan işlem postası | sistem şeridi (`priority = 1`): Forms onayı, Mail onayı bildirimi, etkinlik ve öteki sistem bildirimleri | `graph` |

Her şeridin kendi anahtarı vardır; üçü de varsayılan olarak `off`'tur:

| Değer | Nasıl gider |
|---|---|
| `off` (ya da boş) | bugünkü gibi: `SMTP_FROM`'dan, `SMTP_USER`/`SMTP_PASS` ile SMTP AUTH (LOGIN, `SMTP_PLAIN=true` ise PLAIN). Gönderimin şablonu okunmaz, `X-SkyMail-Queue-Id` yazılmaz |
| `graph` | Microsoft Graph `POST /users/{kutu}/sendMail`, `saveToSentItems: false`: kutunun Gönderilmiş Öğeler'ine kopya düşmez |
| `xoauth2` | `SMTP_HOST:SMTP_PORT` (`smtp.office365.com:587`, STARTTLS), `AUTH XOAUTH2` kutu adına, uygulamanın token'ıyla. Exchange kopyayı Gönderilmiş Öğeler'e koyar (duyuru kutusunda MRM 1 gün) |

Başka bir değer açılışı durdurur. Şerit açıkken:

- `SKYMAIL_FROM_<ŞERİT>` zorunlu: görünen ad ve kutu, ör.
  `SKY LAB Hesap <hesap@bildirim.yildizskylab.com>`. Graph'ta `from` bu
  kutudur (gönderim de bu kutunun adına yapılır), SMTP'de `From` ve zarf
  göndericisi.
- Uygulamanın (Entra `skymail-posta`, yalnız sertifika) kimliği zorunlu:
  `GRAPH_TENANT_ID`, `GRAPH_CLIENT_ID`, `GRAPH_CLIENT_KEY` (özel anahtar,
  PKCS#8 PEM'in tek satır base64'ü; OpenBao'da, Dokploy'da `${{vault…}}`) ve
  `GRAPH_CLIENT_CERT` (açık sertifika; PEM ya da `.cer`'in base64'ü, sır değil).
  Açılışta anahtarın sertifikaya ait olduğu denetlenir; bozuk ya da eksik değer
  açılışı durdurur, hata değeri yazmaz. Bu paket (`internal/msauth`) NDR
  okuyucusuyla (bilet 04) ortaktır.
- Token: client credentials, sertifikalı client assertion (RFC 7523): PS256 +
  `x5t#S256`; token ucu reddederse RS256 + `x5t`. Kapsam Graph için
  `https://graph.microsoft.com/.default`, XOAUTH2 için
  `https://outlook.office365.com/.default`. Token kapsam başına bütün işçilerce
  paylaşılır, süresi bitmeden 5 dakika önce yenilenir (yanıtta `expires_in`
  yoksa 10 dakikalık sayılır). Yenileme sürerken öteki işçiler beklemez,
  eski token'ı alır; yenileme başarısızsa 30 saniye yeniden denenmez, eski
  token son 30 saniyesine kadar kullanılır. XOAUTH2'de token beklemesi de
  Graph'taki gibi gönderim bütçesiyle (1 dakika) sınırlıdır. Graph 401 ya da
  başarısız SMTP AUTH token'ı düşürür, sonraki deneme yenisini alır. Token, anahtar ve token ucunun
  açıklama metni hiçbir yere yazılmaz; hata yalnız HTTP durumu, OAuth kodu ve
  AADSTS numaralarıdır.
- Açılış logu her şeridin yolunu ve kutusunu, Reply-To'yu, sertifikanın
  `x5t#S256`'sını ve bitişini yazar; bitişe 30 günden az kaldıysa ya da
  sertifikanın süresi geçtiyse uyarır (açılışı durdurmaz: kapalı şeritler
  etkilenmesin).

`SMTP_REPLY_TO` (isteğe bağlı, ör. `info@yildizskylab.com`) her postaya,
kapalı şeritlerinkine de, `Reply-To` koyar; yoksa `Reply-To` yazılmaz.

**Bounce eşlemesi.** Şeritler her postaya `X-SkyMail-Queue-Id:
<kuyruk id@SMTP_FQDN>` yazar (`Message-ID` ile aynı değer). Graph `sendMail`
`202` döner, gövdesi yoktur: postanın kimliğini ya da `Message-ID`'sini
vermez. `internetMessageId` yalnız taslakta yazılabilir, taslak gönderilince
Gönderilmiş Öğeler'e düşer; bu yüzden Graph'la giden postanın `Message-ID`'si
Exchange'in kendisinindir. NDR geri dönen başlıkları ekler; `internal/dsn`
önce onların `Message-ID`'sine, sonra `X-SkyMail-Queue-Id`'sine bakar
(aynı `<uuid@SMTP_FQDN>` kuralı). Graph yanıtının `request-id`'si satırın
kimliğiyle debug loguna yazılır (Microsoft desteği için); istek
`client-request-id` olarak satırın kimliğini taşır.

**Graph'ın yanıtı** (`internal/mailer/graph.go`): her `2xx` gönderildi
sayılır (Graph `202` döner; başka bir `2xx`'i yeniden denemek postayı iki kez
gönderirdi). SMTP'deki gibi yalnız alıcıya özgü ret kalıcıdır: `400 ErrorInvalidRecipients` (adres Exchange'in
gönderebileceği bir adres değil; hiçbir şey gitmediği için bastırılmaz) ve
`413` (posta istek sınırını aşıyor; 4 MB'ı aşan istek Graph'a hiç
gönderilmez, aynı kalıcı hatayla düşer) satırı hemen düşürür. Öteki her yanıt
(`401`, `403` ör. Mail.Send henüz verilmemiş ya da RBAC kapsamı yayılıyor,
`404`, başka `400`, `429`, `5xx`) yeniden denenir; `429`/`503`'ün `Retry-After`'ı
beklenir (en çok 1 saat; ilk denemelerin 30 sn'si yerine). Satırın hatası yalnız
durum ve koddur, ör. `graph_rejected: 400 ErrorInvalidRecipients`,
`graph_deferred: 429 ApplicationThrottled retry-after 30s`; Graph'ın mesaj
metni adresi tekrarlayabildiği için yazılmaz.

**JSON gövde.** Graph'ın JSON postasında tek gövde vardır: HTML'i olan posta
HTML gider (düz metin alternatifini Exchange kendisi üretir), olmayan düz
metin. Takip pikseli ve bağlantıları SMTP'deki gibi yerleştirilir. HTML
JSON'da kaçırılmaz (`<` olduğu gibi). Exchange `from`'un görünen adını
kutunun dizindeki adıyla ezebilir: kutuların `DisplayName`'i
`SKYMAIL_FROM_*` adlarıyla aynı tutulur.

**`List-Unsubscribe`.** SkyMail bugün bu başlığı yazmaz (bilet 11). `xoauth2`
yolu postayı SMTP yoluyla aynı kodla kurar; başlık eklendiğinde duyuru
kutusundan da gider. Graph JSON'u yalnız `X-` önekli başlık taşır. Şerit
kuyruk önceliğinden türer: bilet 11 başlığı şeride değil gönderim türüne
(liste, grup, onaylı gönderim, toplu bildirim) bağlamalı; duyuru@'dan giden
takipli tekil postaya abonelik başlığı anlamsızdır.

Gönderim bütçesi (`SKYMAIL_RATE_PER_MINUTE`) şeritlerden bağımsız, hepsi için
ortaktır; Exchange'in dakikada 30 ileti sınırı kutu başınadır.

## Geri dönen posta ve bastırma listesi (`mail_bounces`, `mail_suppressions`)

Exchange Online dış alıcıyı `RCPT TO`'da reddetmez, postayı alır ve sonra
gönderen kutusuna bir teslim raporu (NDR, RFC 3464) yollar. Bu raporları
okuyan `internal/bounce` (`Process`) bir raporu kuyruk satırına bağlar ve
kaydeder; raporu kutudan çeken okuyucu ayrı iştir (hub bileti
skymail-bounce-isleme 04), bu sürümde yoktur. Bugün listeyi dolduran yol
relay'in senkron kalıcı reddidir (aşağıda).

- **Ayrıştırma** (`internal/dsn`, yalnız stdlib): üst tür `multipart/report;
  report-type=delivery-status` olmalı; yalnız `message/delivery-status`
  bölümü ve geri dönen postanın başlıkları okunur. Konu, insan metni ve HTML
  hiç okunmaz (Exchange bunları dile göre yazar). Düz metin bounce ve
  otomatik yanıt `dsn_degil` sayılır. `Diagnostic-Code`'dan yalnız yanıt kodu,
  ek kod ve Exchange'in `AS(…)` belirteci alınır, metin saklanmaz.
- **Eşleme** (beşi birden; sahte rapora karşı): geri dönen başlıklardaki
  `Message-ID`, o değilse `X-SkyMail-Queue-Id` (Graph'la giden posta;
  Gönderen şeritleri) (geri dönen başlık yoksa raporun `In-Reply-To`'su, sonra
  `References`) `<kuyruk id@SMTP_FQDN>` olmalı; satır var, `email`, `sent` ve rapor
  `sent_at`'ten sonra gelmiş olmalı (iki saat arasındaki fark için 2 dakika
  pay); rapor satırın adresini (`Final-Recipient` ya da `Original-Recipient`)
  adlandırmalı; rapor daha önce kaydedilmemiş olmalı (kimliğinin SHA-256'sı
  tekil). Kuyruk id'si rastgele UUID olduğu için dışarıdan atılan bir rapor
  satırın id'sini ve alıcısını birlikte bilmeden bir şey bastıramaz. Başka bir
  adresi adlandıran rapor (yönlendirme, takma ad) kaydedilir
  (`recipient_matched = false`), bastırmaz.
- **Sınıf**: `Action: failed` ve ek kod 5.1.0–5.1.6 ya da 5.1.10 (SMTP
  yolundaki kalıcı retle aynı liste) ya da `5.4.1` + `AS(201806281)`
  (Exchange'in Directory-Based Edge Blocking'i: alıcı yok) **kalıcıdır**:
  adres tek seferde `mail_suppressions`'a girer. Öteki her ret (5.2.x dolu
  kutu, 5.7.x politika, 4.x.x süre aşımı) yumuşaktır, `delayed` gecikmedir:
  ikisi de yalnız `mail_bounces`'a yazılır. Kendiliğinden düşme yoktur.
- **Kayıt**: `mail_bounces` kuyruk satırına bağlıdır (`ON DELETE CASCADE`),
  adres ve metin tutmaz: tür, `Action`, ek kod, SMTP kodu, `AS` belirteci,
  zaman, rapor kimliğinin SHA-256'sı. Bir yıl saklanır (içerik süpürmesi her
  5 dakikada eskileri siler). `mail_suppressions` küçük harfli adres (anahtar),
  biliniyorsa `recipient_sub`, kaynak (`dsn` ya da `smtp`), kod, ilk/son
  görülme, sayı ve son kuyruk satırıdır. Adres satırın kendi adresidir,
  raporunki değil.
- **Hesap silme**: kişinin bastırılmış adresleri (adresle, `sub`'la ya da son
  reddedilen postası kişinin olanla) silinir, sayaç `suppressions_deleted`.
  Bounce satırı kişinin temizlenen kuyruk satırıyla kalır, adres tutmaz.
  Kayıt satırı kilitleyerek (`FOR SHARE`) yazar: silme ile aynı anda gelen
  rapor ya onu bekler ve satırı boş bulur ya da silme onu bekler ve yazdığı
  bastırmayı siler.
- **Senkron ret**: relay'in adrese özgü kalıcı reddi (`smtp_rejected`,
  Kanallar tablosu) satırı `failed` yazdıktan sonra adresi de bastırır
  (`source = smtp`, kod `RCPT TO 550 5.1.1` biçiminde) ve `mail_bounces`'a
  `smtp` kaydı düşer. Geçici retler (`smtp_deferred`) bastırmaz.
- **Kuyruğa alırken**: bastırılmış adresteki alıcı (büyük/küçük harf ve baştaki
  boşluk fark etmez) `failed` yazılır, `error = suppressed: <kaynak> <kod>`
  (ör. `suppressed: dsn 5.1.10`), deneme 0; relay'e ve bütçeye gitmez,
  gönderimin öteki alıcıları kuyruğa girer. Ayrılmış alan adı gibi atlanmış
  sayılır: gönderimin durumunu "Başarısız" yapmaz (`mail_task_status()`,
  `20261021090100`); bütün alıcıları atlanan gönderim (bastırılmış adrese tek
  posta) "Başarısız"dır. Tek posta (`/single`) reddedilmez, kabul edilir.
- **Muafiyet (yalnız hesap postaları)**: şablon anahtarı `keycloak.*` ya da
  `account.*` olan **ve** sistem şablonu (`system = true`, Template seed'inki)
  olan gönderim (giriş/kayıt kodu, e-posta doğrulama, şifre sıfırlama)
  bastırılmış adrese yine gider; anahtar tek başına yetmez (önekler ayrılmış
  değil, sistem olmayan bir `account.x` şablonu bastırılır); satır `suppressed_but_sent = true`,
  bastırma satırı `exempt_flagged_at` alır. Forms, Mail onayı, listeler ve
  öteki her şey bastırılır. Muafiyet şeritle değil anahtarla: sistem
  şeridinde Forms ve Mail onayı bildirimleri de var.
- **Sayılar**: `GET /v1/mail_tasks` ve `/{id}` `recipient_counts`'ta
  `suppressed` (kuyruğa alırken bastırılan, `failed` içinde de),
  `suppressed_but_sent` ve `undelivered` (gönderildi, sonra rapor adresin
  olmadığını söyledi; `sent` içinde de) döner. `GET
  /v1/mail_tasks/{id}/queue` her satırda `suppressed_but_sent` ve
  `undelivered` döner.
- **Operatör uçları** (`skymail:suppressions:manage`, `skymail` istemci
  rolü, gruba verilir): `GET /v1/suppressions?q=&_start=&_end=` (son
  ret en yeni önce, `q` adresin bir parçası, sayfa en çok 100, `X-Total-Count`)
  ve `DELETE /v1/suppressions/{id}` (`204`; yoksa `404`). Satır listenin opak
  `id`'siyle adlandırılır, adres hiçbir URL'de olmaz (hata logu yolu yazar).
  Kaldırma `mail_suppression_removals`'a kim, ne zaman, kaynak, kod, sayı ve
  ilk görülme olarak yazılır; adres yazılmaz. Kaldırılan adrese yeniden
  gönderilir. Kendiliğinden düşme yoktur.
- **Loglar**: adres ve rapor metni yazılmaz; kuyruk id'si, kaynak ve kod
  yazılır.

## Bildirimler (`POST /v1/notifications`)

core kişiye (Keycloak `sub`) bir Bildirim türüyle yazar; SkyMail kişi başına
kanalı seçer (tür varsayılanı ∩ core'un izin verdiği kanallar ∩ adres) ve
kuyruğa alır. `Idempotency-Key` zorunlu; yalnız servis hesabı ve
`skymail:notifications:send`. Türleri Template seed yazar
(`PUT /v1/notification_types/{key}`). Bugün tek kanal e-posta. Sözleşme:
[`docs/notifications.md`](docs/notifications.md).

## Mail kuyruğu kirası (`MAIL_QUEUE_LEASE`)

Dağıtıcı bir kuyruk satırını, kanalı ne olursa olsun, aldığında satır o
sürecin olur: `claimed_by`
süreci (host adı + açılışta seçilen rastgele ek), `claimed_at` alış anını
yazar. Süreç yalnız boş işçisi kadar satır alır. `processing` bir satır ancak
kirası dolunca (`claimed_at` kiradan eski) `pending`'e geri döner: açılışta ve
sonra kiranın beşte birinde bir. Dokploy'un start-first deploy'unda eski görev
o an gönderdiği satırları bitirir, yeni görev onlara dokunmaz (çift e-posta
olmaz). Gönderimin sonucunu (gönderildi, başarısız, yeniden dene) yalnız satırı
tutan süreç yazar; kirası dolmuş ve satırı başkasına geçmiş süreç hiçbir şey
yazmaz, bir uyarı logu bırakır.

`MAIL_QUEUE_LEASE` bir Go süresidir (`10m`, `15m`, `1h`); yoksa `10m`. `5m`'den
kısa, `24h`'ten uzun ya da okunamayan değer açılışı durdurur. Bir SMTP
gönderimi bağlantıdan QUIT'e (karşılama, EHLO, STARTTLS, AUTH, DATA dahil) en
çok 1 dakika sürer: bağlantının deadline'ı go-mail'in kendi deadline'larıyla
uzamaz. Böylece kira her zaman gönderimden uzundur. Ölen bir sürecin satırları
en geç kira + kiranın beşte biri sonra başka süreçle gider. Kira uzatma yoktur:
süreç satırı ancak bir işçi göndermeye hazırken alır. Sonuç yazımı
`claimed_by` ile birlikte `claimed_at`'e de bakar: aynı süreç satırı kirası
dolduktan sonra yeniden almışsa eski talebin sonucu da yazılmaz.

## Öncelik şeritleri (`mail_queue.priority`)

Her kuyruk satırı, kanalından bağımsız, bir şerittedir. `1` sistem şeridi: `POST /mail_tasks/single`
ile gelen tekil postalar (giriş kodu, sertifika, Forms, Mail onayı
bildirimleri). `0` toplu şerit: listeye ve Keycloak grubuna gönderim, onaylı
gönderimler (listeye de kişilere de). Dağıtıcı her turda önce sistem şeridinden
alır. Toplu şerit bir sürecin işçilerinin en çok %70'ini (aşağı yuvarlanır, en
az 1) tutar: 3 işçide 2. Böylece 300 kişilik bir duyuru giderken gelen giriş
kodu boş kalan işçiyle, kuyruğa girdiği andaki uyandırmada gider; hiçbir toplu
gönderimin bitmesini beklemez. Şerit belirtmeden yazılan satır (göçten önceki
satırlar, eski imajın yazdıkları) sistem şeridindedir: bugünkü sıradan daha
geride kalamaz. Çoklu replikada her süreç kendi işçilerini ayrı ayırır.
Kısma ve duraklatma henüz yok (duyuru-haber bilet 05, sonraki adımlar).

Her şeridin bekleyen satırlarının kendi kısmi indeksi var
(`idx_mail_queue_pending_system`, `idx_mail_queue_pending_bulk`): bir tur
yalnız alacağı satırları okur, binlerce toplu satır sistem şeridinin turunu
yavaşlatmaz.

## Gönderim bütçesi (`SKYMAIL_RATE_PER_MINUTE`, `SKYMAIL_DAILY_RECIPIENT_BUDGET`)

Röle sınırları tüm süreçler için birlikte tutulur. Production rölesi
`smtp.office365.com`: Exchange Online'ın genel sınırları dakikada 30 mesaj ve
günde 10.000 alıcı. Varsayılanlar bunların altında kalır.

| Değişken | Varsayılan | Anlamı |
|---|---|---|
| `SKYMAIL_RATE_PER_MINUTE` | `25` | Dakikada en çok bu kadar posta (iki şerit birlikte) |
| `SKYMAIL_DAILY_RECIPIENT_BUDGET` | `9000` | Günde en çok bu kadar alıcı (iki şerit birlikte) |

- Değer tam sayıdır. `0` o sınırı kapatır, ikisi de `0` ise bütçe yoktur
  (bütçeden önceki davranış). Başka bir değer açılışı durdurur.
- Sayaçlar Postgres'te (`mail_send_windows`): dakika ve saat pencereleri,
  şerit başına alınan satır sayısı. Bütün replikalar aynı satırları okur ve yazar,
  zaman olarak veritabanının saatini kullanır.
- Toplu şerit her sınırın en çok %70'ini alır (25'te 17, 9.000'de 6.300). Bir
  süreç toplu satırı yalnız `skymail-backend send budget` advisory lock'u
  altında alır: pencereleri okur, kalan yer kadar satır alır, sayar, commit
  eder. Sonraki süreç bu sayıyı görür.
- Sistem şeridi (`/mail_tasks/single`) bütçe yüzünden hiçbir zaman beklemez ve
  düşmez: satırları her zaman alınır, işçiye verildikten sonra sayılır. Payını
  aşarsa toplu şerit o kadar geri çekilir.
- Dakika: önceki dakika, şimdiye bir dakika içinde kalan kısmı kadar sayılır
  (kayan pencere tahmini). Böylece bir liste payını bir dakikanın sonunda ve
  bir sonrakinin başında iki kez göndermez, dakikaya yayılır. Exchange
  dakikalık aşımı yalnız geciktirir.
- Gün: saat saat sayılır ve tahmin edilmez: son 24 saate giren her saatin
  toplamı (24 saat önceki anı içeren saat dahil, en çok bir saat fazla sayar,
  hiç eksik saymaz). Exchange'in alıcı sınırı kayan 24 saattir ve aşılınca
  hesap, giriş kodları dahil, hiçbir şey gönderemez.
- Bir süreç bütçe kilidini tutarken öteki süreçler onu beklemez
  (`pg_try_advisory_xact_lock`): toplu şerit o turu atlar, sistem şeridi
  hiç etkilenmez.
- Bütçe dolunca toplu satırlar `pending` kalır, dağıtıcının 10 saniyelik
  turunda yer açıldıkça gider. 190 kişilik bir duyuru dakikada 17 ile yaklaşık
  11 dakika sürer.
- Bir satır alındığında sayılır. Yeniden deneme yeniden sayılır. Kapanışta
  geri verilen satırın sayısı düşülmez. İki günden eski pencereler silinir.
  Ayrılmış alan adlı alıcı hiç alınmadığı için sayılmaz; kalıcı olarak
  reddedilen alıcı yalnız tek denemesiyle sayılır (Kanallar).

## Gönderilen içerik (`MAIL_CONTENT_RETENTION_DAYS`)

Her kuyruk satırı alıcısının postasını (konu, düz metin, HTML) tutar.
`GET /v1/mail_tasks/{id}/queue/{itemId}/content` bunu gösterir
(`Cache-Control: no-store`); `access` kimin neyi gördüğünü söyler, alanlar
yalnız `available` ya da `masked` iken doludur:

| Gönderim | Gönderen | `skymail:mails:write` | `skymail:system-mails:read` | Diğer `skymail:mails:read` |
|---|---|---|---|---|
| Hassas, toplu (liste, grup, onaylanmış istek) | maskeli | maskeli | maskeli | `restricted` |
| Hassas, tekil (`/single`; kod, tek kullanımlık bağlantı) | maskeli | `restricted` | maskeli | `restricted` |
| Liste, grup ya da onaylanmış istek (toplu) | görür | görür | görür | `restricted` |
| Tekil posta (`/single`), şablonu görünür diyorsa | görür | `restricted` | görür | `restricted` |

- **Yazarken maskeleme (Yusuf 2026-10-13 "kod/bağlantı maskeli, gerisi
  açık"; #63 incelemesinden sonra orkestratör kararı):** hassas gönderim
  bitince (satırı kalmayınca, ≥5 dk) süpürücü her satırın postasını maskeli
  kopyasıyla, gönderimin `body_variables`'ını maskeli değerlerle
  **değiştirir**; ham kod/bağlantı saklanmaz, okurken maskeleme yoktur. Kod,
  tek kullanımlık bağlantı, IP adresi ve konum `••••••` olur (HTML'de
  `<mark data-skymail-masked="">`). Önce değişkenler (ad, biçim, metin
  içinde) maskelenip posta **gönderim anında yürürlükteki** şablonla (o
  zamandan beri yazılmamış satır ya da o ana kadar yayımlanmış en yeni sürüm)
  yeniden render edilir; birebir tutmazsa desen yedeği. Son denetimde bir
  kimlik bilgisi kalırsa içerik saklanmaz (`not_kept`). Maskelenene kadar
  `pending` (gösterilmez). Her maskeli açılış `sent_content_views`'a yazılır;
  yazılamazsa içerik dönmez. Ayrıntı:
  [`docs/data-lifecycle.md`](docs/data-lifecycle.md#sent-mail-content).
- Hassaslık gönderim anında `mail_tasks.content_sensitive_at_send`'e
  kopyalanır; anahtarı o anda kilitliyse şablon ne derse desin `true`
  yazılır. Gönderim, bu kopya **ya da** şablonun şimdiki değeri ya da
  kilitli anahtar hassas diyorsa hassastır; okuma ve süpürücü önce kopyaya
  bakar. Şablonu sonradan `false` yapmak da anahtarını değiştirmek de eski
  gönderimi açmaz.
- **Kilitli hassas:** `keycloak.*`, `account.*`, `auth.*`, `login.*`,
  `otp.*`, `core.contact-consent-confirm`, `event.ticket-created`,
  `event.reminder` ve anahtarında `login`, `code`, `otp`, `reset`, `verify`,
  `confirm`, `token`, `magic`, `invite`, `ticket`, `link`, `giris`, `kod`,
  `dogrulama`, `sifre`, `parola`, `baglanti`, `davet`, `onay` gibi bir sözcük
  geçen her şablon. Go listesi `internal/handlers/content_sensitivity.go`,
  veritabanınınki `template_key_content_locked()`; bir test ikisini eşit
  tutar.
- **Yeni tekil entegrasyonlar için sözleşme:**
  - Şablon, kimlik bilgisi (kod, tek kullanımlık bağlantı, parola cümlesi)
    taşıyan değişkenlerini Template seed'de `sensitive_variables` ile
    listelesin (`PUT /v1/templates/by-key/{key}`, üst düzey adlar, en çok 50;
    gönderilmezse var olan liste kalır, `[]` siler). Süpürücü listedeki her
    değişkeni, adı ve biçimi ne olursa olsun, altındakilerle birlikte bütünüyle
    maskeler: yalnız harften kod (`kxqpzmwt`), parola cümlesi
    (`elma-armut-kiraz-nar`), 3 haneli kod da. Listede olmayanlar yine ad ve
    biçimle maskelenir.
  - Liste yoksa (eski seed) yalnız ad ve biçim: değişkenin adı `link`, `url`,
    `code`, `token`, `otp`, `pin`, `password`, `secret`, `key`, `invite`,
    `verifier`, `verification`, `dogrulama` ya da `passphrase` ile bitsin.
  - Şablon anahtarı kilitli bir sözcük taşısın.
- `not_kept`: hassas gönderimin içeriği boşaltılmış (2026-10-13 11:16'dan
  yazarken maskelemeye kadar; #61 öncesi gönderimler dahil; ya da güvenle
  maskelenemedi). Geri gelmez.
- **Maskelenemeyen gönderim:** maskeleme hata verirse gönderim geri atılır
  (5 dk, her hatada ×2, en çok 1 gün). Hata yalnız **aynı turda** başka bir
  gönderim maskelendiyse sayılır ve yalnız o turun hatası sayılır: hiçbir
  şeyin maskelenmediği turların (SkyMail çapında arıza) hataları arıza
  bitince toptan sayılmaz. 3 sayılmış hatada içerik boşaltılır. Sayılsın
  sayılmasın, ilk hatasından **7 gün** sonra hâlâ maskelenemeyen hassas
  gönderim de boşaltılır (`mail_tasks.content_mask_failing_since`); kuyrukta
  yalnız bozuk gönderimler kalsa da ham içerik saklama süresi boyunca durmaz.
- **Alarm:** her boşaltma error düzeyinde bir satır yazar:
  `"event":"sensitive_content_mask_attention","reason":"deadline"` (7 gün)
  ya da `"reason":"failures"` (3 hata), `task_id` ile; içerik yazılmaz. Her
  başarısız deneme warning düzeyinde
  `"event":"sensitive_content_mask_failed"` yazar. Olayın adı ve alanları
  (`event`, `reason`, `task_id`) sözleşmedir, değiştirilmez: ADMIN'e otomatik
  mail ops tarafında bu satırı okuyan bir alarmla gelecek (Yusuf, 2026-10-14);
  SkyMail kendisi mail atmaz. O gelene kadar production sunucusunda haftalık
  sayım (salt okunur):

```bash
docker service logs --raw --since 168h sky-lab-production-skymail-tvrjzs 2>&1 | grep -F '"event":"sensitive_content_mask_' | grep -o '"event":"[^"]*"\(,"reason":"[^"]*"\)\?' | sort | uniq -c
```

- `skymail:system-mails:read` SkyMail istemcisinin rolüdür (yönetici: YK, DK,
  ADMIN).
- Süpürücü beş dakikada bir önce hassasları maskeler, sonra
  `MAIL_CONTENT_RETENTION_DAYS` (varsayılan `3650`, 1–3650) dolmuş
  gönderimlerin içeriğini boşaltır, sonra
  `SENT_CONTENT_VIEWS_RETENTION_DAYS`'ten (varsayılan `730`, 1–3650; Yusuf,
  2026-10-14: 2 yıl) eski `sent_content_views` satırlarını siler. Geçersiz
  değer açılışı durdurur.
- **Geri dönüş:** #61 imajına dönmek ilk süpürmede bütün hassas içeriği
  boşaltır (güvenli, geri gelmez). `20261013140000` down migration'ı
  koşturulmaz (açılış kaydını siler).

## Açılma ve tıklama takibi (`SKYMAIL_TRACKING`)

Kişi bazında açılma ve tıklama takibi (hub bilet iletişim-servisi 14, tasarım
notu 07). **Varsayılan kapalı.** Bu sürümde takip verisi, SMTP anında
yerleştirme, kimliksiz piksel/tıklama, yetkili okuma uçları, core'dan geri
alma ucu ve saklama işi (14c) vardır. core tercihleri/rolleri (16/17) ve
aydınlatma metni (18) tamamlanmadan hiçbir ortamda bayrak açılmaz;
production açılışı bilet 19'dadır. `on` geçerli adreslerle başlatılabilir,
ancak bu bir açılış izni değildir.

**Saklama süresi (Yusuf, 2026-10-19): kişi bazındaki açılma ve tıklama
kayıtları alıcının postasının gittiği andan (`sent_at`) 1 yıl
(`tracking.EventRetentionDays` = 365) tutulur** (tasarım notu 90 gün
diyordu). Olay yazımı, raporlar ve saklama işi aynı sabiti kullanır.

| Değişken | Varsayılan | Anlamı |
|---|---|---|
| `SKYMAIL_TRACKING` | `off` | `off` ya da `on`; başka değer açılışı durdurur |
| `SKYMAIL_TRACKING_BASE_URL` | yok | `/t/`'nin herkese açık adresi (`https://api.yildizskylab.com/api/skymail`); yalnız https, kimlik bilgisi, sorgu ve parça yok; `on` iken zorunlu |
| `CORE_URL` | yok | core'un Docker ağındaki adresi (`http://core-backend:8080`); `on` iken zorunlu |
| `SKYMAIL_TRACKING_RETENTION` | `off` | saklama işi: `off`, `dry-run`, `apply` ya da `purge`; başka değer açılışı durdurur; `purge` için `SKYMAIL_TRACKING=off` zorunlu |
| `SKYMAIL_TRACKING_RETENTION_MAX_ROWS` | `50000` | saklama freni: bir apply'da kapanacak belirteç, silinecek olay ya da boşalacak bağlantı sınırı (1–10.000.000); başka değer açılışı durdurur |

Hatalı adres, değeri değil değişkeni söyleyerek açılışı durdurur. Açılışta
`open and click tracking (SKYMAIL_TRACKING)` satırı kipi ve adresleri,
`open and click tracking retention (SKYMAIL_TRACKING_RETENTION)` satırı
saklama kipini, `event_retention_days` ve `max_rows` değerini yazar.

- **Kim takip edilir (gönderim kuyruğa yazılırken sabitlenir):** bayrak açık;
  gönderim `tracking: true` istedi (`POST /v1/mail_tasks`, bir kişinin
  `POST /v1/mail_tasks/single` gönderimi, listeye ya da kişilere Mail onayı
  isteği); satır toplu şeritte; şablon System değil ve hassas değil
  (`mail_task_content_sensitive()`, içerik panelinin tek tanımı; kişilere
  giden gönderimde içeriği hakkında bir şey söylemeyen şablon hassas
  sayılır). Bayrak kapalıyken ya da şablon System/hassasken alan yok
  sayılır: yanıt, gönderim ve onay isteği `tracking: false` der, satırlara
  neden yazılmaz. Sonra alıcının Keycloak `sub`'ı biliniyor
  (`recipient_sub`: Keycloak grubu üyeleri ve kişilere tek tek gönderimde
  adresten bulunan hesap, aşağıda) ve core izin veriyor.
  core'a gönderimin veritabanı işlemi **başlamadan önce** sorulur (Mail
  onayında istek kilitlenmeden önce, grup üyeleriyle birlikte); SkyMail core'u
  hiçbir zaman işlem ya da kilit tutarken beklemez. O yanıtta olmayan kişi
  (sorulmamış, ör. yanıt gelmemiş) takip edilmez.
  Takip edilen satır 16 baytlık rastgele bir belirteç alır
  (`mail_queue.tracking_token`); edilmeyen satır nedenini
  (`tracking_skip_reason`): `no_account`, `preference_off`, `not_member`,
  `inactive`, `not_allowed`, `preference_unavailable`, `flag_off`. Gönderim
  takip istemediyse ya da takip edilemezse (`tracking: false`) ikisi de
  boştur. (`transactional` kısıtta duruyor, yazılmıyor.)
- **Kişilere tek tek gönderim (Yusuf, 2026-10-20):** gönderim formunun
  kişileri (`/single`) ve kişilere Mail onayı isteği de takip isteyebilir;
  her alıcı yukarıdaki kuralla tek tek değerlendirilir. Bu kişiler adresle
  gelir; hesapları gönderimin veritabanı işlemi **başlamadan önce** (onayda
  istek kilitlenmeden önce) Keycloak'ta aranır: realm'in tam e-posta araması
  (`GET /admin/realms/<realm>/users?email=…&exact=true&max=2`), yalnız
  birincil `email` alanı, büyük/küçük harf duyarsız tam eşitlik, kullanıcı
  etkin ve `emailVerified=true`. Tam bir eşleşme yoksa (ya da birden fazla
  varsa) `no_account`; okul adresi gibi ikincil öznitelikler okunmaz. İnternal
  listenin ve grubun adres-yalnız alıcıları aranmaz (`no_account`). Realm
  `duplicateEmailsAllowed=false` (e-skylab-keycloak `account-center-realm.json`,
  uzlaştırıcı doğrular). Arama en çok 5 eşzamanlı, gönderim başına toplam
  5 saniye; Keycloak'ın yanıtlamadığı adres `preference_unavailable` alır,
  gönderim durmaz (`"event":"tracking_accounts_unavailable"` satırı adres
  yazmaz, yalnız sayar). Bulunan hesap satırın `recipient_sub`'ı olur ve core
  ona grup üyeleri gibi sorulur; geri alma ve hesap silme bu satırları da
  kapsar. Servis hesabı Keycloak'ta kullanıcı okuyabilmeli (`view-users`;
  `GET /users/{id}` için zaten gerekiyor). Takip isteyen ve alabilen
  `/single` gönderimi toplu şeride girer (operatör postası); sistem şeridi
  hiçbir zaman takip edilmez. Bir servisin token'ıyla gelen `/single`
  (Keycloak, core, Forms: hesap ve işlem postaları) takip istese de
  `tracking: false` alır, sistem şeridinde kalır. Forms kaynağı gönderimi
  değişmedi: takip istemez, `tracking: false` der.
- **core'a ulaşılamazsa** (5 saniye, hata ya da sözleşme dışı yanıt) o
  gönderimde kimse takip edilmez (`preference_unavailable`), gönderim durmaz.
  Uyarı satırı `"event":"tracking_preferences_unavailable"` (`"cause":"core"`
  ya da sorulmadıysa `"not_asked"`) gönderim kimliğini ve kişi sayısını yazar,
  kimseyi adlandırmaz. System ya da hassas şablonun gönderiminde core'a hiç
  sorulmaz.
- **Bağlantılar:** takip edilen satırı olan gönderimde, bütün alıcıların
  postasında aynı olan `http`/`https` bağlantıları (sırayla, en çok 100)
  kendi 8 baytlık belirteçleriyle `mail_task_links`'e yazılır. Değişkenle
  kişiselleşmiş bağlantı (alıcıdan alıcıya değişen), `mailto:`, `tel:`, `#…`,
  göreli adres, kullanıcı bilgili ya da 2048 karakterden uzun adres, boşluk
  ya da denetim karakteri taşıyan adres, yolunda `communication-preferences`
  ya da `unsubscribe` geçen adres (core'un çıkış bağlantısı, my.'nın tercih
  sayfası), SkyMail'in kendi `/t/` adresleri ve `data-skymail-notrack`
  öznitelikli bağlantı kaydedilmez, postada olduğu gibi kalır.
  `List-Unsubscribe` başlığına dokunulmaz. **İstisna:** tek alıcılı (ya da
  tek postası işlenebilen) gönderimde karşılaştıracak başka posta olmadığı
  için değişkenle kişiselleşmiş bağlantılar da kaydedilir ve yeniden yazılır;
  `mail_task_links.url` o kişiyi adlandırabilir. Hesap silme, yalnız kişiye
  giden gönderimin bağlantılarını boşaltır.
- **Yerleştirme SMTP anında:** saklanan `body_html` değişmez (içerik paneli
  pikselsiz gösterir; operatörün bakması alıcı adına açılma sayılmaz).
  Belirteci açık satırın postası relay'e giderken `<body>`'nin hemen
  arkasına 1×1 piksel (`<base>/t/o/<belirteç>`) alır, kayıtlı bağlantılar
  `<base>/t/c/<belirteç>/<bağlantı>` olur; başka hiçbir bayt değişmez,
  altlığa cümle ya da bağlantı eklenmez (T8). Düz metin değişmez. Belirteci
  kapalı satır (`tracking_disabled_at`) temiz gider. Bayrak kuyruğa yazıldıktan
  sonra kapatıldıysa posta temiz gider ve belirteç `flag_off` ile kapanır.
  `mail_queue.sent_at` relay'in postayı aldığı an.
- **Okuma:** `GET /v1/mail_tasks/summary` → `tracking_available` (gönderim
  formu kutuyu yalnız bu `true` iken gösterir); gönderim satırları, Mail
  onayı isteği ve `POST /v1/mail_tasks/single` yanıtı `tracking` alanı taşır.
- **Hesap silme** kişiye giden satırların olaylarını siler, belirteçlerini
  kapatır, kişiyi adlandıran (yazıldığı gibi ya da URL çözülmüş) ya da yalnız
  ona giden gönderimin bağlantılarının adresini boşaltır; sayaçlar
  `tracking_events_deleted`, `tracking_links_cleared`
  ([`docs/data-lifecycle.md`](docs/data-lifecycle.md#open-and-click-tracking)).

### Piksel, tıklama ve raporlar

Kimliksiz uçların OpenAPI sözleşmesi: [tracking-public.openapi.yaml](docs/tracking-public.openapi.yaml).
Yetkili okuma uçları ana `/docs` belgesindedir.

- `GET|HEAD /t/o/{rt}` daima aynı 43 bayt GIF ve önbelleksiz başlıkları
  döner; geçersiz, kapalı, silinmiş, eski ve DB hatası ayırt edilmez.
  `HEAD` açılma sınıflandırılır (`scanner_likely`); `HEAD` **tıklama** hiçbir
  olay yazmaz.
- `GET|HEAD /t/c/{rt}/{lt}` yalnız kayıtlı hedefe 302 döner. Alıcı belirteci
  geçersiz, kapalı veya başka gönderime ait olsa da bağlantı çalışır; olay
  yazılmaz. Geçersiz bağlantı 404, silinen hedef 410 “kaldırıldı”, bir yıllık
  hedef 410 “süresi doldu” olur. DB kesintisi 503 olur; yanıtlar `no-store`
  ve `Referrer-Policy: no-referrer` taşır. Bayrak kapalıyken de yönlendirme
  çalışır. İstekten hedefe hiçbir parça eklenmez.
- SQL, alıcı satırı kilidi altında tüm replikalar birlikte açılmayı 5
  dakikada 1/toplam 20; tıklamayı bağlantı başına dakikada 1/alıcı başına
  toplam 50 ile sınırlar. Teslim edilmemiş veya `sent_at + 1 yıl` dolmuş
  satır olay alamaz; imha işinin çalışması beklenmez. Süreç başına kayıt
  kovası 50 olay denemesi/sn (patlama 50), en çok 8 eşzamanlı DB işlemi,
  işlem başına 500 ms bütçe ve 2048 girdilik/30 sn negatif önbellek vardır.
  Kova dolunca piksel ve kayıtlı yönlendirme yanıtı değişmez, olay atlanır.
- Sınıflandırıcı sürümü `1`: UA sınıfı ve teslim zamanı sezgileri tasarım
  notu 07 §3.2'deki gibidir. 5 sn içinde 3 farklı bağlantıya tıklamada aynı
  sürümdeki bütün patlama `scanner_likely` olur. IP hiç okunmaz; ham UA ve
  Referer saklanmaz. `/t/o` ve `/t/c` hata/bozuk yol dahil kendi yanıtını
  üretir: günlük yalnız uç adı, sonuç sınıfı ve süre taşır.
- `GET /v1/mail_tasks/{id}/tracking` (`skymail:mails:read`) yanıtı
  `{tracking_available, tracking_requested, retained, report}`. Raporda
  `tracked_recipients`, `untracked_by_reason`, `unique_opens`,
  `unique_likely_human_opens` (otomatik olmayan tıklamadan çıkarım dahil),
  `automated_only_recipients`, `unique_clickers`, `clicks_by_link`
  (`id`, `position`, `clicks`, `unique_clickers`) ve `series` vardır.
  Seri noktaları UTC'de ilk 72 saat saatlik, sonra günlük; `at`, `interval`,
  `likely_human_opens`, `automated_opens`, `likely_human_clicks`,
  `automated_clicks` taşır. Boş kovalar yazılmaz. Kimlik ve hedef URL yoktur
  (tek alıcılı hedef kişiyi adlandırabilir). Son postadan 1 yıl sonra
`retained: true`:
  yalnız ≥10 alıcılı kişisiz toplam, yoksa `report: null`; seri dönmez.
- `GET /v1/mail_tasks/{id}/tracking/recipients` rolü
  `skymail:mails:tracking:read` **ya da** `sent_by = token sub` ister;
  `mails:read` tek başına yetmez, gönderen için ayrıca gerekmez. Parametreler
  `filter=opened|not_opened|clicked|automated_only`, `page` (1–1000000) ve
  `page_size` (1–100, varsayılan 25). Yanıt
  `{tracking_available, page, page_size, total, items}`; kişi satırı `id`,
  `recipient_sub`, `recipient_email`, `recipient_full_name`, `tracked`,
  `tracking_skip_reason`, `first_open_at`, `first_likely_human_open_at`,
  `likely_human_open_source` (`open|click_inferred|null`), `open_count`,
  `last_clicked_at`, `automated_only`, `source_classes`, `clicked_links`
  (`link_id`, `clicks`, `last_clicked_at`) içerir. “opened” açılma veya
  otomatik olmayan tıklama çıkarımını, “not_opened” ikisinin yokluğunu
  belirtir; “okumadı” anlamına gelmez. Token/hedef URL dönmez. Gönderimin
  son `sent_at`'ından (teslim yoksa `created_at`) 1 yıl sonra 410 döner
  ("Kişi bazında takip kaydı 1 yıl sonra silindi.");
  bu tarihten önce de her alıcının kendi 1 yıllık pencere dışı olayları
  gizlenir. İki okuma ucunun bütçesi 2 sn, önbelleği `no-store`'dur.

### Geri alma: `PUT /internal/v1/tracking-withdrawals/{sub}` (core'dan)

Kişi my.'da takibi kapatınca (`enabled=false`) core bu ucu çağırır (bilet 16;
tek yön core → SkyMail, postada kapatma bağlantısı yok, T8). Hesap silme
ucunun kalıbı: `/v1` dışında, yalnız iç Docker ağından (`X-Forwarded-*`,
`Forwarded`, `X-Real-Ip` taşıyan istek çıplak `404`); token realm'in JWKS'i
ile yerelde doğrulanır ve `azp` = `core` (core'un SkyMail'i zaten çağıran
istemcisi), `aud` ∋ `skymail`, `resource_access.skymail.roles` ∋
`skymail:tracking:withdraw` ister. `core-erasure`'ın `skymail:account:erase`
rolü bu uca **yetmez** (`403`). Çağıranın kendi erişim marker'ı her uçtaki
gibi denetlenir.

```http
PUT {SKYMAIL_URL}/internal/v1/tracking-withdrawals/7d3c1f6e-0b9a-4c55-9a51-3f4d2b8e6a10
Authorization: Bearer <core istemcisinin client credentials token'ı>
```

- Gövde yoktur (gönderilirse okunmaz). `{sub}` küçük harfli, tireli UUID;
  başka yazım ya da Silinmiş kullanıcı `400 invalid_withdrawal_subject`.
- Başarı **`204`**, gövdesiz; idempotent: tekrar çağrı da `204` döner,
  hiçbir şey değiştirmez. Bayrak açık ya da kapalı fark etmez.
- Etki, tek işlemde: kişinin `recipient_sub`'ına giden bütün takipli
  satırlar `ORDER BY id FOR UPDATE` kilitlenir, belirteçleri kapanır
  (`tracking_disabled_at`; gönderilmiş ve kuyrukta bekleyen postalar dahil,
  bekleyen posta pikselsiz gider), sonra açılma ve tıklama olayları silinir.
  Bağlantılara dokunulmaz: eski postaların bağlantıları yönlendirmeye devam
  eder. Daha önce yazılmış kişisiz toplamlar değişmez. Sonraki gönderimlerde
  core `tracking:resolve` ile `preference_off` der.
- Hatalar `application/problem+json`, sabit `code` ile: `401
  withdrawal_unauthorized`, `403 withdrawal_forbidden`, `503`
  (`token_keys_unavailable`, `access_gate_unavailable`,
  `withdrawal_store_unavailable`) `Retry-After: 30` ile; core'un tekrar
  denemeli işi 5xx'te yeniden dener.
- Günlük yalnız `"event":"tracking_withdrawal"` satırında sayılar
  (`tokens_closed`, `events_deleted`) ya da reddin sabit kodu; `sub` hiçbir
  yerde yazılmaz ya da yanıtta geri dönmez.
- Bilinen sınır: core'a gönderim kuyruğa yazılmadan hemen önce sorulur;
  geri alma o birkaç saniyelik aralığa denk gelirse o gönderimde kişi yine
  takipli kuyruğa girebilir (bir sonraki geri alma ya da hesap silme onu da
  kapatır).

### Saklama (`SKYMAIL_TRACKING_RETENTION`)

ADR-0062'nin imha işi kalıbı (core `RETENTION_SWEEP_MODE` ile aynı):
`off` (varsayılan, hiçbir şey koşmaz), `dry-run` (her kuralı çalıştırır,
işlemi geri alır: yalnız sayar), `apply` (değiştirir), `purge` (tam geri
dönüş, aşağıda). Açılışta `dry-run`; 1–2 dry-run makbuzu okunur okunmaz
`apply`'a alınır (bilet 19). Bir yıl beklenmez: D7 yalnız `apply`'da
çalışır, ilk yıl apply yalnız D7'ye dokunur.

- **Zamanlama:** açılışta ve saatte bir "zamanı geldi mi" denetlenir; aynı
  kipte son 23 saatte `ok`/`partial`/`refused_large`/`would_refuse_large`
  biten bir koşu yoksa koşar. Bu denetim advisory lock alındıktan **sonra**
  yapılır: iki replika aynı gün iki kez koşmaz. Durum veritabanındaki
  makbuzlardan okunur: yeniden başlatma ya da yayın koşuyu tekrarlamaz; kip
  değişince yeni kip hemen koşar.
- **Tek koşu:** oturum advisory lock'u (`skymail-backend tracking
  retention`); kilidi bulamayan replika hiçbir şey yapmaz, makbuz yazmaz.
- **Kurallar** (gönderim başına tek işlem; önce gönderimin takipli kuyruk
  satırları `ORDER BY id FOR UPDATE`, sonra bağlantılar, sonra olaylar;
  hesap silmenin kilit sırası):
  1. alıcılarından birinin postası 1 yıldan eskiyse, gönderimde en az 10
     takipli alıcı varsa ve henüz yoksa **önce** kişisiz toplam
     (`mail_task_tracking_totals`) gönderimin bütün olaylarından yazılır.
     "Takipli alıcı" yalnız postası gitmiş (`status='sent'`) ve belirteci
     açık (geri almamış, silinmemiş) olandır; ötekiler toplamda
     `not_delivered` / `tracking_closed` nedeniyle görünür;
  2. postası 1 yıldan eski alıcıların belirteçleri kapanır;
  3. 1 yıllık bağlantıların adresi boşalır (`expired`); maskelenmiş ya da
     sonradan hassaslaşmış (`mail_task_content_sensitive()`) gönderimin
     bütün bağlantılarının adresi boşalır (`erased`, inceleme D7);
  4. postası 1 yıldan eski alıcıların olayları silinir;
  5. kuyrukta bekleyen ya da 1 yıl içinde olan postası kalmayan gönderim
     kapanır (`tracking_closed_at`); raporu artık yalnız kişisiz toplamdır.
- **dry-run ile apply aynı tanımdan:** dry-run aynı ifadeleri çalıştırıp
  işlemi geri alır; apply önce dry-run'ı koşar, sonra değiştirir.
- **Fren:** bir koşuda kapanacak belirteç, silinecek olay ya da boşalacak
  bağlantı sayısı `SKYMAIL_TRACKING_RETENTION_MAX_ROWS`'u (varsayılan 50.000)
  aşarsa apply hiçbir şey değiştirmez (`refused_large`, `error` düzeyinde
  `"alarm":"tracking_retention_brake"`). Karar yalnız apply'ın önce koştuğu
  dry-run sayılarına bakar; değişiklik yapan apply hiçbir zaman
  `refused_large` yazmaz. dry-run aynı durumu `would_refuse_large` diye
  raporlar. Biriken iş fren yüzünden her gün reddediliyorsa sınır bir
  yayınlık yükseltilir. `purge`'ün freni yoktur.
- **Makbuz:** her koşu `tracking_retention_runs`'a bir satır yazar (kip,
  başlangıç/bitiş, durum `ok|partial|refused_large|failed`,
  `events_cutoff`, durum ayrıca `would_refuse_large` olabilir; sayılar: `sends`, `totals_written`, `tokens_closed`,
  `events_deleted`, `links_expired`, `links_erased`, `sends_closed`,
  `sends_failed`); kişi, belirteç ya da adres yoktur. Aynı sayılar
  `"event":"tracking_retention_run"` günlük satırındadır.
- **`purge` (tam geri dönüş):** `SKYMAIL_TRACKING=off` ister (yoksa açılış
  durur). Bütün belirteçleri kapatır ve bütün olayları siler, yaşına
  bakmadan; kişisiz toplamlar ve bağlantılar kalır, eski postaların
  bağlantıları yönlendirir. Her koşuda tekrarlanır (zararsız); iş bitince
  kip **`apply`'a** alınır (`off`'a değil: `off`'ta 1 yıllık bağlantı
  boşaltması ve D7 de durur; bayrak kapalıyken `apply` zararsızdır). Elle SQL
  gerekmez.
- **Bilinen sınırlar:** (1) kişisiz toplam ilk alıcının yılı dolduğu koşuda
  bir kez yazılır; aynı gönderimin sonra giden alıcılarının o andan kendi
  yıllarına kadarki olayları toplama girmez (gönderim dakikalar içinde
  gittiği için pratikte etkisiz). (2) Maskeleme süpürücüsü görevi sonra
  kuyruk satırlarını, saklama ise önce kuyruk satırlarını kilitler; bir
  yıllık takipli gönderimin şablonu tam o anda hassaslaşırsa Postgres
  birini ölü kilit diye iptal eder: saklama `sends_failed` yazıp ertesi gün
  dener, maskeleme başarısızlık sayar (hassas içerik için güvenli yön).

Makbuzlar (production sunucusunda, salt okunur):

```bash
docker service logs --raw --since 168h sky-lab-production-skymail-tvrjzs 2>&1 | grep -F '"event":"tracking_retention_run"'
```

### core sözleşmesi (bilet 16)

SkyMail, takip isteyen gönderimde `sub`'ı olan alıcılar için core'a bir kez
(1000'erlik partilerle) sorar:

```http
POST {CORE_URL}/internal/v1/communication-preferences/tracking:resolve
Authorization: Bearer <skymail-backend servis hesabının token'ı>
Content-Type: application/json

{"subs": ["7d3c1f6e-0b9a-4c55-9a51-3f4d2b8e6a10", "…"]}
```

```json
{"results": [
  {"sub": "7d3c1f6e-0b9a-4c55-9a51-3f4d2b8e6a10", "allowed": true, "reason": null},
  {"sub": "…", "allowed": false, "reason": "preference_off"}
]}
```

- Her `sub` küçük harfli UUID, istekte bir kez, en çok 1000. Yanıt `200` ve
  sorulan her `sub`'ı **tam bir kez** içerir, başkasını içermez; değilse
  (ya da başka durum kodu, JSON dışı gövde) yanıt hiç yokmuş sayılır.
- `reason` yalnız `allowed: false` iken okunur: `preference_off` (my.'da
  kapattı), `not_member` (`/UYELER` dışında), `inactive` (hesap etkin
  değil). Bilinmeyen neden `not_allowed` olarak kaydedilir.
- Token: `KEYCLOAK_SERVICE_CLIENT_ID` (`skymail-backend`) servis hesabının
  client credentials token'ı (Admin REST'in kullandığı); core `aud` ∋ `core`
  ve `core` istemcisinin `communication-preferences:read` rolünü
  (`resource_access.core.roles`) arar (bilet 17, core #256).

## Veritabanı migration'ları

Uygulama bekleyen migration'ları servis trafiğe açılmadan önce çalıştırabilir.
Aynı anda açılan görevler sırayla migrate eder: biri bitirene dek ötekiler
bekler (en çok 5 dakika), sonra yapacak iş bulmaz. Mevcut sürümlendirilmemiş
kurulumun güvenli baseline işlemi, expand/contract kuralı ve gerekli
environment değerleri için
[`docs/database-migrations.md`](docs/database-migrations.md) belgesine bakın.

## Veritabanı bağlantıları (`DATABASE_URL`)

`DATABASE_URL` havuz sınırı taşıyabilir ve taşımalıdır:
`postgres://…/skymail?sslmode=…&pool_max_conns=5`. Yoksa pgx'in varsayılanı
max(4, CPU) olur. Bir görev ana havuz + `/ready`'nin kendi 1 bağlantısını
tutar (migrate ederken geçici 1 + golang-migrate'in kendi bağlantısı);
start-first deploy'da iki görev birlikte. `pool_*` ayarları yalnız havuza
gider, migration bağlantıları onları atar.


## Katkıda Bulunanlar 🧙‍♂️

<a href="https://github.com/skylab-kulubu/skymail-backend/graphs/contributors">
  <img src="https://contrib.rocks/image?repo=skylab-kulubu/skymail-backend" />
</a>

<sup><sub>[contrib.rocks](https://contrib.rocks) ile yapıldı.</sub></sup>


## Forms kaynaklı alıcı snapshotı

Forms'un mevcut alıcı aktarımı ve manuel/liste/Keycloak gönderimleri korunur.
Yeni kaynak akışı `POST /v1/form_mail_sources` ile Forms backend'in yetkiyle
hazırladığı alıcıları bir kez kaydeder; tarayıcı alıcıları veya form başlığını
bildirmez. `GET /v1/form_mail_sources/{id}` yalnız sahibine, `no-store` ile
kaynak bilgisini verir. Hazırlanan kaynak 30 dakika içinde sahibince
`POST /v1/mail_tasks/form-source` ile doğrudan gönderilir veya mevcut
`POST /v1/mail_approvals` isteğindeki `form_source_id` ile onaya sunulur.
Doğrudan gönderim mevcut `skymail:mails:send` veya `skymail:mails:write`
yetkisini; onay mevcut şablon okuma ve onay rollerini kullanır.

Yeni kaynak hazırlama şu iki zorunlu değer yokken kapalıdır (503); eski yollar ve
geçmiş gönderimler etkilenmez:

- `SKYMAIL_FORMS_SOURCE_SERVICE_SUB`: Forms'un gerçek servis hesabının tam UUID'si.
- `SKYMAIL_FORMS_SOURCE_MAX_RECIPIENTS`: yalnız bu özellik için pozitif, dağıtımda
  seçilen üst sınır; varsayılan ürün limiti yoktur. Mevcut HTTP body limiti ayrıca
  geçerlidir. Fazlası kesilmez, istek reddedilir. Manuel alıcı sınırı 100 kalır.
- `SKYMAIL_FORMS_SOURCE_CLIENT_ID`: mevcut Forms servis istemcisi; varsayılan `forms`.

Yalnız yeni hazırlama ucunda, aynı bearer Keycloak userinfo tarafından kabul
edildikten sonra tam servis sub'ı, issuer, SkyMail audience'ı ve eşit
`azp`/`client_id` doğrulanır. Genel token audience modu değişmez. Canlı servis
kimliği ve limit dağıtım öncesi doğrulanmadan özellik açılmaz; token veya servis
kimlik bilgisi çıktıya yazılmaz.

Kaynak immutable snapshot'tır: onay yeni form yanıtı çekmez. Bir kaynak tek
onay/gönderime atomik bağlanır; aynı isteğin tekrarı aynı kaydı döndürür.
Alıcı yenilemek yeni kaynak ve yeni onay ister. Onay bir kez, tek bulk task ve
alıcı başına mevcut queue satırıyla gönderilir. Yeni kaynak, ilk sürümde mevcut
adres bilgisini kullanır; kişi izleme kapsamını genişletmez. Gönderim/onay
readback'inde `audience.kind=form`, `source=forms`, form/source kimliği, ad,
hazırlanma zamanı ve alıcı sayısı yer alır; eski audience alanları korunur.

Hesap silme kaynak sahibini ve eşleşen alıcı/başlık bilgisini de temizler.
Etkilenmiş snapshot geçersizleşir; bekleyen kaynakla gönderim/onay 409 verir.
Harcanmamış süresi dolan kaynak mevcut expiry döngüsünde temizlenir. Kaynak
verisi varken migration down veri silerek geri dönmez; uyumlu binary kullanılır.
