fn main() {
    // Свои команды описываем в манифесте: так их можно разрешать поимённо —
    // окну connect одни, странице сервера (удалённый origin) другие.
    let attrs = tauri_build::Attributes::new().app_manifest(tauri_build::AppManifest::new().commands(&[
        "desktop_info",
        "desktop_report_state",
        "desktop_record_binding",
        "desktop_clear_binding",
        "desktop_set_options",
        "desktop_change_server",
        "desktop_check_update",
        "desktop_install_update",
        "desktop_create_shortcut",
        "connect_status",
        "connect_submit",
        "connect_retry",
        "connect_quit",
    ]));
    tauri_build::try_build(attrs).expect("tauri build");
}
