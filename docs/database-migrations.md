# Veritabanı migration'ları

SkyMail migration dosyalarını uygulama binary'sine gömer. Otomatik uygulama,
geriye dönük uyumlu bir rollout için varsayılan olarak kapalıdır.

## Yeni veya sürümlendirilmiş veritabanı

```env
DATABASE_MIGRATIONS_MODE=apply
```

Boş bir veritabanında uygulama tüm migration'ları sırayla uygular. Daha önce bu
çalıştırıcı tarafından sürümlendirilmiş bir veritabanında yalnızca bekleyen
migration'lar uygulanır. Migration başarısız veya kirli kalırsa servis başlamaz.

## Mevcut sürümlendirilmemiş veritabanını devralma

Eski kurulumlarda tablolar bulunmasına rağmen `schema_migrations` kaydı yoktur.
İlk otomatik çalıştırmada doğrulanmış mevcut şema açıkça baseline edilmelidir:

```env
DATABASE_MIGRATIONS_MODE=apply
DATABASE_MIGRATIONS_BASELINE_VERSION=20260919180000
```

Uygulama baseline kaydını yazmadan önce beklenen tabloları, kritik kolonları,
indeksleri ve kaldırılmış eski nesneleri kontrol eder. Şema doğrulanamazsa hiçbir
eski migration yeniden oynatılmaz ve servis başlamaz.

İlk başarılı açılıştan ve `schema_migrations` sürümü doğrulandıktan sonra
`DATABASE_MIGRATIONS_BASELINE_VERSION` kaldırılabilir. Sonraki dağıtımlarda yalnız
`DATABASE_MIGRATIONS_MODE=apply` kalmalıdır.

`migrate-down` yalnızca kontrollü ve yedekli bakım işlemleri içindir; uygulama
başlangıcında otomatik downgrade yapılmaz.
