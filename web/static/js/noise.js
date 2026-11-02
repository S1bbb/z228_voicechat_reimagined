// Шумодав RNNoise (нейросеть, как в Jitsi/OBS) в AudioWorklet: убирает клавиатуру, вентилятор, улицу.
// Нагрузка — порядка процента одного ядра. Файлы лежат в /vendor/noise-suppressor, внешних запросов нет.
import { RnnoiseWorkletNode, loadRnnoise } from "../vendor/noise-suppressor/index.js";

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
 * Пропускает поток микрофона через RNNoise. Возвращает обработанный поток/трек и close(),
 * который освобождает AudioContext (сам микрофон не останавливает).
 */
export async function suppressNoise(rawStream) {
  if (!noiseSuppressionSupported()) throw new Error("AudioWorklet не поддерживается");
  // RNNoise работает на 48 кГц — заводим отдельный контекст с этой частотой
  const ctx = new AudioContext({ sampleRate: 48000, latencyHint: "interactive" });
  let node = null;
  let src = null;
  try {
    const wasmBinary = await loadWasm();
    await ctx.audioWorklet.addModule(BASE + "rnnoise-worklet.js");
    src = ctx.createMediaStreamSource(rawStream);
    node = new RnnoiseWorkletNode(ctx, { wasmBinary, maxChannels: 1 });
    const dest = ctx.createMediaStreamDestination();
    dest.channelCount = 1;
    src.connect(node).connect(dest);
    if (ctx.state === "suspended") await ctx.resume().catch(() => {});
    const track = dest.stream.getAudioTracks()[0];
    return {
      stream: dest.stream,
      track,
      close() {
        try {
          node.destroy();
          src.disconnect();
          node.disconnect();
        } catch {}
        track.stop();
        ctx.close().catch(() => {});
      },
    };
  } catch (e) {
    ctx.close().catch(() => {});
    throw e;
  }
}
