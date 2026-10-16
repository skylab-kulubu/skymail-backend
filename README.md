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
dakika). Göndericinin hatası sonucu söyler: hata yok gönderildi, kalıcı hata
(geçersiz adres) hemen düşer, ötekiler yeniden denenir.

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
  kopyalanır; gönderim, bu kopya **ya da** şablonun şimdiki değeri ya da
  kilitli anahtar hassas diyorsa hassastır. Şablonu sonradan `false` yapmak
  eski gönderimi açmaz.
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
notu 07). **Varsayılan kapalı.** Bu sürümde takip verisi yazılır ve postaya
yerleştirilir; `/t/` uçları, okuma uçları, geri alma ucu ve 90 günlük saklama
işi sonraki sürümlerde gelir. Bayrak onlar yayında ve aydınlatma metni
yayımlanmış olmadan açılmaz: **bu imaj `SKYMAIL_TRACKING=on` ile açılışı
reddeder** (`/t/` uçları yok; 14b bu reddi uçlarla birlikte kaldırır).

| Değişken | Varsayılan | Anlamı |
|---|---|---|
| `SKYMAIL_TRACKING` | `off` | `off` ya da `on`; başka değer açılışı durdurur |
| `SKYMAIL_TRACKING_BASE_URL` | yok | `/t/`'nin herkese açık adresi (`https://api.yildizskylab.com/api/skymail`); yalnız https, kimlik bilgisi, sorgu ve parça yok; `on` iken zorunlu |
| `CORE_URL` | yok | core'un Docker ağındaki adresi (`http://core-backend:8080`); `on` iken zorunlu |

Hatalı adres, değeri değil değişkeni söyleyerek açılışı durdurur. Açılışta
`open and click tracking (SKYMAIL_TRACKING)` satırı kipi ve adresleri yazar.

- **Kim takip edilir (gönderim kuyruğa yazılırken sabitlenir):** bayrak açık;
  gönderim `tracking: true` istedi (`POST /v1/mail_tasks`, Mail onayı isteği);
  satır toplu şeritte; şablon System değil ve hassas değil
  (`mail_task_content_sensitive()`, içerik panelinin tek tanımı). Bayrak
  kapalıyken ya da şablon System/hassasken alan yok sayılır: yanıt, gönderim
  ve onay isteği `tracking: false` der, satırlara neden yazılmaz. Sonra
  alıcının Keycloak `sub`'ı biliniyor
  (`recipient_sub`: yalnız Keycloak grubu üyeleri) ve core izin veriyor.
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
  formu kutuyu yalnız bu `true` iken gösterir); gönderim satırları ve Mail
  onayı isteği `tracking` alanı taşır.
- **Hesap silme** kişiye giden satırların olaylarını siler, belirteçlerini
  kapatır, kişiyi adlandıran (yazıldığı gibi ya da URL çözülmüş) ya da yalnız
  ona giden gönderimin bağlantılarının adresini boşaltır; sayaçlar
  `tracking_events_deleted`, `tracking_links_cleared`
  ([`docs/data-lifecycle.md`](docs/data-lifecycle.md#open-and-click-tracking)).

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
  ve `core:communication-preferences:read` arar (bilet 17).

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
