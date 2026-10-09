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

## Eski imaja geri dönüş

Migration'lar expand-only yazılır: eski imaj yeni sütunları yok sayar, bu
yüzden geri dönüşte şemayı geri almak gerekmez.

1. **Eski imajı `DATABASE_MIGRATIONS_MODE=off` ile deploy edin.** Veritabanı
   eski imajın tanımadığı bir sürümdeyse (ör. `20261009120000`) eski imaj
   `apply` modunda açılamaz: golang-migrate gömülü kaynakta o sürümü bulamaz
   (`no migration found for version …`) ve servis `log.Fatal` ile durur.
   Start-first'te çalışan eski görev ayakta kalır, ama sonraki her deploy ve
   yeniden başlama (gece sır rotasyonu dahil) açılamaz.
2. **Down migration'ı koşmayın.** Yeni imaj çalışırken sütunları düşürmek
   onun sorgularını `column does not exist` ile düşürür; eski imaja dönüldükten
   sonra da gerekmez, sütunlar yerinde kalabilir.
3. **Yeniden ileri yayından hemen önce** geri dönüşün bıraktığı bayat
   değerleri temizleyin, sonra `DATABASE_MIGRATIONS_MODE=apply`'a geri dönüp
   yeni imajı deploy edin. Mail kuyruğu kirası (`20261009120000`) için: eski
   imaj satırı geri alırken ya da yeniden denerken `claimed_at`/`claimed_by`'ı
   bırakır ve sonra o satırı eski bir `claimed_at` ile `processing` yapabilir.
   Yeni görev böyle bir satırı kirası dolmuş sayıp eski görev gönderirken
   `pending`'e geri alır (çift e-posta). Temizlik bu satırları damgalama yoluna
   sokar (kira ilk görüldükleri an başlar):

   ```sql
   UPDATE mail_queue SET claimed_at = NULL, claimed_by = NULL WHERE status IN ('pending', 'processing');
   ```

## Kirli (dirty) sürüm

Bir migration yarıda kalırsa golang-migrate sürümü `dirty` bırakır ve servis
açılmaz (`database migration version … is dirty`). SkyMail'in migration
dosyaları tek örtük işlemde koşar: başarısız olan dosyanın değişiklikleri geri
alınmıştır. Nedeni giderdikten sonra (ör. `20261009120000`'in 5 saniyelik
`lock_timeout`'u: `mail_queue`'yu uzun tutan işlemi bitirin) sürümü bir
önceki migration'a çekip yeniden deploy edin:

```sql
UPDATE schema_migrations SET version = 20260925200000, dirty = false;
```

Değer, başarısız migration'dan bir önceki sürümdür; `db/migrations`'daki
sıraya bakın.
