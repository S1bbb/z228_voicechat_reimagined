//! Проверка сервера: TLS с пином SPKI SHA-256 (самоподписанный сертификат) или обычной цепочкой
//! доверия (Let's Encrypt), затем GET /api/info — убедиться, что по адресу именно z228.

use base64::engine::general_purpose::URL_SAFE_NO_PAD;
use base64::Engine;
use rustls::client::danger::{HandshakeSignatureValid, ServerCertVerified, ServerCertVerifier};
use rustls::client::WebPkiServerVerifier;
use rustls::crypto::{verify_tls12_signature, verify_tls13_signature, CryptoProvider};
use rustls::pki_types::{CertificateDer, ServerName, UnixTime};
use rustls::{DigitallySignedStruct, SignatureScheme};
use sha2::{Digest, Sha256};
use std::io::{Read, Write};
use std::net::{IpAddr, TcpStream, ToSocketAddrs};
use std::sync::{Arc, Mutex};
use std::time::Duration;

use crate::invite::bare_host;

const TIMEOUT: Duration = Duration::from_secs(6);

#[derive(Debug)]
pub enum CheckError {
    /// Сертификат не совпал с пином: чужой сервер или ключ сменился.
    Mismatch,
    /// Без пина: сертификат не доверенный (самоподписанный) — нужен отпечаток.
    Untrusted,
    Network(String),
    NotZ228,
}

impl std::fmt::Display for CheckError {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        match self {
            CheckError::Mismatch => f.write_str(
                "Это не тот сервер, или его сертификат сменился. Попросите у администратора новую строку-приглашение.",
            ),
            CheckError::Untrusted => f.write_str(
                "У сервера самоподписанный сертификат, а в строке нет отпечатка (fp). Скопируйте строку для десктопа в «Администрировании».",
            ),
            CheckError::Network(e) => write!(f, "Сервер недоступен: {e}"),
            CheckError::NotZ228 => f.write_str("По этому адресу отвечает не ZALUPA228."),
        }
    }
}

#[derive(Debug, Clone)]
pub struct ServerInfo {
    pub name: Option<String>,
}

// ---------- SPKI ----------

/// Читает заголовок DER: (тег, начало содержимого, длина содержимого).
fn der_header(b: &[u8]) -> Option<(u8, usize, usize)> {
    let tag = *b.first()?;
    let first = *b.get(1)? as usize;
    if first < 0x80 {
        return Some((tag, 2, first));
    }
    let n = first & 0x7f;
    if n == 0 || n > 4 {
        return None;
    }
    let mut len = 0usize;
    for i in 0..n {
        len = (len << 8) | *b.get(2 + i)? as usize;
    }
    Some((tag, 2 + n, len))
}

/// Целый элемент DER (заголовок + содержимое) и остаток.
fn der_split(b: &[u8]) -> Option<(u8, &[u8], &[u8], &[u8])> {
    let (tag, hl, len) = der_header(b)?;
    let end = hl.checked_add(len)?;
    if end > b.len() {
        return None;
    }
    Some((tag, &b[..end], &b[hl..end], &b[end..]))
}

/// SubjectPublicKeyInfo (DER) из сертификата X.509.
pub fn spki_der(cert: &[u8]) -> Option<&[u8]> {
    let (_, _, cert_body, _) = der_split(cert)?; // Certificate SEQUENCE
    let (_, _, mut tbs, _) = der_split(cert_body)?; // tbsCertificate SEQUENCE
    // [0] version — необязательный
    if tbs.first() == Some(&0xa0) {
        tbs = der_split(tbs)?.3;
    }
    // serialNumber, signature, issuer, validity, subject
    for _ in 0..5 {
        tbs = der_split(tbs)?.3;
    }
    let (tag, whole, _, _) = der_split(tbs)?;
    (tag == 0x30).then_some(whole)
}

/// Пин сертификата: SHA-256 от SPKI, base64url без «=» (как в Go: cert.SPKIPin).
pub fn pin_of_cert(cert: &[u8]) -> Option<String> {
    spki_der(cert).map(|spki| URL_SAFE_NO_PAD.encode(Sha256::digest(spki)))
}

// ---------- TLS ----------

pub fn provider() -> Arc<CryptoProvider> {
    Arc::new(rustls::crypto::ring::default_provider())
}

#[derive(Debug)]
struct PinVerifier {
    expected: Option<String>,
    seen: Mutex<Option<String>>,
    webpki: Arc<WebPkiServerVerifier>,
    provider: Arc<CryptoProvider>,
}

impl ServerCertVerifier for PinVerifier {
    fn verify_server_cert(
        &self,
        end_entity: &CertificateDer<'_>,
        intermediates: &[CertificateDer<'_>],
        server_name: &ServerName<'_>,
        ocsp: &[u8],
        now: UnixTime,
    ) -> Result<ServerCertVerified, rustls::Error> {
        let pin = pin_of_cert(end_entity).ok_or_else(|| rustls::Error::General("bad certificate".into()))?;
        *self.seen.lock().unwrap() = Some(pin.clone());
        match &self.expected {
            // доверие держится на пине: имя и цепочку не проверяем (хост — обычно IP)
            Some(want) if *want == pin => Ok(ServerCertVerified::assertion()),
            Some(_) => Err(rustls::Error::General("pin mismatch".into())),
            None => self.webpki.verify_server_cert(end_entity, intermediates, server_name, ocsp, now),
        }
    }

    fn verify_tls12_signature(&self, message: &[u8], cert: &CertificateDer<'_>, dss: &DigitallySignedStruct) -> Result<HandshakeSignatureValid, rustls::Error> {
        verify_tls12_signature(message, cert, dss, &self.provider.signature_verification_algorithms)
    }

    fn verify_tls13_signature(&self, message: &[u8], cert: &CertificateDer<'_>, dss: &DigitallySignedStruct) -> Result<HandshakeSignatureValid, rustls::Error> {
        verify_tls13_signature(message, cert, dss, &self.provider.signature_verification_algorithms)
    }

    fn supported_verify_schemes(&self) -> Vec<SignatureScheme> {
        self.provider.signature_verification_algorithms.supported_schemes()
    }
}

fn connect_tcp(host: &str, port: u16) -> Result<TcpStream, CheckError> {
    let addrs: Vec<_> = (bare_host(host), port)
        .to_socket_addrs()
        .map_err(|e| CheckError::Network(format!("не удалось найти адрес {host}: {e}")))?
        .collect();
    let mut last = None;
    for a in addrs {
        match TcpStream::connect_timeout(&a, TIMEOUT) {
            Ok(s) => {
                let _ = s.set_read_timeout(Some(TIMEOUT));
                let _ = s.set_write_timeout(Some(TIMEOUT));
                return Ok(s);
            }
            Err(e) => last = Some(e),
        }
    }
    Err(CheckError::Network(match last {
        Some(e) if e.kind() == std::io::ErrorKind::TimedOut => "не отвечает (проверьте адрес, порт и что сервер запущен)".into(),
        Some(e) => e.to_string(),
        None => "адрес не найден".into(),
    }))
}

/// GET path по TLS с проверкой сертификата. Возвращает (код ответа, тело).
fn https_get(host: &str, port: u16, expected: Option<&str>, path: &str) -> Result<(u16, Vec<u8>), CheckError> {
    let provider = provider();
    let mut roots = rustls::RootCertStore::empty();
    roots.extend(webpki_roots::TLS_SERVER_ROOTS.iter().cloned());
    let webpki = WebPkiServerVerifier::builder_with_provider(Arc::new(roots), provider.clone())
        .build()
        .map_err(|e| CheckError::Network(e.to_string()))?;
    let verifier = Arc::new(PinVerifier { expected: expected.map(str::to_string), seen: Mutex::new(None), webpki, provider: provider.clone() });
    let config = rustls::ClientConfig::builder_with_provider(provider)
        .with_safe_default_protocol_versions()
        .map_err(|e| CheckError::Network(e.to_string()))?
        .dangerous()
        .with_custom_certificate_verifier(verifier.clone())
        .with_no_client_auth();
    let bare = bare_host(host);
    let name: ServerName<'static> = match bare.parse::<IpAddr>() {
        Ok(ip) => ServerName::IpAddress(ip.into()),
        Err(_) => ServerName::try_from(bare.to_string()).map_err(|_| CheckError::Network("некорректное имя сервера".into()))?,
    };
    let conn = rustls::ClientConnection::new(Arc::new(config), name).map_err(|e| CheckError::Network(e.to_string()))?;
    let mut tls = rustls::StreamOwned::new(conn, connect_tcp(host, port)?);

    let req = format!("GET {path} HTTP/1.0\r\nHost: {host}:{port}\r\nUser-Agent: ZALUPA228-Desktop\r\nAccept: */*\r\nConnection: close\r\n\r\n");
    let tls_err = |e: std::io::Error| -> CheckError {
        let seen = verifier.seen.lock().unwrap().clone();
        match (&verifier.expected, seen) {
            (Some(want), Some(got)) if *want != got => CheckError::Mismatch,
            (None, Some(_)) if e.to_string().contains("certificate") || e.to_string().contains("Certificate") => CheckError::Untrusted,
            _ => CheckError::Network(e.to_string()),
        }
    };
    tls.write_all(req.as_bytes()).map_err(tls_err)?;
    let mut buf = Vec::new();
    match tls.read_to_end(&mut buf) {
        Ok(_) => {}
        // сервер закрыл соединение без close_notify — данные уже есть
        Err(e) if e.kind() == std::io::ErrorKind::UnexpectedEof && !buf.is_empty() => {}
        Err(e) => return Err(tls_err(e)),
    }
    let split = buf.windows(4).position(|w| w == b"\r\n\r\n").ok_or_else(|| CheckError::Network("пустой ответ сервера".into()))?;
    let head = String::from_utf8_lossy(&buf[..split]);
    let code = head.split_whitespace().nth(1).and_then(|c| c.parse().ok()).unwrap_or(0);
    Ok((code, buf[split + 4..].to_vec()))
}

/// Проверяет сервер: сертификат (по пину или цепочке доверия) и что это z228.
pub fn check(host: &str, port: u16, expected: Option<&str>) -> Result<ServerInfo, CheckError> {
    let (code, body) = https_get(host, port, expected, "/api/info")?;
    if code == 200 {
        if let Ok(v) = serde_json::from_slice::<serde_json::Value>(&body) {
            if v.get("app").and_then(|a| a.as_str()) == Some("z228") {
                return Ok(ServerInfo { name: v.get("name").and_then(|n| n.as_str()).map(str::to_string) });
            }
        }
        return Err(CheckError::NotZ228);
    }
    // сервер старой версии без /api/info — узнаём по заголовку страницы
    let (code, body) = https_get(host, port, expected, "/")?;
    if code == 200 && String::from_utf8_lossy(&body).contains("<title>ZALUPA228") {
        return Ok(ServerInfo { name: None });
    }
    Err(CheckError::NotZ228)
}

#[cfg(test)]
mod tests {
    use super::*;

    // Сертификат и пин сгенерированы Go-кодом сервера (internal/cert), см. testdata.
    #[test]
    fn spki_matches_go() {
        let pem = include_str!("../testdata/selfsigned.crt");
        let want = include_str!("../testdata/selfsigned.pin").trim();
        let b64: String = pem.lines().filter(|l| !l.starts_with("-----")).collect();
        let der = base64::engine::general_purpose::STANDARD.decode(b64).unwrap();
        assert_eq!(pin_of_cert(&der).as_deref(), Some(want));
    }

    #[test]
    fn der_garbage() {
        assert!(spki_der(&[]).is_none());
        assert!(spki_der(&[0x30, 0x82, 0xff]).is_none());
        assert!(spki_der(&[0x30, 0x03, 0x30, 0x01, 0x00]).is_none());
    }
}
