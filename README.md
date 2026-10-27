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

Bir şablonu **sistem şablonu** yapmak ya da bu işareti kaldırmak (`PUT
/v1/templates/by-key/{key}` gövdesindeki `system`) ayrı bir rol ister:
`skymail:templates:system`. Onu yalnız Template seed'in servis hesabı
(`skymail-seed`) taşır, hiçbir kişi ya da grup taşımaz; çünkü `account.*` ya
da `keycloak.*` sistem şablonu bastırma listesini atlar ve hesap şeridinden
gider. Rolü olmayan (`skymail:templates:write` sahibi operatör) aynı uçla
sistem şablonunun içeriğini yine yeniden yazabilir, ama `system` şablonda ne
ise onu göndermelidir (yeni anahtar için `false`); başkasını gönderirse
`403 server.forbidden` döner (`params.missing_roles:
["skymail:templates:system"]`, `params.field: "system"`, `params.system`
şablonun bugünkü işareti) ve hiçbir şey yazılmaz.

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
(varsayılan `https://iletisim.yildizskylab.com`, iletişim konsolunun adresi;
iletişim-servisi 11). Eski `https://mail.yildizskylab.com` yolu ve sorguyu
koruyarak kalıcı 301 ile yeni adrese yönlenir, eski postalardaki linkler
çalışmaya devam eder.

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

Kuyruktaki tek kanal `email`'dir: `recipient_email` kanalın adresidir
(uygulama içi kutu kuyruğa girmez, öğesini bildirimin işleminde yazar;
[`docs/inbox.md`](docs/inbox.md)). Var olan ve
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

## Gönderen şeritleri (`SKYMAIL_SEND_HESAP`, `SKYMAIL_SEND_BILGI`, `SKYMAIL_SEND_DUYURU`, `SKYMAIL_LIST_UNSUBSCRIBE`)

Her posta türüne göre üç paylaşılan kutudan birinden çıkar (hub bileti
skymail-bounce-isleme 10; Yusuf, 2026-10-20). Kural `internal/mailer/lanes.go`
`LaneOf`'tadır, gönderimin şablonuna, gönderimin **amacına**
(`mail_tasks.purpose`, ADR 0072 karar 2; aşağıda "Gönderim amacı ve
kategori") ve satırın şeridine bakar. Amaç yalnız `SKYMAIL_MAIL_PURPOSE=on`
iken okunur; kapalıyken ve amacı olmayan eski satırda kural bugünkü gibidir
(toplu şerit `duyuru`, sistem şeridi `bilgi`):

| Şerit | Ne | Kural | Kararlaştırılan yol |
|---|---|---|---|
| `hesap` | giriş/kayıt kodu, e-posta doğrulama, şifre sıfırlama | şablon **sistem** şablonu ve anahtarı `keycloak.*` ya da `account.*` (`AccountMail`, bastırma muafiyetiyle aynı kural); amacı ve öncelik şeridi ne olursa olsun | `graph` |
| `duyuru` | operatörün kitleye postası (duyuru akışı) | amaç `duyuru`: liste, Keycloak grubu ve varsayılan olarak Forms yanıtlayanları, onaylı gönderim, takipli tekil posta; amacı yoksa toplu şerit (`priority = 0`), toplu bildirim türü dahil | `xoauth2` |
| `bilgi` | geri kalan işlem postası (işlem akışı) | amaç `islem`: servisin tekil postası, operatörün beyanla işlem dediği posta (toplu öncelikte beklese de); amacı yoksa sistem şeridi (`priority = 1`): Forms onayı, Mail onayı bildirimi, etkinlik ve öteki sistem bildirimleri | `graph` |

Kuyruk önceliği akıştan ayrıdır: operatörün işlem amaçlı Forms ya da onaylı
gönderimi toplu öncelikte (bütçenin toplu payında) bekler ama `bilgi@`'dan,
başlıksız çıkar; duyuru amaçlı her gönderim toplu öncelikte kuyruğa girer.

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

**Tek tık çıkış (`SKYMAIL_LIST_UNSUBSCRIBE`, RFC 8058; hub bileti
skymail-bounce-isleme 11).** Çıkış bağlantısı core'undur: tercih core'da
(`communication_preferences`, ekranı Hesap Merkezi'nde), belirteci core imzalar
(`GET|POST /v1/communication-preferences/unsubscribe?t=…`; GET yalnız sayfa,
POST `List-Unsubscribe=One-Click` bağlantının kategorilerini kapatır). SkyMail
anahtarı tutmaz, kendi tercih kaydı yoktur; core'un verdiği bağlantıyı satırda
(`mail_queue.list_unsubscribe_url`) taşır. Bağlantı iki yoldan gelir:
`POST /v1/notifications` alıcısının `unsubscribe_url`'i (yalnız isteğe bağlı
ve `bulk` şeritli türde; [`docs/notifications.md`](docs/notifications.md)) ve
`SKYMAIL_BULK_PREFERENCE=on` iken liste, grup, Forms, kişi ve onaylı duyuru
gönderiminde core'un `recipients:resolve` cevabı ("Toplu postada tercih");
hesabı olmayan adres SkyMail'in kendi bağlantısını alır. Bayrak kapalıyken bu
gönderimlerin satırında bağlantı yoktur.

| `SKYMAIL_LIST_UNSUBSCRIBE` | |
|---|---|
| `off` (ya da boş, varsayılan) | hiçbir postada başlık yok; her şey bugünkü gibi |
| `on` | şeridi `duyuru` olan, duyuru kutusundan (`graph` ya da `xoauth2`) giden ve satırında bağlantı olan posta `List-Unsubscribe: <bağlantı>` ve `List-Unsubscribe-Post: List-Unsubscribe=One-Click` taşır |

Başka değer açılışı durdurur; açılış logu `list_unsubscribe` alanını yazar,
duyuru şeridi `off` iken `on` ise uyarır. `hesap` ve `bilgi` hiçbir zaman
başlık yazmaz, `off` yolu (eski `SMTP_FROM`) da yazmaz.

- **Graph'ta MIME.** Graph JSON'u yalnız `X-` önekli başlık taşır. Bağlantılı
  duyuru postası aynı `sendMail`'e MIME olarak gider: SMTP yolunun kurduğu
  ileti (Reply-To, `Message-ID`, `X-SkyMail-Queue-Id`, düz metin ve HTML),
  base64, `Content-Type: text/plain`; 4 MB sınırı kodlanmış gövdeye uygulanır.
  MIME'da `saveToSentItems` yoktur: Exchange kopyayı Gönderilmiş Öğeler'e
  koyar (duyuru kutusunda MRM 1 gün). Bağlantısız posta bugünkü gibi JSON,
  kopyasız gider.
- **Başlık güvenliği.** Bağlantı mutlak `https` adresi, host'lu, kullanıcısız,
  en çok 960 karakter (başlık tek satır yazılır, RFC 5322 998), yalnız
  görünür ASCII; `<`, `>`, virgül ve tırnak yok. Veritabanı denetimi
  `https://` ve uzunluğu ayrıca tutar; kuralı tutmayan eski bir satır
  başlıksız gider.
- **Gövdede bağlantı.** Kişinin bağlantısı şablona `.UnsubscribeUrl` olarak
  girer (`.Email`, `.FullName` gibi; gönderimin aynı adlı değişkenini ezer).
  Kişinin bağlantısı yoksa değişken boştur; gönderimin verdiği değer
  kullanılmaz.
- **Saklama.** Bağlantı API'de hiç sunulmaz (`json:"-"`); satırın kişisel
  içeriğiyle birlikte boşaltılır (saklama süresi, maskeleme son tarihi,
  saklanmayan gönderim) ve hesap silmede satırla gider.
- **DKIM.** RFC 8058 iki başlığın DKIM imzasında (`h=`) olmasını ister.
  Exchange Online'ın MIME `sendMail`'de bunları imzaya alıp almadığı
  belgelenmemiştir; açılışta gelen postanın `DKIM-Signature: h=` satırı okunur
  (hub bileti 11'in açılış runbook'u).

Gönderim bütçesi (`SKYMAIL_RATE_PER_MINUTE`) şeritlerden bağımsız, hepsi için
ortaktır; Exchange'in dakikada 30 ileti sınırı kutu başınadır.

## Geri dönen posta ve bastırma listesi (`mail_bounces`, `mail_suppressions`)

Exchange Online dış alıcıyı `RCPT TO`'da reddetmez, postayı alır ve sonra
gönderen kutusuna bir teslim raporu (NDR, RFC 3464) yollar. Bu raporları
okuyan `internal/bounce` (`Process`) bir raporu kuyruk satırına bağlar ve
kaydeder; raporları kutudan okuyucu çeker (`internal/bouncereader`,
`SKYMAIL_BOUNCE_READER`, aşağıda; hub bileti skymail-bounce-isleme 04, ADR
0073). Okuyucu kapalıyken listeyi dolduran yol relay'in senkron kalıcı
reddidir (aşağıda).

- **Okuyucu** (`SKYMAIL_BOUNCE_READER`, varsayılan `off`): her 5 dakikada
  gönderen kutularının (`SKYMAIL_BOUNCE_MAILBOXES`) Gelen Kutusu'nu Graph'ın
  delta sorgusuyla okur (`mailFolders/inbox/messages/delta`, yalnız
  `internetMessageId` ve `receivedDateTime`); her yeni öğenin MIME'ını
  (`$value`, en çok 10 MB) bellekte `internal/bounce`'a verir, saklamaz.
  Kimlik gönderen şeritlerinin uygulamasıdır (`GRAPH_*`, `internal/msauth`,
  kapsam Graph); yetki Exchange RBAC for Applications `Application Mail.Read`,
  yalnız gönderen kutularına: okuyucu kutuda hiçbir şeyi silemez ya da
  taşıyamaz (raporları kutunun saklama etiketi 30 günde siler). Birden çok
  replikada aynı anda tek süreç okur (Postgres advisory lock). Okuyucu açıkken
  SMTP postası, relay EHLO'da `DSN` ilan ediyorsa, `RET=HDRS NOTIFY=FAILURE`
  ister: uzak sunucu postanın yalnız başlıklarını ve yalnız reddi bildirir
  (`NOTIFY=SUCCESS` hiç istenmez).

  | Değişken | Değer |
  |---|---|
  | `SKYMAIL_BOUNCE_READER` | `off` (ya da boş, varsayılan): hiçbir kutu okunmaz, başka değişken okunmaz. `dry-run`: okur, her raporun ne yapacağını sayar (`bounce.Evaluate`: aynı sorgular, geri alınan işlem), ret ve bastırma yazmaz. `on`: kaydeder ve bastırır. Başka değer açılışı durdurur |
  | `SKYMAIL_BOUNCE_MAILBOXES` | `dry-run`/`on` için zorunlu: gönderen kutuları, virgülle ayrılmış yalın adresler (ör. `hesap@bildirim.yildizskylab.com,bilgi@bildirim.yildizskylab.com,skylab@duyuru.yildizskylab.com`); en çok 10 |
  | `GRAPH_TENANT_ID`, `GRAPH_CLIENT_ID`, `GRAPH_CLIENT_KEY`, `GRAPH_CLIENT_CERT` | `dry-run`/`on` için zorunlu; gönderen şeritlerininkiyle aynı değerler |

  Okuyucunun kutudaki yeri (delta bağlantısı) ve sayıları
  `mail_bounce_reader_state`'te kutu ve kip başına durur; `dry-run`'ın yeri
  ayrıdır, yani `on`'a geçince Gelen Kutusu baştan okunur ve dry-run'ın
  saydığı raporlar kaydedilir. Yarıda kalan okuma (Graph hatası, 401, 429)
  delta bağlantısını ilerletmez; sonraki okuma aynı yerden başlar, daha önce
  kaydedilen rapor `tekrar` sayılır. Okunamayan rapor (bozuk MIME, 10 MB'tan
  büyük) sayılır ve geçilir. Graph'ın bağlantısı Graph'tan başka bir yere
  gidiyorsa okuma durur (token oraya gitmez). Her okuma
  `bounce reader: read the mailbox` satırına kutu ve sayıları yazar
  (`kalici`, `yumusak`, `gecikme`, `eslesmedi`, `dsn_degil`, `alici_farkli`,
  `tekrar`, `diger`, `okunamadi`); adres ve rapor metni yazılmaz. Açılış
  satırı `bounce reader (SKYMAIL_BOUNCE_READER, SKYMAIL_BOUNCE_MAILBOXES)`.
- **Bounce oranı** (`GET /v1/suppressions/bounces?days=7`,
  `skymail:suppressions:manage`; giriş akışının "bounce %5'i aşarsa"
  eşiği için): son `days` günde (1–90) gönderilen e-postalardan kaçının
  sonradan kalıcı (`hard`), yumuşak (`soft`) ya da gecikme (`delay`) raporu
  aldığı ve `hard_rate = hard / sent`; `reader` her kutunun son okuması,
  hatası (Graph'ın durumu ve kodu) ve sayılarıdır, delta bağlantısı olmadan.

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
  değil, sistem olmayan bir `account.x` şablonu bastırılır); sistem işaretini
  de yalnız `skymail:templates:system` (seed'in servis hesabı) koyar ya da
  kaldırır, `skymail:templates:write` yetmez (bkz. Keycloak); satır `suppressed_but_sent = true`,
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
  rolü, gruba verilir): `GET /v1/suppressions?reason=&q=&_start=&_end=` (en
  yeni önce, `q` adresin bir parçası, sayfa en çok 100, `X-Total-Count`)
  ve `DELETE /v1/suppressions/{id}` (`204`; yoksa `404`). Liste bastırma
  listesiyle [çıkış kaydını](#tek-tık-çıkış-ucu-skymail_unsubscribe_link-mail_unsubscribes)
  birlikte döner: her satırda `reason` (`bounce` | `unsubscribed`) ve `stream`
  (`all` | `duyuru`); `?reason=` biriyle süzer; liste
  `skymail:unsubscribes:reactivate` ile de okunur. Satır listenin opak
  `id`'siyle adlandırılır, adres hiçbir URL'de olmaz (hata logu yolu yazar).
  `DELETE` yalnız bounce satırını kaldırır; çıkış satırında
  `409 suppression.use_reactivate` döner. Kaldırma
  `mail_suppression_removals`'a kim, ne zaman, kaynak, kod, sayı ve ilk
  görülme olarak yazılır; adres yazılmaz. Bu kaldırma kaydı, geri alma kaydı
  gibi, yazılmasından 3 yıl sonra içerik süpürücüsünce silinir (Yusuf,
  2026-10-27; süre sabit, env yok). Kaldırılan adrese yeniden gönderilir.
  Kendiliğinden düşme yoktur.
- **Kişi ucu** (`GET /v1/me/email-delivery`, rol yok; kutu ile aynı zincir:
  token `aud`'unda `skymail`, kişi token'ı, erişim kapısı): kişi kendi
  adreslerinden hangisine posta ulaşmadığını kendi token'ıyla okur. Yalnız
  `recipient_sub` = token `sub`'ı olan satırlar ve adresi token'ın kişiye ait
  adreslerinden biri olan satırlar döner: userinfo `email` (yalnız `email_verified`
  `true` ise; claim yoksa kullanılmaz, `/v1` gibi) ve `school_email` (userinfo'da yoksa token'dan);
  küçük harf + kırpma ile karşılaştırılır, her adres bir kez. Parametre
  almaz; başka adres ya da kişi sorgulanamaz. Yanıt
  `{"undeliverable":[{"address","since"}]}` (`since` = ilk bastırılma, UTC),
  `Cache-Control: no-store`; kaynak, kod ve kimlik dönmez. Log yalnız istek
  kimliğini ve sayıyı yazar. Hesap Merkezi e-posta sayfasındaki "Bu adrese
  posta ulaşmıyor." uyarısı buradan okunur. Kişi bastırmayı kaldıramaz
  (yalnız operatör).
- **Loglar**: adres ve rapor metni yazılmaz; kuyruk id'si, kaynak ve kod
  yazılır.

## Gönderim amacı ve kategori (`SKYMAIL_MAIL_PURPOSE`)

Her posta bir gönderim akışındadır ve akışı gönderimin **amacı** belirler,
kuyruk önceliği belirlemez (ADR 0072 karar 2 ve 8; hub bileti abonelik-cikisi
03). `islem` akışı `hesap@` ve `bilgi@`'dır: tercihe ve çıkış kaydına bakmaz,
`List-Unsubscribe` taşımaz. Takip amaca bağlı değildir, takip kurallarına
tabidir (`SKYMAIL_TRACKING`, `_OPERATOR`, `_SYSTEM`; Yusuf: bütün postalar
takipli). `duyuru` akışı duyuru kutusudur:
her posta core'un bir kategorisini taşır, tercihe (bilet 09) ve çıkış kaydına
uyar, tek tık başlığını ve gövdede görünür bağlantıyı taşır.

| `SKYMAIL_MAIL_PURPOSE` | |
|---|---|
| `off` (ya da boş, varsayılan) | alanlar yok sayılır, hiçbir gönderime amaç yazılmaz, akışı bugünkü gibi öncelik belirler, künye satırı eklenmez |
| `on` | kişinin her gönderimi ve onay isteği amaçla yazılır; akışı amaç belirler; künye satırı eklenir |

Başka değer açılışı durdurur; açılış logu `mail_purpose` alanını yazar.
Arayüz bayrağı `GET /v1/mail_tasks/tracking-mode` ve `/summary`'nin
`purpose_available` alanından okur.

**İstek alanları** (`POST /v1/mail_tasks`, `/single`, `/form-source`,
`POST /v1/mail_approvals`, `POST /v1/mail_approvals/{id}/resubmit`):

| Alan | |
|---|---|
| `purpose` | `islem` ya da `duyuru` |
| `category` | `duyuru`'da zorunlu, `islem`'de yasak: `club:duyuru`, `club:haber`, `team:<Keycloak grup id>:duyuru`, `team:<id>:haber`. Grup id'si UUID'dir, küçük harfle yazılır. Grubun gerçekten ekip olup olmadığını core söyler (bilet 09) |
| `transactional_statement` | `islem`'de zorunlu ve `true`: "bu posta alıcının kendi başvurusu ya da işlemi hakkındadır". `duyuru`'da yok sayılır |
| `commercial_content_absent` | "Postanın asıl amacı tanıtım değildir" beyanı (KVKK aydınlatma metni s6 §5.10; ADR 0072 karar 9): kısa sponsor teşekkürü ve logo serbesttir; indirim kodu, şirket iş ilanı, satış ya da çekiliş postayı tanıtım yapar. `false` postayı tanıtım postası yapar: `SKYMAIL_PROMOTIONAL=on` iken `duyuru`'da kabul edilir ve posta yalnız tanıtım izni verene gider ("Tanıtım postası ve hesapsız liste alıcısı"); bayrak kapalıyken ve `islem`'de her zaman reddedilir. `duyuru`'da `true` saklanır; `SKYMAIL_BULK_PREFERENCE=on` iken amacını `duyuru` diye adlandıran gönderimde zorunludur (`400 mail_task.commercial_statement_required`; amaç adlandırmayan eski istemci varsayılanını alır). `islem`'de `true` yok sayılır |
| `own_event_statement` | Liste ya da Keycloak grubuna `duyuru`'da, `SKYMAIL_LIST_CONSENT=on` iken: "Bu posta listenin oluşturulduğu etkinlik ya da çalışmayla ilgilidir" beyanı (s6 satır 15, Yusuf'un T3 kararı 2026-10-24). Gönderimle, göndereni ve zamanıyla saklanır (`mail_tasks`, `mail_approvals`). Beyanlı posta listenin hesapsız adresine duyuru rızası aranmadan, yine `duyuru` akışında çıkış bağlantısıyla gider. Bayrak kapalıyken, kişiye, Forms kaynağına ve `islem`'de yok sayılır |

**Kurallar** (Yusuf, 2026-10-21; hepsi `400`, hiçbir şey kuyruğa girmez):

| Durum | Kod |
|---|---|
| liste ya da Keycloak grubuna (doğrudan ya da onay isteğiyle) `islem` | `mail_task.purpose_not_allowed` |
| `islem` ama `transactional_statement` yok ya da `false` | `mail_task.transactional_statement_required` |
| `commercial_content_absent: false`, `islem`'de ya da `SKYMAIL_PROMOTIONAL` kapalıyken | `mail_task.commercial_content_not_allowed` |
| `islem` + `category` | `mail_task.category_not_allowed` |
| `duyuru` ama `category` yok | `mail_task.category_required` |
| `category` core'un biçiminde değil | `mail_task.category_invalid` |
| `purpose` olmadan `category` ya da bir beyan | `mail_task.purpose_required` |
| `SKYMAIL_BULK_PREFERENCE=on` iken `duyuru` ama `commercial_content_absent` yok | `mail_task.commercial_statement_required` |
| `purpose` `islem`/`duyuru` dışında | `mail_task.purpose_invalid` |

Denetim sırası: önce `purpose_required`, sonra `commercial_content_not_allowed`,
sonra `purpose_invalid`, en son amacın kendi kuralları (`islem`: hedef,
kategori, beyan; `duyuru`: kategori, sonra beyan). `SKYMAIL_BULK_PREFERENCE=on`
iken kategorinin core'da gerçekten bir ekip olduğunu core söyler; olmayan ekip
`400 mail_task.category_unknown` döner ("Toplu postada tercih"). Forms kaynağının birebir tekrarı aynı amacı taşımalıdır: amacı
değişen tekrar başka bir istektir, `409 form_source.consumed` döner. Liste ya
da gruba giden onay isteği onaylanırken saklı amaç yeniden denetlenir:
`duyuru` değilse `duyuru` + `club:duyuru` gider.

`islem` yalnız iki hedefte seçilebilir: kişilere tek tek (`/single`, kişilere
onay isteği) ve Forms yanıtlayanlarına (`/form-source`, Forms kaynaklı onay
isteği). `islem` gönderim de öteki operatör gönderimleri gibi takip edilir
(`SKYMAIL_TRACKING_OPERATOR=on` iken her zaman; gizli/hassas şablon ve
alıcıdan yapılan bağlantı kuralları aynı). Servisin `islem` postası
`SKYMAIL_TRACKING_SYSTEM` kuralıyla kalır.

**Varsayılanlar.** Alanları göndermeyen kişi gönderimi `duyuru` +
`club:duyuru` yazılır (ADR 0072 karar 2: "Seçmezse posta duyurudur"): liste,
grup, Forms kaynağı, onay isteği ve kişinin `/single`'ı, takipli de takipsiz
de. Bayrak açıkken eski arayüzün takipsiz `/single`'ı bu yüzden toplu şeride
ve `duyuru@`'ya geçer.
Servis token'ıyla (Keycloak, core, Forms) gelen `/single` her zaman
`islem`'dir, alanları yok sayılır. Bayrak kapalıyken gönderilmiş onay isteği
(amaçsız) bayrak açıkken onaylanırsa `duyuru` + `club:duyuru` gider.
`POST /v1/notifications` amaç yazmaz (öncelik kuralı; toplu tür `duyuru`).

**Kuyruk.** Takipsiz `islem` tekil posta sistem şeridine, takipli `islem` ve
her `duyuru` tekil posta (takipsiz de) toplu şeride girer (takip yalnız toplu
şeritte planlanır; takipli `islem` yine `bilgi@`'dan çıkar); liste, grup, Forms ve onaylı gönderim her zaman toplu
şerittedir. Çıkış kaydı (`mail_unsubscribes`) yalnız `duyuru` akışına
uygulanır: işlem amaçlı Forms gönderiminin alıcısı çıkış yapmış olsa da alır.

**Raporda.** `GET /v1/mail_tasks`, `/{id}`, ana sayfanın son gönderimleri ve
Mail onayı istekleri (`GET /v1/mail_approvals`, `/{id}`) `purpose`,
`category`, `transactional_statement` ve `commercial_content_absent` döner;
bayraktan önceki ya da bayrak kapalıyken yazılmış gönderimde dördü de `null`'dur.
Gönderim yanıtı (`CreatedMailTask`) `purpose` ve `category` döner.

**Künye satırı.** Bayrak açıkken, `duyuru` akışındaki ve satırında çıkış
bağlantısı (`list_unsubscribe_url`) olan postanın HTML'inde ya da düz
metninde bağlantı yoksa, gönderim anında sona kısa bir satır eklenir
(`internal/mailer/footer.go`): HTML'de son `</body>`'den önce
`SKY LAB duyurularını almak istemiyorsan: <a href="…">Abonelikten çık</a>`,
düz metinde `SKY LAB duyurularını almak istemiyorsan: Abonelikten çık <…>`.
Her parça ayrı denetlenir; şablon `{{.UnsubscribeUrl}}`'yi kendisi yazıyorsa
(bilet 05'in künyesi) eklenmez. Satır kuyruktaki satıra ve gönderilen içerik
ekranına yazılmaz, bağlantısı tıklama adresi almaz. Bağlantıyı bu bayrak
üretmez: `POST /v1/notifications`'ın `unsubscribe_url`'i ve
`SKYMAIL_BULK_PREFERENCE=on` iken her duyuru satırı ("Toplu postada tercih")
taşır.

**Göç** (`20261023100000`, yalnız ekleme): `mail_tasks` ve `mail_approvals`'a
`purpose`, `category`, `transactional_statement`, `commercial_content_absent`
(nullable), `NOT VALID` denetimlerle: amaç değeri, kategori biçimi, her alanın
kendi amacına ait olması ve (yalnız `mail_tasks`) listeye/gruba giden
gönderimin `islem` olmaması. Eski imaj bu sütunları okumaz, yazmaz.

**Geri dönüş.** `SKYMAIL_MAIL_PURPOSE=off` + yeniden dağıtım: amaç okunmaz,
kuyrukta bekleyen işlem amaçlı toplu satır da yeniden `duyuru@`'dan gider,
künye satırı eklenmez. İmajı geri almak da güvenlidir; sütunlar kalır.

## Tanıtım postası ve hesapsız liste alıcısı (`SKYMAIL_PROMOTIONAL`, `SKYMAIL_LIST_CONSENT`)

KVKK aydınlatma metni s6 §5.8, §5.10, §5.14 ve tablo satır 13–15 (hub
biletleri kvkk-s6-yayin-oncesi 11 ve 12). İki tür `duyuru` postası yalnız
core'un rıza kaydında (`contact_consents`, core `docs/contact-consents.md`)
izni olan kişiye gider:

- **Tanıtım postası.** Asıl amacı bir mal ya da hizmetin tanıtımı olan posta
  (indirim kodu, şirket iş ilanı, satış, çekiliş): `duyuru` +
  `commercial_content_absent: false`. Kulübün kendi etkinliğinin duyurusu,
  ücretli olsa da, ve kısa sponsor teşekkürü ya da logo tanıtım değildir
  (Yusuf). Posta yalnız core'un `promotional` kitlesindeki adrese gider; o
  iznin `withdrawUrl`'i `List-Unsubscribe` başlığı ve künye satırı olur
  ("Tanıtım içerikli e-postaları almak istemiyorsan: İzni geri al"): tek tık
  tanıtım iznini kapatır. Şablon `{{.UnsubscribeUrl}}` yazıyorsa orada duyuru
  bağlantısı (bilet 09'dan sonra) kalır.
- **Hesapsız alıcıya duyuru** (`SKYMAIL_LIST_CONSENT=on`; bayrağın adı ilk
  kapsamı olan listeden kalır). Her `duyuru` postasında (iç liste, Forms
  kaynağının yanıtlayanları, kişiye tek posta, onaylı gönderim; s6 §5.14,
  hub bileti kvkk-s6-yayin-oncesi 23), core'un `recipients:resolve`'ünün
  hiçbir hesaba eşlemediği adres yalnız core'un `event_invitations`
  kitlesindeyse alır; kayıt `davet-v1` metniyle verilmişse (yalnız davet) ya
  da yenileme sorusu bekliyorsa (`renewalDue`) sayılmaz. Hesaplı alıcı
  tercih kuralıyla ("Toplu postada tercih") gider. Keycloak grubu üyeleri ve
  haber gönderiminin alıcıları hesaptır; satırında hesap (`recipient_sub`)
  olan alıcı bu kurala girmez. `islem` postası (servis hesabının
  `/v1/mail_tasks/single` postası, örneğin Forms'un `forms.*` onayları; Forms
  kaynağına ya da kişiye `islem`) ve gönderenin kendine deneme postası
  (`self_test`) de girmez. Gönderen liste ya da grup postasının listenin
  kendi etkinliği ya da çalışmasıyla ilgili olduğunu beyan ettiyse
  (`own_event_statement: true`; s6 satır 15, 5/2-c) rıza aranmaz: posta
  bütün liste alıcılarına, yine duyuru akışında ve çıkış bağlantısıyla
  gider; beyan gönderim kaydında kalır.

**Ne zaman.** Dağıtıcı toplu satırları alırken, aynı işlemde, çıkış kaydından
sonra ve gönderim bütçesi saymadan önce, her turda karar verir. core'a ne
işlemin içinde ne bütçe kilidi altında sorulur (abonelik-cikisi 09): karar
süreç içi önbellekten verilir (kitle ve hesap eşlemesi 60 saniye; geri alınan
izin en çok bu kadar geç sayılır), bilinmeyen şey işlemden sonra arka planda,
en çok 5 saniyede core'dan okunur; dağıtıcı beklemez. Sistem şeridinin
satırları (giriş kodları) toplu şerit alınmadan önce işçilere verilir.

| Sonuç | |
|---|---|
| `suppressed: no_promotional_consent` | tanıtım izni yok: `failed`, atlanmış sayılır |
| `suppressed: no_announcement_consent` | hesapsız adres, duyuru rızası yok: `failed`, atlanmış sayılır |
| bekler | cevap henüz bilinmiyor: satır `pending`'e döner, deneme ve bütçe sayılmaz; okuma sürerken 1 saniye, core cevap vermiyorken yaşına göre 1, 2, 5, 10, en çok 15 dakika sonra yeniden alınır |
| `consent: unavailable` | core başarısızken (devre kesici açık) satır kuyruğa alınalı 48 saati geçti: `failed`, belirsiz (atlanmış sayılmaz). core sağlamken eski satır da sorulur, kapanmaz |

**core kapalıysa** bilinmeyen rıza yok sayılmaz: satır bekler, gönderilmez
(kapalı yönde). core başarısız olunca 30 saniye sorulmaz (devre kesici).
Listedeki hesaplı üye core yüzünden düşürülmez; rızası önbellekte görünen
misafir yine alır. Hesap eşlemesi her zaman `club:duyuru` ile sorulur:
eşleme kategoriye bağlı değildir, sonradan silinmiş bir ekip soruyu bozmaz.
Tanıtım kuralı bayraktan ve şablondan bağımsız her zaman uygulanır: bayrak
açıkken kuyruğa girmiş tanıtım postası bayrak kapatılınca herkese gitmez,
hesap şablonuyla (`keycloak.*`, `account.*`) gönderilen tanıtım da izin arar.
`textVersion`'ı boş kayıt duyuru rızası sayılmaz.

| Değişken | |
|---|---|
| `SKYMAIL_PROMOTIONAL` | `off` (ya da boş, varsayılan): `commercial_content_absent: false` reddedilir. `on`: `duyuru`'da kabul edilir; `GET /v1/mail_tasks/tracking-mode` `promotional_available: true` döner |
| `SKYMAIL_LIST_CONSENT` | `off` (ya da boş, varsayılan): duyuru postası bugünkü gibi, `own_event_statement` yok sayılır. `on`: hesapsız alıcıya her `duyuru`da (liste, Forms kaynağı, kişiye tek posta, onaylı gönderim) duyuru rızası aranır, beyanlı liste ya da grup postası hariç; `islem` etkilenmez; `tracking-mode` `list_consent_available: true` döner |

Başka değer açılışı durdurur; ikisinden biri `on` iken `CORE_URL` gerekir.
`SKYMAIL_PROMOTIONAL=on` ayrıca `SKYMAIL_MAIL_PURPOSE=on`,
`SKYMAIL_LIST_UNSUBSCRIBE=on` ve kapalı olmayan bir duyuru şeridi
(`SKYMAIL_SEND_DUYURU=graph|xoauth2`) ister: tanıtım postası izni geri alma
bağlantısını hem künyede hem tek tık başlığında taşımalıdır. Biri eksikse
açılış durur (yarım yapılandırmayla "tanıtım açık" denmez).
`SKYMAIL_LIST_CONSENT=on` da `SKYMAIL_MAIL_PURPOSE=on` ister: amaç yokken
"listenin kendi etkinliği" beyanı verilemez ve kapı her misafir postasını
tutardı. Hesap şablonuyla (`keycloak.*`, `account.*`) tanıtım gönderimi
`400 mail_task.promotional_account_template` döner: hesap postası ne tek tık
başlığını ne künye satırını taşır.
Açılış logu `promotional`, `list_consent` ve `core` alanlarını yazar.

**core sözleşmesi.** SkyMail'in servis hesabı token'ıyla:
`GET /v1/consents/audience?purpose=promotional|event_invitations` (core
istemcisi rolü `consent:audience:read`; sayfa 1000) ve
`POST /internal/v1/audiences/recipients:resolve` (rol `audience:read`;
tur başına en çok 1000 adres). Rıza kaydı core'da `CONTACT_CONSENT_KEY` ile
açılır; anahtar yokken kitle `503` döner. Bu yüzden production'da iki bayrak
da rıza kaydı açılana (data-lifecycle 10) kadar kapalı kalır.

**Açılış sırası.** core rıza kaydı canlı + SkyMail servis hesabına iki rol →
`SKYMAIL_PROMOTIONAL=on` → arayüz (skymail-frontend: "Bu posta tanıtım
içeriyor" seçimi) → `SKYMAIL_LIST_CONSENT=on`. Geri dönüş: bayrak `off` +
yeniden dağıtım; kuyruktaki tanıtım postası yine yalnız izinliye gider.

## Toplu postada tercih (`SKYMAIL_BULK_PREFERENCE`)

ADR 0072 karar 3 ve 6; hub bileti abonelik-cikisi 09
(`internal/mailer/preference_gate.go`). Bayrak açıkken panelin her `duyuru`
gönderimi (liste, Keycloak grubu, Forms yanıtlayanları, kişiye `duyuru`, onaylı
gönderim) gönderim anında core'daki tercihe uyar ve her postası tek tık çıkış
bağlantısı taşır. Tek kontrol noktası SkyMail'dedir: kuyruğa alma ve dağıtıcı.

**Kuyruğa alırken.** Gönderimin veritabanı işlemi başlamadan önce (Mail
onayında onay anında, isteğin kilidinden önce; sunulduğu an değil) core'a
`POST /internal/v1/audiences/recipients:resolve` ile sorulur: gönderimin
kategorisiyle, en çok 1000'lik sayfalarla, `sub` biliniyorsa (Keycloak grubu
üyeleri) `sub`, yoksa adres. Sayfa başına 10 saniye, gönderim başına toplam 30
saniye. SkyMail core'u işlem ya da kilit tutarken beklemez.

| core'un cevabı | Satır |
|---|---|
| `allowed` | kişinin core bağlantısı `list_unsubscribe_url` olur, `preference_checked_at` yazılır |
| `allowed: false` | `failed`, deneme 0, `error = preference: opted_out`, `preference: inactive` ya da `preference: ambiguous`; atlanmış sayılır (`mail_task_status()` gönderimi "Başarısız" yapmaz) |
| `matched: false` | SkyMail'in kendi bağlantısı (`/u/…`, "Tek tık çıkış ucu"); `preference_checked_at` boş |

- **core yanıt vermezse** (zaman aşımı, `5xx`, sözleşme dışı cevap) hiçbir şey
  yazılmaz: `503 mail_task.preference_unavailable`, `Retry-After: 30`. Onay
  ucu da `503` döner, istek olduğu gibi bekler. Bu fail closed'dır (ADR 0072).
- **Bilinmeyen ekip.** core'un `400 recipients_resolve_category_unknown`'ı
  `400 mail_task.category_unknown` olur.
- **İç liste.** Listenin adresleri işlemden önce okunur ve sorulur; işlem listeyi
  yeniden okur. Arada eklenen adres sorulmamış olur: bağlantısız kalır ve
  güvenlik ağı onu göndermez.
- SkyMail'in çıkış kaydı ve bastırma listesi bugünkü gibi ayrıca uygulanır.
- Bildirimler (`POST /v1/notifications`) kuyruğa alırken sorulmaz: bağlantıyı
  core verir.

**Her dağıtım turunda.** Dağıtıcı toplu satırları alırken, aynı işlemde,
çıkış kaydından ve rıza kapısından sonra, gönderim bütçesi saymadan önce,
core'un hüküm verdiği satırları (`preference_checked_at` dolu) ve kişisini
bilen satırları (`recipient_sub` dolu; haberden gönderimin core kitlesinden
gelen satırları da) kategorisiyle yeniden karara bağlar. Eşik yok: her tur karar verir. Karar süreç içi önbellekten verilir
(hüküm en çok 60 saniye eski); bilinmeyen hüküm işlemden sonra arka planda,
kategori başına tek istekle, en çok 5 saniyede sorulur ve satır bekler (rıza
kapısının kalıbı). Kuyruğa alırken alınan hüküm ilk turlarda kullanılır.

| Sonuç | |
|---|---|
| hâlâ `allowed` | gider; `preference_checked_at` turun hükmüyle güncellenir |
| artık `allowed: false` | `failed`, `preference: <neden>`, gönderilmez |
| bekler | hüküm henüz yok ya da core yanıt vermiyor: `pending`'e döner, deneme ve bütçe sayılmaz; core başarısızken (30 sn devre kesici) yaşına göre 1–15 dakika sonra |
| `preference: unavailable` | core başarısızken satır 48 saati geçti: `failed`, belirsiz (atlanmış sayılmaz) |
| `preference: category_unknown` | ekip kuyruğa alındıktan sonra silinmiş: `failed` (atlanmış sayılmaz) |

Hesapsız satır (SkyMail bağlantılı) core'u beklemez. Tur başına bir log
satırı yazılır (`"event":"bulk_preference_recheck"`, `asked`, `took`, `ok`);
SkyMail'de metrik ucu yok, `skymail_bulk_preference_recheck_seconds` metrik
ucu geldiğinde eklenir.

**Güvenlik ağı.** Bayrak açıkken dağıtıcı `duyuru` şeridindeki bir satırı
`list_unsubscribe_url` olmadan göndermez: `failed`, `error =
unsubscribe_missing` (atlanmış sayılmaz). Bayrak açılmadan önce kuyruğa girmiş
bağlantısız duyuru satırı da böyle düşer: bayrak kuyruk boşken açılır.
Zorunlu bildirim türünün satırı (bilet, sertifika) bağlantı taşıyamaz, bu
kurala girmez. Gönderenin yalnız kendi doğrulanmış adresine giden deneme postası
(`mail_tasks.self_test`; haberden gönderimin deneme postası, List-Unsubscribe
taşımaz) ne sorulur ne bu kurala girer: kimseye ulaşmaz. Toplu
şeritli isteğe bağlı bildirim türünde e-postası gidecek alıcının
`unsubscribe_url`'i yoksa `400 notification.unsubscribe_url_required`.

**Servis token'ı.** Servisin `/single`'ı her zaman `islem`'dir ve sorulmaz
(abonelik-cikisi 03 T3): bir servis duyuru niteliğindeki postayı `/single`
döngüsüyle gönderirse tercih ve çıkış kaydı atlanır. Bugün Forms'un toplu
gönderimi kişi token'ıyla Forms kaynağından, core'unki `/v1/notifications`'tan
gider.

**Rapor.** `GET /v1/mail_tasks` ve `/{id}` `recipient_counts`'ta `opted_out`,
`unsubscribed` (SkyMail'in çıkış kaydı), `inactive` ve `ambiguous` döner;
hepsi `failed` içindedir, `unsubscribed` `suppressed` içinde de.
`GET /v1/mail_tasks/{id}/queue` satırın `error`'unda nedeni döner. Loglarda
adres ve `sub` yok, yalnız sayılar.

| Değişken | |
|---|---|
| `SKYMAIL_BULK_PREFERENCE` | `off` (ya da boş, varsayılan): hiçbir şey sorulmaz, bugünkü gibi. `on`: yukarıdaki her şey |

Başka değer açılışı durdurur. `on` şunları ister, eksikse açılış durur:
`SKYMAIL_MAIL_PURPOSE=on`, `SKYMAIL_LIST_UNSUBSCRIBE=on`, kapalı olmayan duyuru
şeridi, `SKYMAIL_UNSUBSCRIBE_LINK=on` ve halkada bir anahtar, `CORE_URL`
(takibin kullandığı iç adres). Açılış logu `bulk_preference` alanını yazar.
SkyMail'in servis hesabında core'un `audience:read` rolü zaten var.

**Göç** (`20261025200000`, yalnız ekleme): `mail_queue.preference_checked_at`
(nullable), `mail_tasks.self_test` (`NOT NULL DEFAULT false`, yeniden yazmaz)
ve `mail_task_status()`'ın üç `preference:` nedenini atlanmış sayması. Eski imaj sütunu okumaz, yazmaz.

**Geri dönüş.** `SKYMAIL_BULK_PREFERENCE=off` + yeniden dağıtım: core'a
sorulmaz, bağlantısız satır başlıksız gider; çıkış kaydı uygulanmaya devam
eder. İmajı geri almak da güvenlidir; sütun kalır.

## Tek tık çıkış ucu (`SKYMAIL_UNSUBSCRIBE_LINK`, `mail_unsubscribes`)

Hesabı olmayan adresin duyuru akışından çıktığı yer (ADR 0072 karar 4–7; hub
bileti abonelik-cikisi 02). Postmark'ın broadcast stream suppression'ı gibi:
kayıt **akış başınadır** ve yalnız `duyuru` akışını durdurur; tercih değildir,
kategori ya da ekip taşımaz. Kimliği olan kişinin tercihi core'dadır ve
SkyMail'e kopyalanmaz. Satırlara bağlantıyı koymak ve core'a sormak bilet
09'undur; bu bölüm kaydı, ucu ve bağlantının üreticisini anlatır
(`unsubscribe.Config.Link`).

**Kayıt.** `mail_unsubscribes`: küçük harfli adres, `stream` (`duyuru`),
`source` (`link`), `created_at`, opak `id`; anahtar `(address, stream)`.
`mail_suppressions`'ın kardeşidir, ona sütun eklenmez: eski imaj
`mail_suppressions`'ın her satırını bounce sayar ve çıkmış adrese Forms onayı
dahil her postayı durdururdu; yeni tabloyu hiç okumaz (göç testi bunu eski
imajın sorgularıyla kanıtlar). Kendiliğinden düşme yoktur: kişi yeniden abone
olana, panel kişinin isteğiyle geri alana ya da hesap silinene kadar durur.

**Uygulama.** Kayıt bayraktan bağımsız her zaman uygulanır: bayrağı kapatmak
çıkmış adrese posta göndermez.
- **Kuyruğa alırken**: gönderen şeridi `duyuru` olan gönderimde (`LaneOf`:
  `SKYMAIL_MAIL_PURPOSE=on` iken amaç, kapalıyken öncelik) kayıttaki alıcı `failed` yazılır,
  `error = suppressed: unsubscribed`, deneme 0; relay'e ve bütçeye gitmez.
  Bounce gibi atlanmış sayılır, gönderimi "Başarısız" yapmaz
  (`mail_task_status()`).
- **Göndermeden hemen önce**: dağıtıcı bulk satırlarını aldığı işlemde kaydı
  yeniden okur (alınan satırlar üzerinde tek indeksli sorgu); kuyruğa
  alındıktan sonra çıkan adresin satırı aynı hatayla `failed` olur, gönderilmez
  ve bütçeye sayılmaz.
- `hesap` (hesap postaları, liste olarak gönderilse de) ve `bilgi` (Forms
  onayı, bildirimler) kayda bakmaz. Bounce kuralları değişmez: iki kayıt
  birden olan adreste ikisi de geçerlidir.

**Uç.** `GET|HEAD|POST /u/{belirteç}`, herkese açık kökte
(`https://api.yildizskylab.com/api/skymail/u/…`), `/t/` ile aynı katman
(OpenAPI: [unsubscribe-public.openapi.yaml](docs/unsubscribe-public.openapi.yaml)).
- `GET` hiçbir şey yazmaz (tarayıcılar ve posta tarayıcıları açar): "Bu adres
  SKY LAB duyuru postalarından çıkarılsın mı?", maskeli adres (`a…@example.com`),
  tek düğme; kayıt varsa "Çıkarıldı" ve "Yeniden abone ol". Hesap Merkezi'nin
  iletişim tercihleri bağlantısı (`SKYMAIL_UNSUBSCRIBE_ACCOUNT_URL`, varsayılan
  `https://my.yildizskylab.com/communication-preferences`).
- `POST` gövdesi `List-Unsubscribe=One-Click` (form-encoded ya da multipart):
  kaydı yazar (`source = link`), `200 ok`. Sayfa düğmesi `eylem=cikis` ile
  aynısını yapar; `eylem=geri` kaydı siler ve
  `mail_unsubscribe_reactivations`'a `actor = recipient` yazar (adres yok).
  Alanın adı `eylem`'dir, `islem` akış adıyla karışmasın diye. Tekrar
  edilebilir: ikinci POST aynı sonucu verir. Başka gövde `400`.
- Değiştirilmiş, kesilmiş belirteç ya da halkada olmayan sürüm `400` ("Bağlantı
  geçersiz"; tek tıkta `invalid link`). Halka boşsa `503`. Bayrak `off` iken
  `/u/` `404`; başka yöntem `405`, başka yol `404`.
- Çerez, oturum, yönlendirme yok. Her yanıt `Cache-Control: no-store`,
  `Referrer-Policy: no-referrer`, `X-Frame-Options: DENY`,
  `X-Content-Type-Options: nosniff`, betiksiz CSP.
- İstemci adresi başına dakikada 120 GET ve 600 POST, yalnız bellekte (`429`,
  `Retry-After: 60`); IP yazılmaz.
- Log istek başına tek satırdır: yöntem ve sonuç. Yol, belirteç, adres ve IP
  yazılmaz; `/u` önekinin tamamı genel hata işleyicisine (yolu yazar) hiç
  ulaşmaz. Kenarın erişim logu `/api/skymail/u/` yolunu tutmamalıdır (bilet 06).

**Belirteç ve anahtar halkası.** Belirteç sürüm baytı (anahtar kimliği), 24
baytlık rastgele nonce ve XChaCha20-Poly1305 ile şifrelenmiş (akış etiketi,
normalize adres)'tir, unpadded base64url; sürüm ek veriye bağlıdır. Adres
URL'de açık durmaz. Süresizdir ve satıra bağlı değildir: saklama süreleri
bağlantıyı bozmaz. Yeni belirteç geçerli sürümle mühürlenir; eski sürümler
yalnız açmada kullanılır. Halka tam olarak tanımlı `V<n>` değişkenleridir.

Döndürme (orkestratör kararı, 2026-10-21; core bilet 13 ile aynı):
- **Planlı döndürme:** yeni `V<n>` eklenir ve `CURRENT` ona çekilir. Eski sürüm
  açmada **süresiz** kalır, eski postaların bağlantıları çalışmaya devam eder.
- **Sızıntı:** sızan sürümün değişkeni **hemen** kaldırılır, yeni sürüm
  `CURRENT` olur ve deploy edilir. O sürümle giden bağlantılar artık açılmaz:
  sayfa `400` "Bağlantı artık geçerli değil" der ve kişiyi Hesap Merkezi'nin
  iletişim tercihlerine yollar; tek tık `invalid link` alır.
- Bir sürüm numarası başka bir anahtara verilmez.

| Değişken | |
|---|---|
| `SKYMAIL_UNSUBSCRIBE_LINK` | `off` (ya da boş, varsayılan): `/u/` `404`, üretici bağlantı vermez, kayıt yine uygulanır. `on`: uç açık. Başka değer açılışı durdurur |
| `SKYMAIL_UNSUBSCRIBE_BASE_URL` | `/u/`'nun herkese açık kökü, `https` (ör. `https://api.yildizskylab.com/api/skymail`); `on` bunu ister |
| `SKYMAIL_UNSUBSCRIBE_ACCOUNT_URL` | sayfanın Hesap Merkezi bağlantısı (isteğe bağlı) |
| `SKYMAIL_UNSUBSCRIBE_KEY_CURRENT` | yeni bağlantının sürümü (1–255) |
| `SKYMAIL_UNSUBSCRIBE_KEY_V<n>` | `n` sürümünün anahtarı: 32 rastgele bayt, base64 ya da base64url; her biri ayrı OpenBao referansı (ADR-0049) |

Anahtar varken `CURRENT` yoksa, `CURRENT` var olmayan bir sürümü gösteriyorsa
(ör. sızıntıda kaldırılan sürüm), anahtar 32 bayt
değilse ya da iki sürüm aynı anahtarı tutuyorsa açılış durur; hata yalnız
değişkenin adını yazar. `on` ve boş halka hata değildir: uç `503` döner,
açılış logu uyarır (`key_current`, `key_versions`; anahtar hiç yazılmaz). Bilet
09'un bayrağı açılışta halkayı zorunlu tutar. Anahtarı OpenBao'ya ve yeni
sürümü halkaya wizard koyar (bilet 06).

**Panel.** `POST /v1/suppressions/{id}/reactivate`, gövde `{"reason": "…"}`:
kişinin çıkışını yalnız kişi isteyince ya da bir hata olunca geri alır.
- Yalnız `skymail:unsubscribes:reactivate` (`skymail` istemci rolü, gruba
  verilir; Keycloak'ta bilet 10). Rol yoksa `403`; canlıda rol verilene kadar
  kimseye açık değildir.
- Gerekçe zorunlu: 10–500 karakter yazdırılabilir metin, satır sonu yok
  (`400 suppression.reason_required`). Adres içeremez: `@` geçen ya da satırın
  adresini içeren gerekçe `400 suppression.reason_has_address`.
- Bounce satırında `409 suppression.use_delete`; yoksa `404`.
- Başarıda `204`; `mail_unsubscribe_reactivations`'a `actor = operator`, kim
  (`actor_sub`), ne zaman, gerekçe, kaydın akışı, kaynağı ve ilk tarihi yazılır;
  adres yazılmaz. Hesap silme `actor_sub`'ı Silinmiş kullanıcı yapar, kişiyi
  adlandıran gerekçeyi `[silindi]` yapar.
- Geri alma kaydı (kişinin kendi geri alması da) yazılmasından 3 yıl sonra
  içerik süpürücüsünce silinir (KVKK aydınlatma metni s6; süre sabit, env
  yok).
- `GET /v1/suppressions/reactivations?_start=&_end=` aynı rolle geri almaları
  en yeni önce döner (`X-Total-Count`).

**Hesap silme.** Kişinin adreslerinin çıkış kayıtları silinir, sayaç
`unsubscribes_deleted` ([`docs/data-lifecycle.md`](docs/data-lifecycle.md)).

**Geri alma (rollback).** Önce bayrakla yapılır (bilet 08): `off` kaydı korur ve
uygulamaya devam eder. Eski imaj kaydı bilmez: imaj geri alınırsa o süre
çıkmış adreslere duyuru gidebilir, bu yüzden o süre panelden duyuru
gönderilmez. Göçün `down`'ı iki tabloyu siler ve bütün çıkışları kaybettirir:
production'da **kullanılmaz**.

## Bildirimler (`POST /v1/notifications`)

core kişiye (Keycloak `sub`) bir Bildirim türüyle yazar; SkyMail kişi başına
kanalı seçer (tür varsayılanı ∩ core'un izin verdiği kanallar ∩ adres) ve
kuyruğa alır. `Idempotency-Key` zorunlu; yalnız servis hesabı ve
`skymail:notifications:send`. Türleri Template seed yazar
(`PUT /v1/notification_types/{key}`). Kanallar e-posta ve uygulama içi kutu.
Sözleşme: [`docs/notifications.md`](docs/notifications.md).

## Haberden gönderim (`POST /v1/news_sends`, `SKYMAIL_NEWS_SEND`)

Yayımlanmış bir News kaydını (inscribed `news`) bir kitleye postalar (hub
bileti duyuru-haber 06; ADR 0067). Varsayılan kapalı; kapalıyken uç
`503 news_send.not_enabled` döner.

| Değişken | Anlamı |
|---|---|
| `SKYMAIL_NEWS_SEND` | `off` (varsayılan) ya da `on`. `on` iken `SKYMAIL_NEWS_CMS_URL`, `CORE_URL`, `SKYMAIL_LIST_UNSUBSCRIBE=on` ve `SKYMAIL_MAIL_PURPOSE=on` zorunlu; biri eksikse açılış durur (haber postası tek tık çıkış başlığı ya da amacı, kategorisi ve beyanı olmadan gitmez) |
| `SKYMAIL_NEWS_CMS_URL` | inscribed'ın adresi, `/cms` yolu olmadan (`http://inscribed:8080` ya da `https://api.yildizskylab.com/api`) |
| `SKYMAIL_NEWS_IMAGE_HOSTS` | kapak görselinin gelebileceği hostlar, virgülle; her biri alt alan adlarını da kapsar. Varsayılan `yildizskylab.com`. Kapak yalnız `https` ve bu hostlardan gösterilir, öteki atlanır (üçüncü taraf görseli fiilen bir açılma pikseli olur) |
| `SKYMAIL_NEWS_READ_MORE` | "Devamını oku" nereye gider: `home` (varsayılan, sitenin ana sayfası; sitenin haber sayfaları gelene kadar, bilet 11) ya da `pages` (`/haberler/<slug>`, `/duyurular/<slug>`) |

- **Yetki.** Yalnız `skymail:announcements:send` (ADMIN, YK; Karar D4)
  doğrudan gönderir. Öteki kişilerin Mail onayına sunması sonraki dilimde.
- **Kayıt.** inscribed'dan anonim okunur (`GET /cms/collections/news/{slug}`).
  Bilinmeyen ya da arşivli kayıt `404 news_send.not_found`. Faz 1'de yalnız
  herkese açık kayıt postalanır; başka görünürlük `409 news_send.private_record`.
  Şema v2'nin alanları yoksa anlamları geçerli: `kind` yoksa `haber`,
  `audience` yoksa `herkes`; düz metin gövde paragraflara çevrilir.
- **Anlık görüntü.** Kayıt gönderimde olduğu gibi `mail_tasks.source_snapshot`'a
  yazılır, sürümüyle (`source_version`). Sonraki düzenleme web'i değiştirir,
  postayı ve raporu değiştirmez.
- **Kitle core'dan.** `POST /v1/audiences/resolve` (core `docs/news-audience.md`),
  SkyMail'in servis hesabıyla, 500'lük sayfalarla. core tercihi kapalı,
  devre dışı, doğrulanmamış ve adressiz hesabı dışarıda bırakır, kişiye
  kendi çıkış bağlantısını ve "neden aldın" metnini verir. Bağlantısız kişi
  kuyruğa alınmaz (`skipped.no_unsubscribe_url`). core cevap vermezse hiçbir
  şey yazılmaz: `503 news_send.audience_unavailable`.
- **Bir kez.** Bir kaydın postası bir posta kutusuna bir kez gider:
  `mail_queue.source_ref = news:<slug>` ve `(source_ref, lower(recipient_email))`
  kısmi tekil indeksi. Önce ekibe sonra bütün üyelere gönderilirse ekiptekiler
  `already_received` sayılır. Aynı kayıt aynı kitleye ikinci kez
  `409 news_send.already_sent` (`params.task_id`) döner. Kuyruğa alırken
  başarısız yazılan satır (bastırılmış ya da çıkmış adres, render hatası)
  `source_ref` taşımaz.
- **Posta.** System template `club.news` (skymail-frontend, Template seed);
  yoksa `409 news_send.template_missing`. Kanonik slug 200 karakteri aşarsa
  `400 news_send.slug_too_long`. Birden çok ekipli gönderimde görevin
  kategorisi core'un ilk `team:` kategorisidir (rapor sütunu); çıkış
  bağlantısı her kişide kendi kategorilerini kapatır. Toplu şerit, `duyuru` amacı ve
  core'un kategorisi (`SKYMAIL_MAIL_PURPOSE` açıkken), List-Unsubscribe
  satırın core bağlantısından (`SKYMAIL_LIST_UNSUBSCRIBE` açıkken). Gövde
  e-posta izin listesinden geçer; haber için `s`, `code`, `pre`, `hr` eklendi.
  Konu varsayılanı başlık, duyuruda başında "Duyuru: ".
- **Şablon değişkenleri.** Gönderimin: `Subject`, `Preheader`, `Title`,
  `Summary`, `BodyHtml` (`safeHTML` ile; yalnız `format=full`), `FullBody`,
  `HeroUrl`, `HeroAlt`, `CtaHref`, `CtaLabel`, `ReadMoreUrl`, `KindLabel`.
  Alıcının (core'dan): `AudienceReason`, `CategoryLabel`, `UnsubscribeUrl`.
- **Rıza ve çıkış.** Alıcıların hepsi hesaptır (`recipient_sub`), bu yüzden
  `SKYMAIL_LIST_CONSENT`'in hesapsız adres kuralı bu gönderime uygulanmaz.
  SkyMail'in çıkış kaydı (`mail_unsubscribes`) kuyruğa alırken uygulanır.
- **Günlük.** Yalnız sayılar (`news_send_queued`): kişi, adres, bağlantı ve
  kaydın metni yazılmaz.
- **Önizleme, deneme, canlı sayı (bilet 07, 08).** `skymail:announcements:send`
  ya da `skymail:mails:write` ile:
  - `POST /v1/news_sends/preview`: gönderimin kuyruğa alacağı konu, HTML ve
    düz metin, çağıran okur olarak (örnek çıkış bağlantısı: tercihler sayfası;
    "neden aldın" core'un biçiminde örnek). Hiçbir şey yazılmaz.
  - `POST /v1/news_sends/test`: postayı yalnız çağıranın token'daki doğrulanmış
    adresine gönderir; konu "[Deneme]" ile başlar, kaynağı yoktur ("bir kez"e
    ve rapora girmez). Örnek çıkış bağlantısı yalnız gövdededir, List-Unsubscribe
    başlığı yoktur. Kişi başına dakikada en çok 3 (`429`).
  - `POST /v1/news_sends/count`: core'un yalnız sayıları (`countOnly`, adres
    okunmaz): üyeler, takipçiler, atlananlar; `already_received` kaydın bugüne
    dek ulaştığı posta kutusu sayısı (bu kitleden atlanacakların üst sınırı;
    kesin sayı gönderimde); bu kitleye gönderim varsa `already_sent_task_id`.
  - `GET /v1/news_sends/availability`: `SKYMAIL_NEWS_SEND` açık mı.

## Uygulama içi kutu (`/v1/me/inbox`)

Türün kanallarında `inbox` varsa her alıcıya, tercihine bakılmadan, bir kutu
öğesi yazılır (türün kutu metni, bildirim başına bir kez render edilir; kuyruğa
girmez). Kişi kendi öğelerini kendi token'ıyla okur ve işaretler:
`GET /v1/me/inbox` (imleçli, yeniden eskiye), `GET /v1/me/inbox/counts`,
`POST /v1/me/inbox/{id}/seen|read|unread|archive`, `POST /v1/me/inbox/read-all`,
`POST /v1/me/inbox/seen-all`. Token `aud`'unda `skymail` olmalı
(`V1_TOKEN_AUDIENCE_MODE`'dan bağımsız) ve kişi token'ı olmalı; rol, ve
`skymail:access`, istenmez. Başkasının öğesi `404`. Aynı zincirdeki öteki
kişi ucu: `GET /v1/me/email-delivery` (bkz. [bastırma listesi](#geri-dönen-posta-ve-bastırma-listesi-mail_bounces-mail_suppressions)). Öğe en çok 90 gün kalır
(bildirimin `expires_at`'i daha erken silebilir); kutu süpürmesi saatte bir
siler, hesap silme kişinin bütün öğelerini siler. Bu sürümden önceki imaja
dönmeden önce `inbox`'ı türlerden çıkarın. Sözleşme:
[`docs/inbox.md`](docs/inbox.md).

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
  `MAIL_CONTENT_RETENTION_DAYS` (varsayılan `365`, 1–3650) dolmuş
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

**Saklama süresi (KVKK aydınlatma metni s6, 2026-10-23; Yusuf'un
2026-10-19 "1 yıl" kararının yerine): kişi bazındaki açılma ve tıklama
kayıtları alıcının postasının gittiği andan (`sent_at`) 90 gün
(`tracking.EventRetentionDays` = 90) tutulur**, sonra anonimleşir: saklama
işi gönderimin kişisiz toplamını yazar, olayları siler ve alıcının
belirtecini siler (`tracking_token` NULL; bağlantılar kendi belirteçleriyle
yönlendirir);
kişiye geri götüren bir şey (özet, sıra numarası) kalmaz. IP hiç
okunmadığı için saklanacak ya da silinecek IP yoktur. Kişisiz toplam her
gönderimde tutulur: en az 10 takipli alıcıda süresiz; 10'dan azda
gönderimin oluşturulmasından 1 yıl sonra, içeriğiyle birlikte silinir.
Bağlantılar 1 yıl yönlendirir; 90 günden sonraki tıklama kişiye yazılmaz.
Olay yazımı, raporlar ve saklama işi aynı sabiti kullanır.

| Değişken | Varsayılan | Anlamı |
|---|---|---|
| `SKYMAIL_TRACKING` | `off` | `off` ya da `on`; başka değer açılışı durdurur |
| `SKYMAIL_TRACKING_SYSTEM` | `off` | servislerin postaları (hesap ve işlem postaları, bildirimler): `off` ya da `on`; başka değer açılışı durdurur; yalnız `SKYMAIL_TRACKING=on` iken etkili (aşağıda, "Sistem postaları") |
| `SKYMAIL_TRACKING_OPERATOR` | `off` | panelin (operatörün) gönderimleri: `off` kutuyla (yalnız isteyen), `on` her zaman takipli, kutu yok; başka değer açılışı durdurur; yalnız `SKYMAIL_TRACKING=on` iken etkili (aşağıda, "Operatör postaları") |
| `SKYMAIL_TRACKING_BASE_URL` | yok | `/t/`'nin herkese açık adresi (`https://api.yildizskylab.com/api/skymail`); yalnız https, kimlik bilgisi, sorgu ve parça yok; `on` iken zorunlu |
| `CORE_URL` | yok | core'un Docker ağındaki adresi (`http://core-backend:8080`); `on` iken zorunlu |
| `SKYMAIL_TRACKING_RETENTION` | `off` | saklama işi: `off`, `dry-run`, `apply` ya da `purge`; başka değer açılışı durdurur; `purge` için `SKYMAIL_TRACKING=off` zorunlu |
| `SKYMAIL_TRACKING_RETENTION_MAX_ROWS` | `50000` | saklama freni: bir apply'da kapanacak belirteç, silinecek olay ya da boşalacak bağlantı sınırı (1–10.000.000); başka değer açılışı durdurur |

Hatalı adres, değeri değil değişkeni söyleyerek açılışı durdurur. Açılışta
`open and click tracking (SKYMAIL_TRACKING)` satırı kipi ve adresleri,
`open and click tracking of system mail (SKYMAIL_TRACKING_SYSTEM; …)` satırı
sistem postalarının geçerli kipini (`mode`) ve istenen değeri (`requested`),
`open and click tracking of every operator send (SKYMAIL_TRACKING_OPERATOR; …)`
satırı aynısını operatör postaları için,
`open and click tracking retention (SKYMAIL_TRACKING_RETENTION)` satırı
saklama kipini, `event_retention_days` ve `max_rows` değerini yazar.

- **Kim takip edilir (gönderim kuyruğa yazılırken sabitlenir):** bayrak açık;
  gönderim `tracking: true` istedi (`POST /v1/mail_tasks`, bir kişinin
  `POST /v1/mail_tasks/single` gönderimi, listeye ya da kişilere Mail onayı
  isteği); satır toplu şeritte; şablon System değil ve hassas değil
  (`mail_task_content_sensitive()`, içerik panelinin tek tanımı; kişilere
  giden gönderimde içeriği hakkında bir şey söylemeyen şablon hassas
  sayılır). Servislerin postaları bu kurala değil `SKYMAIL_TRACKING_SYSTEM`'e
  bağlıdır (aşağıda, "Sistem postaları"); `SKYMAIL_TRACKING_OPERATOR=on`
  iken bir kişinin her gönderimi istese de istemese de takip edilir ve bu
  paragrafın şablon ve bağlantı kuralı yerine sistem postalarınınki geçer
  (aşağıda, "Operatör postaları"). Bayrak kapalıyken ya da şablon
  System/hassasken alan yok sayılır: yanıt, gönderim ve onay isteği `tracking: false` der, satırlara
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
  istekle hiçbir zaman takip edilmez. Bir servisin token'ıyla gelen
  `/single` (Keycloak, core, Forms: hesap ve işlem postaları) takip isteyemez:
  alan yok sayılır, posta sistem şeridinde kalır ve yalnız
  `SKYMAIL_TRACKING_SYSTEM=on` iken takip edilir (aşağıda). Forms kaynağı
  gönderimi takip istemez, `tracking: false` der; yalnız
  `SKYMAIL_TRACKING_OPERATOR=on` iken takiplidir ("Operatör postaları").
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
  formu kutuyu yalnız bu `true` iken gösterir) ve `tracking_always`
  (`SKYMAIL_TRACKING_OPERATOR=on` iken `true`: her gönderim takipli, form
  kutuyu hiç göstermez); aynı iki alan `GET /v1/mail_tasks/tracking-mode`'da
  yalnız `skymail:access` ile okunur (bir ayardır, gönderim değil:
  `mails:read`'i olmayan, yalnız onaya sunan kişi de görür); gönderim
  satırları, Mail onayı isteği ve `POST /v1/mail_tasks/single` yanıtı
  `tracking` alanı taşır.
- **Hesap silme** kişiye giden satırların olaylarını siler, belirteçlerini
  siler (`tracking_token` NULL), kişiyi adlandıran (yazıldığı gibi ya da URL çözülmüş) ya da yalnız
  ona giden gönderimin bağlantılarının adresini boşaltır; sayaçlar
  `tracking_events_deleted`, `tracking_links_cleared`
  ([`docs/data-lifecycle.md`](docs/data-lifecycle.md#open-and-click-tracking)).

### Sistem postaları (`SKYMAIL_TRACKING_SYSTEM`)

Yusuf, 2026-10-21: takip bütün postalarda (avukat onayı); gizli anahtarlı
bağlantılar takibe girmez. **Varsayılan kapalı**; açılış, KVKK aydınlatma
metni bu kapsamla yayımlandıktan sonra (hub
`notes/hukuki/kvkk-aydinlatma-metni-YENI-20261021-s3-taslak.md`). Yalnız
`SKYMAIL_TRACKING=on` iken etkilidir: `on` + `SKYMAIL_TRACKING=off` hata
değildir, hiçbir şey yapmaz, açılış satırı uyarır (takibi kapatmak SkyMail'in
açılmasını hiçbir zaman engellemez).

- **Hangi postalar:** servislerin istemeden gönderdiği postalar: bir servisin
  token'ıyla gelen `POST /v1/mail_tasks/single` (Keycloak'ın giriş/kayıt
  kodu, doğrulama, parola sıfırlama; core; Forms onayları), core'un
  bildirimleri (`POST /v1/notifications`, her iki şeritte) ve SkyMail'in
  kendi Mail onayı bildirimleri. Şablonun System ya da hassas olması takibi
  engellemez. Bir kişinin (operatörün) gönderimi bu bayrağa bağlı değildir:
  `SKYMAIL_TRACKING_OPERATOR` kapalıyken kutuyu işaretlemediyse takip
  edilmez, işaretlediyse yukarıdaki kuralla; açıkken her zaman (aşağıda,
  "Operatör postaları"). Forms kaynağı gönderimi (yanıtlayanlara operatör
  postası) de o bayrağa bağlıdır.
- **Şerit değişmez:** posta kendi şeridinde kalır (tekil posta ve sistem
  bildirimi sistem şeridinde, toplu bildirim toplu şeritte); gönderen
  şeridi (`LaneOf`) ve gönderim katmanı aynıdır, piksel ve tıklama adresi
  her yolda gönderim anında yerleştirilir.
- **Kim:** her alıcı yukarıdaki kuralla: `sub`'ı bilinen (bildirim) core'a
  sorulur; yalnız adresi bilinen (tekil posta, onay bildirimi) önce
  Keycloak'ta tam doğrulanmış birincil e-postayla aranır, hesap yoksa
  `no_account` (ör. henüz doğrulanmamış kayıt adresi), varsa core karar
  verir. Keycloak ve core gönderimin veritabanı işlemi başlamadan önce,
  **ikisi birlikte en çok 2 saniye** sorulur (giriş kodunu bekleyen biri
  var); süre dolarsa ya da yanıt yoksa `preference_unavailable`, posta
  gider. Mail onayı bildirimlerinde bütün onaycılar tek seferde sorulur
  (Keycloak en çok 5 eşzamanlı, core bir kez), sonra her bildirim kuyruğa
  yazılır: işlem onaycı sayısından bağımsız en çok 2 saniye bekler.
- **Gizli bağlantı hiçbir zaman `/t/`'den geçmez:**
  - İçeriği hassas posta (`mail_task_content_sensitive()`: kodu ya da tek
    kullanımlık bağlantıyı maskelenen posta; `keycloak.*`, `account.*`,
    `auth.*` ve kilitli anahtar sözcükleri, hassas işaretli şablon, içeriği
    hakkında bir şey söylemeyen tekil posta): **yalnız piksel**, hiçbir
    bağlantı yeniden yazılmaz, `mail_task_links`'e hiçbir şey yazılmaz.
    (Saklama işinin D7 kuralı da hassas gönderimin bağlantı adreslerini
    boşaltır.)
  - Öteki sistem postası: piksel, ve tıklama adresi **yalnız şablonun
    kendisinin olduğu gibi yazdığı** bağlantılarda: `href`'inde hiçbir
    şablon eylemi (`{{…}}`) olmayan, dolayısıyla her alıcının postasında
    aynı olan adres (`tracking.LiteralLinks`). Değişkenin doldurduğu ya da
    içine değişken giren her bağlantı (sıfırlama, doğrulama, giriş
    bağlantısı, `?davet=` gibi kişiye özel kod) olduğu gibi gider ve
    kaydedilmez; tek alıcılı gönderimin "değişkenli bağlantı da kaydedilir"
    istisnası burada yoktur. Serbest gövdeli şablonun (`safeHTML .BodyHtml`)
    gövdesindeki bağlantılar da değişkenden geldiği için yazılmaz.
  - `data-skymail-notrack` ve öteki kurallar (`mailto:`, `unsubscribe`,
    `communication-preferences`, `/t/`) her postada geçerlidir.
  - Emsal: SendGrid parola sıfırlama gibi postalarda `clicktracking`'i
    kapatmayı, Postmark bağlantı başına `data-pm-no-track`'i önerir; burada
    varsayılan güvenli taraftır: emin olunamayan bağlantı takibe girmez.
- **Kayıt:** gönderim `tracking_requested = true` olur; tekil postanın
  `POST /v1/mail_tasks/single` yanıtı alıcı gerçekten takip ediliyorsa
  (belirteç aldıysa) `tracking: true`, `no_account`, `preference_off`,
  `preference_unavailable` gibi bir nedenle edilmiyorsa `tracking: false`
  der. Satır nedenleri, raporlar, geri alma, hesap silme ve saklama aynıdır.
- **Kişi raporu yalnız rolle:** sistem şeridindeki gönderimin
  (`mail_queue.priority > 0`: servis tekil postası, SkyMail'in Mail onayı
  bildirimi) kişileri `GET /v1/mail_tasks/{id}/tracking/recipients`'te
  yalnız `skymail:mails:tracking:read` ile okunur; `sent_by` bir kişiyi
  (bildirimi doğuran isteyen ya da karar veren) adlandırsa da o kişi
  okuyamaz (KVKK aydınlatma metni s3 §4: sistemin gönderdiği postalardaki
  kişi bilgisini yalnız YK, DK ve sistem yöneticileri görür). Kişinin
  takipli gönderimi her zaman toplu şerittedir (sistem şeridi istekle ya da
  `SKYMAIL_TRACKING_OPERATOR` ile hiçbir zaman takip edilmez), gönderen
  onu okumaya devam eder. Bu kural bayrağa bağlı değildir; bugün sistem
  şeridinde takipli gönderim olmadığı için birleşmeyle görünür bir şey
  değişmez.
- **Geri dönüş:** `SKYMAIL_TRACKING_SYSTEM=off` + yeniden dağıtım: yeni
  sistem postaları takipsiz. Bilinen sınır: kapatmadan önce kuyruğa girmiş
  (saniyeler içinde giden) sistem postası takipli gider, daha önce gitmiş
  sistem postalarının açılması `SKYMAIL_TRACKING=on` kaldıkça yazılmaya
  devam eder. **Tam geri dönüş için `SKYMAIL_TRACKING=off`:** kuyrukta
  bekleyen takipli satır da temiz gider (belirteci `flag_off` ile kapanır),
  yeni olay yazılmaz; silme için saklama işinin `purge` kipi (aşağıda).
  Eski imaja dönüş açılışı bozmaz: `SKYMAIL_TRACKING_SYSTEM` ve
  `SKYMAIL_TRACKING_OPERATOR` değerleri `off|on` dışına çıkmaz, eski imaj
  `SKYMAIL_TRACKING_OPERATOR`'ı hiç okumaz.

### Operatör postaları (`SKYMAIL_TRACKING_OPERATOR`)

Yusuf, 2026-10-21: panelden giden postalarda takip **her zaman açık**;
gönderim formunda "takip et" kutusu ya da sorusu yok, gönderim başına
kapatılamaz. Sektörden bilinçli sapma: Mailchimp, HubSpot, Dynamics 365
Customer Insights ve SendGrid takibi varsayılan açık tutar ama kampanya ya
da gönderim başına kapatmaya izin verir; burada kapatma yalnız alıcı
tarafında (my.'daki itiraz ayarı, core `tracking:resolve`) vardır.
**Varsayılan kapalı**; açılış, KVKK aydınlatma metni bu kapsamla
yayımlandıktan sonra (hub
`notes/hukuki/kvkk-aydinlatma-metni-YENI-20261021-s3-taslak.md`). Yalnız
`SKYMAIL_TRACKING=on` iken etkilidir; `on` + `SKYMAIL_TRACKING=off` hata
değildir, hiçbir şey yapmaz, açılış satırı uyarır.
`SKYMAIL_TRACKING_SYSTEM`'den bağımsızdır: biri açılıp öteki kapalı
kalabilir, ayrı ayrı geri alınır.

- **Hangi postalar:** bir kişinin token'ıyla kuyruğa giren her gönderim:
  `POST /v1/mail_tasks` (internal liste, Keycloak grubu),
  `POST /v1/mail_tasks/single` (gönderim formunun kişileri),
  `POST /v1/mail_tasks/form-source` (Forms yanıtlayanları) ve onaylanan
  Mail onayı isteği (liste, grup, kişiler, Forms kaynağı). İstekteki
  `tracking` alanı yok sayılır (`false` da); yanıt, gönderim satırı ve onay
  isteği `tracking: true` der (Forms kaynağının tekrarı, ilk gönderimin
  takibini). Onay isteği takipsiz gönderilmiş olsa da bayrak açıkken
  onaylanırsa takipli gider. Servislerin postaları ve SkyMail'in
  bildirimleri bu bayrağa değil `SKYMAIL_TRACKING_SYSTEM`'e bağlıdır.
- **Şablon fark etmez; hassas postada yalnız piksel:** System ya da hassas
  şablon da takip edilir; içeriği hassas gönderim (`mail_task_content_sensitive()`:
  hassas işaretli ya da kilitli anahtarlı şablon, kişiye giden gönderimde
  içeriği hakkında bir şey söylemeyen şablon) **yalnız piksel** alır.
- **Bağlantılar: bütün alıcılara aynı gidenler** (orkestratör, 2026-10-21;
  Mailchimp, HubSpot ve Dynamics kampanya gövdesindeki her bağlantıyı
  sayar, istisna işaretli bağlantıdır): öteki gönderimde tıklama adresi,
  her alıcının postasında aynı olan ve alıcıdan hiçbir şey almayan
  bağlantılara konur: şablonda düz yazılmış, ya da gönderimin kendi
  değişkeninden gelen (serbest gövdeli şablonun `safeHTML .BodyHtml`
  gövdesindeki bağlantılar, herkese bir kez yazılmış adres, `href`'te
  kısmen gönderim değişkeni). Alıcıdan gelen (`.Email`, `.FullName`,
  `.UnsubscribeUrl`, tamamen ya da kısmen) hiçbir bağlantı `/t/`'den
  geçmez, tek kişiye gönderimde de: #85'in "tek alıcılı gönderimde
  değişkenli bağlantı da kaydedilir" istisnası bu bayrak açıkken yoktur.
  Kural, postanın gönderimin değişkenleriyle iki uydurma alıcı için
  (`.invalid` ve `.example` alanlarında, RFC 2606) yeniden işlenmesiyle
  uygulanır: iki uydurma postada ve her gerçek postada aynı olan bağlantı
  alıcıya bağlı değildir. İki uydurma alıcının adı, adresi ve abonelik
  adresi uzunlukta, ilk ve son karakterde, adres yerel kısımda ve alanda
  da farklıdır; `{{len .Email}}` ya da `{{slice .Email 0 1}}` gibi bir
  türevden yapılan bağlantı da iki uydurma postada ayrışır, kaydedilmez. Uydurma posta işlenemezse yalnız
  şablonda düz yazılmış bağlantılar sayılır. Gövde değişkeni şablon olarak
  yorumlanmaz: gövdeye yazılan `{{.Email}}` metindir, alıcının adresine
  dönüşmez. Tek istisna alıcının adı etiketidir ("Serbest duyuruda alıcının
  adı"): yalnız metne girer, hiçbir bağlantının adresine girmez. `data-skymail-notrack` öteki kurallarla her zaman geçerlidir;
  serbest gövdede de: temizleyici `<a>` üzerinde boş ya da `true` değerli
  `data-skymail-notrack`'i geçirir (başka öznitelik geçmez), panel
  düzenleyicisindeki "Bu bağlantıyı sayma" anahtarı bunu yazar. Kişiye
  özel ya da tek kullanımlık bir bağlantı (kişisel davet, toplantı
  adresi) böylece `/t/`'den geçmez ve `mail_task_links`'e yazılmaz
  (SendGrid `clicktracking=off`, Postmark `data-pm-no-track`, Mandrill
  `mc:disable-tracking` gibi). Yalnız bağlantı başına; gönderimin tamamı
  için kapatma yoktur. Sistem postalarının kuralı değişmedi: orada yalnız şablonda
  düz yazılmış bağlantı sayılır, çünkü servisin değişkeni gizli anahtar
  taşıyabilir.
- **Kim:** her alıcı yukarıdaki kuralla: grup üyeleri `sub`'larıyla core'a
  sorulur; gönderim formunun kişileri, onay isteğinin kişileri ve Forms
  yanıtlayanları önce Keycloak'ta aranır (hassas şablonda da), hesap yoksa
  `no_account`; internal listenin adres-yalnız alıcıları aranmaz,
  `no_account`. Bütçeler kişi gönderimininkidir: bir gönderimin bütün
  adresleri tek seferde, en çok 5 eşzamanlı aramayla **toplam 5 sn**
  içinde, sonra core bir kez (5 sn). Forms kaynağında arama kaynağı
  kilitleyen işlemden önce yapılır (kaynak değişmez); süre içinde yanıt
  gelmeyen adres `preference_unavailable` olur, gönderim beklemez. Aynı
  isteğin tekrarı kimseyi yeniden sormaz.
- **Şerit:** her operatör postası toplu şerittedir (gönderen şeridi
  `duyuru`); bayrak kapalıyken kutusuz giden tekil posta sistem şeridine
  (`bilgi`) giderdi. `SKYMAIL_MAIL_PURPOSE=on` iken `islem` amaçlı
  operatör gönderimi de takiplidir, toplu şeritte bekler ve `bilgi@`'dan
  çıkar (yukarıda "Gönderim amacı ve kategori"). Sistem şeridindeki bir posta bu bayrakla hiçbir zaman
  takip edilmez (kişi raporu kuralı buna dayanır, yukarıda).
- **Geri dönüş:** `SKYMAIL_TRACKING_OPERATOR=off` + yeniden dağıtım: form
  kutuyu yeniden gösterir, yeni gönderim yalnız isterse takip edilir.
  Bayrak açıkken gönderilmiş, henüz onaylanmamış onay isteği `tracking: true`
  taşır ve onaylanınca istemiş gibi gider (System/hassas şablonda takipsiz;
  Forms kaynağı takipsiz).
  Kuyrukta bekleyen takipli satır için ve tam geri dönüş için yukarıdaki
  `SKYMAIL_TRACKING=off`.

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
  toplam 50 ile sınırlar. Teslim edilmemiş veya `sent_at + 90 gün` dolmuş
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
  `{tracking_available, tracking_requested, retained, withheld, report}`. Raporda
  `tracked_recipients`, `untracked_by_reason`, `unique_opens`,
  `unique_likely_human_opens` (otomatik olmayan tıklamadan çıkarım dahil),
  `automated_only_recipients`, `unique_clickers`, `clicks_by_link`
  (`id`, `position`, `clicks`, `unique_clickers`) ve `series` vardır.
  Seri noktaları UTC'de ilk 72 saat saatlik, sonra günlük; `at`, `interval`,
  `likely_human_opens`, `automated_opens`, `likely_human_clicks`,
  `automated_clicks` taşır. Boş kovalar yazılmaz. Kimlik ve hedef URL yoktur
  (tek alıcılı hedef kişiyi adlandırabilir). Son postadan 90 gün sonra
`retained: true`:
  yalnız kişisiz toplam (10'dan az alıcılıda gönderimden 1 yıl sonra
  silinir), yoksa `report: null`; seri dönmez.
  **Küçük gönderim ve sistem postası:** takipli alıcısı 10'dan az
  (`tracking.TotalsMinRecipients`) ya da sistem
  şeridindeki bir gönderimin sayıları kişiye dair bilgidir ("1/1 açıldı,
  14:00"; alıcı `GET /{id}/queue`'da görünür). Bu sayıları yalnız kişi
  raporunu okuyabilen (aşağıdaki kural: `skymail:mails:tracking:read` ya
  da sistem şeridi dışında gönderimin kendi göndereni) alır; başka bir
  `mails:read` sahibine `withheld: true` ve `report: null` döner (KVKK
  aydınlatma metni s3 §4).
- `GET /v1/mail_tasks/{id}/tracking/recipients` rolü
  `skymail:mails:tracking:read` **ya da** `sent_by = token sub` ister;
  `mails:read` tek başına yetmez, gönderen için ayrıca gerekmez. Servis
  token'ı ve sistem şeridindeki gönderim (servis tekil postası, Mail onayı
  bildirimi) yalnız rolle okunur ("Sistem postaları"). Parametreler
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
  son `sent_at`'ından (teslim yoksa `created_at`) 90 gün sonra 410 döner
  ("Kişi bazında takip kaydı 90 gün sonra silindi.");
  bu tarihten önce de her alıcının kendi 90 günlük pencere dışı olayları
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
  satırlar `ORDER BY id FOR UPDATE` kilitlenir, açılma ve tıklama olayları
  silinir, sonra belirteçleri silinir (`tracking_token` NULL,
  `tracking_disabled_at` dolar; gönderilmiş ve kuyrukta bekleyen postalar
  dahil, bekleyen posta pikselsiz gider).
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
`apply`'a alınır (bilet 19). 90 gün beklenmez: D7 yalnız `apply`'da
çalışır, ilk 90 gün apply yalnız D7'ye dokunur.

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
  1. alıcılarından birinin postası 90 günden eskiyse ve henüz yoksa
     **önce** kişisiz toplam (`mail_task_tracking_totals`) gönderimin bütün
     olaylarından yazılır; her gönderimde (KVKK s6), 10'dan az takipli
     alıcılıda yalnız gönderimin ilk yılında (geç koşu yazmaz).
     "Takipli alıcı" yalnız postası gitmiş (`status='sent'`) ve belirteci
     açık (geri almamış, silinmemiş) olandır; ötekiler toplamda
     `not_delivered` / `tracking_closed` nedeniyle görünür;
  2. postası 90 günden eski alıcıların belirteçleri silinir
     (`tracking_token` NULL, `tracking_disabled_at` dolar): eski adreslerdeki
     `rt` artık kimseyi göstermez, tıklama bağlantının kendi belirteciyle
     1 yıl yönlendirir;
  3. 1 yıllık bağlantıların adresi boşalır (`expired`); maskelenmiş ya da
     sonradan hassaslaşmış (`mail_task_content_sensitive()`) gönderimin
     bütün bağlantılarının adresi boşalır (`erased`, inceleme D7);
  4. postası 90 günden eski alıcıların olayları silinir (geri dönüşsüz:
     açılma/tıklamayı kişiye bağlayan satır kalmaz);
  5. kuyrukta bekleyen ya da 90 gün içinde olan postası kalmayan gönderim
     kapanır (`tracking_closed_at`); raporu artık yalnız kişisiz toplamdır;
  6. 10'dan az takipli alıcılı gönderimin kişisiz toplamı, gönderimin
     oluşturulmasından 1 yıl sonra silinir (içerikle birlikte).
- **dry-run ile apply aynı tanımdan:** dry-run aynı ifadeleri çalıştırıp
  işlemi geri alır; apply önce dry-run'ı koşar, sonra değiştirir.
- **Fren:** bir koşuda kapanacak belirteç, silinecek olay ya da toplam veya
  boşalacak bağlantı sayısı `SKYMAIL_TRACKING_RETENTION_MAX_ROWS`'u (varsayılan 50.000)
  aşarsa apply hiçbir şey değiştirmez (`refused_large`, `error` düzeyinde
  `"alarm":"tracking_retention_brake"`). Karar yalnız apply'ın önce koştuğu
  dry-run sayılarına bakar; değişiklik yapan apply hiçbir zaman
  `refused_large` yazmaz. dry-run aynı durumu `would_refuse_large` diye
  raporlar. Biriken iş fren yüzünden her gün reddediliyorsa sınır bir
  yayınlık yükseltilir. `purge`'ün freni yoktur.
- **Makbuz:** her koşu `tracking_retention_runs`'a bir satır yazar (kip,
  başlangıç/bitiş, durum `ok|partial|refused_large|failed`,
  `events_cutoff`, durum ayrıca `would_refuse_large` olabilir; sayılar: `sends`, `totals_written`, `tokens_closed`,
  `totals_deleted`, `events_deleted`, `links_expired`, `links_erased`, `sends_closed`,
  `sends_failed`); kişi, belirteç ya da adres yoktur. Aynı sayılar
  `"event":"tracking_retention_run"` günlük satırındadır.
- **`purge` (tam geri dönüş):** `SKYMAIL_TRACKING=off` ister (yoksa açılış
  durur). Bütün belirteçleri siler ve bütün olayları siler, yaşına
  bakmadan; kişisiz toplamlar ve bağlantılar kalır, eski postaların
  bağlantıları yönlendirir. Her koşuda tekrarlanır (zararsız); iş bitince
  kip **`apply`'a** alınır (`off`'a değil: `off`'ta 1 yıllık bağlantı
  boşaltması, 10'dan az alıcılı toplamın 1 yıllık silinmesi ve D7 de durur; bayrak kapalıyken `apply` zararsızdır). Elle SQL
  gerekmez.
- **Bilinen sınırlar:** (1) kişisiz toplam ilk alıcının 90 günü dolduğu
  koşuda bir kez yazılır; aynı gönderimin sonra giden alıcılarının o andan
  kendi 90 günlerine kadarki olayları toplama girmez (gönderim dakikalar içinde
  gittiği için pratikte etkisiz). (2) Maskeleme süpürücüsü görevi sonra
  kuyruk satırlarını, saklama ise önce kuyruk satırlarını kilitler; bir
  yıllık takipli gönderimin şablonu tam o anda hassaslaşırsa Postgres
  birini ölü kilit diye iptal eder: saklama `sends_failed` yazıp ertesi gün
  dener, maskeleme başarısızlık sayar (hassas içerik için güvenli yön).
- **Geri dönüş (`20261023180000`, toplam her gönderimde):** imajı eskiye
  döndürmek yeterli, eski imaj yeni şemada çalışır (yalnız ≥10 toplam
  yazar). Down migration yalnız eski imaj ayaktayken ya da
  `SKYMAIL_TRACKING_RETENTION=off` iken koşulur: önce 10'dan az alıcılı
  toplamları siler (geri gelmez), sonra CHECK'i geri koyar; yeni imajın
  saklama işi arada 10'dan az bir toplam yazarsa CHECK düşer ve sürüm
  `dirty` kalır. Silinen belirteçler hiçbir geri dönüşte geri gelmez.

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

## Serbest duyuruda alıcının adı

Gönderimin kendi değişkenleri veridir, şablon değildir: serbest duyurunun
gövdesine yazılan `{{.Email}}` metin olarak gider. Bunun tek istisnası
alıcının adıdır (sektörün "merge tag"i: Mailchimp `*|FNAME|*`, SendGrid
`{{first_name}}`; hub bileti `skymail-serbest-duyuru-alici-adi` 01). Tam
olarak iki etiket, yazıldığı gibi tanınır (`internal/mailer/recipient_name.go`):

- `{{.FullName}}`: alıcının adı;
- `{{or .FullName "katılımcı"}}`: ad, ya da adı boş alıcıda tırnak içindeki
  yedek (1–60 karakter; `"` `\` `{` `}` `<` `>` `&` ve denetim karakteri
  yok). Yedeksiz etiket adı boş alıcıda hiçbir şey basmaz. Mailchimp,
  HubSpot, Braze ve Klaviyo'nun "varsayılan değer"i budur.

Başka her `{{…}}` (başka değişken, `range`/`if`/işlev, boşluklu ya da
kırpma işaretli etiket) bugünkü gibi metin kalır; hiçbir şablon motoru
çalışmaz, etiket metin olarak bulunup değiştirilir. Etiketlerin anlamı Go'nun
aynı ifadeye verdiği anlamla aynıdır.

- **Nerede:** şablonun konusunun bastığı gönderim değişkenlerinde (free.basic:
  `Subject`; ön başlık da onu basar) ve `safeHTML` ile basılanlarda
  (free.basic: `BodyHtml`). Gövdede yalnız metinde, ad HTML kaçışlı;
  öznitelikte, dolayısıyla bağlantı adresinde asla. Alıcının kendi
  değişkenleri (Haber gönderiminin `AudienceReason`'ı gibi), şablonun kendi
  metni ve öteki değişkenler değişmez.
- **Ad:** satırın `recipient_full_name`'i; boşluk ve denetim karakteri
  dizileri tek boşluk, baş ve sonu kırpılmış. İlk ad türetilmez: satırda
  yalnız tam ad var; iki göbek adlı Türkçe adın hangisinin ilk ad olduğu
  tahmin edilmez.
- **Takip:** bağlantılar herkese aynı kalır (`linksSendLevel`'in iki uydurma
  alıcısı da farklı ad alır, bağlantı ortak kalır); ad yalnız metindedir.
- **Maskeleme ve saklama:** ad, gönderilen içerikte postanın kendisi gibi
  durur (`recipient_full_name` zaten satırdadır); maskeleme postayı aynı
  adla yeniden işler; Account erasure içeriği her zamanki gibi boşaltır.
- **Önizleme:** skymail-frontend aynı iki etiketi aynı kuralla doldurur;
  iki repo aynı vaka dosyasını tutar (`internal/mailer/testdata/recipient-name.json`,
  SHA-256 iki testte sabit).

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

Kaynak bilgisi (`POST` yanıtı ve `GET`) `unnamed_count` ile adı boş
yanıtlayan sayısını da verir: alıcının adını kullanan duyuruda ("Serbest
duyuruda alıcının adı") gönderim sayfası bunu uyarı olarak gösterir.

Hesap silme kaynak sahibini ve eşleşen alıcı/başlık bilgisini de temizler.
Etkilenmiş snapshot geçersizleşir; bekleyen kaynakla gönderim/onay 409 verir.
Harcanmamış süresi dolan kaynak mevcut expiry döngüsünde temizlenir. Kaynak
verisi varken migration down veri silerek geri dönmez; uyumlu binary kullanılır.
