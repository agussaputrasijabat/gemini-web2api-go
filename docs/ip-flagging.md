# Analisis IP Flagging pada Endpoint `StreamGenerate` Gemini Web

Dokumen riset untuk proxy `gemini-web2api`. Fokus: mengapa `POST .../StreamGenerate`
mendapat `HTTP 302 -> sorry/index` dari IP tertentu, sementara `GET /app` tetap `200`,
dan apa yang bisa dilakukan di sisi kode maupun operasional.

Status: **riset baca-saja**. Tidak ada perubahan kode, tidak ada request ke
`gemini.google.com` selama penyusunan. Semua klaim yang tidak bisa diverifikasi
diberi label **tidak terverifikasi**.

---

## Ringkasan

- `302` ke `https://www.google.com/sorry/index` adalah **gerbang reCAPTCHA anti-bot Google**,
  bukan error Gemini, bukan `bl` kedaluwarsa, bukan masalah cookie. Ini sinyal reputasi
  jaringan (terutama IP), dengan sidik jari TLS/HTTP dan pola request sebagai faktor penguat.
- `GET /app` bisa `200` sementara `POST StreamGenerate` kena `302` karena keduanya dinilai
  berbeda: halaman statis ber-cache di edge dengan skor risiko rendah, sedangkan `StreamGenerate`
  adalah RPC internal ber-body signed (`f.req`), berparameter `bl`/`_reqid`, memakai
  `X-Same-Domain` + `SAPISIDHASH`, dan berpola request berulang — profil yang jauh lebih
  "mencurigakan" bagi risk engine.
- Pada kasus ini **bukan sekadar soal datacenter vs residential**: IP yang diblokir
  (`<home-ip>`) terdaftar di AS IDNIC/MyRepublic (broadband/last-mile), dan IP proxy yang
  lolos (`<proxy-ip>`) adalah Telkom Indonesia. Keduanya Indonesia. Jadi pemicunya
  **reputasi IP + pola trafik + sidik jari**, bukan tipe jaringan secara naif.
- Penyebab paling mungkin di kode: `NewClient` **mengabaikan `impersonate` ketika `proxy` diisi**
  (`internal/gemini/client.go:83`), sehingga seluruh trafik berjalan dengan TLS/HTTP2 fingerprint
  stdlib Go sementara `User-Agent` dipotong menjadi string non-browser
  (`client.go:250`). Kombinasi ini adalah mismatch UA-vs-TLS yang klasik.
- Tindakan paling berdampak, berurutan: hentikan retry/pemakaian dari IP yang kena,
  pindah ke IP keluar bersih yang **sticky** (bukan rotasi per-request), lalu perbaiki
  konsistensi sidik jari (TLS profile + UA utuh + header order), baru bicara rate limit.

---

## Mekanisme Blokir

### Apa itu `sorry/index`

`https://www.google.com/sorry/index?continue=<url>` adalah halaman interstitial anti-abuse
Google. Ia muncul ketika sistem otomatis Google mendeteksi trafik yang "dikirim oleh robot,
program komputer, layanan otomatis, atau scraper" — dokumentasi Google menyebut blokir ini
**berbasis jaringan/IP**, bukan semata akun atau perangkat, dan blokir "akan berakhir tak lama
setelah request tersebut berhenti". Halaman ini menampilkan reCAPTCHA; menyelesaikannya akan
memasang cookie sehingga permintaan berikutnya diteruskan.

Konsekuensinya untuk sebuah proxy otomatis: **tidak ada cara menyelesaikan challenge ini tanpa
interaksi browser**. Yang bisa dilakukan hanyalah menurunkan skor risiko, atau berganti IP.

Catatan teknis pihak ketiga (konfigurasi routing Sukka, lihat Rujukan) menyatakan bahwa
`sorry/index` membawa *state token* dan edge Google membandingkan IP klien di `gemini.google.com`
dengan IP klien di `www.google.com`; ketidakcocokan (mis. proxy hanya meng-route satu domain,
atau IP keluar berubah antar-hop) berujung `429`. **Status: tidak terverifikasi** — ini
observasi komunitas, bukan dokumentasi resmi. Namun konsisten dengan gejala di proyek ini
(lihat bagian Pemulihan: jangan campur jalur ber-proxy dan tanpa-proxy).

### Mengapa bisa spesifik ke `POST`, padahal `GET` lolos

`GET https://gemini.google.com/app` dilayani sebagai **dokumen HTML dari edge cache**. Tidak ada
state sesi yang dipersonalisasi (untuk anonim), tidak ada body ter-sign, dan satu request tidak
menghasilkan efek apa pun. Skor risikonya rendah; banyak bot pun bisa menarik HTML.

`POST https://gemini.google.com/_/BardChatUi/data/assistant.lamda.BardFrontendService/StreamGenerate`
berbeda secara kualitatif:

| Aspek | `GET /app` | `POST StreamGenerate` |
|---|---|---|
| Bentuk request | GET tanpa body | POST form `f.req` = array JSON bersarang 100+ slot |
| Parameter kontrol | tidak ada | `bl` (build label), `hl`, `_reqid` (sekuensial), `rt=c`, kadang `f.sid` |
| Header khusus | tidak ada | `X-Same-Domain: 1`, `Origin`, `Referer /app`, `Authorization: SAPISIDHASH <ts>_<sha1>`, `X-Goog-AuthUser` |
| Semantik | baca halaman | *memanggil model*: memicu biaya komputasi & kuota |
| Pola | sekali per muat halaman | berulang, rapat,`_reqid` naik monoton |

Endpoint RPC yang memicu inferensi adalah target penyalahgunaan paling bernilai, sehingga
justru di situ Google memasang penilaian risiko paling ketat. `_reqid` yang naik cepat dari satu
IP, body yang identik strukturnya, dan tidak adanya jejak "manusia" (navigasi, aset statis,
cookie consent, dsb.) adalah pola yang mudah dikenali.

### Sinyal yang dinilai Google

Diurutkan dari yang paling menentukan (berdasarkan laporan komunitas, bukan dokumentasi resmi):

1. **Reputasi IP / ASN / riwayat abuse.** Riwayat IP dipakai ulang untuk scraping/VPN/proxy
   membuat skor buruk meski pemiliknya baru. Ini faktor dominan: ada laporan request `POST`
   `StreamGenerate` **tanpa header apa pun** tetap `200` dari IP bersih.
2. **Sidik jari TLS (JA3/JA4).** Handshake non-browser (Go `crypto/tls`, `python-requests`,
   bahkan `curl_cffi` dengan `impersonate`) dapat dikenali. Issue #323 di `HanaokaYuzu/Gemini-API`
   melaporkan seluruh request kena `429` walau `impersonate="chrome"` dan cookie valid, sementara
   cookie yang sama berhasil dari Chrome asli.
3. **Sidik jari HTTP/2.** Urutan & nilai frame SETTINGS, urutan pseudo-header, window size,
   prioritas stream. Berbeda antar-implementasi HTTP client.
4. **Urutan & kapitalisasi header.** Go `net/http` menulis header menurut urutan map + set
   kanonik; browser punya urutan deterministik yang khas.
5. **Mismatch `User-Agent` vs TLS/HTTP2.** UA mengaku Chrome tetapi handshake Go/curl → sinyal
   paling kuat dan paling murah dideteksi. **Ini terjadi di kode saat ini** (lihat Rekomendasi).
6. **Cookie/akun.** `SID`/`SAPISID` valid menaikkan kepercayaan, tetapi tidak imun: ada laporan
   akun + IP + environment ikut kena. Anonim lebih cepat dicurigai pada endpoint RPC.
7. **Rate & pola waktu.** Interval antar-request yang terlalu teratur, burst, atau retry
   beruntun langsung setelah `302` (retry = sinyal "ini bot").

Catatan penting: sinyal 2–5 adalah **penguat**, bukan penyebab tunggal. IP dengan reputasi bersih
sering lolos meski sidik jarinya jelek; IP dengan reputasi buruk gagal meski sidik jarinya sempurna.
Karena itu memperbaiki TLS saja tidak menyembuhkan IP yang sudah ditandai.

---

## Kenapa IP Ini Kena

Data yang bisa diverifikasi (RDAP APNIC + `ip-api.com`, diambil saat penyusunan):

| | IP diblokir | IP proxy (lolos) |
|---|---|---|
| Alamat | `<home-ip>` | `<proxy-ip>` |
| Blok RDAP | `<home-ip-blok>` | `<proxy-ip-blok>` |
| Nama blok | `<home-isp-block>` | `<proxy-isp-block>` |
| ASN (ip-api) | `AS63859 PT. Eka Mas Republik` (`MYREPUBLIC-AS-ID`) | `AS7713 PT Telekomunikasi Indonesia` (`telkomnet-as-ap`) |
| Negara | Indonesia (Riau, Pekanbaru) | Indonesia (Jakarta) |
| Flag `hosting` / `proxy` / `mobile` | `false` / `false` / `false` | `false` / `false` / `false` |

Kesimpulan yang jujur dari data ini:

- Kedua IP **bukan datacenter** menurut klasifikasi ip-api, dan keduanya **berada di negara yang
  sama**. Jadi teori populer "IP datacenter pasti diblokir, residential pasti aman" **tidak
  menjelaskan kasus ini**.
- Perbedaan yang tersisa adalah **riwayat/reputasi IP** dan **seberapa bersih IP keluar dibanding
  pola trafik yang dikirim darinya**. `<home-ip>` berasal dari blok kecil ISP last-mile yang
  dipakai bersama banyak pelanggan — probabilitas blok itu pernah dipakai untuk trafik otomatis
  jauh lebih tinggi daripada blok besar Telkom.
- `<home-ip>` diblokir pada `POST` tetapi `GET /app` tetap `200`, konsisten dengan penilaian
  per-endpoint (bagian sebelumnya), bukan blokir penuh tingkat jaringan.

**Tidak terverifikasi:** skor fraud (`Scamalytics`), status exit-proxy (`Spur`), riwayat abuse
blok tersebut, dan apakah ada host lain di blok yang sama sedang scraping. Juga **tidak
terverifikasi** apakah `<home-ip>` adalah IP publik langsung mesin ini atau keluar dari
NAT/router kantor; kalau di belakang NAT, yang dinilai adalah IP publik bersama itu.

Cara memverifikasi sendiri (aman, tidak menyentuh Google):

```bash
# 1. Metadata IP — ASN, org, flag hosting/proxy
curl -s "http://ip-api.com/json/<home-ip>?fields=status,country,isp,org,as,asname,proxy,hosting,mobile"
curl -s "http://ip-api.com/json/<proxy-ip>?fields=status,country,isp,org,as,asname,proxy,hosting,mobile"

# 2. RDAP resmi (APNIC) — registran & rentang blok
curl -s "https://rdap.apnic.net/ip/<home-ip>" | python3 -m json.tool | head -40

# 3. whois klasik (kalau tool tersedia)
whois <home-ip>

# 4. IP publik keluar yang benar-benar dipakai saat ini (via proxy vs tanpa proxy)
curl -s https://api.ipify.org; echo
curl -s -x http://127.0.0.1:60000 https://api.ipify.org; echo
```

Perhatikan langkah 4: kalau dua hasilnya berbeda jauh antar-request (rotasi diam-diam), itu sendiri
merusak konsistensi sesi dan mempercepat flag.

---

## Pencegahan

Diurutkan berdasarkan dampak nyata pada kasus ini.

### 1. TLS impersonation — pelengkap, bukan pengganti proxy

Yang **diperbaiki**: sidik jari JA3/JA4 dan (sebagian) perilaku HTTP/2 menjadi mirip Chrome,
sehingga sinyal "UA Chrome tapi handshake Go" hilang.
Yang **tidak diperbaiki**: reputasi IP. IP yang sudah ditandai tetap ditandai meski TLS sempurna.
Bukti pendukung: issue `curl_cffi` #323 — `impersonate="chrome"` + cookie valid tetap `429`
karena (menurut pelapor) fingerprint `curl_cffi` sendiri sudah dikenali, sementara sesi browser asli
lolos. Dengan kata lain: impersonation menaikkan peluang, tidak menjamin.

Urutan prioritas: **IP bersih > konsistensi sidik jari > impersonation profile tertentu.**

### 2. Rate limit + jitter

Batas atas jumlah request per menit per IP keluar, dengan jeda acak. Interval yang persis sama
(contoh: tiap 2000 ms) adalah pola mesin. Jitter ±30–50% cukup untuk menghilangkan periodisitas.
Rate limit juga melindungi dari burst tak sengaja (retry storm dari banyak klien paralel).

### 3. Hindari retry agresif

Retry setelah `302 -> sorry/index` **tidak akan pernah berhasil** dari IP yang sama dan justru
memperkuat sinyal. Retry hanya boleh untuk kegagalan transport/5xx, dengan backoff eksponensial.
(Saat ini kode sudah benar untuk `KindBlocked` — lihat `retryable()` — tapi `KindRateLimited`
masih di-retry dengan ramp linear.)

### 4. Rotasi proxy: kapan menguntungkan, kapan merugikan

- **Menguntungkan** saat IP keluar sudah ter-flag dan tidak ada cara lain: ganti ke IP bersih lain
  di region yang sama.
- **Merugikan** kalau rotasi dilakukan **per-request**. Sesi Gemini bersifat stateful: cookie
  `SAPISID`/`SID` terikat pada jejak sesi, dan `sorry/index` memvalidasi kecocokan IP antar-hop
  (klaim pihak ketiga). Rotasi per-request menghasilkan "satu akun, puluhan IP, dalam semenit" —
  pola yang justru merupakan definisi bot. Gunakan **sticky session** (IP sama selama 10–30 menit
  atau selama satu percakapan).
- Rotasi juga tidak memperbaiki TLS/HTTP2 fingerprint dan tidak menyembuhkan akun yang sudah
  ditandai.

### 5. Konsistensi sidik jari

Satu identitas untuk satu jalur trafik:

- `User-Agent` harus versi Chrome **lengkap** dan **cocok** dengan TLS profile yang dipakai
  (mis. `chrome_131` ↔ UA Chrome/131). UA terpotong tanpa versi = sinyal anomali langsung.
- Header order & casing konsisten dengan browser target (idealnya lewat library yang mengelola
  urutan, bukan `http.Header` map).
- Profil `Accept`, `Accept-Language`, `Accept-Encoding`, `Sec-CH-UA*` konsisten satu sama lain.
- **Satu akun per IP** (atau satu akun per IP-sticky). Login banyak akun berurutan dari IP yang
  sama adalah pola farm akun.
- Jangan mencampur jalur: kalau request POST lewat proxy, semua request lain (BL scrape, upload,
  discovery) juga harus lewat proxy yang sama.

### 6. Warm-up sesi

Manusia membuka `/app` dulu, memuat aset, lalu mengirim pesan. Menembak `POST StreamGenerate`
sebagai request pertama dari IP baru adalah anomali. Alur yang lebih wajar:
`GET /app` (dapat cookie + `bl`) → jeda 1–3 detik → `POST StreamGenerate`.
Kebetulan kode sudah punya jalur GET ini di `FetchLatestBL` (`internal/gemini/bl.go:206`), jadi
warm-up bisa memanfaatkan kode yang ada.

### 7. Jeda manusiawi & kuota harian

Batasi total request per IP per jam dan per hari. Berhenti total (bukan memperlambat) ketika
menerima `302`, minimal selama beberapa menit.

### Yang justru memperburuk

- Retry otomatis langsung setelah `302` / `429`.
- Rotasi IP per-request, atau berganti IP tiap kali kena blok (memperluas jejak flag).
- `impersonate` diisi tetapi UA tetap terpotong (mismatch lebih buruk daripada konsisten jelek).
- Parallelisme tinggi (10+ request bersamaan) dari satu IP.
- Mencampur proxy dan non-proxy untuk domain yang sama dalam satu sesi.
- Berganti akun Google saat IP masih ter-flag (menandai akun tambahan).
- Menyelesaikan reCAPTCHA sekali lalu lanjut scraping dari IP yang sama dengan laju tinggi.
- Membuat `_reqid` melompat-lompat atau memakai `bl` kedaluwarsa (memicu `405`, lalu retry, lalu
  eskalasi sinyal).

---

## Pemulihan

**Lama blokir: tidak terverifikasi.** Tidak ada angka resmi. Google menyatakan blokir "berakhir
tak lama setelah request berhenti"; laporan komunitas berkisar dari beberapa menit sampai
beberapa jam, dan bisa lebih lama kalau trafik terus dikirim. Jangan mengasumsikan angka pasti;
ukur sendiri dengan probe hemat (lihat bawah).

**Apakah akun ikut kena?** Bukti campuran dan **tidak terverifikasi untuk kasus ini**:

- Dokumentasi Google menekankan blokir berbasis jaringan/IP, dan "login tidak membebaskan".
- Namun ada laporan (issue #235 `HanaokaYuzu/Gemini-API`) yang menyatakan **akun + IP +
  environment** sama-sama ter-flag, dan berganti akun tidak menolong.
- Praktisnya: anggap IP sebagai variabel utama, akun sebagai variabel kedua. Uji dengan akun yang
  sama di IP bersih dulu **sebelum** menyimpulkan akun rusak.

### Langkah konkret

1. **Stop.** Hentikan seluruh trafik ke Gemini dari IP yang ter-flag (termasuk retry otomatis dan
   background BL refresh). Setiap request tambahan memperpanjang.
2. **Tunggu.** Minimal 30–60 menit tanpa request apa pun dari IP itu. Jangan probe berulang.
3. **Siapkan IP keluar bersih** yang *sticky* di region yang sama (Indonesia, karena kedua IP
   yang diamati berada di ID). Verifikasi IP keluar lewat `api.ipify.org` lewat proxy, bukan dari
   `ip-api` saja.
4. **Probe hemat dengan GET dulu.** Satu `GET /app` lewat IP baru. `200` + body berisi build label
   `boq_*` = jalur siap. `302 -> sorry/index` = IP baru juga ter-flag; ganti lagi, jangan lanjut POST.
5. **Baru satu POST** percobaan (prompt trivial, non-stream). Kalau `302` lagi: kembali ke langkah 1,
   jangan retry.
6. **Nyalakan trafik bertahap**: 1 req/menit selama beberapa menit, amati status. Naikkan pelan.
7. **Catat**: IP keluar, waktu, status, dan `reqid` per request. Tanpa catatan, mustahil
   membedakan "IP buruk" dari "pola kita yang buruk".
8. Kalau IP bersih tetap gagal sementara browser asli berhasil dengan cookie yang sama, tersangka
   utama berpindah ke sidik jari (TLS/HTTP2/UA), bukan lagi IP.

---

## Rekomendasi Kode

Semua rujukan baris merujuk pada kondisi repo saat dokumen ini ditulis. Tidak ada kode yang diubah.

### (a) Backoff eksponensial + jitter untuk `KindBlocked`

`retryDelay` (`internal/gemini/client.go:356-365`) hanya punya dua cabang: konstan untuk semua
error, dan ramp linear untuk `KindRateLimited`. Untuk `KindBlocked`, menambah delay tidak akan
menolong — yang benar adalah **tidak retry sama sekali** (sudah benar lewat `retryable()`
`client.go:335-347`), tetapi delay perlu dipakai untuk `KindRateLimited` dan `KindTransport`
secara eksponensial:

```go
// client.go:356 — ganti isi retryDelay
func (c *Client) retryDelay(attempt int, err error) time.Duration {
    base := time.Duration(c.Cfg.RetryDelaySec) * time.Second
    if base <= 0 {
        base = time.Second
    }
    // exponensial: base * 2^attempt, cap 60s
    d := base << attempt
    if d > 60*time.Second {
        d = 60 * time.Second
    }
    // jitter +-40% supaya pola tidak periodik
    j := time.Duration(rand.Int63n(int64(d)*8/10+1)) - d*4/10
    return d + j
}
```

Konsekuensi: `KindBlocked` tetap terminal (tanpa retry). Yang berubah hanya kualitas delay untuk
error yang memang layak di-retry.

### (b) Circuit breaker → 503 cepat

Saat ini kegagalan upstream selalu dipetakan ke `502 Bad Gateway`
(`internal/server/chat.go:89`, `internal/server/responses.go:134,149`,
`internal/server/google.go:77,144`). Untuk `KindBlocked`, klien sebaiknya menerima
**`503` + `Retry-After`** dan server berhenti memanggil Google sampai cooldown habis —
ini menghentikan amplifikasi (klien yang retry sendiri akan menambah beban ke IP ter-flag).

Tempat implementasi yang paling murah: di `Client` (bukan di handler), karena `Generate` dan
`GenerateStream` sama-sama lewat sana.

```go
// tambahan di struct Client (client.go:60)
blockedUntil time.Time   // nol seri = tidak ter-flag
```

- Di `triageStatus` ketika `isSorryLocation(loc)` (`client.go:280`): set
  `c.blockedUntil = time.Now().Add(coolDown)` sebelum mengembalikan error.
- Di awal `Generate`/`GenerateStream` (`client.go:400`, `client.go:501`): jika
  `time.Now().Before(blockedUntil)`, langsung return `&UpstreamError{Kind: KindBlocked, ...}`
  tanpa menyentuh jaringan.
- Di handler, petakan `KindBlocked` → `503` + header `Retry-After` (detik sampai `blockedUntil`),
  bukan `502`. Perlu helper kecil di `internal/server/helpers.go` untuk
  `errors.As(err, &upstreamErr)` → status code.

### (c) Health probe `GET /app` sebelum POST mahal

Kode sudah punya building block: `FetchLatestBL` melakukan `GET /app` dengan
`http.NewRequestWithContext(... http.MethodGet ...)` (`internal/gemini/bl.go:206`) dan sudah
mendeteksi `sorry/index` lewat `isSorryLocation` (`bl.go:228`, dengan sentinel `errBLBlocked`
di `bl.go:153`). Artinya: **probe murah sudah ada**, hanya belum dipakai sebagai gate.

Usulan: fungsi baru `func (c *Client) probeHealthy(ctx) error` yang memanggil `FetchLatestBL`
dengan timeout 3–5 detik, lalu:

- dipanggil sebelum POST pertama setelah idle (mis. > 5 menit), atau setelah `blockedUntil` lewat;
- hasil `errBLBlocked` → set `blockedUntil`, jangan lanjut POST;
- hasil sukses → sekaligus menyegarkan `bl` (bonus: mengurangi `405`).

Catatan: probe ini mengeluarkan 1 request GET ke Google. Jangan dipanggil per-request; cukup
sebagai gate setelah idle/cooldown.

### (d) Rate limiter token bucket configurable

Tidak ada rate limiter sama sekali saat ini (tidak ada dependency `golang.org/x/time/rate` di
`go.mod`). Tambahkan field config:

```go
// internal/config/config.go:10-32 (struct) dan Default() config.go:38-51
UpstreamRPS   float64 `json:"upstream_rps"`    // default 0.5 (= 1 req / 2s)
UpstreamBurst int     `json:"upstream_burst"`  // default 1
```

Lalu satu `*rate.Limiter` di `Client`, di-`Wait(ctx)` tepat sebelum `c.HTTP.Do(req)` pada
`client.go:445` dan `client.go:547`. Nilai 0 = nonaktif (backward compatible). Ini juga
menggantikan `time.Sleep(c.retryDelay(...))` yang saat ini memblokir goroutine
(`client.go:494`, `client.go:593`) dengan penantian yang sadar-context.

### (e) Peringatan startup kalau `impersonate` kosong

`main.go:104-107` hanya mencetak `Impersonate: none (stdlib)` — informatif, bukan peringatan.
Karena fingerprint stdlib Go adalah sinyal risiko, kondisi ini layak menjadi warning eksplisit
saat `proxy` diisi:

```go
// main.go:104 — setelah impersonateStatus dihitung
if cfg.Impersonate == "" {
    log.Printf("WARNING: impersonate is empty — upstream requests use the Go stdlib TLS/HTTP2 " +
        "fingerprint while sending a browser User-Agent. This UA/TLS mismatch raises the risk " +
        "score and is a common cause of sorry/index blocks. Set --impersonate chrome_131.")
}
```

### (f) Urutan fallback proxy vs tanpa proxy

Saat ini keputusan bersifat saling meniadakan dan **diam-diam mematikan impersonation**:

```go
// client.go:83
if cfg.Impersonate != "" && cfg.Proxy == "" {
```

Artinya, begitu `proxy` diisi, `impersonate` diabaikan total dan yang dipakai adalah
`http.Transport` stdlib (`client.go:92-111`). Ini menjelaskan konfigurasi saat ini
(`proxy: http://127.0.0.1:60000`, `impersonate: ""`): dua-duanya tidak bisa aktif bersamaan.

Usulan minimal: **dukung keduanya sekaligus**, karena `tls-client` sudah menerima opsi proxy.

```go
// client.go:83 — ganti kondisi
if cfg.Impersonate != "" {
    opts := []http_client.HttpClientOption{
        http_client.WithClientProfile(resolveProfile(cfg.Impersonate)),
        http_client.WithTimeoutSeconds(cfg.RequestTimeoutSec),
        http_client.WithNotFollowRedirects(),
    }
    if cfg.Proxy != "" {
        opts = append(opts, http_client.WithProxyUrl(cfg.Proxy))
    }
    // ... getTLSClient menerima opts
}
```

(Ganti signature `getTLSClient` di `internal/gemini/fingerprint.go:54` agar menerima `proxy string`,
dan tambahkan proxy ke `cacheKey` di `fingerprint.go:57` supaya klien tidak saling tertukar.)

Urutan fallback yang disarankan, dari paling aman:

1. **proxy + impersonate** (IP bersih + sidik jari konsisten) — target utama.
2. **tanpa proxy + impersonate** (kalau IP lokal bersih; berguna untuk debugging).
3. **proxy + stdlib** (kondisi sekarang) — hanya sebagai *last resort*, dan sebaiknya disertai
   warning dari poin (e).
4. **tanpa proxy + stdlib** — jangan dipakai untuk `POST`; hanya untuk probe diagnostik.

### (g) Perbaiki `User-Agent` yang terpotong

`client.go:250` (dan `bl.go:130`) memakai:

```go
h.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36")
```

String ini **tidak valid sebagai UA browser** — tidak ada `Chrome/<versi>` dan `Safari/537.36`
di ujungnya. Browser sungguhan tidak pernah mengirim UA seperti ini, sehingga header ini sendiri
sudah menjadi penanda otomatis. Ganti dengan UA lengkap yang **cocok dengan profil TLS** yang
dipakai, mis. untuk `chrome_131`:

```text
Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36
```

Idealnya UA diturunkan dari profil (`resolveProfile`) supaya tidak bisa mismatch, dan
`Sec-CH-UA`, `Sec-CH-UA-Platform`, `Accept`, `Accept-Language` ikut diselaraskan.

### (h) Catatan kecil lain

- `client.go:266-268`: `Accept-Encoding: identity` hanya diset ketika `impersonate != ""`.
  Untuk profil browser, `identity` justru **tidak wajar** (Chrome mengirim
  `gzip, deflate, br, zstd`). Ini perlu diselaraskan dengan profil, bukan dikunci ke `identity`.
- `client.go:288-293`: `405` diperlakukan sebagai `bl` kedaluwarsa. Perlu diingat, `405` juga bisa
  muncul dari lapisan abuse. Pembeda yang aman: `405` tanpa `Location: sorry/index` = `bl` stale;
  dengan `Location` = blokir (saat ini urutannya sudah benar karena `isSorryLocation` dicek lebih
  dulu di `client.go:280`).
- `retryDelay` dipanggil dengan `time.Sleep` tanpa sadar-context — request yang kliennya sudah
  putus tetap menahan goroutine. Ganti dengan `select` pada `ctx`/timer.

---

## Langkah Sekarang untuk User

Urut dari paling efektif. Jalankan satu per satu; jangan lanjut sebelum langkah sebelumnya jelas.

1. **Hentikan trafik ke Gemini dan turunkan agresivitas retry.** Set `retry_attempts` ke `1`
   sementara, supaya kegagalan tidak diulang dari IP yang sama:

   ```bash
   ./gemini-web2api --config config.json --port 8081   # dengan "retry_attempts": 1 di config.json
   ```

2. **Pastikan IP keluar yang sebenarnya** (bukan asumsi). Bandingkan langsung vs lewat proxy:

   ```bash
   curl -s https://api.ipify.org; echo
   curl -s -x http://127.0.0.1:60000 https://api.ipify.org; echo
   ```

   Kalau hasil lewat proxy berubah-ubah antar panggilan, proxy-nya rotasi → ini masalah, bukan solusi.

3. **Tunggu 30–60 menit tanpa request** dari IP `<home-ip>`. Jangan probe berkala.

4. **Probe hemat: satu GET saja lewat IP bersih** (jangan POST dulu). Cek juga apakah body
   mengandung build label:

   ```bash
   # status + redirect tujuan, tanpa mengikuti redirect
   curl -s -o /dev/null -D - --max-time 15 \
     "https://gemini.google.com/app" | head -20

   # lewat proxy kandidat
   curl -s -o /dev/null -D - --max-time 20 -x http://127.0.0.1:60000 \
     "https://gemini.google.com/app" | head -20

   # kalau 200: pastikan build label ada di body
   curl -s --max-time 20 -x http://127.0.0.1:60000 \
     "https://gemini.google.com/app" | grep -o 'boq_[a-zA-Z0-9_.-]*' | head -3
   ```

   `302` dengan `Location: .../sorry/index` = IP itu masih ter-flag. `200` + `boq_*` = lanjut.

5. **Nyalakan dengan profil TLS + UA yang konsisten.** Jalankan dengan impersonation aktif
   (catatan: kombinasi ini baru berfungsi setelah perbaikan `client.go:83`; sebelum itu,
   `--impersonate` **diabaikan** bila `--proxy` dipakai):

   ```bash
   ./gemini-web2api --proxy http://127.0.0.1:60000 --impersonate chrome_131
   ```

   Sementara (tanpa mengubah kode), pilihan realistis: pakai **salah satu** — proxy saja, atau
   impersonate saja — dan sadari trade-off-nya. Jangan mengira keduanya aktif bersamaan.

6. **Uji satu POST trivial, sekali saja** (non-stream), lalu berhenti dan baca hasilnya:

   ```bash
   curl -s -X POST http://127.0.0.1:8081/v1/chat/completions \
     -H "Authorization: Bearer sk-gemini" \
     -H "Content-Type: application/json" \
     -d '{"model":"gemini-3.6-flash","messages":[{"role":"user","content":"hi"}]}' | head -c 400
   ```

   `502` + pesan `redirected to sorry/index` = IP masih ter-flag → kembali ke langkah 3 dengan
   IP lain. Jangan ulangi POST.

7. **Jeda manusiawi**: setelah satu request sukses, tunggu 2–5 detik sebelum berikutnya; batasi
   total ~1 req/2s. Matikan paralelisme klien selama pemulihan.

8. **Ganti IP keluar bila langkah 4 tetap `302`.** Pilih IP **sticky** di region Indonesia
   (bukan rotasi), lalu ulangi langkah 4. Kalau IP bersih tetap gagal padahal browser asli lolos
   dengan cookie yang sama, masalahnya bukan IP — lanjut ke perbaikan sidik jari (bagian
   Rekomendasi poin (g) dan (f)).

---

## Rujukan

URL yang benar-benar dibuka selama riset:

**Perilaku blokir & `sorry/index`**
- Google Search Help — "Resolve Google Search's 'Unusual traffic from your computer network' message":
  https://support.google.com/websearch/answer/86640?hl=en
- Google Search Community FAQ — "'Unusual traffic' blocks searching or requires reCAPTCHA":
  https://support.google.com/websearch/thread/2596872/faq-unusual-traffic-blocks-searching-or-requires-recaptcha?hl=en
- reCAPTCHA Help — "My computer is sending automated queries":
  https://support.google.com/recaptcha/answer/6081888?hl=en
- reCAPTCHA Developer FAQ:
  https://developers.google.com/recaptcha/docs/faq

**Gemini web reverse engineering — laporan blokir nyata**
- `HanaokaYuzu/Gemini-API` issue #323 — "curl_cffi TLS fingerprint detected by Google — 429 on all
  gemini.google.com/app requests": https://github.com/HanaokaYuzu/Gemini-API/issues/323
- `HanaokaYuzu/Gemini-API` commit `838009c` — penambahan error code `1060` (IP temporarily blocked):
  https://github.com/HanaokaYuzu/Gemini-API/commit/838009c5437f9a331cf646fa5cef5c77213935a1
- `HanaokaYuzu/Gemini-API` issue #235 — "Stream interrupted" (diskusi akun+IP ter-flag):
  https://github.com/HanaokaYuzu/Gemini-API/issues/235
- `xtekky/gpt4free` issue #3302 — CAPTCHA bergantung pada IP, bukan kode:
  https://github.com/xtekky/gpt4free/issues/3302
- `acheong08/Bard` issue #10 — dukungan proxy (`all_proxy`):
  https://github.com/acheong08/Bard/issues/10
- `acheong08/Bard` issue #61 — kegagalan respons dari IP yang tidak didukung:
  https://github.com/acheong08/Bard/issues/61

**Protokol `StreamGenerate` & header autentikasi**
- DeepWiki — "StreamGenerate Protocol & Payload Builder":
  https://deepwiki.com/one880808/gemini-web2api/3.1-streamgenerate-protocol-and-payload-builder
- DeepWiki — "Google Session Authentication (SAPISIDHASH & Cookie)":
  https://deepwiki.com/one880808/gemini-web2api/4.2-google-session-authentication-(sapisidhash-and-cookie)
- `tail_fin_gemini::signing` (format `SAPISIDHASH`):
  https://docs.rs/tail-fin-gemini/latest/tail_fin_gemini/signing/index.html
- Scrape.do — "2 Ways to Scrape Gemini Responses and Sources" (bukti bahwa POST anonim bisa `200`
  dari IP bersih tanpa header, dan catatan bahwa rotasi IP tidak menyentuh TLS/HTTP2 fingerprint):
  https://scrape.do/blog/gemini-scraping/

**`sorry/index` dan state token / kecocokan IP (klaim pihak ketiga, tidak terverifikasi resmi)**
- Konfigurasi routing Sukka (`ai.conf`) — komentar "Here is how gemini.google.com doing HTTP 429 right now":
  https://git.ac/mirror/SukkaSurge/raw/6e23e8576bf64521d3eefe46b022dc7216a7ebf9/Source/non_ip/ai.conf

**Error 1060 & pemilihan IP/proxy**
- Google Gemini Apps Help — "Troubleshoot error codes in Gemini Apps":
  https://support.google.com/gemini/answer/18647528?hl=en
- Gemini Apps Community — "Gemeni access problem (1060)":
  https://support.google.com/gemini/thread/459120963/gemeni-access-problem-1060?hl=en
- IPFLY — "Gemini Isn't Currently Supported in Your Country? How to Fix It" (sticky IP vs rotasi):
  https://www.ipfly.net/blog/gemini-isnt-currently-supported-in-your-country-how-to-fix-it/
- IPPeak — "Gemini and Residential Proxies" (reputasi jaringan vs sekadar tipe proxy):
  https://www.ippeak.com/blog/372
- dementor.cn — "Gemini / ChatGPT Says Your Region Is Unsupported — Even Through a Proxy"
  (reputasi IP, DNS/timing mismatch):
  https://dementor.cn/en/troubleshooting/ai-service-region-blocked/

**Metadata IP (dipakai di bagian "Kenapa IP Ini Kena")**
- RDAP APNIC: https://rdap.apnic.net/ip/<home-ip> dan https://rdap.apnic.net/ip/<proxy-ip>
- ip-api: `http://ip-api.com/json/<home-ip>` dan `http://ip-api.com/json/<proxy-ip>`

**Library**
- `bogdanfinn/tls-client` (Go, profil TLS/JA3-JA4): https://github.com/bogdanfinn/tls-client
  (Context7 ID: `/bogdanfinn/tls-client`; dokumentasi gitbook:
  https://bogdanfinn.gitbook.io/open-source-oasis)
