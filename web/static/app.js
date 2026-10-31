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

  let me = null;      // { id, name, appUrl, isAdmin, canManageMeetings, covers }
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

  // ---------------------------------------------------------------- sharing to the club's group
  // t.me/share/url opens Telegram's chat picker with the message filled in; the member picks the
  // club's group and sends it as themselves. It goes through the member's own Telegram, so it works
  // even though our server can't reach the Bot API from Yandex Cloud.
  // It carries only text and a link, so a photo can't be attached. When a review (or its book) has
  // a photo, the link goes to our public page /s/<token> instead, and Telegram shows the photo as
  // the link's preview (see internal/api/share.go).

  const SHARE_TEXT_MAX = 1500; // long texts make the share link too long for some clients

  const canShare = () => !!(me && me.appUrl);

  function cut(str, max) {
    const chars = Array.from(str); // by code points, so an emoji is never split in half
    return chars.length > max ? chars.slice(0, max).join('').trimEnd() + '…' : str;
  }

  const bookLine = (b) => `«${b.title}»` + (b.author ? ` — ${b.author}` : '');
  const starLine = (n) => '★'.repeat(n) + '☆'.repeat(5 - n) + ` ${n} из 5`;

  // The link opens the Mini App right on the book (the front end reads start_param, see login()).
  // previewPath: the server's page with a photo preview, relative to the app. It goes first, so
  // Telegram builds the preview from it. It can't take the member into the app: opened from a
  // browser, the app doesn't know the member came from the club's group and refuses access. So the
  // Mini App link goes into the text as well, and members open the app by it, from the group.
  function shareToGroup(text, bookId, previewPath) {
    const appLink = new URL(me.appUrl);
    appLink.searchParams.set('startapp', 'book_' + bookId);
    let link = appLink;
    if (previewPath) {
      link = new URL(previewPath, location.href);
      text += '\n\nОткрыть в приложении: ' + appLink.toString();
    }
    const url = 'https://t.me/share/url?url=' + encodeURIComponent(link.toString()) + '&text=' + encodeURIComponent(text);
    if (inTelegram && supports('6.1')) tg.openTelegramLink(url);
    else window.open(url, '_blank', 'noopener');
  }

  function shareBook(b) {
    let text = (b.mine ? '📚 Предлагаю почитать: ' : '📚 ') + bookLine(b);
    if (b.note) text += '\n\n' + cut(b.note, SHARE_TEXT_MAX);
    shareToGroup(text, b.id);
  }

  function shareReview(book, review) {
    let text = `✍️ Мой отзыв на ${bookLine(book)}\n${starLine(review.rating)}`;
    if (review.text) text += '\n\n' + cut(review.text, SHARE_TEXT_MAX);
    shareToGroup(text, book.id, review.shareUrl); // the review's photo, else the cover, in the preview
  }

  function syncColorScheme() {
    if (tg && tg.colorScheme) document.documentElement.style.colorScheme = tg.colorScheme;
  }

  // ---------------------------------------------------------------- API

  class ApiError extends Error {
    constructor(status, message, reason) {
      super(message);
      this.status = status;
      this.reason = reason || '';
    }
  }

  // Header values must be Latin-1, so the dev user (usually Cyrillic) is percent-encoded.
  // Not Authorization: Yandex Serverless Containers consume that header and don't pass it on.
  const authHeaders = () => ({ 'X-Telegram-Auth': inTelegram ? 'tma ' + tg.initData : 'dev ' + encodeURIComponent(devUser) });

  // body: an object (sent as JSON) or a Blob (sent as is, e.g. a photo).
  async function api(method, path, body) {
    const headers = authHeaders();
    let payload;
    if (body instanceof Blob) {
      headers['Content-Type'] = body.type || 'application/octet-stream';
      payload = body;
    } else if (body !== undefined) {
      headers['Content-Type'] = 'application/json';
      payload = JSON.stringify(body);
    }

    let res;
    try {
      res = await fetch(path, { method, headers, body: payload });
    } catch (_) {
      throw new ApiError(0, 'Нет связи с сервером. Проверьте интернет и попробуйте ещё раз.');
    }
    if (res.status === 204) return null;

    let data = null;
    try { data = await res.json(); } catch (_) { /* not JSON */ }
    if (!res.ok) throw new ApiError(res.status, (data && data.error) || 'Что-то пошло не так. Попробуйте ещё раз.', data && data.reason);
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
      err.reason && h('p', { class: 'hint small diag' }, err.reason),
      // Field names only (no values): enough to see what Telegram actually sent.
      err.status === 401 && inTelegram && h('p', { class: 'hint small diag' },
        'Поля от Telegram: ' + [...new URLSearchParams(tg.initData).keys()].join(', ')),
      // Not found: the item is gone, retrying won't bring it back — offer the list instead.
      err.status === 404 && h('button', {
        type: 'button', class: 'btn btn-secondary',
        onclick: () => location.replace(location.hash.startsWith('#/meetings') ? '#/meetings' : '#/books'),
      }, 'К списку'),
      err.status !== 403 && err.status !== 404 && h('button', { type: 'button', class: 'btn btn-secondary', onclick: () => refresh() }, 'Попробовать ещё раз'));
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

  // ---------------------------------------------------------------- cover photos
  // Photos live on the club owner's Yandex Disk; the server hands them out only to members, so an
  // <img src> (which can't carry the Telegram credentials) won't do. Each cover is fetched once with
  // the credentials and shown from a blob: URL. The URL changes with every new photo (?v=…), so the
  // browser may cache it for good.

  const coverCache = new Map(); // cover URL -> Promise of a blob: URL

  function loadCover(url) {
    if (!coverCache.has(url)) {
      const p = fetch(url, { headers: authHeaders() })
        .then((res) => { if (!res.ok) throw new Error('HTTP ' + res.status); return res.blob(); })
        .then((blob) => URL.createObjectURL(blob));
      p.catch(() => coverCache.delete(url)); // let the next screen try again
      coverCache.set(url, p);
    }
    return coverCache.get(url);
  }

  // cover(url, cls): a frame with the photo; without url, an empty frame with a book icon.
  // lazy: start loading only when the frame is about to scroll into view (long lists).
  // alt: what the photo is; a book cover by default (review photos use the same frame).
  function cover(url, cls, lazy, alt) {
    const frame = h('div', { class: 'cover ' + (cls || '') + (url ? '' : ' cover-none'), 'aria-hidden': url ? null : 'true' });
    if (!url) return frame;
    const img = h('img', { alt: alt || 'Фото обложки', decoding: 'async' });
    frame.append(img);
    const show = () => loadCover(url).then(
      (src) => { img.src = src; frame.classList.add('loaded'); },
      () => frame.classList.add('cover-none'));
    if (lazy && 'IntersectionObserver' in window) {
      const io = new IntersectionObserver((entries) => {
        if (entries.some((e) => e.isIntersecting)) { io.disconnect(); show(); }
      }, { rootMargin: '400px 0px' });
      io.observe(frame);
    } else {
      show();
    }
    return frame;
  }

  const PHOTO_MAX_SIDE = 1280;
  const PHOTO_QUALITY = 0.85;

  async function decodeImage(file) {
    if (window.createImageBitmap) {
      try { return await createImageBitmap(file, { imageOrientation: 'from-image' }); } catch (_) { /* older Safari: no options */ }
    }
    return new Promise((resolve, reject) => {
      const img = new Image();
      img.onload = () => resolve(img);
      img.onerror = () => reject(new Error('decode'));
      img.src = URL.createObjectURL(file);
    });
  }

  // A phone photo is 3–10 MB. Shrinks it to at most 1280 px on the long side as JPEG — about
  // 150–300 KB, enough for a cover and quick on mobile internet. Re-encoding also drops the photo's
  // metadata, including where it was taken.
  async function preparePhoto(file) {
    let img;
    try {
      img = await decodeImage(file);
    } catch (_) {
      throw new Error('Не получилось открыть это фото. Выберите другое или сделайте снимок камерой.');
    }
    const w = img.naturalWidth || img.width;
    const hgt = img.naturalHeight || img.height;
    const scale = Math.min(1, PHOTO_MAX_SIDE / Math.max(w, hgt));
    const canvas = document.createElement('canvas');
    canvas.width = Math.max(1, Math.round(w * scale));
    canvas.height = Math.max(1, Math.round(hgt * scale));
    const ctx = canvas.getContext('2d');
    ctx.fillStyle = '#fff'; // transparent PNGs would turn black in JPEG
    ctx.fillRect(0, 0, canvas.width, canvas.height);
    ctx.drawImage(img, 0, 0, canvas.width, canvas.height);
    if (img.close) img.close();
    const blob = await new Promise((resolve) => canvas.toBlob(resolve, 'image/jpeg', PHOTO_QUALITY));
    if (!blob) throw new Error('Не получилось подготовить фото. Попробуйте другое.');
    return blob;
  }

  async function uploadCover(bookId, blob) {
    const res = await api('PUT', `api/books/${bookId}/cover`, blob);
    // We already have the bytes: show them right away instead of downloading them back.
    coverCache.set(res.coverUrl, Promise.resolve(URL.createObjectURL(blob)));
    return res;
  }

  async function uploadReviewPhoto(reviewId, blob) {
    const res = await api('PUT', `api/reviews/${reviewId}/photo`, blob);
    coverCache.set(res.photoUrl, Promise.resolve(URL.createObjectURL(blob)));
    return res;
  }

  // photoPicker(onPick): a hidden file input. On phones it offers the camera or the gallery.
  function photoPicker(onPick) {
    const input = h('input', {
      type: 'file', accept: 'image/*', hidden: true,
      onchange: () => {
        const file = input.files && input.files[0];
        input.value = ''; // picking the same photo again must fire change too
        if (file) onPick(file);
      },
    });
    return input;
  }

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

  // Search ignores case, ё/е and quotes; every word of the query must appear in the title or author,
  // so "булгаков мастер" finds «Мастер и Маргарита».
  const normalize = (str) => str.toLowerCase().replace(/ё/g, 'е').replace(/[«»"„“”']/g, '').replace(/\s+/g, ' ').trim();
  let booksQuery = ''; // kept while the app is open, so going into a book and back keeps the filter

  function renderBooks(seq) {
    load(seq, async () => {
      const { books } = await api('GET', 'api/books');
      const add = h('a', { class: 'btn btn-primary', href: '#/books/new' }, '+ Добавить книгу');
      if (!books.length) {
        return [add, empty('Пока нет ни одной книги', 'Нажмите «Добавить книгу» — и она появится здесь для всего клуба.')];
      }

      // Thumbnails once any book has a photo; then books without one get an empty frame, so titles line up.
      const withCovers = books.some((b) => b.coverUrl);
      const items = books.map((b) => ({ el: bookCard(b, withCovers), key: normalize(`${b.title} ${b.author || ''}`) }));
      const title = h('h2', { class: 'section-title' });
      const nothing = h('div', { class: 'card empty', hidden: true },
        h('div', { class: 'empty-title' }, 'Ничего не нашлось'),
        h('p', { class: 'hint' }, 'Проверьте написание или поищите по фамилии автора.'));

      const input = h('input', {
        type: 'search', id: 'book-search', class: 'search', autocomplete: 'off', enterkeyhint: 'search',
        placeholder: 'Поиск: название или автор', 'aria-label': 'Поиск по книгам клуба', value: booksQuery,
        oninput: () => apply(),
        // "Search" on the phone keyboard just hides the keyboard: results are already on screen.
        onkeydown: (e) => { if (e.key === 'Enter') { e.preventDefault(); e.target.blur(); } },
      });
      const clear = h('button', {
        type: 'button', class: 'search-clear', 'aria-label': 'Очистить поиск', hidden: true,
        onclick: () => { input.value = ''; apply(); input.focus(); },
      }, '✕');

      function apply() {
        booksQuery = input.value;
        const words = normalize(booksQuery).split(' ').filter(Boolean);
        let shown = 0;
        for (const it of items) {
          it.el.hidden = !words.every((w) => it.key.includes(w));
          if (!it.el.hidden) shown++;
        }
        clear.hidden = !booksQuery;
        nothing.hidden = shown > 0;
        title.textContent = words.length ? `Найдено: ${shown} из ${books.length}` : `Книги клуба · ${books.length}`;
      }
      apply();

      return [add, h('div', { class: 'search-wrap' }, input, clear), title, nothing,
        h('div', { class: 'list' }, items.map((it) => it.el))];
    });
  }

  function bookCard(b, withCover) {
    const text = h('div', { class: 'book-card-text' },
      h('div', { class: 'book-title' }, b.title),
      b.author && h('div', { class: 'book-author' }, b.author),
      h('div', { class: 'book-meta' },
        b.rating.count
          ? [stars(b.rating.average), h('span', null, `${fmtAvg(b.rating.average)} · ${reviewsWord(b.rating.count)}`)]
          : h('span', { class: 'hint' }, 'Пока нет отзывов')),
      b.myRating && h('div', { class: 'my-mark' }, `Ваша оценка: ${b.myRating} из 5`));
    return h('a', { class: 'card book-card' + (withCover ? ' with-cover' : ''), href: '#/books/' + b.id },
      withCover && cover(b.coverUrl, 'cover-thumb', true),
      text);
  }

  function renderAddBook() {
    const title = h('input', { type: 'text', maxlength: 200, autocomplete: 'off', enterkeyhint: 'next' });
    const author = h('input', { type: 'text', maxlength: 200, autocomplete: 'off', enterkeyhint: 'next' });
    const link = h('input', { type: 'url', inputmode: 'url', maxlength: 1000, autocomplete: 'off', placeholder: 'https://…' });
    const note = h('textarea', { rows: 3, maxlength: 1000, placeholder: 'Например: о чём книга или почему стоит прочитать' });
    const err = errorLine();
    const submit = h('button', { type: 'submit', class: 'btn btn-primary' }, 'Добавить книгу');

    // The photo is prepared (shrunk) as soon as it's picked and uploaded after the book is saved.
    let photo = null;
    let preparing = null; // Promise while a picked photo is being shrunk
    const photoBox = h('div', { class: 'photo-field' });
    const picker = photoPicker((file) => {
      err.hide();
      preparing = preparePhoto(file).then((blob) => {
        photo = blob;
        paintPhoto();
      }, (ex) => {
        err.show(ex.message);
      }).finally(() => { preparing = null; });
    });
    let previewUrl = '';
    function paintPhoto() {
      if (previewUrl) URL.revokeObjectURL(previewUrl);
      previewUrl = photo ? URL.createObjectURL(photo) : '';
      photoBox.replaceChildren(...[
        photo && h('div', { class: 'cover cover-preview loaded' }, h('img', { alt: 'Фото обложки', src: previewUrl })),
        h('button', { type: 'button', class: 'btn btn-secondary', onclick: () => picker.click() },
          photo ? '📷 Выбрать другое фото' : '📷 Добавить фото обложки'),
        photo && h('button', { type: 'button', class: 'btn-link danger', onclick: () => { photo = null; paintPhoto(); } }, 'Убрать фото'),
        picker].filter(Boolean)); // native replaceChildren would print null as text
    }
    paintPhoto();

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
        if (preparing) await preparing;
        let book;
        try {
          book = await api('POST', 'api/books', { title: title.value, author: author.value, link: link.value, note: note.value });
        } catch (ex) {
          err.show(ex.message);
          busy(submit, false);
          return;
        }
        // The book is saved either way; a failed photo can be added again on the book's page.
        let photoFailed = false;
        if (photo) {
          submit.textContent = 'Загружаю фото…';
          try { await uploadCover(book.id, photo); } catch (_) { photoFailed = true; }
        }
        haptic(photoFailed ? 'warning' : 'success');
        toast(photoFailed ? 'Книга добавлена, но фото не загрузилось — попробуйте ещё раз на странице книги' : 'Книга добавлена');
        location.replace('#/books/' + book.id);
      },
    },
      h('h1', { class: 'screen-title' }, 'Новая книга'),
      field('Название', title),
      field('Автор', author, 'Можно не заполнять'),
      field('Ссылка на книгу', link, 'Необязательно. Например, страница книги в магазине или библиотеке'),
      field('Пара слов о книге', note, 'Необязательно'),
      me.covers && h('div', { class: 'field' }, h('span', { class: 'field-label' }, 'Фото обложки'), photoBox,
        h('span', { class: 'field-hint' }, 'Необязательно. Сфотографируйте обложку или выберите снимок из галереи')),
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
        b.coverUrl && cover(b.coverUrl, 'cover-big'),
        h('h1', { class: 'book-title big' }, b.title),
        b.author && h('div', { class: 'book-author' }, b.author),
        h('div', { class: 'rating-summary' },
          b.rating.count
            ? [stars(b.rating.average, 'lg'), h('span', { class: 'rating-num' }, fmtAvg(b.rating.average)), h('span', { class: 'hint' }, reviewsWord(b.rating.count))]
            : h('span', { class: 'hint' }, 'Отзывов пока нет — ваш может стать первым')),
        b.note && h('p', { class: 'note' }, b.note),
        b.link && h('button', { type: 'button', class: 'btn btn-secondary', onclick: () => openExternal(b.link) }, 'Открыть книгу по ссылке ↗'),
        canShare() && h('button', { type: 'button', class: 'btn btn-secondary', onclick: () => shareBook(b) }, 'Поделиться книгой в группе'),
        me.covers && b.canEdit && coverControls(b),
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
            const whose = b.mine ? '' : ` Её добавил(а) ${b.addedBy}.`;
            if (!(await confirmDialog(`Удалить книгу «${b.title}»?${whose}${warn}`))) return;
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

  // Add / replace / remove the cover photo on the book's page.
  function coverControls(b) {
    const err = errorLine();
    const pick = h('button', { type: 'button', class: 'btn btn-secondary', onclick: () => picker.click() },
      b.coverUrl ? '📷 Заменить фото обложки' : '📷 Добавить фото обложки');
    const picker = photoPicker(async (file) => {
      err.hide();
      busy(pick, true, 'Загружаю фото…');
      try {
        await uploadCover(b.id, await preparePhoto(file));
        haptic('success');
        toast(b.coverUrl ? 'Фото заменено' : 'Фото добавлено');
        refresh();
      } catch (ex) {
        err.show(ex.message);
        busy(pick, false);
      }
    });
    const remove = b.coverUrl && h('button', {
      type: 'button', class: 'btn-link danger',
      onclick: async () => {
        if (!(await confirmDialog('Убрать фото обложки?'))) return;
        try {
          await api('DELETE', `api/books/${b.id}/cover`);
          toast('Фото убрано');
          refresh();
        } catch (ex) {
          err.show(ex.message);
        }
      },
    }, 'Убрать фото');
    return h('div', { class: 'cover-controls' }, pick, picker, err.el, remove);
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

    // One photo per review. Like the new-book form: shrunk as soon as it's picked, uploaded after the
    // review is saved (a new review has no ID before that).
    const savedPhoto = mine && mine.photoUrl;
    let photo = null;        // a newly picked photo, not uploaded yet
    let dropSaved = false;   // remove the saved photo on save
    let preparing = null;    // Promise while a picked photo is being shrunk
    const photoBox = h('div', { class: 'photo-field' });
    const picker = photoPicker((file) => {
      err.hide();
      preparing = preparePhoto(file).then((blob) => {
        photo = blob;
        paintPhoto();
      }, (ex) => {
        err.show(ex.message);
      }).finally(() => { preparing = null; });
    });
    let previewUrl = '';
    function paintPhoto() {
      if (previewUrl) URL.revokeObjectURL(previewUrl);
      previewUrl = photo ? URL.createObjectURL(photo) : '';
      const showSaved = !photo && savedPhoto && !dropSaved;
      photoBox.replaceChildren(...[
        photo && h('div', { class: 'cover review-photo loaded' }, h('img', { alt: 'Фото к отзыву', src: previewUrl })),
        showSaved && cover(savedPhoto, 'review-photo', false, 'Фото к отзыву'),
        h('button', { type: 'button', class: 'btn btn-secondary', onclick: () => picker.click() },
          photo || showSaved ? '📷 Выбрать другое фото' : '📷 Добавить фото'),
        (photo || showSaved) && h('button', {
          type: 'button', class: 'btn-link danger',
          onclick: () => { if (photo) photo = null; else dropSaved = true; paintPhoto(); },
        }, 'Убрать фото'),
        picker].filter(Boolean));
    }
    paintPhoto();

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

    // Shares the saved review, not unsaved edits in the text box.
    const share = mine && canShare() && h('button', {
      type: 'button', class: 'btn btn-secondary', onclick: () => shareReview(book, mine),
    }, 'Поделиться отзывом в группе');

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
        if (preparing) await preparing;
        // Only the photo changed: don't touch the review, or it would show up as "изменён".
        const textChanged = !mine || rating !== mine.rating || text.value.trim() !== (mine.text || '');
        let saved = mine;
        if (textChanged) {
          try {
            saved = await api('PUT', `api/books/${book.id}/review`, { rating, text: text.value });
          } catch (ex) {
            err.show(ex.message);
            busy(save, false);
            return;
          }
        }
        // The review is saved either way; a failed photo can be added again.
        let photoFailed = false;
        try {
          if (photo) {
            save.textContent = 'Загружаю фото…';
            await uploadReviewPhoto(saved.id, photo);
          } else if (dropSaved && savedPhoto) {
            await api('DELETE', `api/reviews/${saved.id}/photo`);
          }
        } catch (_) {
          photoFailed = true;
        }
        haptic(photoFailed ? 'warning' : 'success');
        toast(photoFailed ? 'Отзыв сохранён, но фото не обновилось — попробуйте ещё раз'
          : mine ? 'Отзыв обновлён' : canShare() ? 'Спасибо! Отзывом можно поделиться в группе' : 'Спасибо за отзыв!');
        refresh();
      },
    },
      h('h2', { class: 'card-title' }, mine ? 'Ваш отзыв' : 'Ваш отзыв о книге'),
      starPicker(rating, (v) => { rating = v; err.hide(); }),
      text,
      me.covers && h('div', { class: 'field review-photo-field' }, photoBox,
        h('span', { class: 'field-hint' }, 'Необязательно. Одно фото — например, любимая страница или книга на полке')),
      err.el, save, share, remove);
  }

  function reviewCard(r) {
    const long = r.text && r.text.length > 600;
    const body = r.text && h('p', { class: 'review-text' + (long ? ' clamped' : '') }, r.text);
    const edited = new Date(r.updatedAt) - new Date(r.createdAt) > 60e3;

    const card = h('article', { class: 'card review' },
      h('div', { class: 'review-head' }, h('span', { class: 'review-author' }, r.author), stars(r.rating)),
      body,
      r.photoUrl && cover(r.photoUrl, 'review-photo', true, 'Фото к отзыву'),
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
      if (me.canManageMeetings) nodes.push(h('a', { class: 'btn btn-primary', href: '#/meetings/new' }, '+ Назначить встречу'));

      if (!upcoming.length) {
        nodes.push(empty('Ближайших встреч пока нет', me.canManageMeetings
          ? 'Нажмите «Назначить встречу», чтобы добавить дату.'
          : 'Как только администратор назначит встречу, она появится здесь.'));
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
      me.canManageMeetings && h('a', { class: 'btn btn-secondary', href: `#/meetings/${m.id}/edit` }, 'Изменить'));
  }

  function renderMeetingForm(seq, id) {
    if (!me.canManageMeetings) {
      $view.replaceChildren(errorBox(new ApiError(403, 'Назначать и менять встречи может только администратор клуба.')));
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
          const startsAt = new Date(`${date.value}T${time.value}`); // local time of the member's phone
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

  // Shown while the club isn't fully configured: the values for ADMIN_IDS and CLUB_CHAT_INSTANCE,
  // so the owner can set them up without access to server logs. Disappears once both are set.
  function setupCard(s) {
    const copy = (value) => h('button', {
      type: 'button', class: 'btn-link small',
      onclick: async () => {
        try {
          await navigator.clipboard.writeText(value);
          toast('Скопировано');
        } catch {
          toast('Не получилось скопировать — выделите текст пальцем');
        }
      },
    }, 'Скопировать');
    const row = (label, value) => h('div', { class: 'setup-row' },
      h('span', { class: 'meeting-label' }, label), h('code', null, value), copy(value));

    const fromGroup = s.chatType === 'group' || s.chatType === 'supergroup';
    return h('section', { class: 'card setup' },
      h('div', { class: 'empty-title' }, 'Настройка клуба'),
      s.needAdmins && [
        h('p', { class: 'hint small' }, 'Администраторов пока нет. Ваш Telegram ID — для ADMIN_IDS:'),
        row('Ваш ID', String(s.userId)),
      ],
      s.needAccess && [
        h('p', { class: 'hint small' }, 'Приложение сейчас открывает любой, у кого есть ссылка.'),
        fromGroup
          ? [h('p', { class: 'hint small' }, 'Этот чат — группа. Его chat_instance — для CLUB_CHAT_INSTANCE:'), row('Чат', s.chatInstance)]
          : h('p', { class: 'hint small' },
            'Чтобы увидеть chat_instance группы клуба, откройте приложение по ссылке из этой группы, например из закреплённого сообщения.'),
      ],
      h('p', { class: 'hint small' }, 'Блок исчезнет, когда обе настройки будут заданы.'));
  }

  async function login() {
    $tabs.hidden = true;
    try {
      me = await api('GET', 'api/me');
    } catch (err) {
      $view.replaceChildren(errorBox(err));
      return;
    }
    $footer.replaceChildren(`Вы вошли как ${me.name}${me.isAdmin ? ' · администратор' : ''}`);
    if (me.setup) $footer.append(setupCard(me.setup));
    // The button under the bot's post in the group opens https://t.me/<bot>/<app>?startapp=book_<id>:
    // go straight to that book.
    const start = (tg && tg.initDataUnsafe && tg.initDataUnsafe.start_param) || '';
    const fromPost = /^book_([0-9a-f]+)$/.exec(start);
    const target = routes.some(([re]) => re.test(location.hash)) ? location.hash
      : fromPost ? '#/books/' + fromPost[1] : '#/books';

    // An old post or shared message keeps pointing at its book after the book is deleted. Opening
    // such a link shouldn't start on an error screen: go to the list and say why.
    const book = /^#\/books\/([0-9a-f]+)$/.exec(target);
    if (book && !(await bookExists(book[1]))) {
      toast('Этой книги уже нет в клубе');
      location.replace('#/books'); // the hash was a book's, so hashchange fires and renders the list
      return;
    }
    if (target === location.hash) render(true);
    else location.replace(target);
  }

  // false only for a definite "not found"; on any other error the book screen shows it as usual.
  async function bookExists(id) {
    try {
      await api('GET', 'api/books/' + id);
      return true;
    } catch (err) {
      return err.status !== 404;
    }
  }

  setup();
  login();
})();
