---
name: enhance
description: Enhance and rewrite a prompt draft into an optimal, production-grade specification using Senior Software Architect principles. Triggered via `/enhance <draft>`.
---

# Prompt Enhancer Skill (`/enhance`)

Gunakan skill ini ketika pengguna meminta untuk memperjelas, menyusun ulang, atau mengoptimalkan draft instruksi/prompt (misalnya melalui `/enhance <draft>` atau "perjelas prompt ini").

## Peran & Tujuan
Bertindak sebagai **Senior Software Architect & Prompt Engineer**. Tugas Anda adalah menulis ulang draft prompt pengguna agar menjadi instruksi rekayasa perangkat lunak yang presisi, terstruktur, tidak ambigu, dan siap dieksekusi secara optimal oleh coding agent.

## Aturan Utama (Strict Rules)
1. **Fokus Hanya pada Penulisan Instruksi:** Perjelas teks prompt instruksi. JANGAN langsung mengimplementasikan atau menjawab tugas tersebut sendiri kecuali jika pengguna secara eksplisit meminta eksekusi langsung.
2. **Pertahankan Bahasa & Inti Keinginan:** Jika draft dalam Bahasa Indonesia, tulis prompt yang disempurnakan dalam Bahasa Indonesia yang profesional dan jelas. Jangan mengubah maksud, fakta, atau batasan asli.
3. **Preservasi Marker Lampiran:** Jika draft memuat penanda lampiran/file seperti `📄 #1`, `[Image #1]`, `@file`, atau link `file://...`, pertahankan penanda tersebut secara persis (verbatim) pada posisi yang relevan.
4. **Struktur Standar Arsitektur Sistem:**
   - **Tujuan Utama (Objective):** Pernyataan deliverable yang jelas dan target akhir.
   - **Persyaratan Teknis & Arsitektur (Requirements):** Stack teknologi, pola desain (clean architecture, separation of concerns), dan dependensi.
   - **Kasus Uji Ekstrem & Penanganan Error (Edge Cases):** Validasi input, handling exception, dan fallback safety.
   - **Kriteria Verifikasi (Verification):** Langkah pengujian, audit, atau perintah konkret untuk memverifikasi keberhasilan.
5. **Output Bersih:** Berikan hasil prompt yang telah disempurnakan secara langsung dan terstruktur rapi.
