// «Что нового»: один раз показываем после обновления тем, кто уже бывал на сервере.
// Новую версию — добавить запись в начало NEWS (id растёт: дата обновления).
import { h, lsGet, lsSet } from "./util.js";
import { icon } from "./icons.js";

const SEEN_KEY = "z228.seenNews";

export const NEWS = [
  {
    id: "2026-11-08",
    title: "Большое обновление",
    subtitle: "Собрали ваши предложения из #Предложения",
    items: [
      { icon: "reply", color: "#22d3ee", title: "Личные сообщения", text: "Клик по нику → «Написать сообщение». Переписку видите только вы двое." },
      { icon: "smile", color: "#a78bfa", title: "ИИ прямо в чате", text: "Напишите @ai или /ai и вопрос — ответит коротко, по делу и с подколом. Нужна роль с правом «Звать ИИ»." },
      { icon: "users", color: "#34d399", title: "Кто был в сети", text: "Внизу списка участников — кто заходил и когда, даже если сейчас не в сети." },
      { icon: "plusCircle", color: "#fbbf24", title: "Разовые и временные ссылки", text: "Ссылка на одного человека или на час, день, неделю — истёкшие сами перестают пускать." },
      { icon: "crown", color: "#f472b6", title: "Роли с правами и баны", text: "Ролям можно выдать права — от «Звать ИИ» до полного админа. Появился бан." },
      { icon: "download", color: "#60a5fa", title: "Приложение для Windows", text: "Рация на клавише поверх игр, трей, автообновление. Меню сервера → «Приложение для Windows»." },
    ],
  },
];

/**
 * Показывает новинки, которых человек ещё не видел. returning — человек уже бывал здесь до этой загрузки
 * (новичкам не показываем, просто запоминаем, что всё «видено»).
 */
export function maybeShowWhatsNew(returning) {
  if (!NEWS.length) return;
  const seen = lsGet(SEEN_KEY, "");
  const fresh = NEWS.filter((n) => n.id > seen);
  lsSet(SEEN_KEY, NEWS[0].id);
  if (!fresh.length || !returning) return;
  showWhatsNew(fresh);
}

export function showWhatsNew(list = NEWS.slice(0, 1)) {
  const back = h("div.modal-backdrop.whatsnew-back");
  const close = () => back.remove();
  const items = list.flatMap((n) => n.items);
  const top = list[0];
  const card = h(
    "div.whatsnew",
    { onclick: (e) => e.stopPropagation() },
    h(
      "div.whatsnew-hero",
      null,
      h("div.whatsnew-dots", { "aria-hidden": "true" }, ...Array.from({ length: 14 }, (_, i) => h("i", { style: `--i:${i}` }))),
      h("img.whatsnew-logo", { src: "/logo.png", alt: "" }),
      h("div.whatsnew-kicker", null, "Что нового"),
      h("h2", null, top.title),
      top.subtitle ? h("p", null, top.subtitle) : null
    ),
    h(
      "div.whatsnew-grid",
      null,
      items.map((it, i) =>
        h(
          "div.whatsnew-item",
          { style: `--c:${it.color};animation-delay:${80 + i * 60}ms` },
          h("span.whatsnew-icon", { html: icon(it.icon, 22) }),
          h("div", null, h("b", null, it.title), h("span", null, it.text))
        )
      )
    ),
    h("div.whatsnew-foot", null, h("button.btn.btn-primary", { onclick: close }, "Круто, погнали"))
  );
  back.append(card);
  back.onclick = close;
  back.addEventListener("keydown", (e) => e.key === "Escape" && close());
  document.body.append(back);
  card.querySelector("button").focus({ preventScroll: true });
}
