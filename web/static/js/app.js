import { $, $$, h, avatar, avatarColor, setAvatarSource, lsGet, lsSet, randomId, renderMarkdown, mentionsMe, isEmojiOnly, fmtTime, fmtFull, fmtStamp, fmtDay, sameDay, fmtSize } from "./util.js";
import { icon } from "./icons.js";
import { Voice, micTest, sfx, rtcConstructor, rtcDiagnostics, qualityLevel } from "./voice.js";
import { EMOJI_GROUPS, QUICK_REACTIONS } from "./emoji.js";

// ============================================================ состояние

const DEFAULT_SETTINGS = {
  theme: "dark",
  inputDevice: "",
  outputDevice: "",
  outputVolume: 100,
  echoCancellation: true,
  noiseSuppression: true,
  autoGainControl: true,
  rnnoise: false,
  pushToTalk: false,
  pttKey: "",
  screenQuality: "1080p30",
  sounds: true,
  notifications: false,
};

const S = {
  settings: { ...DEFAULT_SETTINGS, ...lsGet("z228.settings", {}) },
  clientId: lsGet("z228.clientId", "") || randomId(16),
  nick: lsGet("z228.nick", ""),
  password: lsGet("z228.password", ""),
  adminToken: lsGet("z228.adminToken", ""),
  ws: null,
  wsWanted: false,
  reconnectDelay: 1000,
  ready: false,
  me: null, // {id, nickname}
  admin: false,
  server: { name: "z228" },
  maxUploadBytes: 25 << 20,
  channels: [],
  users: [],
  view: "text", // "text" | "voice"
  textId: lsGet("z228.lastChannel", 0),
  stageVoiceId: 0,
  msgs: new Map(), // channelId -> {list, hasMore, loading, loaded}
  lastRead: lsGet("z228.read", {}),
  unread: new Map(), // channelId -> {count, mentions}
  typing: new Map(), // channelId -> Map(nick -> timeoutId)
  lastTypingSent: 0,
  replyTo: null,
  pending: [], // загружаемые вложения
  editingId: 0,
  volumes: lsGet("z228.volumes", {}),
  localMutes: lsGet("z228.localMutes", {}),
  muted: lsGet("z228.muted", false),
  deafened: lsGet("z228.deafened", false),
  voiceState: "disconnected",
  voiceChannelId: 0,
  rejoinVoiceId: 0,
  speaking: new Set(),
  ping: 0,
  focusTile: "",
  prevVoiceMembers: null,
  avatars: {}, // ник в нижнем регистре -> url
  quality: {}, // id участника -> {rtt, loss, jitter}
  myStats: null, // подробная статистика своего соединения
};
setAvatarSource((nick) => S.avatars[nick.toLowerCase()] || "");
lsSet("z228.clientId", S.clientId);

const voice = new Voice((type, payload) => send(type, payload), {
  onState: (st) => {
    S.voiceState = st;
    renderVoicePanel();
    renderStage();
  },
  onSpeaking: (set) => {
    S.speaking = set;
    for (const el of $$("[data-speak]")) el.classList.toggle("speaking", set.has(el.dataset.speak));
  },
  onRemoteVideo: () => renderStage(),
  onScreenEnded: () => {
    send("voice-state", { screen: false });
    renderVoicePanel();
    renderStage();
  },
  onError: (msg) => toast(msg, "error"),
  volumeKey: (userId) => {
    const u = userById(userId);
    return u ? u.nickname.toLowerCase() : userId;
  },
});
voice.settings = S.settings;
voice.volumes = S.volumes;
voice.localMutes = S.localMutes;
voice.muted = S.muted;
voice.deafened = S.deafened;

function saveSettings() {
  lsSet("z228.settings", S.settings);
  voice.settings = S.settings;
}

const userById = (id) => S.users.find((u) => u.id === id);
const channelById = (id) => S.channels.find((c) => c.id === id);
const textChannels = () => S.channels.filter((c) => c.kind === "text");
const voiceChannels = () => S.channels.filter((c) => c.kind === "voice");
const voiceMembers = (chId) => S.users.filter((u) => u.voiceChannelId === chId);
const myVoiceChannel = () => (S.me ? (userById(S.me.id) || {}).voiceChannelId || 0 : 0);

// ============================================================ WebSocket

function wsUrl() {
  return (location.protocol === "https:" ? "wss://" : "ws://") + location.host + "/ws";
}

function connect() {
  S.wsWanted = true;
  const ws = new WebSocket(wsUrl());
  S.ws = ws;
  ws.onopen = () => {
    ws.send(JSON.stringify({ type: "hello", nickname: S.nick, clientId: S.clientId, password: S.password, adminToken: S.adminToken }));
  };
  ws.onmessage = (e) => {
    let m;
    try {
      m = JSON.parse(e.data);
    } catch {
      return;
    }
    try {
      onMessage(m);
    } catch (err) {
      console.error("handler", m.type, err);
    }
  };
  ws.onclose = () => {
    if (S.ws !== ws) return;
    S.ws = null;
    const wasReady = S.ready;
    S.ready = false;
    if (voice.active) {
      S.rejoinVoiceId = voice.channelId;
      voice.leave(false);
    }
    if (!S.wsWanted) return;
    banner(wasReady ? "Связь с сервером потеряна. Переподключение…" : "Сервер недоступен. Повторная попытка…", true);
    setTimeout(() => S.wsWanted && connect(), S.reconnectDelay);
    S.reconnectDelay = Math.min(S.reconnectDelay * 2, 10000);
  };
}

function disconnect() {
  S.wsWanted = false;
  if (S.ws) {
    const ws = S.ws;
    S.ws = null;
    ws.close();
  }
  if (voice.active) voice.leave(false);
  S.ready = false;
}

function send(type, payload = {}) {
  if (S.ws && S.ws.readyState === WebSocket.OPEN) {
    S.ws.send(JSON.stringify({ type, ...payload }));
    return true;
  }
  return false;
}

function onMessage(m) {
  switch (m.type) {
    case "ready":
      return onReady(m);
    case "hello-rejected":
      disconnect();
      if (m.reason === "key" || m.reason === "password") {
        S.password = "";
        lsSet("z228.password", "");
      }
      showLogin(m.message, m.reason === "key" || m.reason === "password");
      return;
    case "replaced":
      disconnect();
      showLogin("Вы открыли чат в другой вкладке. Здесь сессия закрыта.");
      return;
    case "kicked":
      disconnect();
      if (m.reason === "key") {
        S.password = "";
        lsSet("z228.password", "");
      }
      showLogin(m.message || "Вас отключили.", m.reason === "key");
      return;
    case "error":
      toast(m.message, "error");
      return;
    case "presence":
      return onPresence(m.users);
    case "quality":
      if (m.quality) S.quality[m.userId] = m.quality;
      else delete S.quality[m.userId];
      updateQualityEls(m.userId);
      return;
    case "channels":
      S.channels = m.channels;
      if (!channelById(S.textId) || channelById(S.textId).kind !== "text") selectText((textChannels()[0] || {}).id);
      renderAll();
      return;
    case "history":
      return onHistory(m);
    case "message":
      return onNewMessage(m.message, m.nonce, m.from);
    case "message-update":
      return onMessageUpdate(m.message);
    case "message-delete":
      return onMessageDelete(m.id, m.channelId);
    case "typing":
      return onTyping(m.channelId, m.nickname);
    case "voice-joined":
      if (m.joinId !== voice.joinId) return;
      S.voiceChannelId = m.channelId;
      renderVoicePanel();
      return;
    case "voice-left":
      if (m.joinId && m.joinId !== voice.joinId) return; // хвост прошлого входа
      if (voice.active) {
        voice.leave(false);
        sfx("leave");
      }
      S.voiceChannelId = 0;
      if (m.message) toast(m.message, "error");
      renderAll();
      return;
    case "rtc-offer":
      voice.onOffer(m.sdp, m.joinId);
      return;
    case "rtc-candidate":
      voice.onCandidate(m.candidate, m.joinId);
      return;
    case "admin":
      S.admin = !!m.ok;
      if (m.ok && m.token) {
        S.invitesRequested = false;
        S.adminToken = m.token;
        lsSet("z228.adminToken", m.token);
        toast("Вы вошли как администратор.", "ok");
      } else if (m.message) {
        toast(m.message, "error");
      } else if (!m.ok) {
        S.adminToken = "";
        lsSet("z228.adminToken", "");
      }
      renderAll();
      if (settingsSection === "admin") renderSettings();
      return;
    case "avatar":
      if (m.url) S.avatars[m.nickname] = m.url;
      else delete S.avatars[m.nickname];
      renderAll();
      if (S.view === "text") renderMessages({});
      if (settingsSection === "profile") renderSettings();
      return;
    case "invites":
      S.invites = m;
      if (settingsSection === "admin") renderSettings();
      return;
    case "pong":
      S.ping = Math.round(performance.now() - m.t);
      renderVoicePanel();
      return;
  }
}

function onReady(m) {
  S.ready = true;
  S.reconnectDelay = 1000;
  S.me = { id: m.sessionId, nickname: m.nickname };
  S.nick = m.nickname;
  lsSet("z228.nick", S.nick);
  if (S.password) lsSet("z228.password", S.password);
  S.admin = !!m.admin;
  S.server = m.server || S.server;
  S.channels = m.channels || [];
  S.avatars = m.avatars || {};
  S.users = m.users || [];
  S.maxUploadBytes = m.maxUploadBytes || S.maxUploadBytes;
  S.msgs.clear();
  S.prevVoiceMembers = null;
  banner(null);
  document.title = S.server.name;
  $("#login").hidden = true;
  $("#app").hidden = false;
  $("#serverName").textContent = S.server.name;
  $("#serverSlogan").textContent = S.server.slogan || "";
  $("#serverSlogan").hidden = !S.server.slogan;

  if (!channelById(S.textId) || channelById(S.textId).kind !== "text") S.textId = (textChannels()[0] || {}).id || 0;
  selectText(S.textId, S.view !== "text");
  renderAll();

  if (S.rejoinVoiceId && channelById(S.rejoinVoiceId)) {
    const id = S.rejoinVoiceId;
    S.rejoinVoiceId = 0;
    joinVoice(id, true);
  }
}

function onPresence(users) {
  S.users = users;
  for (const u of users) {
    if (u.quality) S.quality[u.id] = u.quality;
    else if (!u.voiceChannelId) delete S.quality[u.id];
  }
  const meU = S.me && userById(S.me.id);
  const sm = !!(meU && meU.serverMuted);
  if (sm !== voice.serverMuted) {
    voice.serverMuted = sm;
    voice.applyMicGate();
    toast(sm ? "Администратор заглушил ваш микрофон." : "Администратор снял заглушение.", sm ? "error" : "ok");
    renderUserPanel();
  }
  const mine = myVoiceChannel();
  if (mine) {
    const ids = voiceMembers(mine).map((u) => u.id);
    voice.syncMembers(ids);
    // звуки входа/выхода собеседников в нашем канале
    if (S.prevVoiceMembers && S.prevVoiceMembers.channel === mine) {
      const prev = S.prevVoiceMembers.ids;
      if (ids.some((id) => !prev.includes(id) && id !== S.me.id)) playSfx("join");
      else if (prev.some((id) => !ids.includes(id) && id !== S.me.id)) playSfx("leave");
    }
    S.prevVoiceMembers = { channel: mine, ids };
  } else {
    S.prevVoiceMembers = null;
  }
  renderChannels();
  renderMembers();
  renderStage();
  renderVoicePanel();
}

// ============================================================ сообщения

function chStore(id) {
  let s = S.msgs.get(id);
  if (!s) {
    s = { list: [], hasMore: true, loading: false, loaded: false };
    S.msgs.set(id, s);
  }
  return s;
}

function requestHistory(channelId, before = 0) {
  const st = chStore(channelId);
  if (st.loading) return;
  st.loading = true;
  send("history", { channelId, before });
}

function onHistory(m) {
  const st = chStore(m.channelId);
  st.loading = false;
  st.hasMore = m.hasMore;
  if (!m.before) {
    const pending = st.list.filter((x) => x.pending);
    st.list = [...m.messages, ...pending];
    st.loaded = true;
  } else {
    const have = new Set(st.list.map((x) => x.id));
    st.list = [...m.messages.filter((x) => !have.has(x.id)), ...st.list];
  }
  if (m.channelId === S.textId) {
    renderMessages({ keepOffset: !!m.before, toBottom: !m.before });
    markRead(m.channelId);
  }
}

function onNewMessage(msg, nonce, from) {
  const st = chStore(msg.channelId);
  if (st.loaded) {
    const i = nonce && from === S.me.id ? st.list.findIndex((x) => x.pending && x.nonce === nonce) : -1;
    if (i >= 0) st.list.splice(i, 1);
    if (!st.list.some((x) => x.id === msg.id)) st.list.push(msg);
  }
  const mine = msg.author === S.me.nickname;
  const visible = msg.channelId === S.textId && S.view === "text" && !document.hidden;
  if (!mine) clearTyping(msg.channelId, msg.author);
  if (msg.channelId === S.textId && st.loaded) {
    const atBottom = isAtBottom();
    renderMessages({ toBottom: atBottom || mine });
  }
  if (visible || mine) {
    markRead(msg.channelId, msg.id);
  } else {
    const u = S.unread.get(msg.channelId) || { count: 0, mentions: 0 };
    u.count++;
    const mention = mentionsMe(msg.text, S.me.nickname) || (msg.replyTo && msg.replyTo.author === S.me.nickname);
    if (mention) {
      u.mentions++;
      playSfx("message");
      notify(msg);
    }
    S.unread.set(msg.channelId, u);
    renderChannels();
    updateTitle();
  }
}

function onMessageUpdate(msg) {
  const st = S.msgs.get(msg.channelId);
  if (!st) return;
  const i = st.list.findIndex((x) => x.id === msg.id);
  if (i < 0) return;
  st.list[i] = msg;
  if (msg.channelId !== S.textId) return;
  const el = $(`.msg[data-id="${msg.id}"]`);
  if (el && S.editingId !== msg.id) {
    const atBottom = isAtBottom();
    el.replaceWith(messageEl(msg, st.list[i - 1]));
    if (atBottom) scrollToBottom();
  }
}

function onMessageDelete(id, channelId) {
  const st = S.msgs.get(channelId);
  if (!st) return;
  st.list = st.list.filter((x) => x.id !== id);
  if (S.replyTo && S.replyTo.id === id) setReply(null);
  if (channelId === S.textId) renderMessages({});
}

function markRead(channelId, lastId) {
  const st = S.msgs.get(channelId);
  const id = lastId || (st && st.list.length ? st.list[st.list.length - 1].id : 0);
  if (id && typeof id === "number") {
    S.lastRead[channelId] = Math.max(S.lastRead[channelId] || 0, id);
    lsSet("z228.read", S.lastRead);
  }
  if (S.unread.has(channelId)) {
    S.unread.delete(channelId);
    renderChannels();
    updateTitle();
  }
}

function updateTitle() {
  let mentions = 0;
  let any = false;
  for (const u of S.unread.values()) {
    mentions += u.mentions;
    if (u.count) any = true;
  }
  document.title = (mentions ? `(${mentions}) ` : any ? "• " : "") + S.server.name;
}

function onTyping(channelId, nick) {
  if (!S.typing.has(channelId)) S.typing.set(channelId, new Map());
  const m = S.typing.get(channelId);
  clearTimeout(m.get(nick));
  m.set(nick, setTimeout(() => clearTyping(channelId, nick), 6000));
  if (channelId === S.textId) renderTyping();
}

function clearTyping(channelId, nick) {
  const m = S.typing.get(channelId);
  if (!m || !m.has(nick)) return;
  clearTimeout(m.get(nick));
  m.delete(nick);
  if (channelId === S.textId) renderTyping();
}

function notify(msg) {
  if (!S.settings.notifications || !document.hidden || !("Notification" in window) || Notification.permission !== "granted") return;
  const ch = channelById(msg.channelId);
  try {
    const n = new Notification(`${msg.author} (#${ch ? ch.name : ""})`, { body: msg.text || "📎 Вложение", icon: "/logo.png", tag: "z228-" + msg.channelId });
    n.onclick = () => {
      window.focus();
      selectText(msg.channelId);
      n.close();
    };
  } catch {}
}

function playSfx(kind) {
  if (S.settings.sounds) sfx(kind);
}

// ============================================================ навигация

function selectText(id, keepView = false) {
  if (!id) return;
  S.textId = id;
  lsSet("z228.lastChannel", id);
  if (!keepView) S.view = "text";
  S.editingId = 0;
  setReply(null);
  const st = chStore(id);
  if (!st.loaded) requestHistory(id);
  closeMobileSidebar();
  renderAll();
  if (S.view === "text") {
    renderMessages({ toBottom: true });
    markRead(id);
    if (!matchMedia("(max-width: 760px)").matches) $("#composerInput").focus();
  }
}

function openStage(id) {
  S.view = "voice";
  S.stageVoiceId = id;
  closeMobileSidebar();
  renderAll();
}

async function joinVoice(id, silent = false) {
  if (myVoiceChannel() === id && voice.active) {
    openStage(id);
    return;
  }
  openStage(id);
  if (!rtcConstructor()) {
    send("client-diag", { text: JSON.stringify(rtcDiagnostics()) });
    toast("В этом браузере отключён WebRTC (часто это делают расширения-блокировщики или VPN-расширения). Отключите их для этого сайта или откройте чат в другом браузере.", "error");
    return;
  }
  try {
    await voice.join(id, S.me.id);
  } catch (e) {
    console.error("voice join", e);
    send("client-diag", { text: JSON.stringify({ error: String(e && (e.stack || e.message || e)), ...rtcDiagnostics() }) });
    toast("Не удалось подключиться к голосу: " + (e.message || e.name), "error");
    voice.leave(false);
    renderAll();
    return;
  }
  if (!silent) playSfx("join");
  send("voice-state", { muted: S.muted, deafened: S.deafened, screen: false });
}

function leaveVoice() {
  if (!voice.active) return;
  voice.leave(true);
  playSfx("leave");
  S.voiceChannelId = 0;
  if (S.view === "voice") S.view = "text";
  renderAll();
}

function toggleMute() {
  if (S.deafened) {
    // как в Discord: включение микрофона снимает и «оглушение»
    S.deafened = false;
    S.muted = false;
    voice.setDeafened(false);
  } else {
    S.muted = !S.muted;
  }
  voice.setMuted(S.muted);
  lsSet("z228.muted", S.muted);
  lsSet("z228.deafened", S.deafened);
  playSfx(S.muted ? "mute" : "unmute");
  send("voice-state", { muted: S.muted, deafened: S.deafened });
  renderUserPanel();
  renderStage();
}

function toggleDeafen() {
  S.deafened = !S.deafened;
  voice.setDeafened(S.deafened);
  lsSet("z228.deafened", S.deafened);
  playSfx(S.deafened ? "deafen" : "undeafen");
  send("voice-state", { muted: S.muted, deafened: S.deafened });
  renderUserPanel();
  renderStage();
}

async function toggleScreen() {
  if (!voice.active) {
    toast("Сначала подключитесь к голосовому каналу.");
    return;
  }
  if (voice.screenStream) {
    voice.stopScreen(true);
    send("voice-state", { screen: false });
  } else {
    const ok = await voice.startScreen(S.settings.screenQuality);
    if (ok) send("voice-state", { screen: true });
  }
  renderVoicePanel();
  renderStage();
}

// ============================================================ рендер: каналы

function renderAll() {
  renderChannels();
  renderHeader();
  renderUserPanel();
  renderVoicePanel();
  renderMembers();
  $("#chatView").hidden = S.view !== "text";
  $("#stageView").hidden = S.view !== "voice";
  renderStage();
  renderComposerState();
}

function renderChannels() {
  const root = $("#channels");
  root.innerHTML = "";
  const addBtn = (kind) =>
    S.admin ? h("button.icon-btn", { title: "Создать канал", html: icon("plus", 16), onclick: (e) => (e.stopPropagation(), createChannelDialog(kind)) }) : null;

  root.append(h("div.category", null, h("span", null, "Текстовые каналы"), addBtn("text")));
  for (const ch of textChannels()) {
    const u = S.unread.get(ch.id);
    const sel = S.view === "text" && S.textId === ch.id;
    root.append(
      h(
        "button.channel" + (sel ? ".selected" : "") + (u && u.count && !sel ? ".unread" : ""),
        {
          onclick: () => selectText(ch.id),
          oncontextmenu: (e) => channelMenu(e, ch),
        },
        h("span", { html: icon("hash", 20) }),
        h("span.name", null, ch.name),
        u && u.mentions ? h("span.badge", null, u.mentions) : null
      )
    );
  }

  root.append(h("div.category", null, h("span", null, "Голосовые каналы"), addBtn("voice")));
  const mine = myVoiceChannel();
  for (const ch of voiceChannels()) {
    const sel = S.view === "voice" && S.stageVoiceId === ch.id;
    root.append(
      h(
        "button.channel" + (sel ? ".selected" : "") + (mine === ch.id ? ".unread" : ""),
        {
          onclick: () => joinVoice(ch.id),
          oncontextmenu: (e) => channelMenu(e, ch),
          title: "Нажмите, чтобы подключиться",
        },
        h("span", { html: icon("speaker", 20) }),
        h("span.name", null, ch.name)
      )
    );
    const members = voiceMembers(ch.id);
    if (members.length) {
      const ul = h("ul.voice-users");
      for (const u of members) {
        ul.append(
          h(
            "li.voice-user",
            { onclick: (e) => userMenu(e, u), oncontextmenu: (e) => userMenu(e, u) },
            avatarFor(u, 24),
            h("span.name", null, u.nickname),
            h(
              "span.flags",
              null,
              u.screen ? h("span.live-badge", null, "В ЭФИРЕ") : null,
              u.serverMuted ? h("span", { html: icon("micOff", 16), title: "Заглушён администратором", style: { color: "var(--danger)" } }) : null,
              u.deafened ? h("span", { html: icon("headphonesOff", 16), title: "Звук выключен" }) : u.muted && !u.serverMuted ? h("span", { html: icon("micOff", 16), title: "Микрофон выключен" }) : null,
              S.localMutes[u.nickname.toLowerCase()] ? h("span", { html: icon("speaker", 16), title: "Заглушён вами", style: { opacity: 0.5 } }) : null,
              qualityEl(u.id)
            )
          )
        );
      }
      root.append(ul);
    }
  }
}

function avatarFor(u, size) {
  const a = avatar(u.nickname, size);
  a.dataset.speak = u.id;
  if (S.speaking.has(u.id)) a.classList.add("speaking");
  return a;
}

function renderHeader() {
  if (S.view === "voice") {
    const ch = channelById(S.stageVoiceId);
    $("#headerIcon").innerHTML = icon("speaker", 22);
    $("#headerTitle").textContent = ch ? ch.name : "";
    $("#headerTopic").textContent = "";
  } else {
    const ch = channelById(S.textId);
    $("#headerIcon").innerHTML = icon("hash", 22);
    $("#headerTitle").textContent = ch ? ch.name : "";
    $("#headerTopic").textContent = "";
  }
}

function renderUserPanel() {
  if (!S.me) return;
  const meAv = $("#meAvatar");
  meAv.innerHTML = "";
  const wrap = h("span.avatar-wrap", null, avatarFor({ id: S.me.id, nickname: S.me.nickname }, 32), h("span.status-dot"));
  meAv.append(wrap);
  $("#meName").textContent = S.me.nickname;
  $("#meStatus").textContent = S.admin ? "Админ · в сети" : "В сети";
  const micOff = S.muted || S.deafened || voice.serverMuted;
  $("#micBtn").innerHTML = icon(micOff ? "micOff" : "mic", 20);
  $("#micBtn").classList.toggle("off", micOff);
  $("#micBtn").title = micOff ? "Включить микрофон (Ctrl+Shift+M)" : "Выключить микрофон (Ctrl+Shift+M)";
  $("#deafBtn").innerHTML = icon(S.deafened ? "headphonesOff" : "headphones", 20);
  $("#deafBtn").classList.toggle("off", S.deafened);
  $("#deafBtn").title = S.deafened ? "Включить звук (Ctrl+Shift+D)" : "Выключить звук (Ctrl+Shift+D)";
  $("#stMic").innerHTML = icon(micOff ? "micOff" : "mic", 24);
  $("#stMic").classList.toggle("off", micOff);
  $("#stDeaf").innerHTML = icon(S.deafened ? "headphonesOff" : "headphones", 24);
  $("#stDeaf").classList.toggle("off", S.deafened);
}

function renderVoicePanel() {
  const panel = $("#voicePanel");
  const chId = voice.active ? voice.channelId : 0;
  panel.hidden = !chId;
  if (!chId) return;
  const ch = channelById(chId);
  const st = $("#voiceStatus");
  const label = {
    connecting: "Подключение к RTC…",
    connected: "Голосовая связь подключена",
    failed: "Нет соединения",
    disconnected: "Отключено",
  }[S.voiceState];
  const lvl = S.voiceState === "connected" ? qualityLevel(S.myStats) : 0;
  st.className =
    "voice-status" +
    (S.voiceState === "failed" ? " bad" : S.voiceState !== "connected" ? "" : lvl === 1 ? " bad" : lvl === 2 ? " warn" : " ok");
  $("#voiceStatusText").textContent = label || S.voiceState;
  st.title = "Нажмите, чтобы посмотреть качество связи";
  const rtt = S.myStats && S.myStats.rtt != null ? S.myStats.rtt : S.ping;
  $("#voiceChannelLink").textContent = (ch ? ch.name : "") + " / " + S.server.name + (rtt && S.voiceState === "connected" ? ` · ${rtt} мс` : "");
  $("#vpScreen").classList.toggle("active", !!voice.screenStream);
  $("#stScreen").classList.toggle("active", !!voice.screenStream);
}

function renderMembers() {
  const root = $("#members");
  root.innerHTML = "";
  const admins = S.users.filter((u) => u.admin);
  const rest = S.users.filter((u) => !u.admin);
  const section = (title, list) => {
    if (!list.length) return;
    root.append(h("div.category", null, h("span", null, `${title} — ${list.length}`)));
    for (const u of list) {
      const ch = u.voiceChannelId ? channelById(u.voiceChannelId) : null;
      root.append(
        h(
          "div.member",
          { onclick: (e) => userMenu(e, u), oncontextmenu: (e) => userMenu(e, u) },
          h("span.avatar-wrap", null, avatarFor(u, 32), h("span.status-dot")),
          h(
            "span.name",
            null,
            u.nickname,
            ch ? h("span.sub", null, (u.screen ? "🔴 Стримит в " : "🔊 ") + ch.name) : null
          ),
          u.admin ? h("span.crown", { html: icon("crown", 14), title: "Администратор" }) : null
        )
      );
    }
  };
  section("Администраторы", admins);
  section("В сети", rest);
}

// ---------- индикатор качества связи ----------

const BARS = "<i></i><i></i><i></i>";

function qualityText(q) {
  if (!q) return "Нет данных о связи";
  const lvl = qualityLevel({ rtt: q.rtt, downLoss: q.loss });
  const word = ["", "Плохая связь", "Связь так себе", "Хорошая связь"][lvl];
  return `${word}: пинг ${q.rtt} мс, потери ${q.loss}%` + (q.jitter ? `, джиттер ${q.jitter} мс` : "");
}

function qualityEl(userId) {
  const el = h("span.qbars", { dataset: { q: userId } });
  fillQuality(el, S.quality[userId]);
  return el;
}

function fillQuality(el, q) {
  const lvl = q ? qualityLevel({ rtt: q.rtt, downLoss: q.loss }) : 0;
  el.className = "qbars q" + lvl;
  el.hidden = !q;
  el.title = qualityText(q);
  el.innerHTML = BARS;
}

function updateQualityEls(userId) {
  for (const el of $$(`.qbars[data-q="${userId}"]`)) fillQuality(el, S.quality[userId]);
}

let qualityPop = null;
function qualityPopoverOpen() {
  return qualityPop && qualityPop.isConnected;
}

function showQualityDetails(anchor) {
  const st = S.myStats;
  const row = (k, v) => h("div.q-row", null, h("span.muted", null, k), h("b", null, v));
  const fmt = (v, unit) => (v == null ? "—" : v + unit);
  const lvl = qualityLevel(st);
  const body = h(
    "div.q-details",
    null,
    h("div.q-title", null, h("span.qbars.q" + lvl, { html: BARS }), ["Нет данных", "Плохая связь", "Связь так себе", "Хорошая связь"][lvl]),
    row("Пинг до сервера", fmt(st && st.rtt, " мс")),
    row("Потери (приём)", fmt(st && st.downLoss, "%")),
    row("Потери (передача)", fmt(st && st.upLoss, "%")),
    row("Джиттер", fmt(st && st.jitter, " мс")),
    row("Входящий поток", fmt(st && st.kbpsIn, " кбит/с")),
    row("Исходящий поток", fmt(st && st.kbpsOut, " кбит/с")),
    row("Соединение", st && st.proto ? st.proto.toUpperCase() + (st.proto === "tcp" ? " (запасной путь, UDP закрыт)" : "") : "—"),
    h("p.muted.small", { style: { margin: "8px 0 0" } }, "Обновляется каждые 3 секунды. Полоски у участников показывают, какая связь у них.")
  );
  if (qualityPopoverOpen() && !anchor) {
    qualityPop.firstChild.replaceWith(body);
    return;
  }
  qualityPop = popover(body, anchor || $("#voiceStatus"), { side: "top" });
}

// ============================================================ рендер: чат

const GROUP_MS = 7 * 60 * 1000;

function grouped(m, prev) {
  return prev && !prev.system && prev.author === m.author && !m.replyTo && m.createdAt - prev.createdAt < GROUP_MS && sameDay(prev.createdAt, m.createdAt);
}

function knownNicks() {
  const s = new Set(S.users.map((u) => u.nickname));
  const st = S.msgs.get(S.textId);
  if (st) for (const m of st.list) s.add(m.author);
  return [...s];
}

function messageEl(m, prev) {
  const head = !grouped(m, prev);
  const mine = S.me && m.author === S.me.nickname;
  const mentioned = !mine && (mentionsMe(m.text, S.me.nickname) || (m.replyTo && m.replyTo.author === S.me.nickname));
  const el = h("div.msg" + (head ? ".head" : "") + (mentioned ? ".mentioned" : "") + (m.pending ? ".pending" : "") + (m.replyTo ? ".has-reply" : ""), {
    dataset: { id: m.id },
    oncontextmenu: (e) => !m.pending && messageMenu(e, m),
  });

  if (head) {
    if (m.replyTo) {
      el.append(
        h(
          "div.msg-reply",
          { onclick: () => jumpTo(m.replyTo.id) },
          avatar(m.replyTo.author, 16),
          h("b", null, "@" + m.replyTo.author),
          h("span", { html: m.replyTo.text ? renderMarkdown(m.replyTo.text.split("\n")[0]) : "<i>вложение</i>" })
        )
      );
    }
    const av = avatar(m.author, 40);
    av.onclick = (e) => userMenuByNick(e, m.author);
    el.append(
      av,
      h(
        "div.msg-header",
        null,
        h("span.msg-author", { style: { color: avatarColorText(m.author) }, onclick: (e) => userMenuByNick(e, m.author) }, m.author),
        h("span.msg-time", { title: fmtFull(m.createdAt) }, fmtStamp(m.createdAt))
      )
    );
  } else {
    el.append(h("span.msg-side-time", { title: fmtFull(m.createdAt) }, fmtTime(m.createdAt)));
  }

  if (S.editingId === m.id) {
    el.append(editorEl(m));
  } else if (m.text) {
    const t = h("div.msg-text" + (isEmojiOnly(m.text) ? ".jumbo" : ""), { html: renderMarkdown(m.text, { nicknames: knownNicks(), me: S.me.nickname }) });
    if (m.editedAt) t.append(h("span.msg-edited", { title: fmtFull(m.editedAt) }, "(изменено)"));
    el.append(t);
  }

  if (m.attachments && m.attachments.length) el.append(attachmentsEl(m.attachments));

  const reacts = Object.entries(m.reactions || {}).filter(([, who]) => who.length);
  if (reacts.length) {
    el.append(
      h(
        "div.reactions",
        null,
        reacts.map(([emoji, who]) =>
          h(
            "button.reaction" + (who.includes(S.me.nickname) ? ".mine" : ""),
            { onclick: () => send("react", { id: m.id, emoji }), "data-tip": who.join(", ") + " — " + emoji },
            h("span", null, emoji),
            h("span.count", null, who.length)
          )
        )
      )
    );
  }

  if (!m.pending && S.editingId !== m.id) {
    const actions = h("div.msg-actions");
    for (const e of QUICK_REACTIONS.slice(0, 3)) {
      actions.append(h("button.icon-btn", { title: "Реакция " + e, onclick: () => send("react", { id: m.id, emoji: e }), style: { fontSize: "18px" } }, e));
    }
    actions.append(
      h("button.icon-btn", { title: "Добавить реакцию", html: icon("smile", 20), onclick: (e) => emojiPicker(e.currentTarget, (emoji) => send("react", { id: m.id, emoji }), el) }),
      h("button.icon-btn", { title: "Ответить", html: icon("reply", 20), onclick: () => setReply(m) })
    );
    if (mine) actions.append(h("button.icon-btn", { title: "Изменить", html: icon("edit", 20), onclick: () => startEdit(m.id) }));
    if (mine || S.admin) actions.append(h("button.icon-btn.danger", { title: "Удалить", html: icon("trash", 20), onclick: (e) => confirmDelete(m, e.shiftKey) }));
    el.append(actions);
  }
  return el;
}

function avatarColorText(nick) {
  return avatarColor(nick);
}

function attachmentsEl(list) {
  const box = h("div.attachments");
  for (const a of list) {
    const mime = a.mime || "";
    const ratio = a.width && a.height ? { aspectRatio: `${a.width} / ${a.height}`, width: Math.min(420, a.width) + "px" } : {};
    if (mime.startsWith("image/")) {
      const img = h("img.att-image", { src: a.url, alt: a.name, loading: "lazy", style: ratio });
      img.onclick = () => openLightbox(a);
      img.onload = () => isAtBottom(200) && scrollToBottom();
      box.append(img);
    } else if (mime.startsWith("video/")) {
      box.append(h("video.att-video", { src: a.url, controls: true, preload: "metadata", style: a.width ? ratio : null }));
    } else if (mime.startsWith("audio/")) {
      box.append(h("div.att-file", null, h("span", { html: icon("file", 28) }), h("div", { style: { flex: 1, minWidth: 0 } }, h("div.att-name", null, a.name), h("audio.att-audio", { src: a.url, controls: true, preload: "none" }))));
    } else {
      box.append(
        h(
          "div.att-file",
          null,
          h("span", { html: icon("file", 28) }),
          h("div", { style: { flex: 1, minWidth: 0 } }, h("a.att-name", { href: `${a.url}?download=1&name=${encodeURIComponent(a.name)}`, style: { display: "block" } }, a.name), h("div.att-size", null, fmtSize(a.size || 0))),
          h("a.icon-btn", { href: `${a.url}?download=1&name=${encodeURIComponent(a.name)}`, html: icon("download", 20), title: "Скачать" })
        )
      );
    }
  }
  return box;
}

function renderMessages({ keepOffset = false, toBottom = false } = {}) {
  if (S.view !== "text") return;
  const scroller = $("#messages");
  const inner = $("#messagesInner");
  const st = chStore(S.textId);
  const ch = channelById(S.textId);
  const offsetFromBottom = scroller.scrollHeight - scroller.scrollTop;
  const wasBottom = isAtBottom();

  inner.innerHTML = "";
  if (!st.loaded) {
    inner.append(h("div.load-more", null, "Загрузка…"));
  } else if (st.hasMore) {
    inner.append(h("div.load-more", null, st.loading ? "Загрузка…" : ""));
  } else if (ch) {
    inner.append(
      h(
        "div.chat-welcome",
        null,
        h("div.welcome-icon", { html: icon("hash", 40) }),
        h("h1", null, "Добро пожаловать в #" + ch.name + "!"),
        h("p", null, "Это начало канала #" + ch.name + ".")
      )
    );
  }
  const lastRead = S.lastRead[S.textId] || 0;
  let prev = null;
  for (const m of st.list) {
    if (!prev || !sameDay(prev.createdAt, m.createdAt)) {
      inner.append(h("div.day-divider", null, h("span", null, fmtDay(m.createdAt))));
      prev = null;
    }
    inner.append(messageEl(m, prev));
    prev = m;
  }
  void lastRead;
  if (keepOffset) scroller.scrollTop = scroller.scrollHeight - offsetFromBottom;
  else if (toBottom || wasBottom) scrollToBottom();
  renderTyping();
}

function isAtBottom(slack = 80) {
  const s = $("#messages");
  return s.scrollHeight - s.scrollTop - s.clientHeight < slack;
}

function scrollToBottom() {
  const s = $("#messages");
  s.scrollTop = s.scrollHeight;
}

function jumpTo(id) {
  const el = $(`.msg[data-id="${id}"]`);
  if (!el) {
    toast("Сообщение слишком старое — прокрутите вверх, чтобы найти его.");
    return;
  }
  el.scrollIntoView({ block: "center", behavior: "smooth" });
  el.classList.remove("flash");
  void el.offsetWidth;
  el.classList.add("flash");
}

function renderTyping() {
  const box = $("#typing");
  const m = S.typing.get(S.textId);
  const names = m ? [...m.keys()].filter((n) => n !== S.me.nickname) : [];
  box.innerHTML = "";
  if (!names.length) return;
  const text =
    names.length === 1
      ? [h("b", null, names[0]), " печатает…"]
      : names.length <= 3
        ? [h("b", null, names.slice(0, -1).join(", ")), " и ", h("b", null, names[names.length - 1]), " печатают…"]
        : ["Несколько человек печатают…"];
  box.append(h("span.typing-dots", null, h("i"), h("i"), h("i")), h("span", null, ...text));
}

// ---------- редактирование ----------

function editorEl(m) {
  const ta = h("textarea", { rows: 1 });
  ta.value = m.text;
  const save = () => {
    const text = ta.value.trim();
    if (!text && !(m.attachments || []).length) {
      confirmDelete(m);
      return;
    }
    if (text !== m.text) send("edit", { id: m.id, text });
    stopEdit();
  };
  ta.addEventListener("keydown", (e) => {
    if (e.key === "Enter" && !e.shiftKey) {
      e.preventDefault();
      save();
    } else if (e.key === "Escape") {
      e.preventDefault();
      stopEdit();
    }
  });
  ta.addEventListener("input", () => autosize(ta));
  setTimeout(() => {
    ta.focus();
    ta.setSelectionRange(ta.value.length, ta.value.length);
    autosize(ta);
  });
  return h(
    "div.msg-editor",
    null,
    ta,
    h("div.hint", null, "Escape — ", h("a", { onclick: stopEdit }, "отмена"), " • Enter — ", h("a", { onclick: save }, "сохранить"))
  );
}

function startEdit(id) {
  S.editingId = id;
  renderMessages({});
  const el = $(`.msg[data-id="${id}"]`);
  if (el) el.scrollIntoView({ block: "nearest" });
}

function stopEdit() {
  S.editingId = 0;
  renderMessages({});
  $("#composerInput").focus();
}

function confirmDelete(m, skip) {
  if (skip) {
    send("delete", { id: m.id });
    return;
  }
  modal({
    title: "Удалить сообщение",
    body: [h("p", null, "Вы точно хотите удалить это сообщение?"), h("p.muted.small", null, "Подсказка: удерживайте Shift, чтобы удалять без подтверждения.")],
    okText: "Удалить",
    danger: true,
    onOk: () => send("delete", { id: m.id }),
  });
}

// ============================================================ композер

function renderComposerState() {
  const ch = channelById(S.textId);
  const ta = $("#composerInput");
  ta.placeholder = ch ? `Написать в #${ch.name}` : "Написать…";
}

function setReply(m) {
  S.replyTo = m;
  const bar = $("#replyBar");
  bar.hidden = !m;
  bar.innerHTML = "";
  if (m) {
    bar.append(
      h("span", null, "Ответ ", h("b", null, m.author)),
      h("button.icon-btn", { html: icon("close", 16), onclick: () => setReply(null), title: "Отмена" })
    );
    $("#composerInput").focus();
  }
}

function autosize(ta) {
  ta.style.height = "auto";
  ta.style.height = Math.min(ta.scrollHeight, window.innerHeight * 0.5) + "px";
}

async function submitComposer() {
  const ta = $("#composerInput");
  const text = ta.value.replace(/\s+$/, "");
  if (S.pending.some((p) => p.error)) {
    toast("Уберите файлы, которые не загрузились.", "error");
    return;
  }
  if (!text.trim() && !S.pending.length) return;
  if (S.pending.some((p) => !p.result)) {
    toast("Дождитесь загрузки файлов…");
    await Promise.all(S.pending.map((p) => p.promise.catch(() => {})));
    if (S.pending.some((p) => !p.result)) return;
  }
  const attachments = S.pending.map((p) => ({ ...p.result, width: p.width || 0, height: p.height || 0 }));
  const nonce = randomId(8);
  const channelId = S.textId;
  if (!send("send", { channelId, text, replyTo: S.replyTo ? S.replyTo.id : 0, attachments, nonce })) {
    toast("Нет связи с сервером.", "error");
    return;
  }
  const st = chStore(channelId);
  st.list.push({
    id: "p" + nonce,
    nonce,
    pending: true,
    channelId,
    author: S.me.nickname,
    text,
    createdAt: Date.now(),
    replyTo: S.replyTo ? { id: S.replyTo.id, author: S.replyTo.author, text: S.replyTo.text } : null,
    attachments,
    reactions: {},
  });
  ta.value = "";
  autosize(ta);
  setReply(null);
  for (const p of S.pending) if (p.preview) URL.revokeObjectURL(p.preview);
  S.pending = [];
  renderPending();
  renderMessages({ toBottom: true });
  closeMentionPopup();
}

function addFiles(files) {
  for (const file of files) {
    if (file.size > S.maxUploadBytes) {
      toast(`«${file.name}» больше ${fmtSize(S.maxUploadBytes)}.`, "error");
      continue;
    }
    if (S.pending.length >= 10) {
      toast("Не больше 10 файлов за раз.", "error");
      break;
    }
    const p = { id: randomId(6), file, name: file.name || "file", progress: 0, result: null, error: null };
    if (file.type.startsWith("image/")) p.preview = URL.createObjectURL(file);
    p.promise = (async () => {
      if (p.preview) {
        const dims = await imageSize(p.preview);
        p.width = dims.w;
        p.height = dims.h;
      }
      p.result = await upload(file, (f) => {
        p.progress = f;
        const bar = $(`.pending-file[data-id="${p.id}"] .progress > div`);
        if (bar) bar.style.width = Math.round(f * 100) + "%";
      }, p);
      renderPending();
    })().catch((e) => {
      p.error = e.message;
      renderPending();
      toast(`Не удалось загрузить «${p.name}»: ${e.message}`, "error");
    });
    S.pending.push(p);
  }
  renderPending();
  $("#composerInput").focus();
}

function imageSize(url) {
  return new Promise((res) => {
    const img = new Image();
    img.onload = () => res({ w: img.naturalWidth, h: img.naturalHeight });
    img.onerror = () => res({ w: 0, h: 0 });
    img.src = url;
  });
}

function upload(file, onProgress, holder) {
  return new Promise((resolve, reject) => {
    const xhr = new XMLHttpRequest();
    holder.xhr = xhr;
    xhr.open("POST", "/api/upload");
    xhr.setRequestHeader("X-Session", S.me.id);
    xhr.upload.onprogress = (e) => e.lengthComputable && onProgress(e.loaded / e.total);
    xhr.onload = () => {
      if (xhr.status === 200) {
        try {
          resolve(JSON.parse(xhr.responseText));
        } catch {
          reject(new Error("плохой ответ сервера"));
        }
      } else reject(new Error(xhr.status === 413 ? "файл слишком большой" : "ошибка " + xhr.status));
    };
    xhr.onerror = () => reject(new Error("сеть"));
    xhr.onabort = () => reject(new Error("отменено"));
    const fd = new FormData();
    fd.append("file", file, file.name || "paste.png");
    xhr.send(fd);
  });
}

function renderPending() {
  const box = $("#pendingFiles");
  box.hidden = !S.pending.length;
  box.innerHTML = "";
  for (const p of S.pending) {
    box.append(
      h(
        "div.pending-file",
        { dataset: { id: p.id } },
        h("div.thumb", null, p.preview ? h("img", { src: p.preview, alt: "" }) : h("span", { html: icon("file", 48) })),
        h("div.name", { title: p.name }, p.error ? "⚠ " + p.name : p.name),
        p.result || p.error ? null : h("div.progress", null, h("div", { style: { width: Math.round(p.progress * 100) + "%" } })),
        h("button.icon-btn.remove", {
          html: icon("trash", 18),
          title: "Убрать",
          onclick: () => {
            if (p.xhr && !p.result) p.xhr.abort();
            if (p.preview) URL.revokeObjectURL(p.preview);
            S.pending = S.pending.filter((x) => x !== p);
            renderPending();
          },
        })
      )
    );
  }
}

// ---------- упоминания ----------

let mentionState = null;

function updateMentionPopup() {
  const ta = $("#composerInput");
  const before = ta.value.slice(0, ta.selectionStart);
  const m = before.match(/(?:^|\s)@([^\s@]{0,32})$/);
  if (!m) return closeMentionPopup();
  const q = m[1].toLowerCase();
  const list = knownNicks()
    .filter((n) => n.toLowerCase().startsWith(q) && n !== S.me.nickname)
    .slice(0, 8);
  if ("everyone".startsWith(q)) list.push("everyone");
  if (!list.length) return closeMentionPopup();
  mentionState = { list, sel: 0, start: before.length - m[1].length - 1 };
  drawMentionPopup();
}

function drawMentionPopup() {
  let pop = $("#mentionPopup");
  if (!pop) {
    pop = h("div.mention-popup#mentionPopup");
    $(".composer-wrap").style.position = "relative";
    $(".composer-wrap").append(pop);
  }
  pop.innerHTML = "";
  pop.append(h("div.label", { style: { padding: "0 8px 6px" } }, "Участники"));
  mentionState.list.forEach((n, i) => {
    pop.append(
      h(
        "div.item" + (i === mentionState.sel ? ".sel" : ""),
        { onmousedown: (e) => (e.preventDefault(), applyMention(n)) },
        avatar(n, 24),
        h("span", null, n)
      )
    );
  });
}

function applyMention(nick) {
  const ta = $("#composerInput");
  const end = ta.selectionStart;
  ta.value = ta.value.slice(0, mentionState.start) + "@" + nick + " " + ta.value.slice(end);
  const pos = mentionState.start + nick.length + 2;
  ta.setSelectionRange(pos, pos);
  closeMentionPopup();
  ta.focus();
}

function closeMentionPopup() {
  mentionState = null;
  const pop = $("#mentionPopup");
  if (pop) pop.remove();
}

function bindComposer() {
  const ta = $("#composerInput");
  ta.addEventListener("keydown", (e) => {
    if (mentionState) {
      if (e.key === "ArrowDown" || e.key === "ArrowUp") {
        e.preventDefault();
        const n = mentionState.list.length;
        mentionState.sel = (mentionState.sel + (e.key === "ArrowDown" ? 1 : n - 1)) % n;
        drawMentionPopup();
        return;
      }
      if (e.key === "Enter" || e.key === "Tab") {
        e.preventDefault();
        applyMention(mentionState.list[mentionState.sel]);
        return;
      }
      if (e.key === "Escape") {
        closeMentionPopup();
        return;
      }
    }
    if (e.key === "Enter" && !e.shiftKey && !e.isComposing) {
      e.preventDefault();
      submitComposer();
    } else if (e.key === "Escape" && S.replyTo) {
      setReply(null);
    } else if (e.key === "ArrowUp" && !ta.value) {
      const st = chStore(S.textId);
      const last = [...st.list].reverse().find((m) => m.author === S.me.nickname && !m.pending);
      if (last) {
        e.preventDefault();
        startEdit(last.id);
      }
    }
  });
  ta.addEventListener("input", () => {
    autosize(ta);
    updateMentionPopup();
    const now = Date.now();
    if (ta.value && now - S.lastTypingSent > 3000) {
      S.lastTypingSent = now;
      send("typing", { channelId: S.textId });
    }
  });
  ta.addEventListener("blur", () => setTimeout(closeMentionPopup, 100));
  ta.addEventListener("paste", (e) => {
    const files = [...(e.clipboardData?.files || [])];
    if (files.length) {
      e.preventDefault();
      addFiles(files);
    }
  });
  $("#attachBtn").onclick = () => $("#fileInput").click();
  $("#fileInput").onchange = (e) => {
    addFiles([...e.target.files]);
    e.target.value = "";
  };
  $("#emojiBtn").onclick = (e) =>
    emojiPicker(e.currentTarget, (emoji) => {
      const pos = ta.selectionStart ?? ta.value.length;
      ta.value = ta.value.slice(0, pos) + emoji + ta.value.slice(pos);
      ta.focus();
      ta.setSelectionRange(pos + emoji.length, pos + emoji.length);
    });

  // drag & drop
  const chat = $("#chatView");
  let depth = 0;
  chat.addEventListener("dragenter", (e) => {
    if (![...e.dataTransfer.types].includes("Files")) return;
    depth++;
    $("#dropOverlay").hidden = false;
  });
  chat.addEventListener("dragleave", () => {
    depth = Math.max(0, depth - 1);
    if (!depth) $("#dropOverlay").hidden = true;
  });
  chat.addEventListener("dragover", (e) => e.preventDefault());
  chat.addEventListener("drop", (e) => {
    e.preventDefault();
    depth = 0;
    $("#dropOverlay").hidden = true;
    const files = [...(e.dataTransfer?.files || [])];
    if (files.length) addFiles(files);
  });

  // подгрузка истории
  $("#messages").addEventListener("scroll", () => {
    const s = $("#messages");
    const st = chStore(S.textId);
    if (s.scrollTop < 300 && st.loaded && st.hasMore && !st.loading && st.list.length) {
      const first = st.list.find((m) => typeof m.id === "number");
      if (first) requestHistory(S.textId, first.id);
    }
    if (isAtBottom(20) && !document.hidden) markRead(S.textId);
  });

  // спойлеры
  $("#messages").addEventListener("click", (e) => {
    const sp = e.target.closest(".md-spoiler");
    if (sp) sp.classList.add("shown");
  });
}

// ============================================================ сцена голосового канала

const videoEls = new Map();

function videoFor(key, stream, mirror = false) {
  let v = videoEls.get(key);
  if (!v) {
    v = h("video", { autoplay: true, playsinline: true });
    v.muted = true;
    videoEls.set(key, v);
  }
  if (v.srcObject !== stream) v.srcObject = stream;
  v.style.transform = mirror ? "scaleX(-1)" : "";
  return v;
}

function renderStage() {
  if (S.view !== "voice") return;
  const chId = S.stageVoiceId;
  const ch = channelById(chId);
  const grid = $("#stageGrid");
  const joined = voice.active && voice.channelId === chId;
  const members = voiceMembers(chId);
  renderHeader();

  $("#stageControls").hidden = !joined;
  $("#stageJoin").hidden = joined;
  if (!joined) {
    grid.innerHTML = "";
    $("#stageJoinTitle").textContent = ch ? ch.name : "";
    $("#stageJoinSub").textContent = members.length ? "Сейчас в канале:" : "В канале пока никого нет.";
    const old = $(".stage-join .avatars");
    if (old) old.remove();
    if (members.length) $("#stageJoinSub").after(h("div.avatars", null, members.map((u) => avatar(u.nickname, 48))));
    $("#stageJoinBtn").onclick = () => joinVoice(chId);
    return;
  }

  const tiles = [];
  for (const u of members) {
    const isMe = u.id === S.me.id;
    if (u.screen) {
      const stream = isMe ? voice.screenStream : voice.remoteVideo(u.id);
      const key = "screen:" + u.id;
      const tile = h("div.tile" + (S.focusTile === key ? ".focused" : ""), { dataset: { key }, onclick: () => toggleFocus(key) });
      if (stream) tile.append(videoFor(key, stream));
      else tile.append(h("div.muted", null, "Подключение к трансляции…"));
      tile.append(
        h("div.tile-label", null, h("span.live-badge", null, "В ЭФИРЕ"), h("span", null, isMe ? "Ваш экран" : u.nickname)),
        h(
          "div.tile-actions",
          null,
          h("button.icon-btn", {
            html: icon("expand", 18),
            title: "Во весь экран",
            onclick: (e) => {
              e.stopPropagation();
              const v = tile.querySelector("video");
              (v || tile).requestFullscreen?.();
            },
          })
        )
      );
      tiles.push(tile);
    }
    const key = "user:" + u.id;
    const tile = h("div.tile" + (S.focusTile === key ? ".focused" : ""), { dataset: { key, speak: u.id }, onclick: (e) => userMenu(e, u) });
    if (S.speaking.has(u.id)) tile.classList.add("speaking");
    tile.style.background = `color-mix(in srgb, ${avatarColor(u.nickname)} 35%, #1e1f22)`;
    tile.append(
      avatar(u.nickname, 80),
      h(
        "div.tile-label",
        null,
        u.deafened ? h("span", { html: icon("headphonesOff", 16) }) : u.muted || u.serverMuted ? h("span", { html: icon("micOff", 16) }) : null,
        S.localMutes[u.nickname.toLowerCase()] && !isMe ? h("span", { title: "Заглушён вами" }, "🔇") : null,
        h("span", null, u.nickname + (isMe ? " (вы)" : "")),
        qualityEl(u.id),
        !isMe && (S.volumes[u.nickname.toLowerCase()] ?? 100) !== 100 ? h("span", { style: { opacity: 0.7, fontSize: "12px" } }, (S.volumes[u.nickname.toLowerCase()] ?? 100) + "%") : null
      )
    );
    tiles.push(tile);
  }
  if (S.focusTile && !tiles.some((t) => t.dataset.key === S.focusTile)) S.focusTile = "";

  grid.innerHTML = "";
  grid.classList.toggle("focus", !!S.focusTile);
  if (S.focusTile) {
    const focused = tiles.find((t) => t.dataset.key === S.focusTile);
    grid.append(focused);
    const strip = h("div.stage-strip", null, tiles.filter((t) => t !== focused));
    grid.append(strip);
    for (const t of strip.children) t.classList.add("focused");
  } else {
    grid.append(...tiles);
  }
  layoutStage();
  for (const v of grid.querySelectorAll("video")) v.play().catch(() => {});
  for (const [key, v] of videoEls) if (!v.isConnected) {
    v.srcObject = null;
    videoEls.delete(key);
  }
}

const TILE_GAP = 8;
const TILE_RATIO = 16 / 9;

/**
 * Как в Discord: плитки 16:9, число колонок подбирается так, чтобы плитки были максимально
 * крупными и все влезали в окно. Неполный последний ряд центрируется (flex-wrap).
 */
function layoutStage() {
  const grid = $("#stageGrid");
  if (!grid || grid.offsetParent === null) return;
  const cs = getComputedStyle(grid);
  const W = grid.clientWidth - parseFloat(cs.paddingLeft) - parseFloat(cs.paddingRight);
  let H = grid.clientHeight - parseFloat(cs.paddingTop) - parseFloat(cs.paddingBottom);
  let n = grid.querySelectorAll(":scope > .tile").length;
  if (grid.classList.contains("focus")) {
    const strip = grid.querySelector(".stage-strip");
    if (strip && strip.children.length) H -= strip.offsetHeight + TILE_GAP;
    n = 1;
  }
  if (!n || W <= 0 || H <= 0) return;
  let best = 0;
  for (let cols = 1; cols <= n; cols++) {
    const rows = Math.ceil(n / cols);
    // столбик из плиток выглядит плохо — рядов не больше, чем колонок (3 → 2+1, 4 → 2×2)
    if (rows > cols) continue;
    const w = Math.min((W - TILE_GAP * (cols - 1)) / cols, ((H - TILE_GAP * (rows - 1)) / rows) * TILE_RATIO);
    if (w > best) best = w;
  }
  const w = Math.max(120, Math.floor(best) - 1);
  grid.style.setProperty("--tile-w", w + "px");
  grid.style.setProperty("--tile-h", Math.floor(w / TILE_RATIO) + "px");
  // аватар в маленьких плитках тоже уменьшаем
  grid.style.setProperty("--tile-av", Math.min(1.4, Math.max(0.5, w / 360)).toFixed(2));
}

function toggleFocus(key) {
  S.focusTile = S.focusTile === key ? "" : key;
  renderStage();
}

// ============================================================ всплывающие меню

function closeLayer() {
  $("#layer").innerHTML = "";
  $$(".msg.menu-open").forEach((el) => el.classList.remove("menu-open"));
}

function popover(content, anchor, { side = "right", x, y } = {}) {
  closeLayer();
  const pop = h("div.popover", null, content);
  $("#layer").append(pop);
  const r = anchor ? anchor.getBoundingClientRect() : { left: x, right: x, top: y, bottom: y };
  const pw = pop.offsetWidth;
  const ph = pop.offsetHeight;
  let left = side === "right" ? r.right + 8 : side === "left" ? r.left - pw - 8 : r.left;
  let top = side === "top" ? r.top - ph - 8 : side === "bottom" ? r.bottom + 8 : r.top;
  if (x != null) {
    left = x;
    top = y;
  }
  left = Math.max(8, Math.min(left, innerWidth - pw - 8));
  top = Math.max(8, Math.min(top, innerHeight - ph - 8));
  pop.style.left = left + "px";
  pop.style.top = top + "px";
  return pop;
}

function menuItem(label, onclick, { danger = false, check = null, iconName = null } = {}) {
  return h(
    "button.menu-item" + (danger ? ".danger" : ""),
    {
      onclick: (e) => {
        e.stopPropagation();
        if (check == null) closeLayer();
        onclick(e);
      },
    },
    h("span", null, label),
    check != null ? h("span.menu-check" + (check ? ".on" : "")) : iconName ? h("span", { html: icon(iconName, 18) }) : null
  );
}

function userMenuByNick(e, nick) {
  const u = S.users.find((x) => x.nickname === nick);
  userMenu(e, u || { id: "", nickname: nick, offline: true });
}

function userMenu(e, u) {
  e.preventDefault();
  e.stopPropagation();
  const isMe = S.me && u.id === S.me.id;
  const key = u.nickname.toLowerCase();
  const card = h("div.profile-card");
  card.append(
    h("div.banner-top", { style: { background: avatarColor(u.nickname) } }),
    avatar(u.nickname, 80),
    h(
      "div.body",
      null,
      h("h3", null, u.nickname),
      h(
        "div.sub",
        null,
        u.offline ? "Не в сети" : u.voiceChannelId ? "🔊 " + ((channelById(u.voiceChannelId) || {}).name || "") : "В сети",
        u.admin ? " · администратор" : ""
      )
    )
  );
  const menu = h("div.ctx-menu");
  const inMyVoice = !isMe && !u.offline && u.voiceChannelId && u.voiceChannelId === myVoiceChannel();
  if (inMyVoice) {
    const val = S.volumes[key] ?? 100;
    const label = h("label", null, `Громкость: ${val}%`);
    const slider = h("input", { type: "range", min: 0, max: 200, step: 5, value: val });
    slider.oninput = () => {
      label.textContent = `Громкость: ${slider.value}%`;
      voice.setVolume(key, u.id, +slider.value);
    };
    slider.onchange = () => {
      S.volumes[key] = +slider.value;
      lsSet("z228.volumes", S.volumes);
      renderStage();
    };
    menu.append(h("div.slider-row", null, label, slider));
    if (val !== 100) {
      menu.append(
        menuItem("Сбросить громкость", () => {
          delete S.volumes[key];
          lsSet("z228.volumes", S.volumes);
          voice.setVolume(key, u.id, 100);
          renderStage();
        })
      );
    }
    const muted = !!S.localMutes[key];
    menu.append(
      menuItem(
        "Заглушить",
        () => {
          S.localMutes[key] = !S.localMutes[key];
          lsSet("z228.localMutes", S.localMutes);
          voice.setLocalMute(key, u.id, S.localMutes[key]);
          closeLayer();
          renderChannels();
          renderStage();
        },
        { check: muted }
      )
    );
  }
  if (!isMe) {
    menu.append(
      menuItem("Упомянуть", () => {
        const ta = $("#composerInput");
        if (S.view !== "text") selectText(S.textId);
        ta.value += (ta.value && !ta.value.endsWith(" ") ? " " : "") + "@" + u.nickname + " ";
        ta.focus();
      })
    );
  }
  if (isMe) {
    menu.append(menuItem("Сменить аватарку", () => openSettings("profile"), { iconName: "edit" }));
    menu.append(menuItem("Настройки", () => openSettings("voice"), { iconName: "gear" }));
  }
  if (S.admin && !isMe && !u.offline) {
    menu.append(h("div.menu-sep"));
    menu.append(menuItem("Заглушить для всех", () => (send("server-mute", { sessionId: u.id, value: !u.serverMuted }), closeLayer()), { check: !!u.serverMuted }));
    if (u.voiceChannelId) menu.append(menuItem("Отключить от голоса", () => send("disconnect-voice", { sessionId: u.id }), { danger: true }));
    menu.append(
      menuItem(
        "Выгнать " + u.nickname,
        () =>
          modal({
            title: "Выгнать " + u.nickname,
            body: h("p", null, "Пользователь будет отключён от сервера (сможет зайти снова)."),
            okText: "Выгнать",
            danger: true,
            onOk: () => send("kick", { sessionId: u.id }),
          }),
        { danger: true }
      )
    );
  }
  if (menu.children.length) card.append(h("div", { style: { padding: "0 8px 8px" } }, menu));
  const anchor = e.currentTarget instanceof Element ? e.currentTarget : null;
  popover(card, anchor, { side: anchor && anchor.closest(".members") ? "left" : "right" });
}

function channelMenu(e, ch) {
  if (!S.admin) return;
  e.preventDefault();
  const menu = h(
    "div.ctx-menu",
    null,
    menuItem("Переименовать", () => renameChannelDialog(ch), { iconName: "edit" }),
    menuItem("Выше", () => send("channel-move", { id: ch.id, dir: -1 }), { iconName: "up" }),
    menuItem("Ниже", () => send("channel-move", { id: ch.id, dir: 1 }), { iconName: "down" }),
    h("div.menu-sep"),
    menuItem("Удалить канал", () => deleteChannelDialog(ch), { danger: true, iconName: "trash" })
  );
  popover(menu, null, { x: e.clientX, y: e.clientY });
}

function messageMenu(e, m) {
  e.preventDefault();
  const mine = m.author === S.me.nickname;
  const el = e.currentTarget;
  el.classList.add("menu-open");
  const quick = h(
    "div",
    { style: { display: "flex", gap: "2px", padding: "4px 4px 6px" } },
    QUICK_REACTIONS.slice(0, 6).map((emoji) =>
      h("button.icon-btn", { style: { fontSize: "20px", width: "34px", height: "34px" }, onclick: () => (send("react", { id: m.id, emoji }), closeLayer()) }, emoji)
    )
  );
  const menu = h("div.ctx-menu", null, quick);
  menu.append(menuItem("Ответить", () => setReply(m), { iconName: "reply" }));
  if (m.text) menu.append(menuItem("Копировать текст", () => navigator.clipboard?.writeText(m.text), { iconName: "file" }));
  if (mine) menu.append(menuItem("Изменить", () => startEdit(m.id), { iconName: "edit" }));
  if (mine || S.admin) menu.append(h("div.menu-sep"), menuItem("Удалить сообщение", () => confirmDelete(m, e.shiftKey), { danger: true, iconName: "trash" }));
  popover(menu, null, { x: e.clientX, y: e.clientY });
}

function emojiPicker(anchor, onPick, msgEl) {
  if (msgEl) msgEl.classList.add("menu-open");
  const grid = h("div.emoji-grid");
  const search = h("input.input.search", { placeholder: "Найти эмодзи" });
  const draw = (q) => {
    grid.innerHTML = "";
    for (const g of EMOJI_GROUPS) {
      const items = q ? g.items.filter(([, words]) => words.includes(q)) : g.items;
      if (!items.length) continue;
      grid.append(h("div.emoji-cat", null, g.name));
      for (const [e, words] of items) {
        grid.append(
          h(
            "button",
            {
              title: words.split(" ")[0],
              onclick: () => {
                onPick(e);
                closeLayer();
              },
            },
            e
          )
        );
      }
    }
  };
  search.oninput = () => draw(search.value.trim().toLowerCase());
  draw("");
  const pop = popover(h("div.emoji-picker", null, search, grid), anchor, { side: "top" });
  pop.addEventListener("click", (e) => e.stopPropagation());
  setTimeout(() => search.focus());
}

function openLightbox(a) {
  const lb = $("#lightbox");
  lb.innerHTML = "";
  lb.append(h("div", { style: { textAlign: "center" } }, h("img", { src: a.url, alt: a.name }), h("a", { href: a.url, target: "_blank", rel: "noopener", onclick: (e) => e.stopPropagation() }, "Открыть оригинал")));
  lb.hidden = false;
  lb.onclick = () => (lb.hidden = true);
}

// ============================================================ модалки

function modal({ title, body, okText = "Готово", cancelText = "Отмена", danger = false, onOk, init }) {
  const back = h("div.modal-backdrop");
  const close = () => back.remove();
  const ok = h("button.btn" + (danger ? ".btn-danger" : ".btn-primary"), {
    onclick: () => {
      if (onOk && onOk(back) === false) return;
      close();
    },
  }, okText);
  const box = h(
    "div.modal",
    { onclick: (e) => e.stopPropagation() },
    h("div.modal-head", null, h("h2", null, title)),
    h("div.modal-body", null, body),
    h("div.modal-foot", null, cancelText ? h("button.btn.btn-link", { onclick: close }, cancelText) : null, ok)
  );
  back.append(box);
  back.onclick = close;
  back.addEventListener("keydown", (e) => {
    if (e.key === "Escape") close();
    if (e.key === "Enter" && e.target.tagName === "INPUT") ok.click();
  });
  document.body.append(back);
  if (init) init(box);
  const inp = box.querySelector("input");
  (inp || ok).focus();
  return back;
}

function createChannelDialog(kind) {
  let k = kind;
  const name = h("input", { placeholder: k === "text" ? "новый-канал" : "Новый канал", maxlength: 50 });
  const rowT = h("div.radio-row", { onclick: () => pick("text") }, h("span", { html: icon("hash", 22) }), h("div", null, h("b", null, "Текстовый"), h("div.muted.small", null, "Сообщения, картинки, файлы")));
  const rowV = h("div.radio-row", { onclick: () => pick("voice") }, h("span", { html: icon("speaker", 22) }), h("div", null, h("b", null, "Голосовой"), h("div.muted.small", null, "Голос и демонстрация экрана")));
  const pick = (x) => {
    k = x;
    rowT.classList.toggle("sel", x === "text");
    rowV.classList.toggle("sel", x === "voice");
  };
  pick(k);
  modal({
    title: "Создать канал",
    body: [h("div.label", { style: { marginBottom: "8px" } }, "Тип канала"), rowT, rowV, h("label.field", { style: { marginTop: "16px" } }, h("span", null, "Название"), name)],
    okText: "Создать",
    onOk: () => {
      if (!name.value.trim()) return false;
      send("channel-create", { kind: k, name: name.value });
    },
  });
}

function renameChannelDialog(ch) {
  const name = h("input", { value: ch.name, maxlength: 50 });
  modal({
    title: "Переименовать канал",
    body: h("label.field", null, h("span", null, "Название"), name),
    okText: "Сохранить",
    onOk: () => {
      if (!name.value.trim()) return false;
      send("channel-rename", { id: ch.id, name: name.value });
    },
  });
}

function deleteChannelDialog(ch) {
  modal({
    title: "Удалить канал",
    body: h("p", null, `Удалить ${ch.kind === "text" ? "#" : "🔊 "}${ch.name}? `, ch.kind === "text" ? "Все сообщения канала будут удалены." : ""),
    okText: "Удалить канал",
    danger: true,
    onOk: () => send("channel-delete", { id: ch.id }),
  });
}

// ============================================================ настройки

let settingsSection = "voice";
let settingsEl = null;
let stopMicTest = null;

function openSettings(section = "voice") {
  closeLayer();
  settingsSection = section;
  if (!settingsEl) {
    settingsEl = h("div.settings");
    document.body.append(settingsEl);
  }
  renderSettings();
}

function closeSettings() {
  S.invitesRequested = false;
  if (stopMicTest) stopMicTest();
  stopMicTest = null;
  if (settingsEl) settingsEl.remove();
  settingsEl = null;
}

function toggleRow(title, desc, value, onChange) {
  const sw = h("button.switch" + (value ? ".on" : ""), { role: "switch", "aria-checked": String(!!value) });
  sw.onclick = () => {
    const v = !sw.classList.contains("on");
    sw.classList.toggle("on", v);
    sw.setAttribute("aria-checked", String(v));
    onChange(v);
  };
  return h("div.toggle-row", null, h("div", null, h("div.t", null, title), desc ? h("div.d", null, desc) : null), sw);
}

function select(options, value, onChange) {
  const sel = h("select.input");
  for (const [v, label] of options) sel.append(h("option", { value: v, selected: v === value }, label));
  sel.onchange = () => onChange(sel.value);
  return sel;
}

function renderSettings() {
  if (!settingsEl) return;
  if (stopMicTest) {
    stopMicTest();
    stopMicTest = null;
  }
  settingsEl.innerHTML = "";
  const navBtn = (id, label, cls = "") =>
    h("button" + (settingsSection === id ? ".sel" : "") + cls, { onclick: () => ((settingsSection = id), renderSettings()) }, label);
  const nav = h(
    "div.settings-nav",
    null,
    h(
      "div.settings-nav-inner",
      null,
      h("div.category", null, "Настройки приложения"),
      navBtn("profile", "Профиль"),
      navBtn("voice", "Голос и видео"),
      navBtn("look", "Внешний вид"),
      navBtn("notify", "Уведомления"),
      navBtn("keys", "Горячие клавиши"),
      h("div.category", null, "Сервер"),
      navBtn("admin", "Администрирование"),
      navBtn("donate", "Поддержать"),
      h("div.menu-sep"),
      h("button.danger", { onclick: logout }, "Выйти")
    )
  );
  const content = h("div.settings-content-inner");
  const wrap = h(
    "div.settings-content",
    null,
    content,
    h("div.settings-close", null, h("button.icon-btn", { html: icon("close", 18), onclick: closeSettings }), "ESC")
  );
  settingsEl.append(nav, wrap);
  ({ profile: settingsProfile, voice: settingsVoice, look: settingsLook, notify: settingsNotify, keys: settingsKeys, admin: settingsAdmin, donate: settingsDonate })[settingsSection](content);
}

async function settingsVoice(c) {
  const s = S.settings;
  c.append(h("h2", null, "Голос и видео"));
  const inSel = h("div");
  const outSel = h("div");
  c.append(h("div.grid2", null, h("label.field", null, h("span", null, "Устройство ввода"), inSel), h("label.field", null, h("span", null, "Устройство вывода"), outSel)));

  const fillDevices = async () => {
    let devs = [];
    try {
      devs = await navigator.mediaDevices.enumerateDevices();
    } catch {}
    const ins = [["", "По умолчанию"], ...devs.filter((d) => d.kind === "audioinput" && d.deviceId && d.deviceId !== "default").map((d, i) => [d.deviceId, d.label || `Микрофон ${i + 1}`])];
    const outs = [["", "По умолчанию"], ...devs.filter((d) => d.kind === "audiooutput" && d.deviceId && d.deviceId !== "default").map((d, i) => [d.deviceId, d.label || `Динамики ${i + 1}`])];
    inSel.innerHTML = "";
    outSel.innerHTML = "";
    inSel.append(
      select(ins, s.inputDevice, (v) => {
        s.inputDevice = v;
        saveSettings();
        voice.restartMic();
      })
    );
    const outSelect = select(outs, s.outputDevice, (v) => {
      s.outputDevice = v;
      saveSettings();
      voice.setOutputDevice(v);
    });
    if (!("setSinkId" in HTMLMediaElement.prototype)) {
      outSelect.disabled = true;
      outSelect.title = "Браузер не поддерживает выбор устройства вывода";
    }
    outSel.append(outSelect);
  };
  fillDevices();

  const volLabel = h("span", null, `Громкость звука: ${s.outputVolume}%`);
  const vol = h("input", { type: "range", min: 0, max: 200, step: 5, value: s.outputVolume });
  vol.oninput = () => {
    s.outputVolume = +vol.value;
    volLabel.textContent = `Громкость звука: ${s.outputVolume}%`;
    voice.settings = s;
    voice.applyAllVolumes();
  };
  vol.onchange = saveSettings;
  c.append(h("label.field", null, volLabel, vol));

  c.append(h("h3", null, "Проверка микрофона"));
  const meter = h("div.meter", null, h("div"));
  const testBtn = h("button.btn.btn-primary", null, "Проверить");
  testBtn.onclick = async () => {
    if (stopMicTest) {
      stopMicTest();
      stopMicTest = null;
      testBtn.textContent = "Проверить";
      meter.firstChild.style.width = "0";
      return;
    }
    try {
      stopMicTest = await micTest(voice.micConstraints(), (lvl) => (meter.firstChild.style.width = Math.round(lvl * 100) + "%"), S.settings.rnnoise);
      testBtn.textContent = "Остановить";
      fillDevices(); // после разрешения появляются названия устройств
    } catch (e) {
      toast("Нет доступа к микрофону: " + e.message, "error");
    }
  };
  c.append(h("div", { style: { display: "flex", gap: "16px", alignItems: "center" } }, testBtn, meter));
  if (!window.isSecureContext) {
    c.append(h("p.info-box", { style: { marginTop: "16px", borderLeftColor: "var(--danger)" } }, "Страница открыта по HTTP не с localhost — браузер не даст доступ к микрофону. Откройте адрес с https://."));
  }

  c.append(h("h3", null, "Режим ввода"));
  c.append(
    toggleRow("Режим рации (Push-to-Talk)", "Микрофон работает, только пока зажата клавиша. Работает, когда окно чата активно.", s.pushToTalk, (v) => {
      s.pushToTalk = v;
      saveSettings();
      voice.applyMicGate();
      renderSettings();
    })
  );
  if (s.pushToTalk) {
    const kb = h("button.keybind", null, s.pttKey ? keyLabel(s.pttKey) : "Не назначена");
    kb.onclick = () => {
      kb.classList.add("recording");
      kb.textContent = "Нажмите клавишу…";
      const onKey = (e) => {
        e.preventDefault();
        e.stopPropagation();
        window.removeEventListener("keydown", onKey, true);
        kb.classList.remove("recording");
        if (e.code !== "Escape") s.pttKey = e.code;
        kb.textContent = s.pttKey ? keyLabel(s.pttKey) : "Не назначена";
        saveSettings();
      };
      window.addEventListener("keydown", onKey, true);
    };
    c.append(h("div.toggle-row", null, h("div.t", null, "Клавиша рации"), kb));
  }

  c.append(h("h3", null, "Обработка звука"));
  const restart = () => {
    saveSettings();
    voice.restartMic();
  };
  c.append(
    toggleRow(
      "Шумодав RNNoise",
      "Нейросеть убирает клавиатуру, вентилятор, шум улицы — заметно лучше встроенного. Нагрузка на процессор — около процента. Проверьте кнопкой «Проверить» выше.",
      s.rnnoise,
      (v) => {
        s.rnnoise = v;
        restart();
        renderSettings();
      }
    ),
    ...(s.rnnoise
      ? []
      : [toggleRow("Шумоподавление браузера", "Простое встроенное шумоподавление. С RNNoise не нужно.", s.noiseSuppression, (v) => ((s.noiseSuppression = v), restart()))]),
    toggleRow("Эхоподавление", "Нужно, если вы без наушников.", s.echoCancellation, (v) => ((s.echoCancellation = v), restart())),
    toggleRow("Автоматическая регулировка усиления", null, s.autoGainControl, (v) => ((s.autoGainControl = v), restart()))
  );

  c.append(h("h3", null, "Демонстрация экрана"));
  c.append(
    h(
      "label.field",
      null,
      h("span", null, "Качество"),
      select(
        [
          ["720p30", "720p, 30 FPS"],
          ["1080p30", "1080p, 30 FPS"],
          ["1080p60", "1080p, 60 FPS (игры)"],
          ["1440p30", "1440p, 30 FPS"],
        ],
        s.screenQuality,
        (v) => ((s.screenQuality = v), saveSettings())
      )
    )
  );
}

function keyLabel(code) {
  return code.replace(/^Key/, "").replace(/^Digit/, "").replace("Backquote", "`").replace("Space", "Пробел").replace("ControlLeft", "Left Ctrl").replace("ShiftLeft", "Left Shift").replace("AltLeft", "Left Alt");
}

// ---------- профиль и аватарка ----------

function settingsProfile(c) {
  const nick = S.me.nickname;
  const has = !!S.avatars[nick.toLowerCase()];
  const input = h("input", { type: "file", accept: "image/png,image/jpeg,image/gif,image/webp", hidden: true });
  const status = h("div.muted.small", { style: { marginTop: "8px" } });
  input.onchange = async () => {
    const file = input.files[0];
    input.value = "";
    if (!file) return;
    status.textContent = "Загрузка…";
    try {
      const prepared = await prepareAvatar(file);
      const res = await upload(prepared, () => {}, {});
      send("set-avatar", { text: res.url });
      status.textContent = "";
      toast("Аватарка обновлена.", "ok");
    } catch (e) {
      status.textContent = "";
      toast("Не удалось загрузить аватарку: " + e.message, "error");
    }
  };
  const big = avatar(nick, 96);
  big.style.cursor = "pointer";
  big.title = "Сменить аватарку";
  big.onclick = () => input.click();
  c.append(
    h("h2", null, "Профиль"),
    h(
      "div.profile-edit",
      null,
      h("div.profile-edit-banner", { style: { background: avatarColor(nick) } }),
      h(
        "div.profile-edit-body",
        null,
        big,
        h("div", { style: { flex: 1, minWidth: 0 } }, h("div.profile-edit-name", null, nick), h("div.muted.small", null, "Аватарка привязана к нику и видна всем.")),
        h(
          "div",
          { style: { display: "flex", gap: "8px", flexWrap: "wrap" } },
          h("button.btn.btn-primary", { onclick: () => input.click() }, "Сменить аватарку"),
          has ? h("button.btn", { onclick: () => send("set-avatar", { text: "" }) }, "Убрать") : null
        )
      )
    ),
    status,
    input,
    h("p.muted.small", { style: { marginTop: "16px" } }, "Картинка обрежется до квадрата 256×256. GIF до 3 МБ остаётся анимированным. Сменить ник: «Выйти» и войти под новым.")
  );
}

/** Квадратная обрезка до 256px; анимированные GIF небольшого размера оставляем как есть. */
async function prepareAvatar(file) {
  if (!file.type.startsWith("image/")) throw new Error("это не картинка");
  if (file.type === "image/gif" && file.size <= 3 << 20) return file;
  const bmp = await createImageBitmap(file);
  const side = Math.min(bmp.width, bmp.height);
  const canvas = document.createElement("canvas");
  canvas.width = canvas.height = 256;
  canvas.getContext("2d").drawImage(bmp, (bmp.width - side) / 2, (bmp.height - side) / 2, side, side, 0, 0, 256, 256);
  bmp.close?.();
  const blob = await new Promise((res) => canvas.toBlob(res, "image/webp", 0.9));
  if (blob && blob.type === "image/webp") return new File([blob], "avatar.webp", { type: "image/webp" });
  const png = await new Promise((res) => canvas.toBlob(res, "image/png"));
  return new File([png], "avatar.png", { type: "image/png" });
}

function settingsLook(c) {
  c.append(h("h2", null, "Внешний вид"), h("h3", null, "Тема"));
  for (const [id, label] of [
    ["dark", "Тёмная"],
    ["light", "Светлая"],
  ]) {
    c.append(
      h(
        "div.radio-row" + (S.settings.theme === id ? ".sel" : ""),
        {
          onclick: () => {
            S.settings.theme = id;
            saveSettings();
            applyTheme();
            renderSettings();
          },
        },
        h("span.menu-check" + (S.settings.theme === id ? ".on" : "")),
        label
      )
    );
  }
}

function settingsNotify(c) {
  c.append(h("h2", null, "Уведомления"));
  c.append(
    toggleRow("Звуки", "Вход/выход в голосовом канале, микрофон, упоминания.", S.settings.sounds, (v) => ((S.settings.sounds = v), saveSettings())),
    toggleRow("Уведомления на рабочем столе", "Когда вас упоминают, а вкладка свёрнута.", S.settings.notifications, async (v) => {
      if (v && "Notification" in window && Notification.permission !== "granted") {
        const p = await Notification.requestPermission();
        if (p !== "granted") {
          toast("Браузер запретил уведомления.", "error");
          v = false;
          renderSettings();
        }
      }
      S.settings.notifications = v;
      saveSettings();
    })
  );
}

function settingsKeys(c) {
  c.append(h("h2", null, "Горячие клавиши"));
  const rows = [
    ["Ctrl + Shift + M", "Выключить / включить микрофон"],
    ["Ctrl + Shift + D", "Выключить / включить звук"],
    ["Alt + ↑ / ↓", "Предыдущий / следующий текстовый канал"],
    ["↑ (в пустом поле)", "Изменить своё последнее сообщение"],
    ["Shift + Enter", "Новая строка"],
    ["Shift + клик по 🗑", "Удалить без подтверждения"],
    ["Esc", "Закрыть окно / отменить ответ"],
  ];
  for (const [k, d] of rows) c.append(h("div.toggle-row", null, h("div.t", null, d), h("span.keybind", { style: { display: "inline-flex", alignItems: "center" } }, k)));
  c.append(h("h3", null, "Разметка сообщений"));
  c.append(h("div.info-box", { html: "<b>**жирный**</b> · <i>*курсив*</i> · <u>__подчёркнутый__</u> · <s>~~зачёркнутый~~</s> · ||спойлер|| · `код` · ```блок кода``` · &gt; цитата · @ник" }));
}

function settingsAdmin(c) {
  c.append(h("h2", null, "Администрирование"));
  if (!S.admin) {
    const pw = h("input", { type: "password", placeholder: "Пароль администратора" });
    const go = () => send("admin-login", { password: pw.value });
    pw.onkeydown = (e) => e.key === "Enter" && go();
    c.append(
      h("p.muted", null, "Пароль администратора выводится в консоль сервера при первом запуске (или задаётся через ADMIN_PASSWORD / -admin-password)."),
      h("div", { style: { display: "flex", gap: "8px", maxWidth: "420px" } }, pw, h("button.btn.btn-primary", { onclick: go }, "Войти"))
    );
    pw.classList.add("input");
    setTimeout(() => pw.focus());
    return;
  }
  settingsAccess(c);
  c.append(h("h3", null, "Каналы"));
  const list = h("div.admin-list");
  for (const ch of S.channels) {
    list.append(
      h(
        "div.admin-row",
        null,
        h("span", { html: icon(ch.kind === "text" ? "hash" : "speaker", 18) }),
        h("span.name", null, ch.name),
        h("button.icon-btn", { html: icon("up", 18), title: "Выше", onclick: () => send("channel-move", { id: ch.id, dir: -1 }) }),
        h("button.icon-btn", { html: icon("down", 18), title: "Ниже", onclick: () => send("channel-move", { id: ch.id, dir: 1 }) }),
        h("button.icon-btn", { html: icon("edit", 18), title: "Переименовать", onclick: () => renameChannelDialog(ch) }),
        h("button.icon-btn.danger", { html: icon("trash", 18), title: "Удалить", onclick: () => deleteChannelDialog(ch) })
      )
    );
  }
  c.append(
    list,
    h(
      "div",
      { style: { display: "flex", gap: "8px", marginTop: "12px" } },
      h("button.btn", { onclick: () => createChannelDialog("text") }, "+ Текстовый канал"),
      h("button.btn", { onclick: () => createChannelDialog("voice") }, "+ Голосовой канал")
    )
  );
  c.append(h("h3", null, `В сети — ${S.users.length}`));
  const ul = h("div.admin-list");
  for (const u of S.users) {
    const isMe = u.id === S.me.id;
    ul.append(
      h(
        "div.admin-row",
        null,
        avatar(u.nickname, 24),
        h("span.name", null, u.nickname, u.voiceChannelId ? h("span.muted.small", null, "  🔊 " + ((channelById(u.voiceChannelId) || {}).name || "")) : null),
        !isMe ? h("button.btn.btn-sm", { onclick: () => send("server-mute", { sessionId: u.id, value: !u.serverMuted }) }, u.serverMuted ? "Снять мьют" : "Мьют") : null,
        !isMe && u.voiceChannelId ? h("button.btn.btn-sm", { onclick: () => send("disconnect-voice", { sessionId: u.id }) }, "Из голоса") : null,
        !isMe ? h("button.btn.btn-sm.btn-danger", { onclick: () => send("kick", { sessionId: u.id }) }, "Выгнать") : null
      )
    );
  }
  c.append(ul, h("div", { style: { marginTop: "24px" } }, h("button.btn", { onclick: () => send("admin-logout") }, "Выйти из режима администратора")));
}

// ---------- ключи доступа ----------

function inviteLink(code) {
  return `${location.origin}/#k=${code}`;
}

async function copyText(text) {
  try {
    await navigator.clipboard.writeText(text);
  } catch {
    const ta = h("textarea", { style: { position: "fixed", opacity: 0 } });
    ta.value = text;
    document.body.append(ta);
    ta.select();
    document.execCommand("copy");
    ta.remove();
  }
  toast("Ссылка скопирована — отправьте её другу.", "ok");
}

function settingsAccess(c) {
  if (!S.invitesRequested) {
    S.invitesRequested = true;
    send("invites");
  }
  c.append(h("h3", null, "Доступ к серверу"));
  const info = S.invites;
  if (!info) {
    c.append(h("p.muted", null, "Загрузка…"));
    return;
  }
  c.append(
    h(
      "div.info-box",
      { style: { borderLeftColor: info.required ? "var(--success)" : "var(--warning)", marginBottom: "12px" } },
      info.required
        ? "🔒 Вход только по ключу. Без ссылки-приглашения на сервер не зайти — ни людям, ни ботам."
        : "🔓 Сервер открыт: зайти может любой, кто знает адрес. Создайте ключ — и вход станет только по приглашению.",
      info.master ? h("div.small", { style: { marginTop: "6px" } }, "Также действует общий ключ из настроек сервера (SERVER_PASSWORD).") : null
    )
  );
  const list = h("div.admin-list");
  for (const inv of info.invites) {
    const link = inviteLink(inv.code);
    list.append(
      h(
        "div.admin-row",
        { style: { flexWrap: "wrap" } },
        h("span", { html: icon("crown", 16), style: { color: "var(--brand)" } }),
        h(
          "span.name",
          null,
          h("b", null, inv.label || "Без названия"),
          h("span.muted.small", null, `  · входов: ${inv.uses}` + (inv.createdBy ? ` · создал ${inv.createdBy}` : ""))
        ),
        h("input.input", { value: link, readonly: true, style: { flex: "1 1 260px", height: "30px", fontSize: "13px" }, onclick: (e) => e.target.select() }),
        h("button.btn.btn-sm.btn-primary", { onclick: () => copyText(link) }, "Копировать"),
        h("button.icon-btn.danger", {
          html: icon("trash", 18),
          title: "Отозвать ключ",
          onclick: () =>
            modal({
              title: "Отозвать ключ",
              body: h("p", null, `Ключ «${inv.label || "Без названия"}» перестанет работать, а все, кто сейчас зашёл по нему, будут отключены.`),
              okText: "Отозвать",
              danger: true,
              onOk: () => send("invite-delete", { text: inv.code }),
            }),
        })
      )
    );
  }
  if (info.invites.length) c.append(list);
  const label = h("input.input", { placeholder: "Для кого ключ (необязательно): например, «брат»", maxlength: 40 });
  const create = () => {
    const first = !info.required;
    const go = () => {
      send("invite-create", { name: label.value });
      label.value = "";
    };
    if (!first) return go();
    modal({
      title: "Закрыть сервер ключом?",
      body: [
        h("p", null, "После создания первого ключа зайти можно будет только по ссылке-приглашению."),
        h("p.muted.small", null, "Тем, кто сейчас в чате, ничего не будет, но при следующем входе им понадобится ссылка — разошлите её. Вы как админ входите без ключа."),
      ],
      okText: "Создать ключ",
      onOk: go,
    });
  };
  label.onkeydown = (e) => e.key === "Enter" && create();
  c.append(
    h("div", { style: { display: "flex", gap: "8px", marginTop: "12px", maxWidth: "560px" } }, label, h("button.btn.btn-primary", { onclick: create }, "Создать ключ")),
    h(
      "p.muted.small",
      { style: { marginTop: "8px" } },
      "Совет: делайте отдельный ключ на каждого — тогда можно отозвать доступ одному человеку, не трогая остальных."
    )
  );
}

function settingsDonate(c) {
  c.append(
    h("h2", null, "Поддержать проект"),
    h("p.muted", null, "Если чат пригодился — можно закинуть на развитие :)"),
    h("img", { src: "/donate-qr.png", alt: "QR для доната", style: { width: "240px", borderRadius: "12px", background: "#fff", padding: "8px" } })
  );
}

function logout() {
  closeSettings();
  disconnect();
  S.nick = "";
  lsSet("z228.nick", "");
  lsSet("z228.adminToken", "");
  S.adminToken = "";
  showLogin();
}

function applyTheme() {
  document.documentElement.dataset.theme = S.settings.theme === "light" ? "light" : "dark";
}

// ============================================================ вход

function showLogin(error, needPassword) {
  $("#app").hidden = true;
  $("#login").hidden = false;
  banner(null);
  $("#loginNick").value = S.nick || "";
  $("#loginTitle").textContent = S.nick ? "С возвращением!" : "Добро пожаловать!";
  $("#loginSubtitle").textContent = S.nick ? "Рады видеть тебя снова." : "Придумай ник, чтобы войти.";
  if (needPassword || S.password) $("#loginPasswordField").hidden = false;
  $("#loginPassword").value = S.password || "";
  const err = $("#loginError");
  err.hidden = !error;
  err.textContent = error || "";
  $("#loginHint").textContent = window.isSecureContext ? "" : "⚠ Страница открыта по HTTP — микрофон будет недоступен. Используйте https://";
  setTimeout(() => (needPassword ? $("#loginPassword") : $("#loginNick")).focus());
}

$("#loginForm").addEventListener("submit", (e) => {
  e.preventDefault();
  const nick = $("#loginNick").value.trim();
  if (!nick) return;
  S.nick = nick;
  S.password = $("#loginPassword").value;
  $("#loginError").hidden = true;
  $("#loginBtn").disabled = true;
  setTimeout(() => ($("#loginBtn").disabled = false), 1500);
  disconnect();
  connect();
});

// ============================================================ уведомления-тосты, баннер

function toast(text, kind = "") {
  // одинаковое сообщение не дублируем — просто продлеваем уже показанное
  const same = $$(".toast").find((t) => t.textContent === text);
  if (same) {
    same.remove();
  }
  const el = h("div.toast" + (kind ? "." + kind : ""), null, text);
  $("#toasts").append(el);
  setTimeout(() => el.remove(), kind === "error" ? 6000 : 3500);
}

function banner(text, warn) {
  const b = $("#connBanner");
  b.hidden = !text;
  b.textContent = text || "";
  b.classList.toggle("error", !warn);
}

// ============================================================ мобильная раскладка

function closeMobileSidebar() {
  $("#app").classList.remove("show-sidebar");
}

// ============================================================ ширина панелей

const PANEL_LIMITS = { sidebar: [200, 440, 240], members: [180, 420, 240] };

function applyPanelWidths() {
  const w = lsGet("z228.panels", {});
  for (const [name, [min, max]] of Object.entries(PANEL_LIMITS)) {
    if (w[name]) document.documentElement.style.setProperty(`--${name}-w`, Math.min(max, Math.max(min, w[name])) + "px");
    else document.documentElement.style.removeProperty(`--${name}-w`);
  }
}

function bindResizer(handle, name, panel, fromRight) {
  const [min, max] = PANEL_LIMITS[name];
  handle.addEventListener("pointerdown", (e) => {
    if (e.button !== 0) return;
    e.preventDefault();
    const startX = e.clientX;
    const startW = panel.getBoundingClientRect().width;
    handle.setPointerCapture(e.pointerId);
    handle.classList.add("dragging");
    document.body.classList.add("resizing");
    let width = startW;
    const move = (ev) => {
      const dx = ev.clientX - startX;
      width = Math.round(Math.min(max, Math.max(min, startW + (fromRight ? -dx : dx))));
      document.documentElement.style.setProperty(`--${name}-w`, width + "px");
    };
    const up = () => {
      handle.removeEventListener("pointermove", move);
      handle.removeEventListener("pointerup", up);
      handle.removeEventListener("pointercancel", up);
      handle.classList.remove("dragging");
      document.body.classList.remove("resizing");
      const w = lsGet("z228.panels", {});
      w[name] = width;
      lsSet("z228.panels", w);
    };
    handle.addEventListener("pointermove", move);
    handle.addEventListener("pointerup", up);
    handle.addEventListener("pointercancel", up);
  });
  handle.addEventListener("dblclick", () => {
    const w = lsGet("z228.panels", {});
    delete w[name];
    lsSet("z228.panels", w);
    applyPanelWidths();
  });
}

// ============================================================ события

function bindUi() {
  applyPanelWidths();
  new ResizeObserver(() => layoutStage()).observe($("#stageGrid"));
  bindResizer($("#sidebarResizer"), "sidebar", $("#sidebar"), false);
  bindResizer($("#membersResizer"), "members", $("#members"), true);

  for (const el of $$("[data-icon]")) el.innerHTML = icon(el.dataset.icon, el.classList.contains("round-btn") ? 24 : 20);

  $("#micBtn").onclick = toggleMute;
  $("#deafBtn").onclick = toggleDeafen;
  $("#stMic").onclick = toggleMute;
  $("#stDeaf").onclick = toggleDeafen;
  $("#settingsBtn").onclick = () => openSettings("voice");
  $("#vpLeave").onclick = leaveVoice;
  $("#stLeave").onclick = leaveVoice;
  $("#vpScreen").onclick = toggleScreen;
  $("#stScreen").onclick = toggleScreen;
  $("#voiceChannelLink").onclick = () => voice.channelId && openStage(voice.channelId);
  $("#meBtn").onclick = (e) => userMenu(e, userById(S.me.id) || { id: S.me.id, nickname: S.me.nickname });
  $("#membersToggle").onclick = () => {
    const app = $("#app");
    if (matchMedia("(max-width: 1100px)").matches) app.classList.toggle("show-members");
    else {
      $("#members").hidden = !$("#members").hidden;
      $("#membersResizer").hidden = $("#members").hidden;
    }
  };
  $("#mobileMenuBtn").onclick = () => $("#app").classList.toggle("show-sidebar");
  $("#main").addEventListener("click", (e) => {
    if (!e.target.closest("#mobileMenuBtn")) closeMobileSidebar();
  });

  $("#serverHeader").onclick = (e) => {
    e.stopPropagation();
    const m = $("#serverMenu");
    if (!m.hidden) return (m.hidden = true);
    m.innerHTML = "";
    if (S.admin) {
      m.append(
        menuItem("Создать канал", () => createChannelDialog("text"), { iconName: "plus" }),
        menuItem("Администрирование", () => openSettings("admin"), { iconName: "crown" }),
        h("div.menu-sep")
      );
    } else {
      m.append(menuItem("Войти как админ", () => openSettings("admin"), { iconName: "crown" }));
    }
    m.append(
      menuItem("Настройки", () => openSettings("voice"), { iconName: "gear" }),
      menuItem("Поддержать", () => openSettings("donate"), { iconName: "heart" }),
      h("div.menu-sep"),
      menuItem("Выйти", logout, { danger: true, iconName: "logout" })
    );
    m.hidden = false;
  };

  document.addEventListener("click", (e) => {
    if (!e.target.closest(".popover")) closeLayer();
    if (!e.target.closest("#serverMenu")) $("#serverMenu").hidden = true;
  });

  document.addEventListener("keydown", (e) => {
    if (e.key === "Escape") {
      if (!$("#lightbox").hidden) $("#lightbox").hidden = true;
      else if ($("#layer").children.length) closeLayer();
      else if (settingsEl && !document.querySelector(".modal-backdrop")) closeSettings();
    }
    if (e.ctrlKey && e.shiftKey && e.code === "KeyM") {
      e.preventDefault();
      toggleMute();
    }
    if (e.ctrlKey && e.shiftKey && e.code === "KeyD") {
      e.preventDefault();
      toggleDeafen();
    }
    if (e.altKey && (e.key === "ArrowUp" || e.key === "ArrowDown")) {
      const list = textChannels();
      const i = list.findIndex((c) => c.id === S.textId);
      const next = list[(i + (e.key === "ArrowDown" ? 1 : list.length - 1)) % list.length];
      if (next) {
        e.preventDefault();
        selectText(next.id);
      }
    }
    // push-to-talk
    if (S.settings.pushToTalk && S.settings.pttKey && e.code === S.settings.pttKey && !e.repeat) voice.setPttHeld(true);
  });
  document.addEventListener("keyup", (e) => {
    if (S.settings.pushToTalk && e.code === S.settings.pttKey) voice.setPttHeld(false);
  });
  window.addEventListener("blur", () => voice.setPttHeld(false));

  document.addEventListener("visibilitychange", () => {
    if (!document.hidden && S.view === "text" && isAtBottom(40)) markRead(S.textId);
  });

  // подсказки (реакции)
  let tip = null;
  document.addEventListener("mouseover", (e) => {
    const t = e.target.closest("[data-tip]");
    if (tip) {
      tip.remove();
      tip = null;
    }
    if (!t) return;
    tip = h("div.tooltip", null, t.dataset.tip);
    document.body.append(tip);
    const r = t.getBoundingClientRect();
    tip.style.left = Math.max(8, Math.min(r.left + r.width / 2 - tip.offsetWidth / 2, innerWidth - tip.offsetWidth - 8)) + "px";
    tip.style.top = r.top - tip.offsetHeight - 8 + "px";
  });

  // пинг для индикатора в голосовой панели
  setInterval(() => {
    if (voice.active && S.ready) send("ping", { t: performance.now() });
  }, 5000);

  // качество связи: снимаем статистику WebRTC и сообщаем серверу, чтобы видели другие
  setInterval(async () => {
    if (!voice.active || !S.ready || !S.me) return;
    const st = await voice.collectStats().catch(() => null);
    if (!st) return;
    S.myStats = st;
    const loss = Math.max(st.downLoss || 0, st.upLoss || 0);
    if (st.rtt != null) {
      S.quality[S.me.id] = { rtt: st.rtt, loss, jitter: st.jitter || 0 };
      send("voice-quality", { rtt: st.rtt, loss, jitter: st.jitter || 0 });
    }
    renderVoicePanel();
    updateQualityEls(S.me.id);
    if (qualityPopoverOpen()) showQualityDetails();
  }, 3000);
  $("#voiceStatus").onclick = (e) => {
    e.stopPropagation();
    showQualityDetails(e.currentTarget);
  };

  // браузеры не дают проигрывать звук до первого клика — разблокируем
  document.addEventListener("pointerdown", () => voice.audioCtx(), { once: true });

  window.addEventListener("beforeunload", () => {
    if (S.ws) S.ws.close();
  });

  bindComposer();
}

// ============================================================ старт

// ключ из ссылки-приглашения вида https://сервер/#k=КЛЮЧ — запоминаем и убираем из адресной строки
function readInviteFromUrl() {
  const m = location.hash.match(/[#&]k=([A-Za-z0-9_-]{4,128})/);
  if (!m) return false;
  S.password = m[1];
  lsSet("z228.password", S.password);
  history.replaceState(null, "", location.pathname + location.search);
  return true;
}
readInviteFromUrl();
// ссылку вставили в уже открытую вкладку — страница не перезагружается, меняется только #
window.addEventListener("hashchange", () => {
  if (!readInviteFromUrl() || S.ready) return;
  $("#loginPasswordField").hidden = false;
  $("#loginPassword").value = S.password;
  $("#loginError").hidden = true;
  if (S.nick) {
    disconnect();
    connect();
  }
});

bindUi();
applyTheme();
if (S.nick) {
  connect();
} else {
  showLogin();
}
