// Мост с десктоп-приложением ZALUPA228 (Tauri v2). В обычном браузере модуль ничего не делает.
//
// Rust → страница: window.dispatchEvent(new CustomEvent("z228-desktop", {detail: {type, ...}}))
//   type: "ptt" {down} — глобальная клавиша рации (задержку отпускания уже отсчитал Rust),
//         "mute-toggle", "deafen-toggle", "update-available" {version, notes}, "update-progress" {pct},
//         "update-none", "update-error" {message}, "hotkeys-blocked" {app}.
// Страница → Rust: window.__TAURI__.core.invoke("desktop_*", ...).

export const isDesktop = !!(window.__TAURI__ && window.__TAURI__.core && window.__TAURI__.core.invoke);

let api = null; // {S, voice, toggleMute, toggleDeafen, sfx, saveSettings, toast, onInfo}
let info = null; // ответ desktop_info
let pttDown = false;
let lastReport = "";
let reportTimer = 0;
let lastReportAt = 0;
let updateBar = null;

export function invoke(cmd, args) {
  return window.__TAURI__.core.invoke(cmd, args || {});
}

/** Данные приложения: {version, bindings: {ptt, mute, deafen} → {name}|null, pttReleaseMs, pttSound, server, name}. */
export function desktopInfo() {
  return info;
}

/** Работает ли глобальная рация приложения (тогда веб-обработчики клавиш рации отключены). */
export function desktopPtt() {
  return !!(info && info.bindings && info.bindings.ptt);
}

/** Назначена ли глобальная клавиша для действия ("mute" | "deafen"). */
export function desktopBinding(action) {
  return !!(info && info.bindings && info.bindings[action]);
}

export async function initDesktop(opts) {
  if (!isDesktop) return;
  api = opts;
  document.documentElement.classList.add("is-desktop");
  window.addEventListener("z228-desktop", (e) => onEvent(e.detail || {}));
  await refreshInfo();
  // обновление нашлось ещё до загрузки страницы
  if (info && info.update) showUpdateBar(`Доступно обновление ${info.update.version}`, true);
  // запасной опрос: события состояния и так приходят, но вдруг что-то пропустили
  setInterval(reportState, 1000);
}

export async function refreshInfo() {
  try {
    info = await invoke("desktop_info");
  } catch (e) {
    console.warn("desktop_info", e);
    return null;
  }
  syncPttMode();
  if (api && api.onInfo) api.onInfo(info);
  return info;
}

// Назначена глобальная клавиша рации — включаем режим рации; сняли — выключаем.
function syncPttMode() {
  const s = api.S.settings;
  const want = desktopPtt();
  if (want && !s.pushToTalk) {
    s.pushToTalk = true;
    s.desktopPtt = true;
    api.saveSettings();
  } else if (!want && s.desktopPtt) {
    s.pushToTalk = false;
    s.desktopPtt = false;
    api.saveSettings();
  }
  if (!want && pttDown) setPtt(false);
  api.voice.applyMicGate();
}

function setPtt(down) {
  if (down === pttDown) return;
  pttDown = down;
  api.voice.setPttHeld(down);
  if (info && info.pttSound && api.voice.active && api.S.settings.pushToTalk) api.sfx(down ? "ptt-on" : "ptt-off");
  stateChanged();
}

function onEvent(d) {
  switch (d.type) {
    case "ptt":
      if (desktopPtt()) setPtt(!!d.down);
      return;
    case "mute-toggle":
      api.toggleMute();
      return;
    case "deafen-toggle":
      api.toggleDeafen();
      return;
    case "update-available":
      showUpdateBar(`Доступно обновление ${d.version}`, true);
      return;
    case "update-progress":
      showUpdateBar(`Загрузка обновления… ${Math.round(d.pct || 0)}%`, false);
      return;
    case "update-none":
      api.toast("Установлена последняя версия.", "ok");
      return;
    case "update-error":
      if (updateBar) updateBar.remove();
      updateBar = null;
      api.toast("Обновление: " + (d.message || "ошибка"), "error");
      return;
    case "hotkeys-blocked":
      api.toast(
        `${d.app || "Активное окно"} запущено от администратора — Windows не пускает к нему горячие клавиши. Запустите ZALUPA228 тоже от администратора.`,
        "error"
      );
      return;
  }
}

function showUpdateBar(text, withButton) {
  if (!updateBar) {
    updateBar = document.createElement("div");
    updateBar.className = "desktop-update";
    document.body.append(updateBar);
  }
  updateBar.innerHTML = "";
  const span = document.createElement("span");
  span.textContent = text;
  updateBar.append(span);
  if (withButton) {
    const go = document.createElement("button");
    go.className = "btn btn-sm btn-primary";
    go.textContent = "Обновить и перезапустить";
    go.onclick = () => {
      showUpdateBar("Загрузка обновления…", false);
      invoke("desktop_install_update").catch((e) => onEvent({ type: "update-error", message: String(e) }));
    };
    const later = document.createElement("button");
    later.className = "btn btn-sm";
    later.textContent = "Позже";
    later.onclick = () => {
      updateBar.remove();
      updateBar = null;
    };
    updateBar.append(go, later);
  }
}

// ---------- состояние для трея (и будущего оверлея) ----------

/** Вызывать при любом изменении голоса: отчёт уйдёт не чаще 10 раз в секунду. */
export function stateChanged() {
  if (!api || reportTimer) return;
  const wait = Math.max(0, 100 - (performance.now() - lastReportAt));
  reportTimer = setTimeout(() => {
    reportTimer = 0;
    reportState();
  }, wait);
}

function reportState() {
  if (!api) return;
  const { S, voice } = api;
  const inVoice = !!voice.active;
  const meId = S.me && S.me.id;
  const ch = inVoice ? S.channels.find((c) => c.id === voice.channelId) : null;
  const micOpen = inVoice && voice.micOpen();
  const transmitting = micOpen && ((S.settings.pushToTalk && voice.pttHeld) || (!!meId && S.speaking.has(meId)));
  const members = inVoice
    ? S.users
        .filter((u) => u.voiceChannelId === voice.channelId)
        .map((u) => ({ id: u.id, nickname: u.nickname, speaking: S.speaking.has(u.id), muted: !!(u.muted || u.serverMuted), deafened: !!u.deafened }))
    : [];
  const st = {
    inVoice,
    channel: ch ? ch.name : "",
    server: (S.server && S.server.name) || "",
    muted: !!(S.muted || voice.serverMuted),
    deafened: !!S.deafened,
    transmitting: !!transmitting,
    members,
  };
  const key = JSON.stringify(st);
  if (key === lastReport) return;
  lastReport = key;
  lastReportAt = performance.now();
  invoke("desktop_report_state", { state: st }).catch(() => {});
}
