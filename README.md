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

## Mail kuyruğu kirası (`MAIL_QUEUE_LEASE`)

Dağıtıcı bir kuyruk satırını aldığında satır o sürecin olur: `claimed_by`
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

Her kuyruk satırı bir şerittedir. `1` sistem şeridi: `POST /mail_tasks/single`
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

## Gönderilen içerik (`MAIL_CONTENT_RETENTION_DAYS`)

Her kuyruk satırı, alıcısına gönderildiği hâliyle konuyu, düz metni ve HTML'i
zaten tutar. `GET /v1/mail_tasks/{id}/queue/{itemId}/content` bunu gösterir;
`access` kimin neyi gördüğünü söyler, alanlar yalnız `available` iken doludur:

| Gönderim | Gönderen | `skymail:mails:write` | `skymail:system-mails:read` | Diğer `skymail:mails:read` |
|---|---|---|---|---|
| Hassas şablon (kod, tek kullanımlık bağlantı) | `sensitive` | `sensitive` | `sensitive` | `sensitive` |
| Liste, grup ya da onaylanmış istek (toplu) | görür | görür | görür | `restricted` |
| Tekil posta (`/single`), şablonu görünür diyorsa | görür | `restricted` | görür | `restricted` |

- Hassaslık şablonun `content_sensitive` alanıdır: `true` hiç gösterilmez,
  `false` gösterilir, `null` gönderim türüne bırakır: tekil posta gizli, toplu
  gönderim görünür. Template seed yazar (`PUT /v1/templates/by-key/{key}`,
  `content_sensitive`), panel iki yönde değiştirir
  (`PUT /v1/templates/{id}/content-sensitive`, `skymail:templates:write`). Her
  değişiklik kim, ne zaman, eski→yeni olarak kaydedilir
  (`GET /v1/templates/{id}/content-sensitive/events`). Migration bilinen
  şablonları işaretler: kimlik bilgisi taşıyanlar `true`, seed'in görünür
  dediği tekiller (`core.welcome`, `core.certificate`, `club.team-membership`,
  `mail.approval-*`, `ops.send-failed`, …) `false`; böylece seed'den önceki ilk
  süpürme onları boşaltmaz.
- **Kilitli gizli:** `keycloak.*`, `account.*`, `core.contact-consent-confirm`,
  `event.ticket-created`, `event.reminder` (giriş/doğrulama kodu, sıfırlama ya
  da hesap bağlantısı, bilet kodu). Panel de seed de bunları görünür yapamaz
  (`409 template.content_sensitive_locked`); okuma anında da gizli sayılırlar.
  Liste tek yerde: `internal/handlers/content_sensitivity.go`.
- `skymail:system-mails:read` SkyMail istemcisinin rolüdür (yönetici: YK, DK,
  ADMIN). Keycloak'ta verilene kadar tekil postaların içeriğini yalnız gönderen
  görür.
- Bir gönderimin `body_variables`'ı da içeriktir: gönderim listesi ve gönderim
  sayfası onu yalnız `content_access: available` iken verir.
- Süpürücü beş dakikada bir, gönderilecek satırı kalmamış gönderimlerin
  içeriğini boşaltır (20'lik partiler): hassas olanı hemen (oluşturulduktan beş
  dakika sonra), ötekini `MAIL_CONTENT_RETENTION_DAYS` gün sonra (varsayılan
  izin verilen en çok değer `3650`; 1–3650 arası tam sayı, başka değer açılışı
  durdurur). Satırın konusu ve gövdesi `''`, HTML'i `NULL`, gönderimin
  `body_variables`'ı `{}` olur, `mail_tasks.content_cleared_at` zamanı yazar.
  Durum, alıcı, hata ve sayılar kalır. Ayrıntı ve KVKK notu:
  [`docs/data-lifecycle.md`](docs/data-lifecycle.md#sent-mail-content).
- **İlk yayında** süpürücü bütün hassas gönderimlerin içeriğini bir kez
  boşaltır. Geri alınamaz (yalnız yedekte kalır).

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
