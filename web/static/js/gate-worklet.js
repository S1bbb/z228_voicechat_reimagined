// Шумовой гейт (AudioWorklet): пропускает звук громче порога, остальное глушит.
// Плавные открытие/закрытие (без щелчков), удержание, чтобы не резать концы слов,
// и гистерезис 6 дБ, чтобы гейт не «дребезжал» на границе.
//
// Сообщения в port:
//   {thresholdDb}      — новый порог
//   {monitor: true}    — присылать {level, open} ~30 раз в секунду (для индикатора в настройках)
//   {measure: ms, id}  — замерить шум: в ответ {measured: {maxDb, p95Db}, id}

const BLOCK = 128;
const HYSTERESIS_DB = 6;

const toDb = (x) => (x > 1e-9 ? 20 * Math.log10(x) : -180);

class GateProcessor extends AudioWorkletProcessor {
  constructor(options) {
    super();
    const p = (options && options.processorOptions) || {};
    this.setThreshold(p.thresholdDb ?? -50);
    this.holdBlocks = Math.ceil(((p.holdMs ?? 250) / 1000) * (sampleRate / BLOCK));
    this.attack = 1 - Math.exp(-1 / (sampleRate * 0.005)); // ~5 мс
    this.release = 1 - Math.exp(-1 / (sampleRate * 0.08)); // ~80 мс
    this.gain = 0;
    this.open = false;
    this.hold = 0;
    this.monitor = false;
    this.monitorEvery = Math.max(1, Math.round(sampleRate / BLOCK / 30));
    this.monitorTick = 0;
    this.measure = null;
    this.port.onmessage = (e) => {
      const d = e.data || {};
      if (d.thresholdDb != null) this.setThreshold(d.thresholdDb);
      if (d.monitor != null) this.monitor = !!d.monitor;
      if (d.measure) this.measure = { left: Math.ceil((d.measure / 1000) * (sampleRate / BLOCK)), values: [], id: d.id };
    };
  }

  setThreshold(db) {
    this.openLin = 10 ** (db / 20);
    this.closeLin = 10 ** ((db - HYSTERESIS_DB) / 20);
  }

  process(inputs, outputs) {
    const input = inputs[0];
    const output = outputs[0];
    if (!input || !input.length || !input[0]) return true;
    const x = input[0];
    let sum = 0;
    for (let i = 0; i < x.length; i++) sum += x[i] * x[i];
    const rms = Math.sqrt(sum / x.length);

    if (rms > this.openLin) {
      this.open = true;
      this.hold = this.holdBlocks;
    } else if (this.open) {
      if (rms < this.closeLin) {
        if (--this.hold <= 0) this.open = false;
      } else {
        this.hold = this.holdBlocks;
      }
    }

    const target = this.open ? 1 : 0;
    const coef = target > this.gain ? this.attack : this.release;
    let g = this.gain;
    const n = x.length;
    const gains = this.gains && this.gains.length === n ? this.gains : (this.gains = new Float32Array(n));
    for (let i = 0; i < n; i++) {
      g += (target - g) * coef;
      gains[i] = g;
    }
    this.gain = g;
    for (let ch = 0; ch < output.length; ch++) {
      const inp = input[ch] || x;
      const out = output[ch];
      for (let i = 0; i < n; i++) out[i] = inp[i] * gains[i];
    }

    if (this.measure) {
      this.measure.values.push(rms);
      if (--this.measure.left <= 0) {
        const v = this.measure.values.sort((a, b) => a - b);
        this.port.postMessage({
          measured: { maxDb: toDb(v[v.length - 1] || 0), p95Db: toDb(v[Math.floor(v.length * 0.95)] || 0) },
          id: this.measure.id,
        });
        this.measure = null;
      }
    }
    if (this.monitor) {
      // показываем максимум за интервал — короткие щелчки клавиатуры не проскочат мимо индикатора
      this.monitorMax = Math.max(this.monitorMax || 0, rms);
      if (++this.monitorTick >= this.monitorEvery) {
        this.port.postMessage({ level: toDb(this.monitorMax), open: this.open });
        this.monitorTick = 0;
        this.monitorMax = 0;
      }
    }
    return true;
  }
}

registerProcessor("z228-gate", GateProcessor);
