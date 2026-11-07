//! Ярлык ZALUPA228 на рабочем столе: предлагаем при первом запуске и даём создать в настройках.

use std::path::PathBuf;

const NAME: &str = "ZALUPA228.lnk";

/// Путь к ярлыку на рабочем столе (с учётом перенесённой папки «Рабочий стол», OneDrive и т.п.).
pub fn desktop_path() -> Option<PathBuf> {
    // в отладочной сборке для автотестов: не трогаем настоящий рабочий стол
    #[cfg(debug_assertions)]
    if let Some(d) = std::env::var_os("Z228_DESKTOP_DIR") {
        return Some(PathBuf::from(d).join(NAME));
    }
    win::desktop_dir().map(|d| d.join(NAME))
}

pub fn exists() -> bool {
    desktop_path().map(|p| p.exists()).unwrap_or(false)
}

/// Создаёт (или пересоздаёт) ярлык на рабочем столе на текущий exe.
pub fn create() -> Result<PathBuf, String> {
    let exe = std::env::current_exe().map_err(|e| e.to_string())?;
    let lnk = desktop_path().ok_or("не удалось найти рабочий стол")?;
    // COM нужен в своём потоке (STA): так не мешаем потокам Tauri
    let lnk2 = lnk.clone();
    std::thread::spawn(move || win::create_link(&exe, &lnk2))
        .join()
        .map_err(|_| "сбой при создании ярлыка".to_string())??;
    Ok(lnk)
}

#[cfg(windows)]
mod win {
    use std::os::windows::ffi::OsStrExt;
    use std::path::{Path, PathBuf};
    use windows::core::{Interface, HSTRING, PCWSTR};
    use windows::Win32::System::Com::{CoCreateInstance, CoInitializeEx, CoTaskMemFree, CoUninitialize, IPersistFile, CLSCTX_INPROC_SERVER, COINIT_APARTMENTTHREADED};
    use windows::Win32::UI::Shell::{FOLDERID_Desktop, IShellLinkW, SHGetKnownFolderPath, ShellLink, KF_FLAG_DEFAULT};

    pub fn desktop_dir() -> Option<PathBuf> {
        unsafe {
            let p = SHGetKnownFolderPath(&FOLDERID_Desktop, KF_FLAG_DEFAULT, None).ok()?;
            let s = p.to_string().ok();
            CoTaskMemFree(Some(p.0 as *const _));
            s.map(PathBuf::from)
        }
    }

    fn wide(p: &Path) -> Vec<u16> {
        p.as_os_str().encode_wide().chain(Some(0)).collect()
    }

    pub fn create_link(exe: &Path, lnk: &Path) -> Result<(), String> {
        unsafe {
            let init = CoInitializeEx(None, COINIT_APARTMENTTHREADED);
            let r = (|| -> windows::core::Result<()> {
                let link: IShellLinkW = CoCreateInstance(&ShellLink, None, CLSCTX_INPROC_SERVER)?;
                let exe_w = wide(exe);
                link.SetPath(PCWSTR(exe_w.as_ptr()))?;
                if let Some(dir) = exe.parent() {
                    let dir_w = wide(dir);
                    link.SetWorkingDirectory(PCWSTR(dir_w.as_ptr()))?;
                }
                link.SetIconLocation(PCWSTR(exe_w.as_ptr()), 0)?;
                link.SetDescription(&HSTRING::from("ZALUPA228 — голосовой чат"))?;
                let file: IPersistFile = link.cast()?;
                file.Save(&HSTRING::from(lnk.as_os_str()), true)?;
                Ok(())
            })();
            if init.is_ok() {
                CoUninitialize();
            }
            r.map_err(|e| e.message().to_string())
        }
    }
}

#[cfg(not(windows))]
mod win {
    use std::path::{Path, PathBuf};
    pub fn desktop_dir() -> Option<PathBuf> {
        None
    }
    pub fn create_link(_: &Path, _: &Path) -> Result<(), String> {
        Err("только для Windows".into())
    }
}
