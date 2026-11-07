// В релизе без консольного окна.
#![cfg_attr(not(debug_assertions), windows_subsystem = "windows")]

mod bridge;
mod config;
mod hooks;
mod invite;
mod pin;
mod shortcut;
mod tray;
mod update;
mod webview2;

fn main() {
    bridge::run();
}
