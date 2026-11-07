//! Настройки приложения: %APPDATA%\ZALUPA228\config.json.
//! Ключ доступа сюда не пишем — его хранит сам сайт в localStorage WebView2.

use serde::{Deserialize, Serialize};
use std::collections::BTreeMap;
use std::fs;
use std::io;
use std::path::PathBuf;

/// Действия, на которые можно назначить глобальную клавишу.
pub const ACTIONS: [&str; 3] = ["ptt", "mute", "deafen"];

#[derive(Clone, Copy, Debug, Serialize, Deserialize, PartialEq, Eq)]
#[serde(rename_all = "lowercase")]
pub enum BindKind {
    /// Клавиша клавиатуры, `code` — virtual-key code Windows.
    Key,
    /// Кнопка мыши, `code`: 3 — средняя, 4 — Mouse4 (XBUTTON1), 5 — Mouse5 (XBUTTON2).
    Mouse,
}

#[derive(Clone, Debug, Serialize, Deserialize, PartialEq, Eq)]
#[serde(rename_all = "camelCase")]
pub struct Binding {
    pub kind: BindKind,
    pub code: u32,
    #[serde(default)]
    pub ctrl: bool,
    #[serde(default)]
    pub shift: bool,
    #[serde(default)]
    pub alt: bool,
    #[serde(default)]
    pub win: bool,
    /// Человекочитаемое имя: «Mouse4», «Ctrl+Shift+M», «V».
    pub name: String,
}

impl Binding {
    /// Та же физическая клавиша/кнопка с теми же модификаторами.
    pub fn same_input(&self, o: &Binding) -> bool {
        self.kind == o.kind && self.code == o.code && self.ctrl == o.ctrl && self.shift == o.shift && self.alt == o.alt && self.win == o.win
    }
}

#[derive(Clone, Debug, Serialize, Deserialize)]
#[serde(rename_all = "camelCase", default)]
pub struct Config {
    /// "host:port" сервера (IPv6 — в квадратных скобках).
    pub server: Option<String>,
    /// SHA-256 SPKI сертификата сервера (base64url без «=»). None — у сервера доверенный сертификат.
    pub spki: Option<String>,
    /// Название сервера для окна и трея.
    pub name: Option<String>,
    pub bindings: BTreeMap<String, Binding>,
    pub ptt_release_ms: u32,
    pub ptt_sound: bool,
    /// Уже показывали уведомление «работает в трее».
    pub tray_hint_shown: bool,
    /// Уже предлагали ярлык на рабочем столе (при первом входе).
    pub shortcut_offered: bool,
}

impl Default for Config {
    fn default() -> Self {
        Config {
            server: None,
            spki: None,
            name: None,
            bindings: BTreeMap::new(),
            ptt_release_ms: 150,
            ptt_sound: true,
            tray_hint_shown: false,
            shortcut_offered: false,
        }
    }
}

pub fn dir() -> PathBuf {
    let base = std::env::var_os("APPDATA").map(PathBuf::from).unwrap_or_else(std::env::temp_dir);
    base.join("ZALUPA228")
}

pub fn path() -> PathBuf {
    dir().join("config.json")
}

pub fn load() -> Config {
    let mut c: Config = fs::read(path()).ok().and_then(|b| serde_json::from_slice(&b).ok()).unwrap_or_default();
    c.ptt_release_ms = c.ptt_release_ms.min(1000);
    c.bindings.retain(|k, _| ACTIONS.contains(&k.as_str()));
    c
}

pub fn save(c: &Config) -> io::Result<()> {
    fs::create_dir_all(dir())?;
    let tmp = dir().join("config.json.tmp");
    fs::write(&tmp, serde_json::to_vec_pretty(c).map_err(io::Error::other)?)?;
    fs::rename(tmp, path())
}
