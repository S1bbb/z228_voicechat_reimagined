//! Автообновление через GitHub Releases (tauri-plugin-updater, подпись ключом Tauri).
//! Проверка при старте и раз в 6 часов; ставим только по кнопке — не прерываем голос посреди игры.

use serde_json::json;
use std::time::Duration;
use tauri::{AppHandle, Manager};
use tauri_plugin_updater::UpdaterExt;

use crate::bridge::{emit_page, AppState};

pub fn spawn_loop(app: AppHandle) {
    tauri::async_runtime::spawn(async move {
        tokio::time::sleep(Duration::from_secs(20)).await;
        loop {
            if let Err(e) = check(&app).await {
                eprintln!("updater: {e}");
            }
            tokio::time::sleep(Duration::from_secs(6 * 3600)).await;
        }
    });
}

/// Есть ли новая версия; если есть — запоминает её и показывает баннер на странице.
pub async fn check(app: &AppHandle) -> Result<Option<String>, String> {
    let found = app.updater().map_err(|e| e.to_string())?.check().await.map_err(|e| e.to_string())?;
    let Some(u) = found else { return Ok(None) };
    let version = u.version.clone();
    emit_page(app, json!({ "type": "update-available", "version": version, "notes": u.body.clone().unwrap_or_default() }));
    *app.state::<AppState>().pending_update.lock().unwrap() = Some(u);
    Ok(Some(version))
}

pub async fn install(app: &AppHandle) -> Result<(), String> {
    let pending = app.state::<AppState>().pending_update.lock().unwrap().take();
    let u = match pending {
        Some(u) => u,
        None => app.updater().map_err(|e| e.to_string())?.check().await.map_err(|e| e.to_string())?.ok_or("обновлений нет")?,
    };
    let mut got: u64 = 0;
    let mut last_pct = u64::MAX;
    let app2 = app.clone();
    u.download_and_install(
        move |chunk, total| {
            got += chunk as u64;
            if let Some(t) = total.filter(|t| *t > 0) {
                let pct = got * 100 / t;
                if pct != last_pct {
                    last_pct = pct;
                    emit_page(&app2, json!({ "type": "update-progress", "pct": pct }));
                }
            }
        },
        || {},
    )
    .await
    .map_err(|e| e.to_string())?;
    app.restart();
}
