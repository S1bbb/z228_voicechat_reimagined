import { buildMicChain, noiseSuppressionSupported } from "./noise.js";
import { needsFilter } from "./voicefx.js";

// Голос: одно RTCPeerConnection с сервером (SFU), микрофон, демонстрация экрана,
// воспроизведение собеседников, громкость по людям и индикация речи.

const SPEAK_THRESHOLD = 0.018;
const SPEAK_HOLD_MS = 280;

/** RTCPeerConnection, в том числе под старыми префиксами. */
export function rtcConstructor() {
  return window.RTCPeerConnection || window.webkitRTCPeerConnection || window.mozRTCPeerConnection || null;
}

/** Что умеет браузер — отправляем на сервер, когда голос не запускается. */
/** Оценка качества: 3 — хорошо, 2 — так себе, 1 — плохо, 0 — нет данных. */
export function qualityLevel(q) {
  if (!q || q.rtt == null) return 0;
  const loss = Math.max(q.downLoss || 0, q.upLoss || 0);
  if (q.rtt < 150 && loss < 2) return 3;
  if (q.rtt < 300 && loss < 8) return 2;
  return 1;
}

export function rtcDiagnostics() {
  const md = navigator.mediaDevices;
  return {
    ua: navigator.userAgent,
    secure: window.isSecureContext,
    rtc: typeof window.RTCPeerConnection,
    webkitRtc: typeof window.webkitRTCPeerConnection,
    mediaDevices: !!md,
    getUserMedia: !!(md && md.getUserMedia),
    protocol: location.protocol,
    host: location.host,
    // антивирусы и расширения часто подменяют/удаляют WebRTC, внедряя свои скрипты
    scripts: Array.from(document.scripts).map((s) => s.src).filter((s) => s && !s.startsWith(location.origin)).slice(0, 5),
  };
}

export class Voice {
  /**
   * @param {(type:string, payload:object)=>void} send — отправка в WebSocket
   * @param {object} hooks — колбэки: onState, onSpeaking, onRemoteVideo, onScreenEnded, onError
   */
  constructor(send, hooks) {
    this.send = send;
    this.hooks = hooks;
    this.pc = null;
    this.joinId = "";
    this.channelId = 0;
    this.micStream = null;
    this.micTrack = null;
    this.screenStream = null;
    this.chain = Promise.resolve();
    this.pendingIce = [];
    this.remote = new Map(); // userId -> {audio:{track,el,src,gain,analyser}, video:{track}}
    this.speaking = new Set();
    this.ctx = null;
    this.muted = false;
    this.deafened = false;
    this.serverMuted = false;
    this.pttHeld = false;
    this.settings = {};
    this.volumes = {}; // userId/nick -> 0..200
    this.localMutes = {};
    this.meterTimer = null;
    this.localAnalyser = null;
    this.myId = "";
  }

  audioCtx() {
    if (!this.ctx) {
      const AC = window.AudioContext || window.webkitAudioContext;
      this.ctx = new AC();
    }
    if (this.ctx.state === "suspended") this.ctx.resume().catch(() => {});
    return this.ctx;
  }

  get active() {
    return !!this.pc;
  }

  micConstraints() {
    const s = this.settings;
    const audio = {
      echoCancellation: s.echoCancellation !== false,
      // с RNNoise встроенный шумодав браузера выключаем — двойная обработка портит голос
      noiseSuppression: s.rnnoise ? false : s.noiseSuppression !== false,
      autoGainControl: s.autoGainControl !== false,
      channelCount: 1,
    };
    if (s.inputDevice) audio.deviceId = { exact: s.inputDevice };
    return { audio, video: false };
  }

  async acquireMic() {
    try {
      return await navigator.mediaDevices.getUserMedia(this.micConstraints());
    } catch (e) {
      if (this.settings.inputDevice && e.name === "OverconstrainedError") {
        // устройство пропало — берём по умолчанию
        return await navigator.mediaDevices.getUserMedia({ audio: true });
      }
      throw e;
    }
  }

  async join(channelId, myId) {
    if (this.pc) this.leave(false);
    // У каждого входа свой id: сервер помечает им offer/кандидатов, и всё, что относится
    // к прошлому входу (двойной клик, переподключение), отбрасывается с обеих сторон.
    const joinId = Math.random().toString(36).slice(2, 12);
    this.joinId = joinId;
    this.myId = myId;
    this.channelId = channelId;
    this.hooks.onState("connecting");
    this.audioCtx();

    const PC = rtcConstructor();
    const pc = new PC({ iceServers: [], bundlePolicy: "max-bundle", rtcpMuxPolicy: "require" });
    this.pc = pc;
    pc.onicecandidate = (e) => {
      if (e.candidate && this.pc === pc) this.send("rtc-candidate", { candidate: e.candidate.toJSON(), joinId });
    };
    pc.ontrack = (e) => this.onTrack(e);
    pc.onconnectionstatechange = () => {
      if (this.pc !== pc) return;
      const st = pc.connectionState;
      if (st === "connected") this.hooks.onState("connected");
      else if (st === "failed") {
        this.hooks.onState("failed");
        this.hooks.onError("Не удалось установить голосовое соединение с сервером. Проверьте, что UDP/TCP-порт голоса открыт.");
      } else if (st === "disconnected") this.hooks.onState("connecting");
    };
    this.startMeters();
    this.send("voice-join", { channelId, joinId });
    // Микрофон подключаем параллельно: пока браузер ждёт разрешения, голос уже работает на приём,
    // и вход не «зависает», если окошко разрешения закрыли или оно спряталось.
    this.attachMic(joinId);
  }

  async attachMic(joinId) {
    let mic = null;
    const hint = setTimeout(() => {
      if (this.joinId === joinId && !this.micTrack) {
        this.hooks.onError("Браузер ждёт разрешения на микрофон — нажмите на значок 🔒/🎤 в адресной строке и разрешите доступ. Пока вы только слушаете.");
      }
    }, 5000);
    try {
      if (!window.isSecureContext) throw Object.assign(new Error("insecure"), { name: "InsecureContext" });
      mic = await this.acquireMic();
    } catch (e) {
      clearTimeout(hint);
      if (this.joinId !== joinId) return;
      const why =
        e.name === "InsecureContext"
          ? "Микрофон доступен только по HTTPS (или на localhost). Вы подключены только на прослушивание."
          : e.name === "NotAllowedError"
            ? "Нет доступа к микрофону — разрешите его в настройках сайта (значок 🔒 в адресной строке). Вы подключены только на прослушивание."
            : e.name === "NotReadableError"
              ? "Микрофон занят другой программой или недоступен. Вы подключены только на прослушивание."
              : "Микрофон не найден. Вы подключены только на прослушивание.";
      this.hooks.onError(why);
      return;
    }
    clearTimeout(hint);
    if (this.joinId !== joinId) {
      // пока ждали разрешения, начался другой вход или выход
      mic.getTracks().forEach((t) => t.stop());
      return;
    }
    const m = await this.processMic(mic);
    if (this.joinId !== joinId) {
      this.releaseMicObj(m);
      return;
    }
    this.releaseMic();
    await this.setMic(m);
    if (this.hooks.onMic) this.hooks.onMic(true);
  }

  /** Сырой поток микрофона -> то, что отправляем (через RNNoise, гейт и фильтр голоса, если включены). */
  async processMic(raw) {
    const s = this.settings;
    const plain = { raw, stream: raw, track: raw.getAudioTracks()[0] || null, close: null, chain: null };
    const gain = s.inputVolume ?? 100;
    if ((!s.rnnoise && !s.gate && !needsFilter(s.voiceFilter) && gain === 100) || !noiseSuppressionSupported()) return plain;
    try {
      const chain = await buildMicChain(raw, { rnnoise: s.rnnoise, gate: s.gate, thresholdDb: s.gateDb, filter: s.voiceFilter, gain });
      return { raw, stream: chain.stream, track: chain.track, close: chain.close, chain };
    } catch (e) {
      console.warn("mic chain", e);
      this.hooks.onError("Обработка голоса (шумодав, гейт, фильтр) не запустилась в этом браузере — голос идёт как есть.");
      return plain;
    }
  }

  /** Громкость микрофона: на лету, если цепочка обработки уже есть; иначе перезапуск микрофона с цепочкой. */
  setInputVolume(pct) {
    this.settings.inputVolume = pct;
    if (this.mic && this.mic.chain) this.mic.chain.setGain(pct);
    else if (this.mic && pct !== 100) this.restartMic();
  }

  /** Порог гейта меняем на лету, без перезапуска микрофона. */
  setGateThreshold(db) {
    this.settings.gateDb = db;
    if (this.mic && this.mic.chain) this.mic.chain.setThreshold(db);
  }

  async setMic(m) {
    this.mic = m;
    this.micStream = m.stream;
    this.micTrack = m.track;
    this.localAnalyser = null;
    this.applyMicGate();
    // если offer уже обработан — подставляем трек в готовый transceiver, иначе это сделает onOffer
    const t = this.sendTransceiver("0");
    if (t && t.sender.track !== this.micTrack) await t.sender.replaceTrack(this.micTrack).catch(() => {});
  }

  releaseMicObj(m) {
    if (!m) return;
    if (m.close) m.close();
    m.raw.getTracks().forEach((t) => t.stop());
  }

  releaseMic() {
    this.releaseMicObj(this.mic);
    this.mic = null;
    this.micStream = null;
    this.micTrack = null;
  }

  leave(notify = true) {
    if (notify && this.pc) this.send("voice-leave", {});
    if (this.pc) {
      try {
        this.pc.close();
      } catch {}
    }
    this.pc = null;
    this.joinId = "";
    this.channelId = 0;
    this.prevStats = null;
    this.lastStats = null;
    this.pendingIce = [];
    this.chain = Promise.resolve();
    this.stopScreen(false);
    this.stopCamera(false);
    this.releaseMic();
    this.micTrack = null;
    for (const id of [...this.remote.keys()]) this.dropRemote(id);
    this.stopMeters();
    this.speaking.clear();
    this.hooks.onSpeaking(this.speaking);
    this.hooks.onState("disconnected");
  }

  // ---------- сигналинг ----------

  onOffer(sdp, joinId) {
    const pc = this.pc;
    if (!pc || joinId !== this.joinId) return;
    this.chain = this.chain
      .then(async () => {
        if (this.pc !== pc) return;
        await pc.setRemoteDescription({ type: "offer", sdp });
        // m-line 0/1 — наши исходящие audio/video
        for (const t of pc.getTransceivers()) {
          // m-line 0/1/2 — наши исходящие: микрофон, экран, камера
          if (t.mid === "0" || t.mid === "1" || t.mid === "2") {
            if (t.direction !== "sendrecv") t.direction = "sendrecv";
            const want = t.mid === "0" ? this.micTrack : t.mid === "1" ? this.screenTrack() : this.cameraTrack();
            if (t.sender.track !== want) await t.sender.replaceTrack(want);
            if (t.mid === "1") this.tuneVideoSender(t.sender);
            if (t.mid === "2") this.tuneCameraSender(t.sender);
          }
        }
        const answer = await pc.createAnswer();
        await pc.setLocalDescription(answer);
        this.send("rtc-answer", { sdp: pc.localDescription.sdp, joinId });
        const queued = this.pendingIce;
        this.pendingIce = [];
        for (const c of queued) await pc.addIceCandidate(c).catch(() => {});
      })
      .catch((e) => console.error("rtc offer", e));
  }

  onCandidate(c, joinId) {
    const pc = this.pc;
    if (!pc || !c || joinId !== this.joinId) return;
    if (!pc.remoteDescription) {
      this.pendingIce.push(c);
      return;
    }
    pc.addIceCandidate(c).catch((e) => console.warn("ICE", e));
  }

  sendTransceiver(mid) {
    if (!this.pc) return null;
    return this.pc.getTransceivers().find((t) => t.mid === mid) || null;
  }

  // ---------- входящие треки ----------

  onTrack(e) {
    const stream = e.streams[0];
    const userId = stream ? stream.id : "";
    if (!userId || userId === this.myId) return;
    const track = e.track;
    const r = this.remote.get(userId) || {};
    this.remote.set(userId, r);
    if (track.kind === "audio") {
      this.teardownAudio(r);
      r.audio = this.buildAudio(userId, track);
    } else {
      // сервер называет треки "<id>-screen" и "<id>-camera"
      const slot = track.id.endsWith("-camera") ? "camera" : "video";
      r[slot] = { track, stream: new MediaStream([track]) };
      this.hooks.onRemoteVideo(userId, r[slot].stream);
    }
    track.addEventListener("ended", () => {
      const cur = this.remote.get(userId);
      if (!cur) return;
      if (track.kind === "audio" && cur.audio && cur.audio.track === track) this.teardownAudio(cur);
      for (const slot of ["video", "camera"]) {
        if (cur[slot] && cur[slot].track === track) {
          cur[slot] = null;
          this.hooks.onRemoteVideo(userId, null);
        }
      }
    });
  }

  buildAudio(userId, track) {
    const ctx = this.audioCtx();
    const stream = new MediaStream([track]);
    const el = new Audio();
    el.autoplay = true;
    el.srcObject = stream;
    if (this.settings.outputDevice && el.setSinkId) el.setSinkId(this.settings.outputDevice).catch(() => {});
    el.play().catch(() => {});
    const src = ctx.createMediaStreamSource(stream);
    const analyser = ctx.createAnalyser();
    analyser.fftSize = 512;
    src.connect(analyser);
    const gain = ctx.createGain();
    src.connect(gain);
    const a = { track, el, src, analyser, gain, routed: false, userId };
    this.applyVolume(userId, a);
    return a;
  }

  teardownAudio(r) {
    const a = r.audio;
    if (!a) return;
    try {
      a.el.pause();
      a.el.srcObject = null;
      a.src.disconnect();
      a.gain.disconnect();
    } catch {}
    r.audio = null;
  }

  dropRemote(userId) {
    const r = this.remote.get(userId);
    if (!r) return;
    this.teardownAudio(r);
    if (r.video || r.camera) this.hooks.onRemoteVideo(userId, null);
    this.remote.delete(userId);
    this.speaking.delete(userId);
  }

  /** Убрать данные тех, кого уже нет в канале (по presence). */
  syncMembers(userIds) {
    const keep = new Set(userIds);
    for (const id of [...this.remote.keys()]) if (!keep.has(id)) this.dropRemote(id);
  }

  remoteVideo(userId) {
    const r = this.remote.get(userId);
    return r && r.video ? r.video.stream : null;
  }

  remoteCamera(userId) {
    const r = this.remote.get(userId);
    return r && r.camera ? r.camera.stream : null;
  }

  // ---------- громкость ----------

  setVolume(key, userId, percent) {
    this.volumes[key] = percent;
    const r = this.remote.get(userId);
    if (r && r.audio) this.applyVolume(userId, r.audio);
  }

  setLocalMute(key, userId, muted) {
    this.localMutes[key] = muted;
    const r = this.remote.get(userId);
    if (r && r.audio) this.applyVolume(userId, r.audio);
  }

  applyAllVolumes() {
    for (const [id, r] of this.remote) if (r.audio) this.applyVolume(id, r.audio);
  }

  applyVolume(userId, a) {
    const key = this.hooks.volumeKey(userId);
    const pct = this.volumes[key] ?? 100;
    const silent = this.deafened || this.localMutes[key];
    const master = (this.settings.outputVolume ?? 100) / 100;
    const v = silent ? 0 : (pct / 100) * master;
    if (v <= 1) {
      // обычный путь — через <audio>, так работает эхоподавление Chrome
      if (a.routed) {
        a.gain.disconnect();
        a.routed = false;
      }
      a.el.muted = false;
      a.el.volume = v;
    } else {
      // громче 100% — только через WebAudio
      a.el.muted = true;
      a.gain.gain.value = v;
      if (!a.routed) {
        a.gain.connect(this.audioCtx().destination);
        a.routed = true;
      }
    }
  }

  async setOutputDevice(id) {
    this.settings.outputDevice = id;
    for (const r of this.remote.values()) {
      if (r.audio && r.audio.el.setSinkId) await r.audio.el.setSinkId(id || "").catch(() => {});
    }
    if (this.ctx && this.ctx.setSinkId) await this.ctx.setSinkId(id || "").catch(() => {});
    setSfxOutput(id);
  }

  // ---------- микрофон ----------

  setMuted(m) {
    this.muted = m;
    this.applyMicGate();
  }

  setDeafened(d) {
    this.deafened = d;
    this.applyAllVolumes();
    this.applyMicGate();
  }

  setPttHeld(held) {
    this.pttHeld = held;
    this.applyMicGate();
  }

  micOpen() {
    if (!this.micTrack || this.muted || this.deafened || this.serverMuted) return false;
    if (this.settings.pushToTalk) return this.pttHeld;
    return true;
  }

  applyMicGate() {
    if (this.micTrack) this.micTrack.enabled = this.micOpen();
  }

  /** Перезапустить микрофон с новыми настройками (устройство, шумодав). */
  async restartMic() {
    if (!this.pc) return;
    try {
      const m = await this.processMic(await this.acquireMic());
      if (!this.pc) {
        this.releaseMicObj(m);
        return;
      }
      this.releaseMic();
      await this.setMic(m);
    } catch (e) {
      this.hooks.onError("Не удалось переключить микрофон: " + e.message);
    }
  }

  /** Браузер не играет звук до первого клика: после автовозврата в голос «размораживаем» всё по клику. */
  unlockAudio() {
    this.audioCtx();
    for (const r of this.remote.values()) if (r.audio) r.audio.el.play().catch(() => {});
    if (this.mic && this.mic.chain && this.mic.chain.resume) this.mic.chain.resume();
  }

  // ---------- камера ----------

  cameraTrack() {
    return this.cameraStream ? this.cameraStream.getVideoTracks()[0] || null : null;
  }

  async startCamera() {
    if (!this.pc) return false;
    const video = { width: { ideal: 1280 }, height: { ideal: 720 }, frameRate: { ideal: 30, max: 30 } };
    if (this.settings.videoDevice) video.deviceId = { exact: this.settings.videoDevice };
    let stream;
    try {
      stream = await navigator.mediaDevices.getUserMedia({ video, audio: false });
    } catch (e) {
      if (this.settings.videoDevice && e.name === "OverconstrainedError") {
        stream = await navigator.mediaDevices.getUserMedia({ video: true, audio: false }).catch(() => null);
      }
      if (!stream) {
        this.hooks.onError(
          e.name === "NotAllowedError"
            ? "Нет доступа к камере — разрешите её в настройках сайта (значок 🔒 в адресной строке)."
            : e.name === "NotReadableError"
              ? "Камера занята другой программой."
              : "Камера не найдена."
        );
        return false;
      }
    }
    if (!this.pc) {
      stream.getTracks().forEach((t) => t.stop());
      return false;
    }
    this.stopCamera(false);
    this.cameraStream = stream;
    const track = stream.getVideoTracks()[0];
    track.contentHint = "motion";
    track.addEventListener("ended", () => {
      if (this.cameraStream === stream) {
        this.stopCamera(true);
        if (this.hooks.onCameraEnded) this.hooks.onCameraEnded();
      }
    });
    const t = this.sendTransceiver("2");
    if (t) {
      await t.sender.replaceTrack(track);
      this.tuneCameraSender(t.sender);
    }
    return true;
  }

  tuneCameraSender(sender) {
    if (!this.cameraStream) return;
    const params = sender.getParameters();
    if (!params.encodings || !params.encodings.length) return;
    params.encodings[0].maxBitrate = 1_200_000;
    params.encodings[0].maxFramerate = 30;
    sender.setParameters(params).catch(() => {});
  }

  stopCamera(replace = true) {
    if (this.cameraStream) this.cameraStream.getTracks().forEach((t) => t.stop());
    this.cameraStream = null;
    if (replace) {
      const t = this.sendTransceiver("2");
      if (t) t.sender.replaceTrack(null).catch(() => {});
    }
  }

  // ---------- демонстрация экрана ----------

  screenTrack() {
    return this.screenStream ? this.screenStream.getVideoTracks()[0] || null : null;
  }

  async startScreen(quality = "1080p30") {
    if (!this.pc) return false;
    const [res, fps] = { "720p30": [720, 30], "1080p30": [1080, 30], "1080p60": [1080, 60], "1440p30": [1440, 30] }[quality] || [1080, 30];
    let stream;
    try {
      stream = await navigator.mediaDevices.getDisplayMedia({
        video: { height: { ideal: res }, frameRate: { ideal: fps, max: fps } },
        audio: false,
      });
    } catch (e) {
      if (e.name !== "NotAllowedError") this.hooks.onError("Не удалось начать демонстрацию: " + e.message);
      return false;
    }
    this.stopScreen(false);
    this.screenStream = stream;
    this.screenQuality = { res, fps };
    const track = stream.getVideoTracks()[0];
    track.contentHint = fps >= 60 ? "motion" : "detail";
    track.addEventListener("ended", () => {
      if (this.screenStream === stream) {
        this.stopScreen(true);
        this.hooks.onScreenEnded();
      }
    });
    const t = this.sendTransceiver("1");
    if (t) {
      await t.sender.replaceTrack(track);
      this.tuneVideoSender(t.sender);
    }
    return true;
  }

  tuneVideoSender(sender) {
    if (!this.screenStream) return;
    const params = sender.getParameters();
    if (!params.encodings || !params.encodings.length) return;
    const { res = 1080, fps = 30 } = this.screenQuality || {};
    params.encodings[0].maxBitrate = Math.round((res >= 1440 ? 6 : res >= 1080 ? 3.5 : 2) * (fps >= 60 ? 1.6 : 1) * 1e6);
    params.encodings[0].maxFramerate = fps;
    params.degradationPreference = fps >= 60 ? "maintain-framerate" : "maintain-resolution";
    sender.setParameters(params).catch(() => {});
  }

  stopScreen(replace = true) {
    if (this.screenStream) this.screenStream.getTracks().forEach((t) => t.stop());
    this.screenStream = null;
    if (replace) {
      const t = this.sendTransceiver("1");
      if (t) t.sender.replaceTrack(null).catch(() => {});
    }
  }

  // ---------- качество связи ----------

  /**
   * Снимок качества соединения с сервером: пинг, потери на приём/передачу, джиттер, протокол и битрейт.
   * Потери считаются по приращению счётчиков с прошлого вызова.
   */
  async collectStats() {
    const pc = this.pc;
    if (!pc || pc.connectionState !== "connected") return null;
    const report = await pc.getStats();
    const prev = this.prevStats || {};
    const now = performance.now();
    let rtt = null;
    let proto = "";
    let candType = "";
    let recv = 0;
    let lost = 0;
    let jitter = 0;
    let jitterN = 0;
    let bytesIn = 0;
    let bytesOut = 0;
    let upLoss = null;
    const byId = new Map();
    report.forEach((r) => byId.set(r.id, r));
    report.forEach((r) => {
      if (r.type === "transport" && r.selectedCandidatePairId) {
        const pair = byId.get(r.selectedCandidatePairId);
        if (pair && pair.currentRoundTripTime != null) rtt = pair.currentRoundTripTime * 1000;
        const local = pair && byId.get(pair.localCandidateId);
        if (local) {
          proto = local.protocol || "";
          candType = local.candidateType || "";
        }
      }
      if (r.type === "candidate-pair" && r.nominated && r.state === "succeeded" && rtt == null && r.currentRoundTripTime != null) {
        rtt = r.currentRoundTripTime * 1000;
        const local = byId.get(r.localCandidateId);
        if (local && !proto) {
          proto = local.protocol || "";
          candType = local.candidateType || "";
        }
      }
      if (r.type === "inbound-rtp" && r.kind === "audio") {
        recv += r.packetsReceived || 0;
        lost += Math.max(0, r.packetsLost || 0);
        if (r.jitter != null) {
          jitter += r.jitter * 1000;
          jitterN++;
        }
      }
      if (r.type === "inbound-rtp") bytesIn += r.bytesReceived || 0;
      if (r.type === "outbound-rtp") bytesOut += r.bytesSent || 0;
      // отчёт сервера о том, сколько наших пакетов потерялось по дороге к нему
      if (r.type === "remote-inbound-rtp" && r.kind === "audio" && r.fractionLost != null) upLoss = r.fractionLost * 100;
    });
    const dRecv = recv - (prev.recv || 0);
    const dLost = lost - (prev.lost || 0);
    const dt = prev.t ? (now - prev.t) / 1000 : 0;
    const downLoss = dRecv + dLost > 20 ? (dLost / (dRecv + dLost)) * 100 : null;
    const out = {
      rtt: rtt == null ? null : Math.round(rtt),
      downLoss: downLoss == null ? null : Math.max(0, +downLoss.toFixed(1)),
      upLoss: upLoss == null ? null : +upLoss.toFixed(1),
      jitter: jitterN ? Math.round(jitter / jitterN) : null,
      proto,
      candType,
      kbpsIn: dt ? Math.round(((bytesIn - (prev.bytesIn || 0)) * 8) / dt / 1000) : null,
      kbpsOut: dt ? Math.round(((bytesOut - (prev.bytesOut || 0)) * 8) / dt / 1000) : null,
    };
    this.prevStats = { recv, lost, bytesIn, bytesOut, t: now };
    this.lastStats = out;
    return out;
  }

  // ---------- индикация речи ----------

  startMeters() {
    this.stopMeters();
    const buf = new Float32Array(512);
    const last = new Map();
    const rms = (an) => {
      an.getFloatTimeDomainData(buf);
      let s = 0;
      for (let i = 0; i < buf.length; i++) s += buf[i] * buf[i];
      return Math.sqrt(s / buf.length);
    };
    this.meterTimer = setInterval(() => {
      const now = performance.now();
      let changed = false;
      const check = (id, level) => {
        if (level > SPEAK_THRESHOLD) last.set(id, now);
        const on = now - (last.get(id) || 0) < SPEAK_HOLD_MS;
        if (on !== this.speaking.has(id)) {
          on ? this.speaking.add(id) : this.speaking.delete(id);
          changed = true;
        }
      };
      for (const [id, r] of this.remote) {
        const silent = !r.audio || this.deafened;
        check(id, silent ? 0 : rms(r.audio.analyser));
      }
      if (this.micTrack && !this.localAnalyser) {
        const ctx = this.audioCtx();
        this.localAnalyser = ctx.createAnalyser();
        this.localAnalyser.fftSize = 512;
        ctx.createMediaStreamSource(this.micStream).connect(this.localAnalyser);
      }
      if (this.myId) check(this.myId, this.localAnalyser && this.micOpen() ? rms(this.localAnalyser) : 0);
      if (changed) this.hooks.onSpeaking(this.speaking);
    }, 60);
  }

  stopMeters() {
    clearInterval(this.meterTimer);
    this.meterTimer = null;
    this.localAnalyser = null;
  }
}

/** Индикатор уровня микрофона для проверки в настройках. */
/**
 * Проверка микрофона в настройках: та же обработка, что в звонке (RNNoise, гейт), плюс индикатор.
 * onLevel({db, open}) — уровень после шумодава (до гейта) и состояние гейта.
 * Возвращает {stop(), setThreshold(db), measure(ms)}.
 */
/**
 * «Послушать себя»: микрофон → та же обработка, что уйдёт в звонок → в наушники.
 * Возвращает {stop()}.
 */
export async function voicePreview(constraints, opts, sinkId) {
  const stream = await navigator.mediaDevices.getUserMedia(constraints);
  let chain;
  try {
    chain = await buildMicChain(stream, { rnnoise: opts.rnnoise, filter: opts.filter, gain: opts.gain });
  } catch (e) {
    stream.getTracks().forEach((t) => t.stop());
    throw e;
  }
  const el = new Audio();
  el.srcObject = chain.stream;
  if (sinkId && el.setSinkId) await el.setSinkId(sinkId).catch(() => {});
  await el.play().catch(() => {});
  return {
    stop() {
      el.pause();
      el.srcObject = null;
      chain.close();
      stream.getTracks().forEach((t) => t.stop());
    },
  };
}

export async function micTest(constraints, onLevel, opts = {}) {
  const stream = await navigator.mediaDevices.getUserMedia(constraints);
  let chain = null;
  if (noiseSuppressionSupported()) {
    chain = await buildMicChain(stream, { rnnoise: opts.rnnoise, gate: opts.gate, thresholdDb: opts.thresholdDb, monitor: true }).catch((e) => {
      console.warn("mic test chain", e);
      return null;
    });
  }
  if (chain) {
    chain.onLevel((db, open) => onLevel({ db, open }));
    return {
      stop() {
        chain.close();
        stream.getTracks().forEach((t) => t.stop());
      },
      setThreshold: (db) => chain.setThreshold(db),
      measure: (ms) => chain.measure(ms),
    };
  }
  // запасной путь без AudioWorklet: просто уровень
  const AC = window.AudioContext || window.webkitAudioContext;
  const ctx = new AC();
  const an = ctx.createAnalyser();
  an.fftSize = 512;
  ctx.createMediaStreamSource(stream).connect(an);
  const buf = new Float32Array(512);
  const timer = setInterval(() => {
    an.getFloatTimeDomainData(buf);
    let s = 0;
    for (let i = 0; i < buf.length; i++) s += buf[i] * buf[i];
    const rms = Math.sqrt(s / buf.length);
    onLevel({ db: rms > 1e-9 ? 20 * Math.log10(rms) : -180, open: true });
  }, 50);
  return {
    stop() {
      clearInterval(timer);
      stream.getTracks().forEach((t) => t.stop());
      ctx.close();
    },
    setThreshold() {},
    measure: () => Promise.reject(new Error("браузер не поддерживает замер")),
  };
}

// ---------- звуки интерфейса ----------

let sfxCtx = null;
let sfxSink = "";

/** Звуки интерфейса идут на то же устройство вывода, что и голос. */
export function setSfxOutput(id) {
  sfxSink = id || "";
  if (sfxCtx && sfxCtx.setSinkId) sfxCtx.setSinkId(sfxSink).catch(() => {});
}

// Браузер не даёт звучать, пока человек ничего не нажал на странице: «будим» звук на первое нажатие,
// иначе сигнал о личном сообщении молчит (например, в приложении, сразу свёрнутом в трей).
export function unlockSfx() {
  try {
    const AC = window.AudioContext || window.webkitAudioContext;
    if (!sfxCtx) {
      sfxCtx = new AC();
      if (sfxSink && sfxCtx.setSinkId) sfxCtx.setSinkId(sfxSink).catch(() => {});
    }
    if (sfxCtx.state === "suspended") sfxCtx.resume().catch(() => {});
  } catch {}
}

export function sfx(kind) {
  try {
    const AC = window.AudioContext || window.webkitAudioContext;
    if (!sfxCtx) {
      sfxCtx = new AC();
      if (sfxSink && sfxCtx.setSinkId) sfxCtx.setSinkId(sfxSink).catch(() => {});
    }
    const ctx = sfxCtx;
    if (ctx.state === "suspended") ctx.resume();
    const seq = {
      join: [[523, 0], [784, 0.09]],
      leave: [[784, 0], [523, 0.09]],
      mute: [[440, 0], [330, 0.06]],
      unmute: [[330, 0], [440, 0.06]],
      message: [[880, 0], [1175, 0.07]],
      // личное сообщение — заметнее: три ноты вверх
      dm: [[784, 0, 0.13], [988, 0.08, 0.13], [1319, 0.16, 0.12, 0.22]],
      deafen: [[392, 0], [262, 0.07]],
      ping: [[1319, 0]],
      undeafen: [[262, 0], [392, 0.07]],
      // рация: тихий короткий щелчок [частота, начало, громкость, длительность]
      "ptt-on": [[1400, 0, 0.045, 0.035]],
      "ptt-off": [[950, 0, 0.035, 0.035]],
    }[kind];
    if (!seq) return;
    const t0 = ctx.currentTime;
    for (const [freq, at, vol = 0.12, dur = 0.16] of seq) {
      const o = ctx.createOscillator();
      const g = ctx.createGain();
      o.type = "sine";
      o.frequency.value = freq;
      g.gain.setValueAtTime(0, t0 + at);
      g.gain.linearRampToValueAtTime(vol, t0 + at + Math.min(0.01, dur / 4));
      g.gain.exponentialRampToValueAtTime(0.0001, t0 + at + dur);
      o.connect(g).connect(ctx.destination);
      o.start(t0 + at);
      o.stop(t0 + at + dur + 0.02);
    }
  } catch {}
}
