//! Разбор строки-приглашения:
//!   z228://<host>:<port>/?fp=<SPKI-SHA256-base64url>&k=<ключ>&n=<название>
//! Запасной вариант — веб-ссылка https://host:port/#k=<ключ> плюс отпечаток отдельным полем.

use base64::engine::general_purpose::{STANDARD, URL_SAFE_NO_PAD};
use base64::Engine;
use url::Url;

#[derive(Clone, Debug, PartialEq, Eq)]
pub struct Invite {
    /// Хост как в URL: IPv4, имя или IPv6 в квадратных скобках.
    pub host: String,
    pub port: u16,
    /// Отпечаток SPKI (base64url без «=»), None — доверенный сертификат.
    pub fp: Option<String>,
    pub key: Option<String>,
    pub name: Option<String>,
}

impl Invite {
    pub fn addr(&self) -> String {
        format!("{}:{}", self.host, self.port)
    }
}

/// Хост без квадратных скобок — для TCP и имени в TLS.
pub fn bare_host(host: &str) -> &str {
    host.trim_start_matches('[').trim_end_matches(']')
}

/// Делит "host:port" из config на части.
pub fn split_addr(addr: &str) -> Option<(String, u16)> {
    let u = Url::parse(&format!("https://{addr}/")).ok()?;
    Some((u.host_str()?.to_string(), u.port().unwrap_or(443)))
}

/// Приводит отпечаток к base64url без «=»; принимает и обычный base64, и hex.
pub fn normalize_fp(s: &str) -> Result<String, String> {
    let s = s.trim();
    let bytes = if s.len() == 64 && s.chars().all(|c| c.is_ascii_hexdigit()) {
        (0..32).map(|i| u8::from_str_radix(&s[i * 2..i * 2 + 2], 16).unwrap()).collect()
    } else {
        URL_SAFE_NO_PAD
            .decode(s.trim_end_matches('='))
            .or_else(|_| STANDARD.decode(s))
            .map_err(|_| "Отпечаток сертификата повреждён — скопируйте строку целиком.".to_string())?
    };
    if bytes.len() != 32 {
        return Err("Отпечаток сертификата повреждён — скопируйте строку целиком.".into());
    }
    Ok(URL_SAFE_NO_PAD.encode(bytes))
}

fn valid_key(k: &str) -> bool {
    (4..=128).contains(&k.len()) && k.chars().all(|c| c.is_ascii_alphanumeric() || c == '_' || c == '-')
}

fn clean_name(n: &str) -> Option<String> {
    let n: String = n.chars().filter(|c| !c.is_control()).take(64).collect();
    let n = n.trim().to_string();
    (!n.is_empty()).then_some(n)
}

/// `input` — строка-приглашение или веб-ссылка; `fp_field` — отпечаток из отдельного поля (может быть пустым).
pub fn parse(input: &str, fp_field: &str) -> Result<Invite, String> {
    let raw = input.trim();
    if raw.is_empty() {
        return Err("Вставьте строку-приглашение от администратора сервера.".into());
    }
    let lower = raw.to_ascii_lowercase();
    let with_scheme = if lower.starts_with("z228://") || lower.starts_with("https://") {
        raw.to_string()
    } else if lower.starts_with("http://") {
        return Err("Нужен адрес с https:// — по HTTP приложение не подключается.".into());
    } else if lower.contains("://") {
        return Err("Это не похоже на приглашение ZALUPA228 (ожидается z228://… или https://…).".into());
    } else {
        format!("https://{raw}")
    };
    let u = Url::parse(&with_scheme).map_err(|_| "Не удалось разобрать адрес сервера.".to_string())?;
    let host = u.host_str().filter(|h| !h.is_empty()).ok_or("В строке нет адреса сервера.")?.to_string();
    let port = u.port().unwrap_or(443);

    let mut fp = None;
    let mut key = None;
    let mut name = None;
    for (k, v) in u.query_pairs() {
        match k.as_ref() {
            "fp" if !v.is_empty() => fp = Some(normalize_fp(&v)?),
            "k" if !v.is_empty() => key = Some(v.to_string()),
            "n" => name = clean_name(&v),
            _ => {}
        }
    }
    // веб-ссылка: ключ во фрагменте (#k=...)
    if let Some(frag) = u.fragment() {
        for part in frag.split('&') {
            if let Some(v) = part.strip_prefix("k=") {
                if !v.is_empty() {
                    key = Some(v.to_string());
                }
            }
        }
    }
    if !fp_field.trim().is_empty() {
        fp = Some(normalize_fp(fp_field)?);
    }
    if let Some(k) = &key {
        if !valid_key(k) {
            return Err("Ключ доступа в строке повреждён — скопируйте её целиком.".into());
        }
    }
    Ok(Invite { host, port, fp, key, name })
}

#[cfg(test)]
mod tests {
    use super::*;

    const FP: &str = "q83vEjRWeJCrze8SNFZ4kKvN7xI0VniQq83vEjRWeJA";

    #[test]
    fn z228_scheme() {
        let i = parse(&format!("z228://203.0.113.10:3443/?fp={FP}&k=abcdEFGH23&n=%D0%9D%D0%B0%D1%88+%D1%81%D0%B5%D1%80%D0%B2%D0%B5%D1%80"), "").unwrap();
        assert_eq!(i.host, "203.0.113.10");
        assert_eq!(i.port, 3443);
        assert_eq!(i.fp.as_deref(), Some(FP));
        assert_eq!(i.key.as_deref(), Some("abcdEFGH23"));
        assert_eq!(i.name.as_deref(), Some("Наш сервер"));
        assert_eq!(i.addr(), "203.0.113.10:3443");
    }

    #[test]
    fn web_link_and_fp_field() {
        let std_b64 = STANDARD.encode(URL_SAFE_NO_PAD.decode(FP).unwrap());
        let i = parse("https://26.1.2.3:3443/#k=secretkey", &std_b64).unwrap();
        assert_eq!(i.host, "26.1.2.3");
        assert_eq!(i.key.as_deref(), Some("secretkey"));
        assert_eq!(i.fp.as_deref(), Some(FP));
    }

    #[test]
    fn bare_host_and_defaults() {
        let i = parse("chat.example.com", "").unwrap();
        assert_eq!((i.host.as_str(), i.port, i.fp.clone(), i.key.clone()), ("chat.example.com", 443, None, None));
        let v6 = parse(&format!("z228://[::1]:3443/?fp={FP}"), "").unwrap();
        assert_eq!(v6.host, "[::1]");
        assert_eq!(bare_host(&v6.host), "::1");
        assert_eq!(split_addr(&v6.addr()), Some(("[::1]".to_string(), 3443)));
    }

    #[test]
    fn rejects_garbage() {
        assert!(parse("", "").is_err());
        assert!(parse("http://1.2.3.4:3000/", "").is_err());
        assert!(parse("z228://1.2.3.4:3443/?fp=short", "").is_err());
        assert!(parse("z228://1.2.3.4:3443/?k=bad<key>", "").is_err());
        assert!(parse("ftp://x", "").is_err());
    }
}
