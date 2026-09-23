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

`GET /health` is process-only liveness. `GET /ready` additionally verifies the
exact shared account-access contract when the access gate is in `enforce` mode.
API documentation under `/docs` remains public. See
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
`mails:*` kaynak rolleri, `skymail:mails:approve` mail onayı.

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

Mail onayı bildirimlerindeki linkler `SKYMAIL_UI_URL` altına kurulur
(varsayılan `https://mail.yildizskylab.com`).

## Veritabanı migration'ları

Uygulama bekleyen migration'ları servis trafiğe açılmadan önce çalıştırabilir.
Mevcut sürümlendirilmemiş kurulumun güvenli baseline işlemi ve gerekli environment
değerleri için [`docs/database-migrations.md`](docs/database-migrations.md)
belgesine bakın.


## Katkıda Bulunanlar 🧙‍♂️

<a href="https://github.com/skylab-kulubu/skymail-backend/graphs/contributors">
  <img src="https://contrib.rocks/image?repo=skylab-kulubu/skymail-backend" />
</a>

<sup><sub>[contrib.rocks](https://contrib.rocks) ile yapıldı.</sub></sup>
