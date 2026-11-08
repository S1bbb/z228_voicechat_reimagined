// Фильтры голоса: встраиваются последним звеном в цепочку микрофона (после шумодава и гейта).
// Всё считается у того, кто говорит; сервер пересылает уже изменённый голос, нагрузки на него нет.
// Робот, рация, эхо, под водой — встроенные узлы Web Audio; сдвиг тона — свой маленький worklet.

export const VOICE_FILTERS = [
  ["none", "Без фильтра", "🎙️"],
  ["robot", "Робот", "🤖"],
  ["radio", "Рация", "📻"],
  ["chipmunk", "Бурундук", "🐿️"],
  ["demon", "Демон", "😈"],
  ["echo", "Эхо", "🏔️"],
  ["underwater", "Под водой", "🫧"],
];

export const needsFilter = (name) => !!name && name !== "none" && VOICE_FILTERS.some(([id]) => id === name);

// мягкое ограничение (перегруз «как в рации», заодно не даёт клиппинга)
function softClip(ctx, drive) {
  const ws = ctx.createWaveShaper();
  const n = 1024;
  const curve = new Float32Array(n);
  for (let i = 0; i < n; i++) {
    const x = (i / (n - 1)) * 2 - 1;
    curve[i] = Math.tanh(x * drive) / Math.tanh(drive);
  }
  ws.curve = curve;
  ws.oversample = "2x";
  return ws;
}

async function pitchNode(ctx, ratio) {
  await ctx.audioWorklet.addModule("/js/pitch-worklet.js");
  return new AudioWorkletNode(ctx, "z228-pitch", {
    numberOfInputs: 1,
    numberOfOutputs: 1,
    outputChannelCount: [1],
    processorOptions: { ratio, windowMs: 40 },
  });
}

/**
 * Подключает фильтр к input. Возвращает {output, nodes} — nodes закрываются вместе с цепочкой.
 */
export async function applyVoiceFilter(ctx, input, name) {
  const nodes = [];
  const add = (n) => (nodes.push(n), n);
  const out = add(ctx.createGain());
  switch (name) {
    case "robot": {
      // кольцевая модуляция + короткая гребёнка (металлический призвук)
      const ring = add(ctx.createGain());
      ring.gain.value = 0;
      const osc = add(ctx.createOscillator());
      osc.frequency.value = 60;
      osc.connect(ring.gain);
      osc.start();
      input.connect(ring);
      const comb = add(ctx.createDelay(0.05));
      comb.delayTime.value = 0.006;
      const fb = add(ctx.createGain());
      fb.gain.value = 0.55;
      ring.connect(comb);
      comb.connect(fb);
      fb.connect(comb);
      ring.connect(out);
      comb.connect(out);
      out.gain.value = 1.4;
      break;
    }
    case "radio": {
      const hp = add(ctx.createBiquadFilter());
      hp.type = "highpass";
      hp.frequency.value = 450;
      const lp = add(ctx.createBiquadFilter());
      lp.type = "lowpass";
      lp.frequency.value = 2800;
      const pre = add(ctx.createGain());
      pre.gain.value = 3;
      const clip = add(softClip(ctx, 2.5));
      input.connect(hp).connect(lp).connect(pre).connect(clip).connect(out);
      out.gain.value = 0.8;
      break;
    }
    case "chipmunk":
    case "demon": {
      const p = add(await pitchNode(ctx, name === "chipmunk" ? 1.6 : 0.68));
      if (name === "demon") {
        // демону — бас и лёгкий перегруз
        const low = add(ctx.createBiquadFilter());
        low.type = "lowshelf";
        low.frequency.value = 220;
        low.gain.value = 6;
        const clip = add(softClip(ctx, 1.6));
        input.connect(p).connect(low).connect(clip).connect(out);
      } else {
        input.connect(p).connect(out);
        out.gain.value = 1.4; // две головки вперемешку дают ~3 дБ тише
      }
      break;
    }
    case "echo": {
      const d = add(ctx.createDelay(1));
      d.delayTime.value = 0.26;
      const fb = add(ctx.createGain());
      fb.gain.value = 0.38;
      const wet = add(ctx.createGain());
      wet.gain.value = 0.55;
      const tone = add(ctx.createBiquadFilter()); // каждое повторение глуше
      tone.type = "lowpass";
      tone.frequency.value = 3000;
      input.connect(out);
      input.connect(d);
      d.connect(tone).connect(fb).connect(d);
      d.connect(wet).connect(out);
      break;
    }
    case "underwater": {
      // глухо + «плавающий» фильтр + лёгкое дрожание высоты
      const lp = add(ctx.createBiquadFilter());
      lp.type = "lowpass";
      lp.frequency.value = 650;
      lp.Q.value = 4;
      const lfo = add(ctx.createOscillator());
      lfo.frequency.value = 0.7;
      const lfoAmt = add(ctx.createGain());
      lfoAmt.gain.value = 250;
      lfo.connect(lfoAmt).connect(lp.frequency);
      const wob = add(ctx.createDelay(0.05));
      wob.delayTime.value = 0.012;
      const vib = add(ctx.createOscillator());
      vib.frequency.value = 4.5;
      const vibAmt = add(ctx.createGain());
      vibAmt.gain.value = 0.003;
      vib.connect(vibAmt).connect(wob.delayTime);
      lfo.start();
      vib.start();
      input.connect(wob).connect(lp).connect(out);
      out.gain.value = 1.3;
      break;
    }
    default:
      input.connect(out);
      return { output: out, nodes };
  }
  // общий лимитер: громкие фильтры не хрипят и не перекрикивают остальных
  const lim = add(ctx.createDynamicsCompressor());
  lim.threshold.value = -6;
  lim.knee.value = 4;
  lim.ratio.value = 12;
  lim.attack.value = 0.003;
  lim.release.value = 0.1;
  out.connect(lim);
  return { output: lim, nodes };
}
