// The environment-variable editor on /{owner}/{repo}/_app.
//
// Rendering a fixed number of blank rows meant guessing: too few and adding a
// database URL with its credentials took three saves and three restarts, too
// many and the form was mostly empty boxes. The page ships one blank row and
// this adds more on request, which is the same shape as every other "add
// another" form the people using this have met.
//
// Names have to stay unique because the server pairs env_name_N with
// env_value_N (company/app_page.go); rows are numbered from a counter rather
// than from the current row count so removing a row cannot make the next one
// collide with a row that is still there.

function nextIndex(container: HTMLElement): string {
  const n = Number(container.getAttribute('data-company-env-next') ?? '0');
  container.setAttribute('data-company-env-next', String(n + 1));
  return `new${n}`;
}

function addRow(container: HTMLElement): void {
  const template = container.querySelector<HTMLTemplateElement>('[data-company-env-template]');
  const body = container.querySelector<HTMLElement>('[data-company-env-rows]');
  if (!template || !body) return;

  const row = template.content.firstElementChild?.cloneNode(true) as HTMLElement | undefined;
  if (!row) return;

  const index = nextIndex(container);
  for (const input of row.querySelectorAll<HTMLInputElement>('input[data-name-prefix]')) {
    input.name = `${input.getAttribute('data-name-prefix')}${index}`;
    input.removeAttribute('data-name-prefix');
  }
  body.append(row);
  row.querySelector<HTMLInputElement>('input[type=text]')?.focus();
}

// Checkboxes mark rows for deletion on the next save. The header box is a
// convenience for clearing several at once and is deliberately not submitted
// itself — only the per-row boxes name anything the server acts on.
function initSelectAll(container: HTMLElement): void {
  const all = container.querySelector<HTMLInputElement>('[data-company-env-all]');
  if (!all) return;
  const boxes = () => container.querySelectorAll<HTMLInputElement>('[data-company-env-delete]');
  all.addEventListener('change', () => {
    for (const box of boxes()) box.checked = all.checked;
  });
  container.addEventListener('change', (e) => {
    if (!(e.target as HTMLElement).matches('[data-company-env-delete]')) return;
    const list = Array.from(boxes());
    all.checked = list.length > 0 && list.every((b) => b.checked);
    all.indeterminate = !all.checked && list.some((b) => b.checked);
  });
}

export function initCompanyAppEnv(): void {
  const container = document.querySelector<HTMLElement>('[data-company-env]');
  if (!container) return;

  container.querySelector('[data-company-env-add]')?.addEventListener('click', (e) => {
    e.preventDefault();
    addRow(container);
  });
  initSelectAll(container);

  container.querySelector('[data-company-env-remove]')?.addEventListener('click', (e) => {
    const checked = Array.from(container.querySelectorAll<HTMLInputElement>('[data-company-env-delete]:checked'));
    if (checked.length === 0) {
      e.preventDefault();
      window.alert('삭제할 항목을 선택해 주세요.');
      return;
    }

    // A row that was never saved has nothing on the server to delete, so it
    // is dropped from the form here. Doing it the other way — submitting so
    // the page comes back without it — would discard whatever was typed into
    // every other unsaved row on the way.
    const unsaved = checked.filter((box) => box.hasAttribute('data-company-env-new'));
    const saved = checked.filter((box) => !box.hasAttribute('data-company-env-new'));

    // Deleting a stored secret cannot be undone by retyping it — nobody can
    // read the old value back — so it is confirmed and counted. Removing a
    // row someone is still typing into is not worth a dialog.
    if (saved.length > 0 &&
        !window.confirm(`저장된 환경변수 ${saved.length}개를 삭제합니다. 값은 다시 볼 수 없습니다. 계속할까요?`)) {
      e.preventDefault();
      return;
    }
    for (const box of unsaved) box.closest('tr')?.remove();
    if (saved.length === 0) {
      e.preventDefault(); // nothing for the server to do
    }
  });
}
