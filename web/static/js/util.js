// Мелкие помощники: DOM, время, аватарки, markdown в стиле Discord.

export const $ = (sel, root = document) => root.querySelector(sel);
export const $$ = (sel, root = document) => Array.from(root.querySelectorAll(sel));

/** h("div.cls#id", {attrs}, ...children) — компактное создание элементов. */
export function h(tag, attrs, ...children) {
  const m = tag.match(/^([a-z0-9-]+)?((?:[.#][\w-]+)*)$/i);
  const el = document.createElement((m && m[1]) || "div");
  if (m && m[2]) {
    for (const part of m[2].match(/[.#][\w-]+/g)) {
      if (part[0] === ".") el.classList.add(part.slice(1));
      else el.id = part.slice(1);
    }
  }
  if (attrs) {
    for (const [k, v] of Object.entries(attrs)) {
      if (v == null || v === false) continue;
      if (k.startsWith("on") && typeof v === "function") el.addEventListener(k.slice(2).toLowerCase(), v);
      else if (k === "html") el.innerHTML = v;
      else if (k === "text") el.textContent = v;
      else if (k === "style" && typeof v === "object") Object.assign(el.style, v);
      else if (k === "dataset") Object.assign(el.dataset, v);
      else if (v === true) el.setAttribute(k, "");
      else el.setAttribute(k, String(v));
    }
  }
  for (const c of children.flat()) {
    if (c == null || c === false) continue;
    el.append(c instanceof Node ? c : document.createTextNode(String(c)));
  }
  return el;
}

export function escapeHtml(s) {
  return String(s).replace(/[&<>"']/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" })[c]);
}

export function lsGet(key, def) {
  try {
    const v = localStorage.getItem(key);
    return v == null ? def : JSON.parse(v);
  } catch {
    return def;
  }
}

export function lsSet(key, value) {
  try {
    localStorage.setItem(key, JSON.stringify(value));
  } catch {}
}

export function randomId(n = 16) {
  const a = new Uint8Array(n);
  crypto.getRandomValues(a);
  return Array.from(a, (b) => b.toString(16).padStart(2, "0")).join("");
}

// ---------- аватарки ----------

const AVATAR_COLORS = ["#0891b2", "#3ba55c", "#faa61a", "#ed4245", "#eb459e", "#1abc9c", "#e67e22", "#9b59b6", "#3498db", "#7f8c8d"];

export function hashString(s) {
  let x = 2166136261;
  for (let i = 0; i < s.length; i++) {
    x ^= s.charCodeAt(i);
    x = Math.imul(x, 16777619);
  }
  return x >>> 0;
}

export function avatarColor(nick) {
  return AVATAR_COLORS[hashString(String(nick).toLowerCase()) % AVATAR_COLORS.length];
}

export function avatar(nick, size = 40, extraClass = "") {
  const letter = Array.from(String(nick).trim())[0] || "?";
  return h(
    "div.avatar" + (extraClass ? "." + extraClass : ""),
    { style: { width: size + "px", height: size + "px", background: avatarColor(nick), fontSize: Math.round(size * 0.42) + "px" } },
    letter.toUpperCase()
  );
}

// ---------- время ----------

const timeFmt = new Intl.DateTimeFormat("ru-RU", { hour: "2-digit", minute: "2-digit" });
const dateFmt = new Intl.DateTimeFormat("ru-RU", { day: "numeric", month: "long", year: "numeric" });
const fullFmt = new Intl.DateTimeFormat("ru-RU", { dateStyle: "full", timeStyle: "short" });

export function fmtTime(ms) {
  return timeFmt.format(new Date(ms));
}

export function fmtFull(ms) {
  return fullFmt.format(new Date(ms));
}

export function fmtDay(ms) {
  return dateFmt.format(new Date(ms));
}

export function fmtStamp(ms) {
  const d = new Date(ms);
  const now = new Date();
  const yesterday = new Date(now);
  yesterday.setDate(now.getDate() - 1);
  if (sameDay(d, now)) return "Сегодня, в " + fmtTime(ms);
  if (sameDay(d, yesterday)) return "Вчера, в " + fmtTime(ms);
  return d.toLocaleDateString("ru-RU") + " " + fmtTime(ms);
}

export function sameDay(a, b) {
  a = new Date(a);
  b = new Date(b);
  return a.getFullYear() === b.getFullYear() && a.getMonth() === b.getMonth() && a.getDate() === b.getDate();
}

export function fmtSize(n) {
  if (n < 1024) return n + " Б";
  if (n < 1024 * 1024) return (n / 1024).toFixed(1) + " КБ";
  return (n / 1024 / 1024).toFixed(1) + " МБ";
}

// ---------- markdown ----------

/**
 * Discord-подобная разметка: ```блоки```, `код`, **жирный**, *курсив*, __подчёркнутый__,
 * ~~зачёркнутый~~, ||спойлер||, > цитаты, ссылки и @упоминания.
 * На вход — сырой текст, на выход — безопасный HTML.
 */
export function renderMarkdown(text, { nicknames = [], me = "" } = {}) {
  const blocks = [];
  // 1) вырезаем блоки кода, чтобы внутри них ничего не форматировать
  let src = String(text).replace(/```(?:([\w+-]{1,20})\n)?([\s\S]*?)```/g, (_, lang, code) => {
    blocks.push(`<pre class="md-pre"><code>${escapeHtml(code.replace(/^\n+|\n+$/g, ""))}</code></pre>`);
    return `\u0000B${blocks.length - 1}\u0000`;
  });
  const inlines = [];
  src = src.replace(/`([^`\n]+)`/g, (_, code) => {
    inlines.push(`<code class="md-code">${escapeHtml(code)}</code>`);
    return `\u0000I${inlines.length - 1}\u0000`;
  });

  // ссылки тоже прячем, чтобы _ и * внутри URL не превратились в курсив
  const links = [];
  src = src.replace(/\bhttps?:\/\/[^\s<]+[^\s<.,:;"')\]!?]/g, (url) => {
    links.push(`<a href="${escapeHtml(url)}" target="_blank" rel="noopener noreferrer">${escapeHtml(url)}</a>`);
    return `\u0000L${links.length - 1}\u0000`;
  });

  let html = escapeHtml(src);

  html = html
    .replace(/\*\*([^*\n][\s\S]*?)\*\*/g, "<strong>$1</strong>")
    .replace(/__([^_\n][\s\S]*?)__/g, "<u>$1</u>")
    .replace(/(^|[^*\w])\*([^*\s][^*\n]*?)\*(?!\w)/g, "$1<em>$2</em>")
    .replace(/(^|[^_\w])_([^_\s][^_\n]*?)_(?!\w)/g, "$1<em>$2</em>")
    .replace(/~~([\s\S]+?)~~/g, "<s>$1</s>")
    .replace(/\|\|([\s\S]+?)\|\|/g, '<span class="md-spoiler" tabindex="0">$1</span>');

  // упоминания: сначала известные ники (длинные первыми — ники бывают с пробелами)
  const names = [...new Set(nicknames)].sort((a, b) => b.length - a.length);
  const mentionMarks = [];
  for (const n of names) {
    const esc = escapeHtml(n).replace(/[.*+?^${}()|[\]\\]/g, "\\$&");
    html = html.replace(new RegExp(`@${esc}(?![\\w])`, "gi"), () => {
      mentionMarks.push(`<span class="mention${n.toLowerCase() === me.toLowerCase() ? " me" : ""}">@${escapeHtml(n)}</span>`);
      return `\u0000M${mentionMarks.length - 1}\u0000`;
    });
  }
  html = html.replace(/@(everyone|here)\b/g, '<span class="mention me">@$1</span>');
  html = html.replace(/\u0000M(\d+)\u0000/g, (_, i) => mentionMarks[+i]);

  // цитаты: строки, начинающиеся с "> "
  html = html
    .split("\n")
    .map((line) => (line.startsWith("&gt; ") ? `<span class="md-quote">${line.slice(5)}</span>` : line))
    .join("\n")
    .replace(/<\/span>\n<span class="md-quote">/g, "<br>");

  html = html.replace(/\n/g, "<br>");
  html = html.replace(/\u0000L(\d+)\u0000/g, (_, i) => links[+i]);
  html = html.replace(/\u0000I(\d+)\u0000/g, (_, i) => inlines[+i]);
  html = html.replace(/\u0000B(\d+)\u0000/g, (_, i) => blocks[+i]);
  return html;
}

/** Есть ли в тексте упоминание пользователя me (или @everyone/@here). */
export function mentionsMe(text, me) {
  if (!me) return false;
  const t = String(text).toLowerCase();
  return t.includes("@" + me.toLowerCase()) || /@(everyone|here)\b/.test(t);
}

/** Сообщение состоит только из эмодзи (до 27 штук) — показываем крупно, как в Discord. */
export function isEmojiOnly(text) {
  const t = String(text).trim();
  if (!t || t.length > 200) return false;
  return /^(?:\p{Extended_Pictographic}|\p{Emoji_Component}|‍|️|\s)+$/u.test(t) && !/^[\d#*\s]+$/.test(t);
}
