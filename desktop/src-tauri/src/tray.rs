//! Иконка в трее: состояние (не в голосе / в голосе / передаю / мьют), подсказка и меню.

use serde::Deserialize;
use tauri::image::Image;
use tauri::menu::{CheckMenuItem, Menu, MenuItem, PredefinedMenuItem};
use tauri::tray::{MouseButton, MouseButtonState, TrayIcon, TrayIconBuilder, TrayIconEvent};
use tauri::{AppHandle, Manager, Wry};

/// Состояние, которое присылает страница (desktop_report_state). Список участников — задел для оверлея.
#[derive(Debug, Clone, Default, Deserialize, PartialEq)]
#[serde(rename_all = "camelCase", default)]
pub struct PageState {
    pub in_voice: bool,
    pub channel: String,
    pub server: String,
    pub muted: bool,
    pub deafened: bool,
    pub transmitting: bool,
    pub members: Vec<Member>,
}

#[derive(Debug, Clone, Default, Deserialize, PartialEq)]
#[serde(rename_all = "camelCase", default)]
pub struct Member {
    pub id: String,
    pub nickname: String,
    pub speaking: bool,
    pub muted: bool,
    pub deafened: bool,
}

#[derive(Clone, Copy, PartialEq, Eq, Debug)]
pub enum Look {
    Idle,
    Voice,
    Talking,
    Muted,
}

impl PageState {
    pub fn look(&self) -> Look {
        match self {
            s if !s.in_voice => Look::Idle,
            s if s.muted || s.deafened => Look::Muted,
            s if s.transmitting => Look::Talking,
            _ => Look::Voice,
        }
    }

    pub fn tooltip(&self, server_name: &str) -> String {
        let mut t = String::from("ZALUPA228");
        if !self.in_voice {
            if !server_name.is_empty() {
                t += &format!(" — {server_name}");
            }
            return t + " · не в голосе";
        }
        t += &format!(" — {}", if self.channel.is_empty() { "голос" } else { &self.channel });
        if self.deafened {
            t += " · звук выключен";
        } else if self.muted {
            t += " · мьют";
        } else if self.transmitting {
            t += " · говорю";
        }
        t
    }
}

pub struct Icons {
    idle: Image<'static>,
    voice: Image<'static>,
    talking: Image<'static>,
    muted: Image<'static>,
}

impl Icons {
    pub fn get(&self, l: Look) -> Image<'static> {
        match l {
            Look::Idle => self.idle.clone(),
            Look::Voice => self.voice.clone(),
            Look::Talking => self.talking.clone(),
            Look::Muted => self.muted.clone(),
        }
    }
}

/// Иконки состояний рисуем из логотипа: серая, обычная, с зелёной и с красной точкой.
pub fn make_icons() -> Icons {
    let base = Image::from_bytes(include_bytes!("../icons/32x32.png")).expect("tray icon");
    let (w, h) = (base.width(), base.height());
    let rgba = base.rgba().to_vec();

    let mut gray = rgba.clone();
    for px in gray.chunks_exact_mut(4) {
        let l = (px[0] as u32 * 30 + px[1] as u32 * 59 + px[2] as u32 * 11) / 100;
        let l = (l * 7 / 10 + 40).min(255) as u8;
        px[0] = l;
        px[1] = l;
        px[2] = l;
        px[3] = (px[3] as u32 * 75 / 100) as u8;
    }
    let dot = |color: [u8; 3]| {
        let mut img = rgba.clone();
        draw_dot(&mut img, w, h, color);
        Image::new_owned(img, w, h)
    };
    Icons {
        idle: Image::new_owned(gray, w, h),
        voice: Image::new_owned(rgba.clone(), w, h),
        talking: dot([0x23, 0xc5, 0x5e]),
        muted: dot([0xef, 0x44, 0x44]),
    }
}

/// Точка в правом нижнем углу с тёмной обводкой, со сглаживанием.
fn draw_dot(img: &mut [u8], w: u32, h: u32, color: [u8; 3]) {
    let s = w.min(h) as f32;
    let r = s * 0.19;
    let border = s * 0.06;
    let (cx, cy) = (w as f32 - r - border, h as f32 - r - border);
    for y in 0..h {
        for x in 0..w {
            let d = ((x as f32 + 0.5 - cx).powi(2) + (y as f32 + 0.5 - cy).powi(2)).sqrt();
            let outer = (r + border + 0.5 - d).clamp(0.0, 1.0);
            if outer <= 0.0 {
                continue;
            }
            let inner = (r + 0.5 - d).clamp(0.0, 1.0);
            let i = ((y * w + x) * 4) as usize;
            let px = &mut img[i..i + 4];
            // сначала обводка (цвет фона трея неизвестен — тёмная), поверх — цвет точки
            for c in 0..3 {
                let dark = 0x1a as f32;
                let v = px[c] as f32 * (1.0 - outer) + dark * outer;
                px[c] = (v * (1.0 - inner) + color[c] as f32 * inner) as u8;
            }
            px[3] = (px[3] as f32 + (255.0 - px[3] as f32) * outer) as u8;
        }
    }
}

pub struct TrayMenu {
    pub mute: CheckMenuItem<Wry>,
    pub deafen: CheckMenuItem<Wry>,
}

pub fn build(app: &AppHandle) -> tauri::Result<(TrayIcon, TrayMenu)> {
    let open = MenuItem::with_id(app, "open", "Открыть", true, None::<&str>)?;
    let mute = CheckMenuItem::with_id(app, "mute", "Мьют", true, false, None::<&str>)?;
    let deafen = CheckMenuItem::with_id(app, "deafen", "Оглушение", true, false, None::<&str>)?;
    let server = MenuItem::with_id(app, "server", "Сменить сервер…", true, None::<&str>)?;
    let update = MenuItem::with_id(app, "update", "Проверить обновления", true, None::<&str>)?;
    let quit = MenuItem::with_id(app, "quit", "Выход", true, None::<&str>)?;
    let menu = Menu::with_items(
        app,
        &[&open, &PredefinedMenuItem::separator(app)?, &mute, &deafen, &PredefinedMenuItem::separator(app)?, &server, &update, &PredefinedMenuItem::separator(app)?, &quit],
    )?;
    let icons = app.state::<crate::bridge::AppState>().icons.get(Look::Idle);
    let tray = TrayIconBuilder::with_id("main")
        .icon(icons)
        .tooltip("ZALUPA228")
        .menu(&menu)
        .show_menu_on_left_click(false)
        .on_menu_event(|app, ev| crate::bridge::on_tray_menu(app, ev.id().as_ref()))
        .on_tray_icon_event(|tray, ev| {
            if let TrayIconEvent::Click { button: MouseButton::Left, button_state: MouseButtonState::Up, .. } = ev {
                crate::bridge::show_main_or_connect(tray.app_handle());
            }
        })
        .build(app)?;
    Ok((tray, TrayMenu { mute, deafen }))
}

#[cfg(test)]
mod tests {
    use super::*;

    /// Превью иконок трея: Z228_TRAY_PREVIEW=<папка> cargo test tray_preview -- --ignored
    #[test]
    #[ignore]
    fn tray_preview() {
        let dir = std::env::var("Z228_TRAY_PREVIEW").expect("Z228_TRAY_PREVIEW");
        let icons = make_icons();
        for (name, look) in [("idle", Look::Idle), ("voice", Look::Voice), ("talking", Look::Talking), ("muted", Look::Muted)] {
            let img = icons.get(look);
            let mut out = Vec::new();
            for px in img.rgba().chunks_exact(4) {
                out.extend_from_slice(px);
            }
            // простейший PPM с подложкой цвета тёмной панели задач, чтобы видеть прозрачность
            let (w, h) = (img.width(), img.height());
            let mut ppm = format!("P6 {w} {h} 255\n").into_bytes();
            for px in out.chunks_exact(4) {
                let a = px[3] as u32;
                for c in 0..3 {
                    ppm.push(((px[c] as u32 * a + 0x20 * (255 - a)) / 255) as u8);
                }
            }
            std::fs::write(format!("{dir}/{name}.ppm"), ppm).unwrap();
        }
    }
}
