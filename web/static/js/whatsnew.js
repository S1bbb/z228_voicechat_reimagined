// «Что нового»: один раз показываем после обновления тем, кто уже бывал на сервере.
// Новую версию — добавить запись в начало NEWS (id растёт: дата обновления).
import { h, lsGet, lsSet } from "./util.js";
import { icon } from "./icons.js";

const SEEN_KEY = "z228.seenNews";

export const NEWS = [
  {
    id: "2026-11-09b",
    title: "Статистика и умный ИИ",
    subtitle: "Кто сколько сидит в голосовых — и бот, который всё помнит",
    items: [
      { icon: "chart", color: "#22d3ee", title: "Кто где сидел", text: "Меню сервера → «Статистика голоса»: топ по времени за день, неделю, месяц и по каналам." },
      { icon: "reply", color: "#a78bfa", title: "«@ai что я пропустил»", text: "Бот перескажет последние ~200 сообщений: кто что предлагал, до чего договорились, главные срачи." },
      { icon: "crown", color: "#f472b6", title: "Память", text: "«@ai запомни, что Вася вечно умирает первым» — и бот будет это помнить. «@ai забудь про Васю» — сотрёт." },
      { icon: "screen", color: "#34d399", title: "Бот видит картинки", text: "«@ai что на скрине» — смотрит картинку из сообщения, из ответа или свежую выше. Админ подключает модель со зрением в «ИИ»." },
    ],
  },
  {
    id: "2026-11-09",
    title: "Мелкие улучшения",
    subtitle: "По вашим сообщениям в чате",
    items: [
      { icon: "expand", color: "#22d3ee", title: "Просмотр фото", text: "Стрелки ← → листают картинки канала, колесо или клик — приближение, перетаскиванием двигаете увеличенное фото." },
      { icon: "smile", color: "#fbbf24", title: "Кнопка смайликов", text: "Снова открывает выбор эмодзи — в поле ввода и в реакциях." },
      { icon: "crown", color: "#f472b6", title: "Цвета ролей", text: "Готовые цвета в один клик, своя палитра — в последнем кружке." },
      { icon: "gear", color: "#a78bfa", title: "Ровные заголовки настроек", text: "Разделы меню настроек выглядят одинаково во всех дизайнах." },
    ],
  },
  {
    id: "2026-11-08b",
    title: "Новые дизайны",
    subtitle: "Выберите, как выглядит чат: Настройки → Внешний вид → Дизайн",
    items: [
      { icon: "screen", color: "#a78bfa", title: "Графит 2.0", text: "Та же раскладка, но чище: мягче контраст, аккуратнее выделение каналов и реакции-пилюли." },
      { icon: "expand", color: "#22d3ee", title: "Острова", text: "Панели — отдельными скруглёнными карточками, мягкие формы. Классический дизайн никуда не делся." },
      { icon: "crown", color: "#f472b6", title: "Админка по полочкам", text: "Ключи, участники и баны, роли, каналы, ИИ и копии — отдельными пунктами в меню настроек." },
      { icon: "edit", color: "#34d399", title: "Починили ввод в админке", text: "В поля пароля, ролей и ключей снова можно печатать." },
    ],
  },
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
