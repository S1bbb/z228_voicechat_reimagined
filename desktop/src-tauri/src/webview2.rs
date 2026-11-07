//! WebView2: пин самоподписанного сертификата и разрешения (микрофон, камера) только для нашего сервера.
//!
//! WebView2 сам не откроет сайт с самоподписанным сертификатом. Обработчик
//! ServerCertificateErrorDetected разрешает сертификат, только если это наш host:port и SPKI совпал
//! с пином из config; всё остальное — CANCEL.

use std::sync::Arc;
use tauri::{AppHandle, WebviewWindow};

/// Текущий сервер: читается при каждом событии, чтобы сервер можно было сменить без перезапуска.
pub type OriginFn = Arc<dyn Fn() -> Option<Origin> + Send + Sync>;

#[derive(Clone)]
pub struct Origin {
    /// Хост как в URL (IPv6 — в скобках), в нижнем регистре.
    pub host: String,
    pub port: u16,
    pub spki: Option<String>,
}

impl Origin {
    pub fn matches(&self, uri: &str) -> bool {
        let Ok(u) = url::Url::parse(uri) else { return false };
        // wss:// — WebSocket сайта, у него та же проверка сертификата
        matches!(u.scheme(), "https" | "wss") && u.host_str().map(|h| h.eq_ignore_ascii_case(&self.host)).unwrap_or(false) && u.port_or_known_default() == Some(self.port)
    }
}

/// Пин из PEM, который отдаёт WebView2.
pub fn pin_of_pem(pem: &str) -> Option<String> {
    use base64::Engine;
    let start = pem.find("-----BEGIN CERTIFICATE-----")? + "-----BEGIN CERTIFICATE-----".len();
    let end = pem[start..].find("-----END CERTIFICATE-----")? + start;
    let b64: String = pem[start..end].chars().filter(|c| !c.is_whitespace()).collect();
    let der = base64::engine::general_purpose::STANDARD.decode(b64).ok()?;
    crate::pin::pin_of_cert(&der)
}

#[cfg(windows)]
pub fn install(window: &WebviewWindow, origin: OriginFn, app: AppHandle, then: impl FnOnce() + Send + 'static) -> tauri::Result<()> {
    window.with_webview(move |wv| unsafe {
        let r = win::install(&wv.controller(), origin, app);
        crate::bridge::debug_log(&format!("webview2 handlers: {r:?}"));
        then();
    })
}

/// Видимость страницы: в трее document.hidden = true, и сайт сам отписывается от видео.
/// TrySuspend не вызываем — голос в трее должен работать.
#[cfg(windows)]
pub fn set_visible(window: &WebviewWindow, visible: bool) {
    let _ = window.with_webview(move |wv| unsafe {
        let _ = wv.controller().SetIsVisible(visible);
    });
}

#[cfg(not(windows))]
pub fn set_visible(_: &WebviewWindow, _: bool) {}

#[cfg(not(windows))]
pub fn install(_: &WebviewWindow, _: OriginFn, _: AppHandle, then: impl FnOnce() + Send + 'static) -> tauri::Result<()> {
    then();
    Ok(())
}

#[cfg(windows)]
mod win {
    use super::*;
    use webview2_com::Microsoft::Web::WebView2::Win32::*;
    use webview2_com::{take_pwstr, PermissionRequestedEventHandler, ServerCertificateErrorDetectedEventHandler};
    use windows::core::{Interface, PWSTR};

    pub unsafe fn install(controller: &ICoreWebView2Controller, origin: OriginFn, app: AppHandle) -> windows::core::Result<()> {
        let core = controller.CoreWebView2()?;
        let mut token = Default::default();

        // ---- сертификат ----
        if let Ok(core14) = core.cast::<ICoreWebView2_14>() {
            let o = origin.clone();
            let app2 = app.clone();
            let handler = ServerCertificateErrorDetectedEventHandler::create(Box::new(move |_, args| {
                let Some(args) = args else { return Ok(()) };
                let mut uri = PWSTR::null();
                args.RequestUri(&mut uri)?;
                let uri = take_pwstr(uri);
                let mut action = COREWEBVIEW2_SERVER_CERTIFICATE_ERROR_ACTION_CANCEL;
                if let Some(o) = o().filter(|o| o.matches(&uri)) {
                    if let Some(want) = &o.spki {
                        let cert = args.ServerCertificate()?;
                        let mut pem = PWSTR::null();
                        cert.ToPemEncoding(&mut pem)?;
                        let got = super::pin_of_pem(&take_pwstr(pem));
                        if got.as_deref() == Some(want.as_str()) {
                            action = COREWEBVIEW2_SERVER_CERTIFICATE_ERROR_ACTION_ALWAYS_ALLOW;
                        } else {
                            // сертификат сменился или подменён — молча не доверяем, просим новую строку
                            let app = app2.clone();
                            std::thread::spawn(move || crate::bridge::pin_mismatch(&app));
                        }
                    }
                }
                crate::bridge::debug_log(&format!("cert error {uri}: {}", action.0));
                args.SetAction(action)?;
                Ok(())
            }));
            core14.add_ServerCertificateErrorDetected(&handler, &mut token)?;
        } else {
            eprintln!("webview2: ICoreWebView2_14 недоступен — обновите WebView2 Runtime");
        }

        // ---- разрешения ----
        let o = origin;
        let handler = PermissionRequestedEventHandler::create(Box::new(move |_, args| {
            let Some(args) = args else { return Ok(()) };
            let mut uri = PWSTR::null();
            args.Uri(&mut uri)?;
            let uri = take_pwstr(uri);
            let mut kind = COREWEBVIEW2_PERMISSION_KIND_UNKNOWN_PERMISSION;
            args.PermissionKind(&mut kind)?;
            if !o().map(|o| o.matches(&uri)).unwrap_or(false) {
                args.SetState(COREWEBVIEW2_PERMISSION_STATE_DENY)?;
            } else if kind == COREWEBVIEW2_PERMISSION_KIND_MICROPHONE
                || kind == COREWEBVIEW2_PERMISSION_KIND_CAMERA
                || kind == COREWEBVIEW2_PERMISSION_KIND_NOTIFICATIONS
                || kind == COREWEBVIEW2_PERMISSION_KIND_AUTOPLAY
                || kind == COREWEBVIEW2_PERMISSION_KIND_CLIPBOARD_READ
            {
                args.SetState(COREWEBVIEW2_PERMISSION_STATE_ALLOW)?;
            }
            Ok(())
        }));
        core.add_PermissionRequested(&handler, &mut token)?;
        Ok(())
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn origin_match() {
        let o = Origin { host: "82.47.130.62".into(), port: 3443, spki: None };
        assert!(o.matches("https://82.47.130.62:3443/"));
        assert!(o.matches("https://82.47.130.62:3443/ws"));
        assert!(o.matches("wss://82.47.130.62:3443/ws"));
        assert!(!o.matches("ws://82.47.130.62:3443/ws"));
        assert!(!o.matches("https://82.47.130.62:3444/"));
        assert!(!o.matches("http://82.47.130.62:3443/"));
        assert!(!o.matches("https://evil.example/"));
        let d = Origin { host: "chat.example.com".into(), port: 443, spki: None };
        assert!(d.matches("https://Chat.Example.com/"));
    }

    #[test]
    fn pem_pin() {
        let pem = include_str!("../testdata/selfsigned.crt");
        assert_eq!(pin_of_pem(pem).as_deref(), Some(include_str!("../testdata/selfsigned.pin").trim()));
    }
}
