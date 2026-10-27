// Книжный клуб — Telegram Mini App. Plain JS, no build step.
// Every piece of user data goes into the DOM through textContent (see h()), never innerHTML.
(() => {
  'use strict';

  const tg = window.Telegram && window.Telegram.WebApp;
  const inTelegram = !!(tg && tg.initData);
  // Outside Telegram (local testing with DEV_MODE=1 on the server): ?dev=2:Маша picks the fake user.
  const devUser = new URLSearchParams(location.search).get('dev') || '1:Разработчик';

  const $view = document.getElementById('view');
  const $tabs = document.getElementById('tabs');
  const $back = document.getElementById('back');
  const $footer = document.getElementById('footer');
  const $toast = document.getElementById('toast');

  let me = null;      // { id, name, isAdmin }
  let renderSeq = 0;  // bumps on every navigation; late async results for old screens are dropped

  // ---------------------------------------------------------------- Telegram helpers

  const supports = (v) => !!(tg && tg.isVersionAtLeast && tg.isVersionAtLeast(v));

  function haptic(kind) {
    if (!supports('6.1')) return;
    if (kind === 'select') tg.HapticFeedback.selectionChanged();
    else tg.HapticFeedback.notificationOccurred(kind);
  }

  function confirmDialog(message) {
    return new Promise((resolve) => {
      if (inTelegram && supports('6.2')) tg.showConfirm(message, (ok) => resolve(!!ok));
      else resolve(window.confirm(message));
    });
  }

  function openExternal(url) {
    if (inTelegram && supports('6.1')) tg.openLink(url);
    else window.open(url, '_blank', 'noopener');
  }

  function syncColorScheme() {
    if (tg && tg.colorScheme) document.documentElement.style.colorScheme = tg.colorScheme;
  }

  // ---------------------------------------------------------------- API

  class ApiError extends Error {
    constructor(status, message) {
      super(message);
      this.status = status;
    }
  }

  async function api(method, path, body) {
    // Header values must be Latin-1, so the dev user (usually Cyrillic) is percent-encoded.
    const headers = { Authorization: inTelegram ? 'tma ' + tg.initData : 'dev ' + encodeURIComponent(devUser) };
    if (body !== undefined) headers['Content-Type'] = 'application/json';

    let res;
    try {
      res = await fetch(path, { method, headers, body: body === undefined ? undefined : JSON.stringify(body) });
    } catch (_) {
      throw new ApiError(0, 'Нет связи с сервером. Проверьте интернет и попробуйте ещё раз.');
    }
    if (res.status === 204) return null;

    let data = null;
    try { data = await res.json(); } catch (_) { /* not JSON */ }
    if (!res.ok) throw new ApiError(res.status, (data && data.error) || 'Что-то пошло не так. Попробуйте ещё раз.');
    return data;
  }

  // ---------------------------------------------------------------- DOM helpers

  // h('a', { class: 'x', href: '#', onclick: fn }, 'text', child, [more]) — strings become text nodes.
  function h(tag, props, ...children) {
    const el = document.createElement(tag);
    let value;
    for (const [k, v] of Object.entries(props || {})) {
      if (v == null || v === false) continue;
      if (k === 'class') el.className = v;
      else if (k === 'value') value = v;
      else if (k.startsWith('on')) el.addEventListener(k.slice(2), v);
      else el.setAttribute(k, v === true ? '' : String(v));
    }
    for (const c of children.flat(Infinity)) {
      if (c == null || c === false || c === '') continue;
      el.append(c instanceof Node ? c : document.createTextNode(String(c)));
    }
    if (value !== undefined) el.value = value; // after children, so <select> already has its options
    return el;
  }

  function field(label, control, hint) {
    return h('label', { class: 'field' },
      h('span', { class: 'field-label' }, label),
      control,
      hint && h('span', { class: 'field-hint' }, hint));
  }

  function errorLine() {
    const el = h('p', { class: 'form-error', role: 'alert', hidden: true });
    return {
      el,
      show(msg) { el.textContent = msg; el.hidden = false; haptic('error'); },
      hide() { el.hidden = true; },
    };
  }

  function busy(btn, on, label) {
    if (on) {
      btn.dataset.label = btn.textContent;
      btn.textContent = label || 'Сохраняю…';
      btn.disabled = true;
    } else {
      btn.textContent = btn.dataset.label || btn.textContent;
      btn.disabled = false;
    }
  }

  let toastTimer;
  function toast(msg) {
    $toast.textContent = msg;
    $toast.classList.add('show');
    clearTimeout(toastTimer);
    toastTimer = setTimeout(() => $toast.classList.remove('show'), 2400);
  }

  function empty(title, text) {
    return h('div', { class: 'card empty' },
      h('div', { class: 'empty-title' }, title),
      text && h('p', { class: 'hint' }, text));
  }

  function errorBox(err) {
    let msg = err.message || 'Что-то пошло не так.';
    if (!inTelegram && err.status === 401) {
      msg = 'Это приложение открывается из Telegram — через кнопку в группе книжного клуба.';
    }
    return h('div', { class: 'card empty' },
      h('div', { class: 'empty-title' }, 'Не получилось'),
      h('p', null, msg),
      err.status !== 403 && h('button', { type: 'button', class: 'btn btn-secondary', onclick: () => refresh() }, 'Попробовать ещё раз'));
  }

  // Shows "Загрузка…" only if loading takes a moment, so fast screens don't flicker.
  async function load(seq, build) {
    const spinner = setTimeout(() => {
      if (seq === renderSeq) $view.replaceChildren(h('p', { class: 'loading' }, 'Загрузка…'));
    }, 250);
    try {
      const nodes = await build();
      if (seq === renderSeq) $view.replaceChildren(...[].concat(nodes).flat().filter(Boolean));
    } catch (err) {
      if (seq === renderSeq) $view.replaceChildren(errorBox(err));
    } finally {
      clearTimeout(spinner);
    }
  }

  // ---------------------------------------------------------------- formatting

  function plural(n, one, few, many) {
    const m10 = n % 10, m100 = n % 100;
    if (m10 === 1 && m100 !== 11) return one;
    if (m10 >= 2 && m10 <= 4 && (m100 < 12 || m100 > 14)) return few;
    return many;
  }

  const reviewsWord = (n) => `${n} ${plural(n, 'отзыв', 'отзыва', 'отзывов')}`;
  const fmtAvg = (x) => x.toLocaleString('ru-RU', { minimumFractionDigits: 1, maximumFractionDigits: 1 });
  const capitalize = (s) => s.charAt(0).toUpperCase() + s.slice(1);
  const pad = (n) => String(n).padStart(2, '0');

  function fmtDate(iso) {
    const d = new Date(iso);
    const opts = { day: 'numeric', month: 'long' };
    if (d.getFullYear() !== new Date().getFullYear()) opts.year = 'numeric';
    return d.toLocaleDateString('ru-RU', opts);
  }

  function fmtDay(d) {
    const opts = { weekday: 'long', day: 'numeric', month: 'long' };
    if (d.getFullYear() !== new Date().getFullYear()) opts.year = 'numeric';
    return capitalize(d.toLocaleDateString('ru-RU', opts));
  }

  const fmtTime = (d) => d.toLocaleTimeString('ru-RU', { hour: '2-digit', minute: '2-digit' });

  function daysUntil(d) {
    const a = new Date(); a.setHours(0, 0, 0, 0);
    const b = new Date(d); b.setHours(0, 0, 0, 0);
    return Math.round((b - a) / 864e5);
  }

  function whenLabel(d) {
    const n = daysUntil(d);
    if (n < 0) return '';
    if (n === 0) return 'Сегодня';
    if (n === 1) return 'Завтра';
    if (n === 2) return 'Послезавтра';
    return `Через ${n} ${plural(n, 'день', 'дня', 'дней')}`;
  }

  const isUrl = (s) => /^https?:\/\/\S+$/i.test(s);

  // ---------------------------------------------------------------- stars

  function stars(value, extraClass) {
    const n = Math.max(0, Math.min(5, Math.round(value)));
    return h('span', { class: 'stars ' + (extraClass || ''), role: 'img', 'aria-label': `Оценка ${fmtAvg(value)} из 5` },
      '★'.repeat(n),
      n < 5 && h('span', { class: 'stars-off' }, '★'.repeat(5 - n)));
  }

  const STAR_CAPTIONS = ['Нажмите на звезду, чтобы поставить оценку', 'Не понравилась', 'Так себе', 'Неплохо', 'Хорошая книга', 'Отличная книга!'];

  function starPicker(initial, onChange) {
    let value = initial || 0;
    const caption = h('div', { class: 'star-caption' });
    const buttons = [1, 2, 3, 4, 5].map((i) => h('button', {
      type: 'button', class: 'star-btn', role: 'radio', 'aria-label': `${i} из 5`,
      onclick: () => { value = i; paint(); onChange(i); haptic('select'); },
    }, '★'));
    function paint() {
      buttons.forEach((b, i) => {
        b.classList.toggle('on', i < value);
        b.setAttribute('aria-checked', String(i + 1 === value));
      });
      caption.textContent = STAR_CAPTIONS[value];
    }
    paint();
    return h('div', null, h('div', { class: 'star-picker', role: 'radiogroup', 'aria-label': 'Ваша оценка' }, buttons), caption);
  }

  // ---------------------------------------------------------------- navigation

  const ROOTS = ['#/books', '#/meetings'];

  const routes = [
    [/^#\/books$/, renderBooks],
    [/^#\/books\/new$/, renderAddBook],
    [/^#\/books\/([0-9a-f]+)$/, (seq, m) => renderBook(seq, m[1])],
    [/^#\/meetings$/, renderMeetings],
    [/^#\/meetings\/new$/, (seq) => renderMeetingForm(seq, null)],
    [/^#\/meetings\/([0-9a-f]+)\/edit$/, (seq, m) => renderMeetingForm(seq, m[1])],
  ];

  function goBack() {
    if (history.length > 1) history.back();
    else location.replace('#/books');
  }

  function setChrome(hash) {
    const isRoot = ROOTS.includes(hash);
    $tabs.hidden = !isRoot;
    for (const t of $tabs.querySelectorAll('.tab')) {
      const active = hash === '#/' + t.dataset.tab;
      t.classList.toggle('active', active);
      if (active) t.setAttribute('aria-current', 'page'); else t.removeAttribute('aria-current');
    }
    if (inTelegram && supports('6.1')) {
      if (isRoot) tg.BackButton.hide(); else tg.BackButton.show();
    } else {
      $back.hidden = isRoot;
    }
  }

  function render(resetScroll) {
    const hash = location.hash;
    const route = routes.find(([re]) => re.test(hash));
    if (!route) {
      location.replace('#/books');
      return;
    }
    const seq = ++renderSeq;
    setChrome(hash);
    if (resetScroll) window.scrollTo(0, 0);
    route[1](seq, hash.match(route[0]));
  }

  // refresh re-renders the current screen in place (after saving), keeping the scroll position.
  // Before the user is known it retries the login instead.
  function refresh() {
    if (me) render(false);
    else login();
  }

  // ---------------------------------------------------------------- screens: books

  function renderBooks(seq) {
    load(seq, async () => {
      const { books } = await api('GET', 'api/books');
      const add = h('a', { class: 'btn btn-primary', href: '#/books/new' }, '+ Добавить книгу');
      if (!books.length) {
        return [add, empty('Пока нет ни одной книги', 'Нажмите «Добавить книгу» — и она появится здесь для всего клуба.')];
      }

      const items = books.map((b) => ({ el: bookCard(b), key: `${b.title} ${b.author || ''}`.toLowerCase() }));
      const nodes = [add];

      if (books.length > 6) {
        const nothing = h('p', { class: 'hint center', hidden: true }, 'Ничего не нашлось');
        const search = h('input', {
          type: 'search', class: 'search', placeholder: 'Найти книгу или автора', 'aria-label': 'Поиск по книгам',
          oninput: (e) => {
            const q = e.target.value.trim().toLowerCase();
            let shown = 0;
            for (const it of items) {
              it.el.hidden = !!q && !it.key.includes(q);
              if (!it.el.hidden) shown++;
            }
            nothing.hidden = shown > 0;
          },
        });
        nodes.push(search, nothing);
      }

      nodes.push(h('h2', { class: 'section-title' }, `Книги клуба · ${books.length}`),
        h('div', { class: 'list' }, items.map((it) => it.el)));
      return nodes;
    });
  }

  function bookCard(b) {
    return h('a', { class: 'card book-card', href: '#/books/' + b.id },
      h('div', { class: 'book-title' }, b.title),
      b.author && h('div', { class: 'book-author' }, b.author),
      h('div', { class: 'book-meta' },
        b.rating.count
          ? [stars(b.rating.average), h('span', null, `${fmtAvg(b.rating.average)} · ${reviewsWord(b.rating.count)}`)]
          : h('span', { class: 'hint' }, 'Пока нет отзывов')),
      b.myRating && h('div', { class: 'my-mark' }, `Ваша оценка: ${b.myRating} из 5`));
  }

  function renderAddBook() {
    const title = h('input', { type: 'text', maxlength: 200, autocomplete: 'off', enterkeyhint: 'next' });
    const author = h('input', { type: 'text', maxlength: 200, autocomplete: 'off', enterkeyhint: 'next' });
    const link = h('input', { type: 'url', inputmode: 'url', maxlength: 1000, autocomplete: 'off', placeholder: 'https://…' });
    const note = h('textarea', { rows: 3, maxlength: 1000, placeholder: 'Например: о чём книга или почему стоит прочитать' });
    const err = errorLine();
    const submit = h('button', { type: 'submit', class: 'btn btn-primary' }, 'Добавить книгу');

    const form = h('form', {
      class: 'card form', novalidate: true,
      onsubmit: async (e) => {
        e.preventDefault();
        err.hide();
        if (!title.value.trim()) {
          err.show('Напишите название книги.');
          title.focus();
          return;
        }
        busy(submit, true);
        try {
          const book = await api('POST', 'api/books', { title: title.value, author: author.value, link: link.value, note: note.value });
          haptic('success');
          toast('Книга добавлена');
          location.replace('#/books/' + book.id);
        } catch (ex) {
          err.show(ex.message);
          busy(submit, false);
        }
      },
    },
      h('h1', { class: 'screen-title' }, 'Новая книга'),
      field('Название', title),
      field('Автор', author, 'Можно не заполнять'),
      field('Ссылка на книгу', link, 'Необязательно. Например, страница книги в магазине или библиотеке'),
      field('Пара слов о книге', note, 'Необязательно'),
      err.el,
      submit);

    $view.replaceChildren(form);
    title.focus();
  }

  function renderBook(seq, id) {
    load(seq, async () => {
      const d = await api('GET', 'api/books/' + id);
      const b = d.book;

      const head = h('section', { class: 'card book-head' },
        h('h1', { class: 'book-title big' }, b.title),
        b.author && h('div', { class: 'book-author' }, b.author),
        h('div', { class: 'rating-summary' },
          b.rating.count
            ? [stars(b.rating.average, 'lg'), h('span', { class: 'rating-num' }, fmtAvg(b.rating.average)), h('span', { class: 'hint' }, reviewsWord(b.rating.count))]
            : h('span', { class: 'hint' }, 'Отзывов пока нет — ваш может стать первым')),
        b.note && h('p', { class: 'note' }, b.note),
        b.link && h('button', { type: 'button', class: 'btn btn-secondary', onclick: () => openExternal(b.link) }, 'Открыть книгу по ссылке ↗'),
        h('p', { class: 'hint small added' }, `Добавлено: ${b.addedBy}, ${fmtDate(b.createdAt)}`));

      const nodes = [head, reviewForm(b, d.myReview),
        h('h2', { class: 'section-title' }, d.reviews.length ? `Отзывы участников · ${d.reviews.length}` : 'Отзывы участников')];

      if (d.reviews.length) nodes.push(h('div', { class: 'list' }, d.reviews.map(reviewCard)));
      else nodes.push(h('p', { class: 'hint pad' }, 'Другие участники пока не оставили отзывов.'));

      if (b.canDelete) {
        nodes.push(h('button', {
          type: 'button', class: 'btn btn-danger spaced',
          onclick: async (e) => {
            const warn = b.rating.count ? ` Вместе с ней удалятся отзывы (${b.rating.count}).` : '';
            const btn = e.currentTarget; // currentTarget is null once we await
            if (!(await confirmDialog(`Удалить книгу «${b.title}»?${warn}`))) return;
            busy(btn, true, 'Удаляю…');
            try {
              await api('DELETE', 'api/books/' + b.id);
              toast('Книга удалена');
              goBack();
            } catch (ex) {
              busy(btn, false);
              toast(ex.message);
            }
          },
        }, 'Удалить книгу'));
      }
      return nodes;
    });
  }

  function reviewForm(book, mine) {
    let rating = mine ? mine.rating : 0;
    const text = h('textarea', {
      rows: 4, maxlength: 4000, 'aria-label': 'Текст отзыва',
      placeholder: 'Что понравилось, что нет? Можно оставить пустым — достаточно оценки.',
      value: mine ? mine.text || '' : '',
    });
    const err = errorLine();
    const save = h('button', { type: 'submit', class: 'btn btn-primary' }, mine ? 'Сохранить изменения' : 'Опубликовать отзыв');

    const remove = mine && h('button', {
      type: 'button', class: 'btn-link danger',
      onclick: async () => {
        if (!(await confirmDialog('Удалить ваш отзыв?'))) return;
        try {
          await api('DELETE', 'api/reviews/' + mine.id);
          toast('Отзыв удалён');
          refresh();
        } catch (ex) {
          err.show(ex.message);
        }
      },
    }, 'Удалить мой отзыв');

    return h('form', {
      class: 'card review-form', novalidate: true,
      onsubmit: async (e) => {
        e.preventDefault();
        err.hide();
        if (!rating) {
          err.show('Поставьте оценку — нажмите на одну из звёзд.');
          return;
        }
        busy(save, true);
        try {
          await api('PUT', `api/books/${book.id}/review`, { rating, text: text.value });
          haptic('success');
          toast(mine ? 'Отзыв обновлён' : 'Спасибо за отзыв!');
          refresh();
        } catch (ex) {
          err.show(ex.message);
          busy(save, false);
        }
      },
    },
      h('h2', { class: 'card-title' }, mine ? 'Ваш отзыв' : 'Ваш отзыв о книге'),
      starPicker(rating, (v) => { rating = v; err.hide(); }),
      text, err.el, save, remove);
  }

  function reviewCard(r) {
    const long = r.text && r.text.length > 600;
    const body = r.text && h('p', { class: 'review-text' + (long ? ' clamped' : '') }, r.text);
    const edited = new Date(r.updatedAt) - new Date(r.createdAt) > 60e3;

    const card = h('article', { class: 'card review' },
      h('div', { class: 'review-head' }, h('span', { class: 'review-author' }, r.author), stars(r.rating)),
      body,
      long && h('button', {
        type: 'button', class: 'btn-link',
        onclick: (e) => { body.classList.remove('clamped'); e.currentTarget.remove(); },
      }, 'Читать полностью'),
      h('div', { class: 'review-foot' },
        h('span', { class: 'hint small' }, fmtDate(r.updatedAt) + (edited ? ' · изменён' : '')),
        r.canDelete && h('button', {
          type: 'button', class: 'btn-link danger small',
          onclick: async () => {
            if (!(await confirmDialog(`Удалить отзыв участника ${r.author}?`))) return;
            try {
              await api('DELETE', 'api/reviews/' + r.id);
              toast('Отзыв удалён');
              refresh();
            } catch (ex) {
              toast(ex.message);
            }
          },
        }, 'Удалить')));
    return card;
  }

  // ---------------------------------------------------------------- screens: meetings

  // A meeting stays "upcoming" until 3 hours after it starts, so it's still on top during the evening.
  const STILL_UPCOMING_MS = 3 * 3600e3;

  function renderMeetings(seq) {
    load(seq, async () => {
      const { meetings } = await api('GET', 'api/meetings');
      const now = Date.now();
      const upcoming = meetings.filter((m) => new Date(m.startsAt).getTime() + STILL_UPCOMING_MS >= now);
      const past = meetings.filter((m) => !upcoming.includes(m)).reverse();

      const nodes = [];
      if (me.isAdmin) nodes.push(h('a', { class: 'btn btn-primary', href: '#/meetings/new' }, '+ Назначить встречу'));

      if (!upcoming.length) {
        nodes.push(empty('Ближайших встреч пока нет',
          me.isAdmin ? 'Нажмите «Назначить встречу», чтобы добавить дату.' : 'Как только дату назначат, она появится здесь.'));
      } else {
        const [next, ...rest] = upcoming;
        nodes.push(h('h2', { class: 'section-title' }, 'Ближайшая встреча'), meetingCard(next, true));
        if (rest.length) nodes.push(h('h2', { class: 'section-title' }, 'Потом'), h('div', { class: 'list' }, rest.map((m) => meetingCard(m))));
      }

      if (past.length) {
        nodes.push(h('details', { class: 'past' },
          h('summary', null, `Прошедшие встречи · ${past.length}`),
          h('div', { class: 'list' }, past.map((m) => meetingCard(m)))));
      }
      return nodes;
    });
  }

  function meetingCard(m, featured) {
    const d = new Date(m.startsAt);
    const when = featured ? whenLabel(d) : '';
    const online = m.place && isUrl(m.place);

    return h('article', { class: 'card meeting' + (featured ? ' featured' : '') },
      when && h('div', { class: 'badge' }, when),
      h('div', { class: 'meeting-day' }, fmtDay(d)),
      h('div', { class: 'meeting-time' }, 'в ' + fmtTime(d)),
      m.place && !online && h('div', { class: 'meeting-row' }, h('span', { class: 'meeting-label' }, 'Где'), h('span', null, m.place)),
      m.book && h('a', { class: 'meeting-row meeting-book', href: '#/books/' + m.book.id },
        h('span', { class: 'meeting-label' }, 'Книга'),
        h('span', null, `«${m.book.title}»${m.book.author ? ', ' + m.book.author : ''}`)),
      m.note && h('p', { class: 'note' }, m.note),
      online && h('button', { type: 'button', class: 'btn btn-secondary', onclick: () => openExternal(m.place) }, 'Подключиться онлайн ↗'),
      me.isAdmin && h('a', { class: 'btn btn-secondary', href: `#/meetings/${m.id}/edit` }, 'Изменить'));
  }

  function renderMeetingForm(seq, id) {
    if (!me.isAdmin) {
      $view.replaceChildren(errorBox(new ApiError(403, 'Назначать встречи может только администратор клуба.')));
      return;
    }
    load(seq, async () => {
      const [booksResp, meetingsResp] = await Promise.all([api('GET', 'api/books'), id ? api('GET', 'api/meetings') : null]);
      const current = id ? meetingsResp.meetings.find((x) => x.id === id) : null;
      if (id && !current) throw new ApiError(404, 'Встреча не найдена — возможно, её удалили.');

      const start = current ? new Date(current.startsAt) : defaultStart();
      const date = h('input', { type: 'date', value: `${start.getFullYear()}-${pad(start.getMonth() + 1)}-${pad(start.getDate())}` });
      const time = h('input', { type: 'time', step: 300, value: `${pad(start.getHours())}:${pad(start.getMinutes())}` });
      const place = h('input', { type: 'text', maxlength: 200, placeholder: 'Адрес или ссылка на видеозвонок', value: current ? current.place || '' : '' });
      const book = h('select', { value: current && current.book ? current.book.id : '' },
        h('option', { value: '' }, '— не выбрана —'),
        booksResp.books.map((b) => h('option', { value: b.id }, b.title + (b.author ? ' — ' + b.author : ''))));
      const note = h('textarea', { rows: 3, maxlength: 1000, placeholder: 'Например: прочитать до 10-й главы', value: current ? current.note || '' : '' });
      const err = errorLine();
      const submit = h('button', { type: 'submit', class: 'btn btn-primary' }, current ? 'Сохранить' : 'Назначить встречу');

      const form = h('form', {
        class: 'card form', novalidate: true,
        onsubmit: async (e) => {
          e.preventDefault();
          err.hide();
          const startsAt = new Date(`${date.value}T${time.value}`); // local time of the admin's phone
          if (!date.value || !time.value || isNaN(startsAt)) {
            err.show('Укажите дату и время встречи.');
            return;
          }
          busy(submit, true);
          try {
            const payload = { startsAt: startsAt.toISOString(), place: place.value, bookId: book.value, note: note.value };
            await api(current ? 'PUT' : 'POST', current ? 'api/meetings/' + current.id : 'api/meetings', payload);
            haptic('success');
            toast(current ? 'Встреча обновлена' : 'Встреча назначена');
            goBack();
          } catch (ex) {
            err.show(ex.message);
            busy(submit, false);
          }
        },
      },
        h('h1', { class: 'screen-title' }, current ? 'Изменить встречу' : 'Новая встреча'),
        h('div', { class: 'row-2' }, field('Дата', date), field('Время', time)),
        field('Где', place, 'Необязательно'),
        field('Какую книгу обсуждаем', book),
        field('Комментарий', note, 'Необязательно'),
        err.el,
        submit);

      const nodes = [form];
      if (current) {
        nodes.push(h('button', {
          type: 'button', class: 'btn btn-danger spaced',
          onclick: async (e) => {
            const btn = e.currentTarget; // currentTarget is null once we await
            if (!(await confirmDialog('Удалить эту встречу?'))) return;
            busy(btn, true, 'Удаляю…');
            try {
              await api('DELETE', 'api/meetings/' + current.id);
              toast('Встреча удалена');
              goBack();
            } catch (ex) {
              busy(btn, false);
              toast(ex.message);
            }
          },
        }, 'Удалить встречу'));
      }
      return nodes;
    });
  }

  function defaultStart() {
    const d = new Date();
    d.setDate(d.getDate() + 7);
    d.setHours(18, 0, 0, 0);
    return d;
  }

  // ---------------------------------------------------------------- start

  function setup() {
    if (tg) {
      tg.ready();
      tg.expand();
      syncColorScheme();
      tg.onEvent('themeChanged', syncColorScheme);
      if (supports('6.1')) {
        tg.setHeaderColor('secondary_bg_color');
        tg.setBackgroundColor('secondary_bg_color');
        tg.BackButton.onClick(goBack);
      }
      // Stops the app from closing when someone scrolls up with a swipe — easy to do by accident.
      if (supports('7.7')) tg.disableVerticalSwipes();
    }
    $back.addEventListener('click', goBack);

    // Switching tabs replaces history, so the back gesture never cycles through tabs.
    $tabs.addEventListener('click', (e) => {
      const tab = e.target.closest('.tab');
      if (!tab) return;
      e.preventDefault();
      if (location.hash !== tab.getAttribute('href')) location.replace(tab.getAttribute('href'));
    });

    window.addEventListener('hashchange', () => {
      if (me) render(true);
    });
  }

  async function login() {
    $tabs.hidden = true;
    try {
      me = await api('GET', 'api/me');
    } catch (err) {
      $view.replaceChildren(errorBox(err));
      return;
    }
    $footer.textContent = `Вы вошли как ${me.name} · ID ${me.id}${me.isAdmin ? ' · администратор' : ''}`;
    if (routes.some(([re]) => re.test(location.hash))) render(true);
    else location.replace('#/books');
  }

  setup();
  login();
})();
