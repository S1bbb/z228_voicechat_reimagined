// Обработка микрофона перед отправкой: шумодав RNNoise (нейросеть, как в Jitsi/OBS) и шумовой гейт.
// Всё работает в AudioWorklet на 48 кГц; файлы лежат у нас (/vendor/noise-suppressor, /js/gate-worklet.js).
import { RnnoiseWorkletNode, loadRnnoise } from "../vendor/noise-suppressor/index.js";
import { applyVoiceFilter, needsFilter } from "./voicefx.js";

const BASE = "/vendor/noise-suppressor/";
let wasmPromise = null;

function loadWasm() {
  if (!wasmPromise) {
    wasmPromise = loadRnnoise({ url: BASE + "rnnoise.wasm", simdUrl: BASE + "rnnoise_simd.wasm" }).catch((e) => {
      wasmPromise = null;
      throw e;
    });
  }
  return wasmPromise;
}

export function noiseSuppressionSupported() {
  return typeof AudioWorkletNode !== "undefined" && typeof WebAssembly !== "undefined";
}

/**
 * Цепочка: микрофон → [RNNoise] → [гейт] → [фильтр голоса] → поток для отправки.
 * opts: {rnnoise, gate, thresholdDb, monitor, filter}. Гейт ставится и при monitor (для индикатора в настройках);
 * если сам гейт выключен, он всегда открыт и только меряет уровень.
 * Возвращает {stream, track, close(), setThreshold(db), onLevel(cb), measure(ms)}.
 */
export async function buildMicChain(rawStream, opts = {}) {
  if (!noiseSuppressionSupported()) throw new Error("AudioWorklet не поддерживается");
  const ctx = new AudioContext({ sampleRate: 48000, latencyHint: "interactive" });
  const nodes = [];
  let gateNode = null;
  try {
    const src = ctx.createMediaStreamSource(rawStream);
    nodes.push(src);
    let tail = src;
    if (opts.rnnoise) {
      const wasmBinary = await loadWasm();
      await ctx.audioWorklet.addModule(BASE + "rnnoise-worklet.js");
      const rn = new RnnoiseWorkletNode(ctx, { wasmBinary, maxChannels: 1 });
      nodes.push(rn);
      tail = tail.connect(rn);
    }
    if (opts.gate || opts.monitor) {
      await ctx.audioWorklet.addModule("/js/gate-worklet.js");
      gateNode = new AudioWorkletNode(ctx, "z228-gate", {
        numberOfInputs: 1,
        numberOfOutputs: 1,
        outputChannelCount: [1],
        processorOptions: { thresholdDb: opts.gate ? opts.thresholdDb ?? -50 : -200, holdMs: 250 },
      });
      nodes.push(gateNode);
      tail = tail.connect(gateNode);
    }
    if (needsFilter(opts.filter)) {
      // фильтр — последним: шумодав и гейт работают с настоящим голосом
      const fx = await applyVoiceFilter(ctx, tail, opts.filter);
      nodes.push(...fx.nodes);
      tail = fx.output;
    }
    const dest = ctx.createMediaStreamDestination();
    dest.channelCount = 1;
    tail.connect(dest);
    if (ctx.state === "suspended") await ctx.resume().catch(() => {});
    const track = dest.stream.getAudioTracks()[0];

    let levelCb = null;
    const pending = new Map();
    let seq = 0;
    if (gateNode) {
      gateNode.port.onmessage = (e) => {
        const d = e.data || {};
        if (d.level != null && levelCb) levelCb(d.level, d.open);
        if (d.measured && pending.has(d.id)) {
          pending.get(d.id)(d.measured);
          pending.delete(d.id);
        }
      };
      if (opts.monitor) gateNode.port.postMessage({ monitor: true });
    }
    return {
      stream: dest.stream,
      track,
      hasGate: !!gateNode,
      resume: () => ctx.resume().catch(() => {}),
      setThreshold(db) {
        if (gateNode) gateNode.port.postMessage({ thresholdDb: db });
      },
      onLevel(cb) {
        levelCb = cb;
      },
      /** Замер уровня (после шумодава) за ms миллисекунд: {maxDb, p95Db}. */
      measure(ms) {
        if (!gateNode) return Promise.reject(new Error("нет гейта"));
        const id = ++seq;
        return new Promise((resolve) => {
          pending.set(id, resolve);
          gateNode.port.postMessage({ measure: ms, id });
        });
      },
      close() {
        for (const n of nodes) {
          try {
            n.disconnect();
          } catch {}
          if (n.destroy) n.destroy();
        if (n.stop && n instanceof OscillatorNode) n.stop();
        }
        track.stop();
        ctx.close().catch(() => {});
      },
    };
  } catch (e) {
    ctx.close().catch(() => {});
    throw e;
  }
}
