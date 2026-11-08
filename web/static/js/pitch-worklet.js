// Сдвиг высоты голоса без изменения темпа: две «читающие головки» по линии задержки,
// которые бегут быстрее/медленнее записи и плавно сменяют друг друга (треугольные окна).
// Дёшево: пара умножений на сэмпл, задержка — около половины окна (~20 мс).
class PitchShifter extends AudioWorkletProcessor {
  constructor(options) {
    super();
    const o = (options && options.processorOptions) || {};
    this.ratio = o.ratio || 1; // 1.5 — выше (бурундук), 0.7 — ниже (демон)
    this.win = Math.round(sampleRate * (o.windowMs || 40) / 1000);
    this.buf = new Float32Array(this.win * 2 + 8);
    this.w = 0;
    this.d = 0; // текущая задержка первой головки, 0..win
    this.port.onmessage = (e) => {
      if (e.data && e.data.ratio) this.ratio = e.data.ratio;
    };
  }

  read(delay) {
    const n = this.buf.length;
    let pos = this.w - delay - 2;
    while (pos < 0) pos += n;
    const i = Math.floor(pos);
    const f = pos - i;
    const a = this.buf[i % n];
    const b = this.buf[(i + 1) % n];
    return a + (b - a) * f;
  }

  process(inputs, outputs) {
    const out = outputs[0] && outputs[0][0];
    if (!out) return true;
    const inp = inputs[0] && inputs[0][0];
    const n = this.buf.length;
    const win = this.win;
    const step = 1 - this.ratio;
    for (let i = 0; i < out.length; i++) {
      this.buf[this.w] = inp ? inp[i] : 0;
      this.d += step;
      if (this.d >= win) this.d -= win;
      else if (this.d < 0) this.d += win;
      const d2 = (this.d + win / 2) % win;
      // треугольные окна со сдвигом в полпериода в сумме дают 1
      const g1 = 1 - Math.abs((2 * this.d) / win - 1);
      const g2 = 1 - g1;
      out[i] = g1 * this.read(this.d) + g2 * this.read(d2);
      this.w = (this.w + 1) % n;
    }
    return true;
  }
}

registerProcessor("z228-pitch", PitchShifter);
