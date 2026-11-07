//! Глобальные клавиши: низкоуровневые хуки WH_KEYBOARD_LL + WH_MOUSE_LL.
//!
//! * События никогда не «съедаем» — всегда CallNextHookEx: клавиша рации доходит и до игры.
//! * Колбэк хука только кладёт событие в канал; вся логика — в отдельном потоке. Иначе Windows
//!   (LowLevelHooksTimeout) молча снимет хук. Сторож переустанавливает хук, если он «замолчал».
//! * Рация страхуется опросом GetAsyncKeyState: если отпускание не дошло (например, ушло окну,
//!   запущенному от администратора), микрофон всё равно закроется.

use std::collections::{BTreeMap, HashSet};
use std::sync::atomic::{AtomicBool, AtomicU32, Ordering};
use std::sync::mpsc::{self, Receiver, RecvTimeoutError, Sender};
use std::sync::{Arc, OnceLock};
use std::thread;
use std::time::{Duration, Instant};

use crate::config::{BindKind, Binding};

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Action {
    PttDown,
    PttUp,
    MuteToggle,
    DeafenToggle,
}

#[derive(Debug, Clone, Copy)]
enum Raw {
    KeyDown(u32),
    KeyUp(u32),
    MouseDown(u32),
    MouseUp(u32),
}

/// Модификаторы и автоповтор снимаются прямо в колбэке хука: там GetAsyncKeyState ещё показывает
/// состояние ДО события — без гонки с быстрыми нажатиями.
enum Msg {
    Input(Raw, Mods, bool),
    Record(tokio::sync::oneshot::Sender<Option<Binding>>),
    CancelRecord,
    Bindings(BTreeMap<String, Binding>),
    ReleaseMs(u32),
}

static TX: OnceLock<Sender<Msg>> = OnceLock::new();
/// GetTickCount последнего события, которое видел любой из хуков (для сторожа).
static LAST_HOOK_TICK: AtomicU32 = AtomicU32::new(0);
static HOOK_THREAD: AtomicU32 = AtomicU32::new(0);
/// Идёт запись клавиши — сторожу и предупреждениям это не мешает, просто флаг для отладки.
static RECORDING: AtomicBool = AtomicBool::new(false);

#[derive(Clone)]
pub struct Hotkeys {
    tx: Sender<Msg>,
}

impl Hotkeys {
    pub fn set_bindings(&self, b: BTreeMap<String, Binding>) {
        let _ = self.tx.send(Msg::Bindings(b));
    }

    pub fn set_release_ms(&self, ms: u32) {
        let _ = self.tx.send(Msg::ReleaseMs(ms));
    }

    /// Ловит следующее нажатие (клавиша, сочетание или Mouse3/4/5). None — отменили (Esc).
    pub fn record(&self) -> tokio::sync::oneshot::Receiver<Option<Binding>> {
        let (tx, rx) = tokio::sync::oneshot::channel();
        let _ = self.tx.send(Msg::Record(tx));
        rx
    }

    pub fn cancel_record(&self) {
        let _ = self.tx.send(Msg::CancelRecord);
    }
}

/// Запускает хуки. `on_action` вызывается из служебного потока; `on_blocked(имя_программы)` —
/// когда в фокусе окно, запущенное от администратора (хуки его ввод не видят).
pub fn start(
    bindings: BTreeMap<String, Binding>,
    release_ms: u32,
    on_action: impl Fn(Action) + Send + 'static,
    on_blocked: impl Fn(String) + Send + 'static,
) -> Hotkeys {
    let (tx, rx) = mpsc::channel();
    let _ = TX.set(tx.clone());
    let has_bindings = Arc::new(AtomicBool::new(!bindings.is_empty()));
    {
        let has_bindings = has_bindings.clone();
        thread::Builder::new()
            .name("hotkeys".into())
            .spawn(move || Processor::new(bindings, release_ms, has_bindings).run(rx, on_action))
            .expect("hotkeys thread");
    }
    #[cfg(windows)]
    {
        thread::Builder::new().name("hooks".into()).spawn(win::hook_thread).expect("hooks thread");
        thread::Builder::new()
            .name("hooks-watchdog".into())
            .spawn(move || win::watchdog(has_bindings, on_blocked))
            .expect("watchdog thread");
    }
    #[cfg(not(windows))]
    let _ = (on_blocked, has_bindings);
    Hotkeys { tx }
}

// ---------- логика ----------

struct Processor {
    bindings: BTreeMap<String, Binding>,
    release_ms: u32,
    has_bindings: Arc<AtomicBool>,
    ptt_active: bool,
    ptt_release_at: Option<Instant>,
    /// Когда зажали рацию: GetAsyncKeyState обновляется уже после хука, первые мгновения ему не верим.
    ptt_since: Instant,
    recording: Option<tokio::sync::oneshot::Sender<Option<Binding>>>,
    rec_mod: Option<u32>,
}

impl Processor {
    fn new(bindings: BTreeMap<String, Binding>, release_ms: u32, has_bindings: Arc<AtomicBool>) -> Self {
        Processor { bindings, release_ms, has_bindings, ptt_active: false, ptt_release_at: None, ptt_since: Instant::now(), recording: None, rec_mod: None }
    }

    fn run(mut self, rx: Receiver<Msg>, on_action: impl Fn(Action)) {
        loop {
            // ждём либо событие, либо срок отпускания рации, либо очередной опрос зажатой клавиши
            let timeout = match (self.ptt_active, self.ptt_release_at) {
                (_, Some(at)) => at.saturating_duration_since(Instant::now()),
                (true, None) => Duration::from_millis(120),
                _ => Duration::from_secs(3600),
            };
            match rx.recv_timeout(timeout) {
                Ok(msg) => self.handle(msg, &on_action),
                Err(RecvTimeoutError::Timeout) => {}
                Err(RecvTimeoutError::Disconnected) => return,
            }
            if let Some(at) = self.ptt_release_at {
                if Instant::now() >= at {
                    self.ptt_release_at = None;
                    self.ptt_active = false;
                    on_action(Action::PttUp);
                }
            } else if self.ptt_active && self.ptt_since.elapsed() > Duration::from_millis(400) && !self.ptt_physically_down() {
                // отпускание потерялось — закрываем микрофон
                self.schedule_ptt_release(&on_action);
            }
        }
    }

    fn handle(&mut self, msg: Msg, on_action: &impl Fn(Action)) {
        match msg {
            Msg::Bindings(b) => {
                self.has_bindings.store(!b.is_empty(), Ordering::Relaxed);
                self.bindings = b;
                if self.ptt_active && !self.bindings.contains_key("ptt") {
                    self.ptt_active = false;
                    self.ptt_release_at = None;
                    on_action(Action::PttUp);
                }
            }
            Msg::ReleaseMs(ms) => self.release_ms = ms.min(1000),
            Msg::Record(tx) => {
                if let Some(old) = self.recording.replace(tx) {
                    let _ = old.send(None);
                }
                self.rec_mod = None;
                RECORDING.store(true, Ordering::Relaxed);
            }
            Msg::CancelRecord => self.finish_record(None),
            Msg::Input(raw, mods, repeat) => self.input(raw, mods, repeat, on_action),
        }
    }

    fn finish_record(&mut self, b: Option<Binding>) {
        if let Some(tx) = self.recording.take() {
            let _ = tx.send(b);
        }
        self.rec_mod = None;
        RECORDING.store(false, Ordering::Relaxed);
    }

    fn input(&mut self, raw: Raw, mods: Mods, repeat: bool, on_action: &impl Fn(Action)) {
        let (is_mouse, code, pressed) = match raw {
            Raw::KeyDown(c) => (false, c, true),
            Raw::KeyUp(c) => (false, c, false),
            Raw::MouseDown(c) => (true, c, true),
            Raw::MouseUp(c) => (true, c, false),
        };
        // автоповтор: повторный keydown зажатой клавиши ничего не делает
        if pressed && repeat {
            return;
        }

        if self.recording.is_some() {
            self.record_input(is_mouse, code, pressed, mods);
            return;
        }

        let kind = if is_mouse { BindKind::Mouse } else { BindKind::Key };
        let matches = |b: &Binding| b.kind == kind && b.code == code;

        if let Some(b) = self.bindings.get("ptt").filter(|b| matches(b)) {
            if pressed {
                // для рации лишние модификаторы не мешают (бежим с Shift и говорим)
                if mods.covers(b) {
                    self.ptt_release_at = None;
                    self.ptt_since = Instant::now();
                    if !self.ptt_active {
                        self.ptt_active = true;
                        on_action(Action::PttDown);
                    }
                }
            } else if self.ptt_active && self.ptt_release_at.is_none() {
                self.schedule_ptt_release(on_action);
            }
        }
        if pressed {
            for (name, act) in [("mute", Action::MuteToggle), ("deafen", Action::DeafenToggle)] {
                if let Some(b) = self.bindings.get(name).filter(|b| matches(b)) {
                    if mods.exact(b, is_mouse, code) {
                        on_action(act);
                    }
                }
            }
        }
    }

    fn schedule_ptt_release(&mut self, on_action: &impl Fn(Action)) {
        if self.release_ms == 0 {
            self.ptt_active = false;
            on_action(Action::PttUp);
        } else {
            self.ptt_release_at = Some(Instant::now() + Duration::from_millis(self.release_ms as u64));
        }
    }

    fn ptt_physically_down(&self) -> bool {
        match self.bindings.get("ptt") {
            Some(b) => physically_down(b),
            None => false,
        }
    }

    fn record_input(&mut self, is_mouse: bool, code: u32, pressed: bool, mods: Mods) {
        if is_mouse {
            if pressed {
                let b = make_binding(BindKind::Mouse, code, mods.without(None));
                self.finish_record(Some(b));
            }
            return;
        }
        const ESC: u32 = 0x1b;
        if pressed {
            if code == ESC && !mods.any() {
                self.finish_record(None);
            } else if is_modifier(code) {
                self.rec_mod = Some(code); // ждём: может, это начало сочетания
            } else {
                let b = make_binding(BindKind::Key, code, mods.without(None));
                self.finish_record(Some(b));
            }
        } else if self.rec_mod == Some(code) {
            // отпустили модификатор, ничего не нажав, — назначаем его самого (например, Left Alt)
            let b = make_binding(BindKind::Key, code, Mods::default());
            self.finish_record(Some(b));
        }
    }
}

// ---------- модификаторы ----------

#[derive(Debug, Default, Clone, Copy, PartialEq, Eq)]
struct Mods {
    ctrl: bool,
    shift: bool,
    alt: bool,
    win: bool,
}

impl Mods {
    fn any(&self) -> bool {
        self.ctrl || self.shift || self.alt || self.win
    }

    /// Все модификаторы привязки зажаты (лишние допустимы).
    fn covers(&self, b: &Binding) -> bool {
        (!b.ctrl || self.ctrl) && (!b.shift || self.shift) && (!b.alt || self.alt) && (!b.win || self.win)
    }

    /// Зажаты ровно модификаторы привязки (сама клавиша-модификатор не в счёт).
    fn exact(&self, b: &Binding, is_mouse: bool, code: u32) -> bool {
        let m = self.without(if is_mouse { None } else { Some(code) });
        m.ctrl == b.ctrl && m.shift == b.shift && m.alt == b.alt && m.win == b.win
    }

    fn without(&self, key: Option<u32>) -> Mods {
        let mut m = *self;
        if let Some(k) = key {
            match mod_class(k) {
                Some(0) => m.ctrl = false,
                Some(1) => m.shift = false,
                Some(2) => m.alt = false,
                Some(3) => m.win = false,
                _ => {}
            }
        }
        m
    }
}

fn mod_class(vk: u32) -> Option<u8> {
    match vk {
        0x11 | 0xa2 | 0xa3 => Some(0),
        0x10 | 0xa0 | 0xa1 => Some(1),
        0x12 | 0xa4 | 0xa5 => Some(2),
        0x5b | 0x5c => Some(3),
        _ => None,
    }
}

fn is_modifier(vk: u32) -> bool {
    mod_class(vk).is_some()
}

fn make_binding(kind: BindKind, code: u32, mods: Mods) -> Binding {
    let mut b = Binding { kind, code, ctrl: mods.ctrl, shift: mods.shift, alt: mods.alt, win: mods.win, name: String::new() };
    // у клавиши-модификатора свой класс в модификаторы не пишем: «Ctrl», а не «Ctrl+Ctrl»
    if kind == BindKind::Key {
        let m = mods.without(Some(code));
        b.ctrl = m.ctrl;
        b.shift = m.shift;
        b.alt = m.alt;
        b.win = m.win;
    }
    b.name = binding_name(&b);
    b
}

pub fn binding_name(b: &Binding) -> String {
    let mut parts: Vec<String> = Vec::new();
    if b.ctrl {
        parts.push("Ctrl".into());
    }
    if b.shift {
        parts.push("Shift".into());
    }
    if b.alt {
        parts.push("Alt".into());
    }
    if b.win {
        parts.push("Win".into());
    }
    parts.push(match b.kind {
        BindKind::Mouse => match b.code {
            3 => "Mouse3".into(),
            4 => "Mouse4".into(),
            5 => "Mouse5".into(),
            c => format!("Mouse{c}"),
        },
        BindKind::Key => key_name(b.code),
    });
    parts.join("+")
}

pub fn key_name(vk: u32) -> String {
    let s = match vk {
        0x30..=0x39 | 0x41..=0x5a => return char::from_u32(vk).unwrap().to_string(),
        0x70..=0x87 => return format!("F{}", vk - 0x6f),
        0x60..=0x69 => return format!("Num {}", vk - 0x60),
        0x08 => "Backspace",
        0x09 => "Tab",
        0x0d => "Enter",
        0x13 => "Pause",
        0x14 => "CapsLock",
        0x1b => "Esc",
        0x20 => "Space",
        0x21 => "PageUp",
        0x22 => "PageDown",
        0x23 => "End",
        0x24 => "Home",
        0x25 => "←",
        0x26 => "↑",
        0x27 => "→",
        0x28 => "↓",
        0x2c => "PrintScreen",
        0x2d => "Insert",
        0x2e => "Delete",
        0x5b => "Left Win",
        0x5c => "Right Win",
        0x5d => "Menu",
        0x6a => "Num *",
        0x6b => "Num +",
        0x6d => "Num -",
        0x6e => "Num .",
        0x6f => "Num /",
        0x90 => "NumLock",
        0x91 => "ScrollLock",
        0xa0 => "Left Shift",
        0xa1 => "Right Shift",
        0xa2 => "Left Ctrl",
        0xa3 => "Right Ctrl",
        0xa4 => "Left Alt",
        0xa5 => "Right Alt",
        0xad => "Mute",
        0xae => "Volume Down",
        0xaf => "Volume Up",
        0xb0 => "Next Track",
        0xb1 => "Prev Track",
        0xb3 => "Play/Pause",
        0xba => ";",
        0xbb => "=",
        0xbc => ",",
        0xbd => "-",
        0xbe => ".",
        0xbf => "/",
        0xc0 => "`",
        0xdb => "[",
        0xdc => "\\",
        0xdd => "]",
        0xde => "'",
        0xe2 => "\\ (102)",
        _ => return format!("Key 0x{vk:02X}"),
    };
    s.to_string()
}

#[cfg(windows)]
fn physically_down(b: &Binding) -> bool {
    win::physically_down(b)
}

#[cfg(not(windows))]
fn physically_down(_: &Binding) -> bool {
    true
}

// ---------- Windows ----------

#[cfg(windows)]
mod win {
    use super::*;
    use windows::core::PWSTR;
    use windows::Win32::Foundation::{CloseHandle, HANDLE, HWND, LPARAM, LRESULT, WPARAM};
    use windows::Win32::Security::{GetTokenInformation, TokenElevation, TOKEN_ELEVATION, TOKEN_QUERY};
    use windows::Win32::System::LibraryLoader::GetModuleHandleW;
    use windows::Win32::System::SystemInformation::GetTickCount;
    use windows::Win32::System::Threading::{
        GetCurrentProcess, GetCurrentProcessId, GetCurrentThreadId, OpenProcess, OpenProcessToken, QueryFullProcessImageNameW, PROCESS_NAME_WIN32,
        PROCESS_QUERY_LIMITED_INFORMATION,
    };
    use windows::Win32::UI::Input::KeyboardAndMouse::{GetAsyncKeyState, GetLastInputInfo, LASTINPUTINFO};
    use windows::Win32::UI::WindowsAndMessaging::{
        CallNextHookEx, GetForegroundWindow, GetMessageW, GetWindowThreadProcessId, PostThreadMessageW, SetWindowsHookExW, UnhookWindowsHookEx, HHOOK,
        KBDLLHOOKSTRUCT, MSG, MSLLHOOKSTRUCT, WH_KEYBOARD_LL, WH_MOUSE_LL, WM_APP, WM_KEYDOWN, WM_KEYUP, WM_MBUTTONDOWN, WM_MBUTTONUP, WM_SYSKEYDOWN,
        WM_SYSKEYUP, WM_XBUTTONDOWN, WM_XBUTTONUP,
    };

    const WM_REINSTALL: u32 = WM_APP + 1;

    fn push(raw: Raw) {
        let repeat = match raw {
            Raw::KeyDown(vk) => key_down(vk as i32),
            _ => false,
        };
        if let Some(tx) = TX.get() {
            let _ = tx.send(Msg::Input(raw, current_mods(), repeat));
        }
    }

    unsafe extern "system" fn keyboard_proc(code: i32, wparam: WPARAM, lparam: LPARAM) -> LRESULT {
        if code >= 0 {
            let k = &*(lparam.0 as *const KBDLLHOOKSTRUCT);
            LAST_HOOK_TICK.store(k.time, Ordering::Relaxed);
            match wparam.0 as u32 {
                WM_KEYDOWN | WM_SYSKEYDOWN => push(Raw::KeyDown(k.vkCode)),
                WM_KEYUP | WM_SYSKEYUP => push(Raw::KeyUp(k.vkCode)),
                _ => {}
            }
        }
        CallNextHookEx(None, code, wparam, lparam)
    }

    unsafe extern "system" fn mouse_proc(code: i32, wparam: WPARAM, lparam: LPARAM) -> LRESULT {
        if code >= 0 {
            let m = &*(lparam.0 as *const MSLLHOOKSTRUCT);
            LAST_HOOK_TICK.store(m.time, Ordering::Relaxed);
            // движения мыши и левая/правая кнопки сюда не идут — только то, что можно назначить
            let x = || if (m.mouseData >> 16) & 0xffff == 1 { 4 } else { 5 };
            match wparam.0 as u32 {
                WM_XBUTTONDOWN => push(Raw::MouseDown(x())),
                WM_XBUTTONUP => push(Raw::MouseUp(x())),
                WM_MBUTTONDOWN => push(Raw::MouseDown(3)),
                WM_MBUTTONUP => push(Raw::MouseUp(3)),
                _ => {}
            }
        }
        CallNextHookEx(None, code, wparam, lparam)
    }

    unsafe fn install() -> (Option<HHOOK>, Option<HHOOK>) {
        let hmod = GetModuleHandleW(None).ok();
        let hinst = hmod.map(|m| windows::Win32::Foundation::HINSTANCE(m.0));
        let kb = SetWindowsHookExW(WH_KEYBOARD_LL, Some(keyboard_proc), hinst, 0).ok();
        let ms = SetWindowsHookExW(WH_MOUSE_LL, Some(mouse_proc), hinst, 0).ok();
        LAST_HOOK_TICK.store(GetTickCount(), Ordering::Relaxed);
        (kb, ms)
    }

    unsafe fn uninstall(h: (Option<HHOOK>, Option<HHOOK>)) {
        if let Some(k) = h.0 {
            let _ = UnhookWindowsHookEx(k);
        }
        if let Some(m) = h.1 {
            let _ = UnhookWindowsHookEx(m);
        }
    }

    /// Поток хуков: свой цикл сообщений (без него LL-хуки не вызываются).
    pub fn hook_thread() {
        unsafe {
            HOOK_THREAD.store(GetCurrentThreadId(), Ordering::Relaxed);
            let mut hooks = install();
            let mut msg = MSG::default();
            while GetMessageW(&mut msg, None, 0, 0).as_bool() {
                if msg.message == WM_REINSTALL {
                    uninstall(hooks);
                    hooks = install();
                }
            }
            uninstall(hooks);
        }
    }

    fn key_down(vk: i32) -> bool {
        unsafe { (GetAsyncKeyState(vk) as u16 & 0x8000) != 0 }
    }

    pub fn current_mods() -> Mods {
        Mods { ctrl: key_down(0x11), shift: key_down(0x10), alt: key_down(0x12), win: key_down(0x5b) || key_down(0x5c) }
    }

    pub fn physically_down(b: &Binding) -> bool {
        match b.kind {
            BindKind::Key => key_down(b.code as i32),
            BindKind::Mouse => key_down(match b.code {
                3 => 0x04,
                4 => 0x05,
                _ => 0x06,
            }),
        }
    }

    unsafe fn token_elevated(process: HANDLE) -> Option<bool> {
        let mut token = HANDLE::default();
        OpenProcessToken(process, TOKEN_QUERY, &mut token).ok()?;
        let mut el = TOKEN_ELEVATION::default();
        let mut len = 0u32;
        let r = GetTokenInformation(token, TokenElevation, Some(&mut el as *mut _ as *mut _), std::mem::size_of::<TOKEN_ELEVATION>() as u32, &mut len);
        let _ = CloseHandle(token);
        r.ok()?;
        Some(el.TokenIsElevated != 0)
    }

    /// Окно в фокусе принадлежит процессу «от администратора»? Возвращает имя exe.
    unsafe fn elevated_foreground() -> Option<String> {
        let hwnd: HWND = GetForegroundWindow();
        if hwnd.0.is_null() {
            return None;
        }
        let mut pid = 0u32;
        GetWindowThreadProcessId(hwnd, Some(&mut pid));
        if pid == 0 || pid == GetCurrentProcessId() {
            return None;
        }
        // не открылся процесс совсем — не знаем (так бывает у защищённых античитом игр): молчим
        let p = OpenProcess(PROCESS_QUERY_LIMITED_INFORMATION, false, pid).ok()?;
        let mut buf = [0u16; 520];
        let mut len = buf.len() as u32;
        let name = if QueryFullProcessImageNameW(p, PROCESS_NAME_WIN32, PWSTR(buf.as_mut_ptr()), &mut len).is_ok() {
            let full = String::from_utf16_lossy(&buf[..len as usize]);
            full.rsplit(['\\', '/']).next().unwrap_or(&full).to_string()
        } else {
            String::new()
        };
        // токен не открылся (доступ запрещён) — почти наверняка процесс повышенный
        let elevated = token_elevated(p).unwrap_or(true);
        let _ = CloseHandle(p);
        elevated.then_some(name)
    }

    /// Сторож: переустанавливает хуки, если Windows их сняла, и предупреждает об окнах администратора.
    pub fn watchdog(has_bindings: Arc<AtomicBool>, on_blocked: impl Fn(String)) {
        let self_elevated = unsafe { token_elevated(GetCurrentProcess()).unwrap_or(false) };
        let mut warned: HashSet<String> = HashSet::new();
        loop {
            thread::sleep(Duration::from_secs(3));
            let elevated_fg = if self_elevated { None } else { unsafe { elevated_foreground() } };
            if let Some(name) = &elevated_fg {
                if has_bindings.load(Ordering::Relaxed) && warned.insert(name.to_lowercase()) {
                    on_blocked(name.clone());
                }
                continue; // ввод идёт в окно администратора — тишина в хуках ожидаема
            }
            let mut lii = LASTINPUTINFO { cbSize: std::mem::size_of::<LASTINPUTINFO>() as u32, dwTime: 0 };
            if !unsafe { GetLastInputInfo(&mut lii) }.as_bool() {
                continue;
            }
            let now = unsafe { GetTickCount() };
            let last_hook = LAST_HOOK_TICK.load(Ordering::Relaxed);
            let input_recent = now.wrapping_sub(lii.dwTime) < 10_000;
            let hook_behind = lii.dwTime.wrapping_sub(last_hook) as i32 > 1500;
            if input_recent && hook_behind {
                let tid = HOOK_THREAD.load(Ordering::Relaxed);
                #[cfg(debug_assertions)]
                eprintln!("[z228] hooks: тишина при вводе — переустанавливаю");
                if tid != 0 {
                    unsafe {
                        let _ = PostThreadMessageW(tid, WM_REINSTALL, WPARAM(0), LPARAM(0));
                    }
                }
            }
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn kb(code: u32, ctrl: bool, shift: bool) -> Binding {
        let mut b = Binding { kind: BindKind::Key, code, ctrl, shift, alt: false, win: false, name: String::new() };
        b.name = binding_name(&b);
        b
    }

    #[test]
    fn names() {
        assert_eq!(kb(0x56, false, false).name, "V");
        assert_eq!(kb(0x4d, true, true).name, "Ctrl+Shift+M");
        assert_eq!(make_binding(BindKind::Mouse, 4, Mods::default()).name, "Mouse4");
        // модификатор сам по себе
        let alt = make_binding(BindKind::Key, 0xa4, Mods { alt: true, ..Default::default() });
        assert_eq!(alt.name, "Left Alt");
        assert!(!alt.alt);
    }

    #[test]
    fn modifier_matching() {
        let b = kb(0x4d, true, false);
        let ctrl = Mods { ctrl: true, ..Default::default() };
        let ctrl_shift = Mods { ctrl: true, shift: true, ..Default::default() };
        assert!(ctrl.exact(&b, false, 0x4d));
        assert!(!ctrl_shift.exact(&b, false, 0x4d));
        assert!(ctrl_shift.covers(&b));
        assert!(!Mods::default().covers(&b));
    }

    fn proc_with(b: BTreeMap<String, Binding>, ms: u32) -> Processor {
        Processor::new(b, ms, Arc::new(AtomicBool::new(true)))
    }

    #[test]
    fn ptt_ignores_autorepeat_and_toggles_fire_once() {
        let mut map = BTreeMap::new();
        map.insert("ptt".to_string(), make_binding(BindKind::Mouse, 4, Mods::default()));
        map.insert("mute".to_string(), kb(0x4d, false, false));
        let mut p = proc_with(map, 0);
        let got = std::cell::RefCell::new(Vec::new());
        let on = |a| got.borrow_mut().push(a);
        let m = Mods::default();
        p.input(Raw::MouseDown(4), m, false, &on);
        p.input(Raw::MouseUp(4), m, false, &on);
        p.input(Raw::KeyDown(0x4d), m, false, &on);
        p.input(Raw::KeyDown(0x4d), m, true, &on); // автоповтор
        p.input(Raw::KeyUp(0x4d), m, false, &on);
        // мьют на M без модификаторов: с Ctrl не срабатывает
        p.input(Raw::KeyDown(0x4d), Mods { ctrl: true, ..m }, false, &on);
        assert_eq!(*got.borrow(), vec![Action::PttDown, Action::PttUp, Action::MuteToggle]);
    }

    #[test]
    fn ptt_release_delay_and_repress() {
        let mut map = BTreeMap::new();
        map.insert("ptt".to_string(), kb(0x56, false, false));
        let mut p = proc_with(map, 150);
        let got = std::cell::RefCell::new(Vec::new());
        let on = |a| got.borrow_mut().push(a);
        let m = Mods::default();
        p.input(Raw::KeyDown(0x56), m, false, &on);
        p.input(Raw::KeyDown(0x56), m, true, &on);
        p.input(Raw::KeyUp(0x56), m, false, &on);
        assert!(p.ptt_release_at.is_some());
        p.input(Raw::KeyDown(0x56), Mods { shift: true, ..m }, false, &on); // снова (бежим с Shift) до истечения задержки — без нового PttDown
        assert!(p.ptt_release_at.is_none());
        assert_eq!(*got.borrow(), vec![Action::PttDown]);
    }

    #[test]
    fn recording() {
        let mut p = proc_with(BTreeMap::new(), 0);
        let none = |_| {};
        let (tx, mut rx) = tokio::sync::oneshot::channel();
        p.handle(Msg::Record(tx), &none);
        let m = Mods::default();
        p.input(Raw::KeyDown(0x1b), m, false, &none);
        assert_eq!(rx.try_recv().unwrap(), None);

        // сочетание: Ctrl, Shift, M
        let (tx, mut rx) = tokio::sync::oneshot::channel();
        p.handle(Msg::Record(tx), &none);
        p.input(Raw::KeyDown(0xa2), m, false, &none);
        p.input(Raw::KeyDown(0xa0), Mods { ctrl: true, ..m }, false, &none);
        p.input(Raw::KeyDown(0x4d), Mods { ctrl: true, shift: true, ..m }, false, &none);
        assert_eq!(rx.try_recv().unwrap().unwrap().name, "Ctrl+Shift+M");

        // модификатор сам по себе
        let (tx, mut rx) = tokio::sync::oneshot::channel();
        p.handle(Msg::Record(tx), &none);
        p.input(Raw::KeyDown(0xa4), m, false, &none);
        p.input(Raw::KeyUp(0xa4), Mods { alt: true, ..m }, false, &none);
        assert_eq!(rx.try_recv().unwrap().unwrap().name, "Left Alt");

        let (tx, mut rx) = tokio::sync::oneshot::channel();
        p.handle(Msg::Record(tx), &none);
        p.input(Raw::MouseDown(5), m, false, &none);
        assert_eq!(rx.try_recv().unwrap().unwrap().name, "Mouse5");
    }
}
