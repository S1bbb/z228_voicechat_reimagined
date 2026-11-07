//! Сборка приложения, окна, трей и мост Rust ↔ страница.
//!
//! Rust → страница: `window.dispatchEvent(new CustomEvent("z228-desktop", {detail}))` через eval.
//! Страница → Rust: команды `desktop_*`; права на них странице сервера выдаются во время работы
//! (origin задаёт пользователь), окну connect — статически (capabilities/default.json).

use serde_json::{json, Value};
use std::collections::HashSet;
use std::sync::atomic::{AtomicBool, Ordering};
use std::sync::Mutex;
use std::time::Duration;
use tauri::ipc::CapabilityBuilder;
use tauri::tray::TrayIcon;
use tauri::{AppHandle, Manager, WebviewUrl, WebviewWindow, WebviewWindowBuilder, WindowEvent};
use tauri_plugin_notification::NotificationExt;

use crate::config::{self, Config, ACTIONS};
use crate::hooks::{self, Action, Hotkeys};
use crate::invite;
use crate::pin::{self, CheckError};
use crate::shortcut;
use crate::tray::{self, Icons, Look, PageState, TrayMenu};
use crate::update;
use crate::webview2::{self, Origin};

/// Одинаковые для всех окон: WebView2 не создаст второе окно с другими аргументами браузера.
const BROWSER_ARGS: &str = "--disable-features=msWebOOUI,msPdfOOUI,msSmartScreenProtection \
--autoplay-policy=no-user-gesture-required --disable-background-timer-throttling --disable-renderer-backgrounding";

/// Аргументы браузера; в отладочной сборке Z228_DEVTOOLS_PORT включает удалённую отладку (для автотестов).
fn browser_args() -> String {
    #[allow(unused_mut)]
    let mut args = BROWSER_ARGS.to_string();
    #[cfg(debug_assertions)]
    if let Ok(port) = std::env::var("Z228_DEVTOOLS_PORT") {
        args += &format!(" --remote-debugging-port={port}");
    }
    args
}

/// Команды, доступные странице сервера.
const PAGE_COMMANDS: [&str; 9] = [
    "allow-desktop-info",
    "allow-desktop-report-state",
    "allow-desktop-record-binding",
    "allow-desktop-clear-binding",
    "allow-desktop-set-options",
    "allow-desktop-change-server",
    "allow-desktop-check-update",
    "allow-desktop-install-update",
    "allow-desktop-create-shortcut",
];

pub struct AppState {
    pub cfg: Mutex<Config>,
    pub hotkeys: Hotkeys,
    pub icons: Icons,
    pub page: Mutex<PageState>,
    pub tray: Mutex<Option<(TrayIcon, TrayMenu, Look, String)>>,
    pub connect_error: Mutex<Option<(String, bool)>>,
    pub pending_update: Mutex<Option<tauri_plugin_updater::Update>>,
    pub caps: Mutex<HashSet<String>>,
    pub origin: Mutex<Option<Origin>>,
    pub mismatch: AtomicBool,
}

fn state(app: &AppHandle) -> tauri::State<'_, AppState> {
    app.state::<AppState>()
}

pub fn debug_log(_msg: &str) {
    #[cfg(debug_assertions)]
    eprintln!("[z228] {_msg}");
}

// ---------- события для страницы ----------

pub fn emit_page(app: &AppHandle, detail: Value) {
    if let Some(w) = app.get_webview_window("main") {
        let js = format!("window.dispatchEvent(new CustomEvent('z228-desktop',{{detail:{detail}}}));");
        let _ = w.eval(&js);
    }
}

pub fn notify(app: &AppHandle, body: &str) {
    let _ = app.notification().builder().title("ZALUPA228").body(body).show();
}

// ---------- окна ----------

/// Показать главное окно (из трея).
pub fn show_main(w: &WebviewWindow) {
    let _ = w.unminimize();
    let _ = w.show();
    webview2::set_visible(w, true);
    let _ = w.set_focus();
}

/// Спрятать главное окно в трей.
pub fn hide_main(w: &WebviewWindow) {
    let _ = w.hide();
    webview2::set_visible(w, false);
}

pub fn show_main_or_connect(app: &AppHandle) {
    if let Some(w) = app.get_webview_window("main") {
        show_main(&w);
    } else if let Some(w) = app.get_webview_window("connect") {
        let _ = w.show();
        let _ = w.set_focus();
    } else {
        show_connect(app, None);
    }
}

/// Окно первого входа / смены сервера. `error` — (текст, можно ли просто повторить).
pub fn show_connect(app: &AppHandle, error: Option<(String, bool)>) {
    *state(app).connect_error.lock().unwrap() = error;
    if let Some(w) = app.get_webview_window("connect") {
        let _ = w.eval("window.loadStatus && window.loadStatus()");
        let _ = w.unminimize();
        let _ = w.show();
        let _ = w.set_focus();
        return;
    }
    let app2 = app.clone();
    // окна создаём не из потока цикла событий: так безопаснее на Windows
    std::thread::spawn(move || {
        let r = WebviewWindowBuilder::new(&app2, "connect", WebviewUrl::App("connect.html".into()))
            .title("ZALUPA228 — подключение")
            .inner_size(480.0, 680.0)
            .resizable(false)
            .maximizable(false)
            .center()
            .additional_browser_args(&browser_args())
            .build();
        if let Err(e) = r {
            eprintln!("connect window: {e}");
        }
    });
}

fn add_page_capability(app: &AppHandle, addr: &str) {
    let id = format!("server-{}", addr.replace(|c: char| !c.is_ascii_alphanumeric(), "-"));
    if !state(app).caps.lock().unwrap().insert(id.clone()) {
        return;
    }
    let mut cap = CapabilityBuilder::new(id).remote(format!("https://{addr}/*")).window("main");
    for p in PAGE_COMMANDS {
        cap = cap.permission(p);
    }
    if let Err(e) = app.add_capability(cap) {
        eprintln!("capability: {e}");
    }
}

/// Открывает (или переводит на новый сервер) главное окно с сайтом сервера.
pub fn open_main(app: &AppHandle, key: Option<String>) {
    let cfg = state(app).cfg.lock().unwrap().clone();
    let Some(addr) = cfg.server.clone() else { return show_connect(app, None) };
    let Some((host, port)) = invite::split_addr(&addr) else { return show_connect(app, None) };
    let mut url = format!("https://{addr}/");
    if let Some(k) = &key {
        url += &format!("#k={k}");
    }
    let Ok(target) = url::Url::parse(&url) else { return };
    *state(app).origin.lock().unwrap() = Some(Origin { host: host.to_ascii_lowercase(), port, spki: cfg.spki.clone() });
    state(app).mismatch.store(false, Ordering::Relaxed);
    add_page_capability(app, &addr);
    let title = cfg.name.clone().map(|n| format!("{n} — ZALUPA228")).unwrap_or_else(|| "ZALUPA228".into());

    if let Some(w) = app.get_webview_window("main") {
        let _ = w.set_title(&title);
        let _ = w.navigate(target);
        show_main(&w);
        return;
    }
    // вызывается не из главного потока (async-команда или свой поток) — окно можно строить сразу
    let app2 = app.clone();
    {
        let version = app2.package_info().version.to_string();
        let init = format!("window.z228Desktop = Object.freeze({{ version: {} }});", json!(version));
        // сначала пустая страница: обработчик сертификата должен встать до первой загрузки сайта
        let w = WebviewWindowBuilder::new(&app2, "main", WebviewUrl::External("about:blank".parse().unwrap()))
            .title(&title)
            .inner_size(1280.0, 800.0)
            .min_inner_size(820.0, 560.0)
            .center()
            .additional_browser_args(&browser_args())
            .initialization_script(&init)
            .build();
        let w: WebviewWindow = match w {
            Ok(w) => w,
            Err(e) => return eprintln!("main window: {e}"),
        };
        let nav = w.clone();
        let app3 = app2.clone();
        let origin: webview2::OriginFn = std::sync::Arc::new(move || state(&app3).origin.lock().unwrap().clone());
        let r = webview2::install(&w, origin, app2.clone(), move || {
            let r = nav.navigate(target.clone());
            debug_log(&format!("navigate {target} -> {r:?}"));
        });
        if let Err(e) = r {
            eprintln!("webview2 install: {e}");
        }
    }
}

/// Сертификат сервера перестал совпадать с пином (вызывается из обработчика WebView2).
pub fn pin_mismatch(app: &AppHandle) {
    if state(app).mismatch.swap(true, Ordering::Relaxed) {
        return;
    }
    if let Some(w) = app.get_webview_window("main") {
        hide_main(&w);
    }
    show_connect(app, Some((CheckError::Mismatch.to_string(), false)));
}

/// Проверка сохранённого сервера при запуске.
fn start_with_saved_server(app: &AppHandle) {
    let cfg = state(app).cfg.lock().unwrap().clone();
    let Some((host, port)) = cfg.server.as_deref().and_then(invite::split_addr) else {
        return show_connect(app, None);
    };
    let app = app.clone();
    std::thread::spawn(move || match pin::check(&host, port, cfg.spki.as_deref()) {
        Ok(_) => {
            debug_log("saved server ok");
            open_main(&app, None)
        }
        Err(e) => {
            debug_log(&format!("saved server: {e}"));
            let retry = matches!(e, CheckError::Network(_));
            show_connect(&app, Some((e.to_string(), retry)));
        }
    });
}

// ---------- трей ----------

fn update_tray(app: &AppHandle, st: &PageState) {
    let s = state(app);
    let name = s.cfg.lock().unwrap().name.clone().unwrap_or_default();
    let server = if st.server.is_empty() { name } else { st.server.clone() };
    let look = st.look();
    let tip = st.tooltip(&server);
    let mut guard = s.tray.lock().unwrap();
    if let Some((tray, menu, last_look, last_tip)) = guard.as_mut() {
        if *last_look != look {
            let _ = tray.set_icon(Some(s.icons.get(look)));
            *last_look = look;
        }
        if *last_tip != tip {
            debug_log(&format!("tray: {look:?} «{tip}»"));
            let _ = tray.set_tooltip(Some(&tip));
            *last_tip = tip;
        }
        let _ = menu.mute.set_checked(st.muted);
        let _ = menu.deafen.set_checked(st.deafened);
        let _ = menu.mute.set_enabled(app.get_webview_window("main").is_some());
        let _ = menu.deafen.set_enabled(app.get_webview_window("main").is_some());
    }
}

pub fn on_tray_menu(app: &AppHandle, id: &str) {
    match id {
        "open" => show_main_or_connect(app),
        "mute" | "deafen" => {
            emit_page(app, json!({ "type": format!("{id}-toggle") }));
            // галочку ставит отчёт страницы; до него возвращаем прежнее состояние
            let st = state(app).page.lock().unwrap().clone();
            update_tray(app, &st);
        }
        "server" => show_connect(app, None),
        "update" => {
            let app = app.clone();
            tauri::async_runtime::spawn(async move {
                match update::check(&app).await {
                    Ok(Some(v)) => {
                        notify(&app, &format!("Доступно обновление {v}. Откройте окно, чтобы установить."));
                        show_main_or_connect(&app);
                    }
                    Ok(None) => notify(&app, "Установлена последняя версия."),
                    Err(e) => notify(&app, &format!("Не удалось проверить обновления: {e}")),
                }
            });
        }
        "quit" => app.exit(0),
        _ => {}
    }
}

// ---------- команды страницы ----------

#[tauri::command]
fn desktop_info(app: AppHandle) -> Value {
    let s = state(&app);
    let c = s.cfg.lock().unwrap().clone();
    let bindings: serde_json::Map<String, Value> =
        ACTIONS.iter().map(|a| (a.to_string(), c.bindings.get(*a).map(|b| json!({ "name": b.name })).unwrap_or(Value::Null))).collect();
    let update = s.pending_update.lock().unwrap().as_ref().map(|u| json!({ "version": u.version, "notes": u.body }));
    json!({
        "version": app.package_info().version.to_string(),
        "bindings": bindings,
        "pttReleaseMs": c.ptt_release_ms,
        "pttSound": c.ptt_sound,
        "server": c.server,
        "name": c.name,
        "update": update,
        "shortcut": shortcut::exists(),
    })
}

#[tauri::command]
fn desktop_report_state(app: AppHandle, state: PageState) {
    {
        let s = self::state(&app);
        let mut page = s.page.lock().unwrap();
        if *page == state {
            return;
        }
        *page = state.clone();
    }
    update_tray(&app, &state);
}

#[tauri::command]
async fn desktop_record_binding(app: AppHandle, action: String) -> Result<Option<String>, String> {
    if !ACTIONS.contains(&action.as_str()) {
        return Err("неизвестное действие".into());
    }
    let s = state(&app);
    let rx = s.hotkeys.record();
    let b = match tokio::time::timeout(Duration::from_secs(15), rx).await {
        Ok(Ok(b)) => b,
        Ok(Err(_)) => None,
        Err(_) => {
            s.hotkeys.cancel_record();
            return Err("время вышло — нажмите «Назначить» ещё раз".into());
        }
    };
    let Some(b) = b else { return Ok(None) };
    let mut c = s.cfg.lock().unwrap();
    // одна клавиша — одно действие
    c.bindings.retain(|k, v| *k == action || !v.same_input(&b));
    c.bindings.insert(action, b.clone());
    config::save(&c).map_err(|e| e.to_string())?;
    s.hotkeys.set_bindings(c.bindings.clone());
    Ok(Some(b.name))
}

#[tauri::command]
fn desktop_clear_binding(app: AppHandle, action: String) -> Result<(), String> {
    let s = state(&app);
    let mut c = s.cfg.lock().unwrap();
    c.bindings.remove(&action);
    config::save(&c).map_err(|e| e.to_string())?;
    s.hotkeys.set_bindings(c.bindings.clone());
    Ok(())
}

#[tauri::command]
fn desktop_set_options(app: AppHandle, ptt_release_ms: u32, ptt_sound: bool) -> Result<(), String> {
    let s = state(&app);
    let mut c = s.cfg.lock().unwrap();
    c.ptt_release_ms = ptt_release_ms.min(1000);
    c.ptt_sound = ptt_sound;
    config::save(&c).map_err(|e| e.to_string())?;
    s.hotkeys.set_release_ms(c.ptt_release_ms);
    Ok(())
}

#[tauri::command]
fn desktop_change_server(app: AppHandle) {
    show_connect(&app, None);
}

#[tauri::command]
async fn desktop_check_update(app: AppHandle) -> Result<Value, String> {
    let v = update::check(&app).await?;
    Ok(json!({ "available": v.is_some(), "version": v }))
}

#[tauri::command]
async fn desktop_create_shortcut() -> Result<(), String> {
    tauri::async_runtime::spawn_blocking(shortcut::create).await.map_err(|e| e.to_string())?.map(|_| ())
}

#[tauri::command]
async fn desktop_install_update(app: AppHandle) -> Result<(), String> {
    update::install(&app).await
}

// ---------- команды окна connect ----------

#[tauri::command]
fn connect_status(app: AppHandle) -> Value {
    let s = state(&app);
    let c = s.cfg.lock().unwrap().clone();
    let err = s.connect_error.lock().unwrap().clone();
    json!({
        "error": err.as_ref().map(|e| e.0.clone()),
        "retry": err.map(|e| e.1).unwrap_or(false),
        "server": c.server,
        "name": c.name,
        "hasMain": app.get_webview_window("main").is_some(),
        // предложить ярлык: его нет; галочка по умолчанию — если ещё не предлагали
        "shortcut": !shortcut::exists(),
        "shortcutDefault": !c.shortcut_offered,
        "version": app.package_info().version.to_string(),
    })
}

async fn check_blocking(host: String, port: u16, fp: Option<String>) -> Result<pin::ServerInfo, CheckError> {
    tauri::async_runtime::spawn_blocking(move || pin::check(&host, port, fp.as_deref()))
        .await
        .unwrap_or_else(|e| Err(CheckError::Network(e.to_string())))
}

fn close_connect(app: &AppHandle) {
    if let Some(w) = app.get_webview_window("connect") {
        let _ = w.close();
    }
}

#[tauri::command]
async fn connect_submit(app: AppHandle, invite: String, fp: String, shortcut: Option<bool>) -> Result<(), String> {
    let inv = invite::parse(&invite, &fp)?;
    let info = check_blocking(inv.host.clone(), inv.port, inv.fp.clone()).await.map_err(|e| e.to_string())?;
    {
        let s = state(&app);
        let mut c = s.cfg.lock().unwrap();
        c.server = Some(inv.addr());
        c.spki = inv.fp.clone();
        c.name = inv.name.clone().or(info.name);
        if shortcut.is_some() {
            c.shortcut_offered = true;
        }
        config::save(&c).map_err(|e| format!("не удалось сохранить настройки: {e}"))?;
        *s.connect_error.lock().unwrap() = None;
    }
    if shortcut == Some(true) {
        if let Err(e) = tauri::async_runtime::spawn_blocking(shortcut::create).await.map_err(|e| e.to_string()).and_then(|r| r) {
            notify(&app, &format!("Не удалось создать ярлык: {e}. Его можно создать в настройках → «Приложение»."));
        }
    }
    // ключ в config не пишем: сайт сам запомнит его из #k= и уберёт из адреса
    open_main(&app, inv.key);
    close_connect(&app);
    Ok(())
}

#[tauri::command]
async fn connect_retry(app: AppHandle) -> Result<(), String> {
    let c = state(&app).cfg.lock().unwrap().clone();
    let (host, port) = c.server.as_deref().and_then(invite::split_addr).ok_or("сервер не настроен")?;
    check_blocking(host, port, c.spki.clone()).await.map_err(|e| e.to_string())?;
    *state(&app).connect_error.lock().unwrap() = None;
    open_main(&app, None);
    close_connect(&app);
    Ok(())
}

#[tauri::command]
fn connect_quit(app: AppHandle) {
    if app.get_webview_window("main").is_some() && !state(&app).mismatch.load(Ordering::Relaxed) {
        close_connect(&app);
    } else {
        app.exit(0);
    }
}

// ---------- запуск ----------

pub fn run() {
    tauri::Builder::default()
        .plugin(tauri_plugin_single_instance::init(|app, _args, _cwd| show_main_or_connect(app)))
        .plugin(tauri_plugin_notification::init())
        .plugin(tauri_plugin_updater::Builder::new().build())
        .invoke_handler(tauri::generate_handler![
            desktop_info,
            desktop_report_state,
            desktop_record_binding,
            desktop_clear_binding,
            desktop_set_options,
            desktop_change_server,
            desktop_check_update,
            desktop_install_update,
            desktop_create_shortcut,
            connect_status,
            connect_submit,
            connect_retry,
            connect_quit,
        ])
        .setup(|app| {
            let handle = app.handle().clone();
            let cfg = config::load();
            let h1 = handle.clone();
            let h2 = handle.clone();
            let hotkeys = hooks::start(
                cfg.bindings.clone(),
                cfg.ptt_release_ms,
                move |a| {
                    let detail = match a {
                        Action::PttDown => json!({ "type": "ptt", "down": true }),
                        Action::PttUp => json!({ "type": "ptt", "down": false }),
                        Action::MuteToggle => json!({ "type": "mute-toggle" }),
                        Action::DeafenToggle => json!({ "type": "deafen-toggle" }),
                    };
                    emit_page(&h1, detail);
                },
                move |name| {
                    let who = if name.is_empty() { "Активное окно".to_string() } else { name };
                    notify(&h2, &format!("{who} запущено от администратора — горячие клавиши в нём не работают. Запустите ZALUPA228 тоже от администратора."));
                    emit_page(&h2, json!({ "type": "hotkeys-blocked", "app": who }));
                },
            );
            app.manage(AppState {
                cfg: Mutex::new(cfg),
                hotkeys,
                icons: tray::make_icons(),
                page: Mutex::new(PageState::default()),
                tray: Mutex::new(None),
                connect_error: Mutex::new(None),
                pending_update: Mutex::new(None),
                caps: Mutex::new(HashSet::new()),
                origin: Mutex::new(None),
                mismatch: AtomicBool::new(false),
            });
            let (icon, menu) = tray::build(&handle)?;
            *state(&handle).tray.lock().unwrap() = Some((icon, menu, Look::Idle, String::new()));
            update_tray(&handle, &PageState::default());
            start_with_saved_server(&handle);
            update::spawn_loop(handle);
            Ok(())
        })
        .on_window_event(|window, event| {
            let app = window.app_handle();
            match (window.label(), event) {
                ("main", WindowEvent::CloseRequested { api, .. }) => {
                    // крестик сворачивает в трей; выход — через меню трея
                    api.prevent_close();
                    if let Some(w) = app.get_webview_window("main") {
                        hide_main(&w);
                    }
                    let s = state(app);
                    let mut c = s.cfg.lock().unwrap();
                    if !c.tray_hint_shown {
                        c.tray_hint_shown = true;
                        let _ = config::save(&c);
                        notify(app, "ZALUPA228 работает в трее. Голос и горячие клавиши продолжают работать. Выход — правой кнопкой по значку.");
                    }
                }
                ("connect", WindowEvent::Destroyed) => {
                    if app.get_webview_window("main").is_none() || state(app).mismatch.load(Ordering::Relaxed) {
                        app.exit(0);
                    }
                }
                _ => {}
            }
        })
        .run(tauri::generate_context!())
        .expect("ZALUPA228");
}
