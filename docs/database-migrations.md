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

## Aynı anda açılan görevler

Bir görev, sürümü okumadan önceden işi bitene dek oturum düzeyinde bir advisory
lock tutar: `pg_try_advisory_lock(hashtextextended('skymail-backend migrations', 0))`
(`migrations.LockName`). Aynı anda açılan öbür görevler (ölçekleme, iki kopya,
start-first deploy'un yeni görevi) kilidi 500 ms'de bir yeniden ister, en çok
5 dakika (`migrations.LockWait`) bekler, sonra sürümü temiz bulup yapacak iş
bulmaz. Eskiden göç sürerken açılan görev golang-migrate'in uygulama boyunca
koyduğu `dirty` işaretini ya da öbürünün doldurduğu boş veritabanını görüp
`log.Fatal` ile çıkıyordu (yeniden başlama döngüsü). Gerçekten yarıda kalmış
bir migration (süreci ölmüş, sürüm hâlâ `dirty`) eskisi gibi servisi durdurur:
aşağıdaki "Kirli (dirty) sürüm" bölümü. Bekleme sırasında gelen durdurma
sinyali beklemeyi bitirir; kilidi almış bir görevin migration'ı sinyalle
yarıda kesilmez. 5 dakikayı aşan bir migration'da bekleyen görev başarısız
olur ve yeniden başlar; böyle bir yayında Swarm health check'inin
`StartPeriod`'u da büyütülmelidir (`docs/health-and-shutdown.md`).

Alternatif (gerekirse): migration'ı deploy'dan önce tek seferlik bir işte
koşmak (`DATABASE_MIGRATIONS_MODE=apply` ile tek bir konteyner, servis
`DATABASE_MIGRATIONS_MODE=off`). Kilit bunu bugün gereksiz kılıyor.

## Expand, sonra contract

Dokploy start-first deploy eder: eski görev, yeni görev migrate edip açılırken
yeni şemayla çalışmaya devam eder. Bu yüzden her migration bir önceki sürümü
çalışır bırakmalıdır:

- **expand** (bu sürüm): tablo, nullable sütun, varsayılanlı sütun, indeks
  (büyük tabloda `CONCURRENTLY`; çok ifadeli bir dosya tek örtük işlemde
  koştuğu için kendi dosyasında tek ifade olarak), yeni kısıt önce
  `NOT VALID`;
- **contract** (sonraki bir sürüm, çalışan hiçbir kod okumadığında): sütun ya
  da tablo düşürme veya yeniden adlandırma, `NOT NULL` yapma, kısıtı
  doğrulama (`VALIDATE CONSTRAINT`).

Yeniden adlandırma bir expand (yeni sütunu ekle, ikisine de yaz) ve sonraki
bir contract'tır (eskisini düşür). Bu kurala uyamayan bir sürüm bir kez
stop-first, kesinti duyurularak yayınlanır. sqlc sorgulardaki `*`'ı üretirken
sütun listesine açar, bu yüzden eski imaj yeni sütunları yok sayar.

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
