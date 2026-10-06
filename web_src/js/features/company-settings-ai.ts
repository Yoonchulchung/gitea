import {registerGlobalInitFunc} from '../modules/observer.ts';
import {POST} from '../modules/fetch.ts';
import {fomanticQuery} from '../modules/fomantic/base.ts';

// /user/settings/ai (company/settings_ai.go, custom/templates/company/settings_ai.tmpl).
// Model ID is a dropdown that still accepts a typed-in value
// (allowAdditions), not a locked-down list, since an internal
// OpenAI-compatible gateway's model names are deployment-specific — not a
// fixed set this codebase could ever hardcode correctly.
//
// Claude's model family is small and effectively fixed, so its suggestions
// are just listed here directly, no request needed. These are pulled from
// this assistant's own known-current model IDs (see AGENTS.md/CLAUDE.md
// context) rather than guessed — Anthropic doesn't publish a public,
// unauthenticated "list models" endpoint. This list will drift as new
// models ship; the field stays free text either way, so an outdated
// suggestion never blocks entering a newer ID by hand.
export const ANTHROPIC_MODEL_SUGGESTIONS = [
  'claude-opus-5',
  'claude-sonnet-5',
  'claude-fable-5',
  'claude-haiku-4-5',
];

export function initCompanySettingsAI(): void {
  registerGlobalInitFunc('initCompanyHeaderRows', initHeaderRows);
  registerGlobalInitFunc('initCompanyPlatformAI', initPlatformAIModels);
  registerGlobalInitFunc('initCompanySettingsAI', (form: HTMLFormElement) => {
    const modelsUrl = form.getAttribute('data-models-url')!;
    const providerInput = form.querySelector<HTMLInputElement>('.company-settings-ai-provider input[name="provider"]')!;
    const keyInput = form.querySelector<HTMLInputElement>('.company-settings-ai-key')!;
    const fetchButton = form.querySelector<HTMLButtonElement>('.company-settings-ai-fetch-models')!;
    const modelDropdown = form.querySelector<HTMLElement>('.company-settings-ai-model')!;
    const modelValue = modelDropdown.querySelector<HTMLInputElement>('input[name="model_id"]')!;
    const modelMenu = modelDropdown.querySelector<HTMLElement>('.menu')!;
    const modelHelp = form.querySelector<HTMLElement>('.company-settings-ai-model-help')!;
    const modelsEmptyText = modelDropdown.closest('.field')!.getAttribute('data-i18n-models-empty')!;

    fomanticQuery(modelDropdown).dropdown({
      allowAdditions: true, // a gateway model ID that isn't in the list must still be enterable
      forceSelection: false,
      fullTextSearch: 'exact',
    });

    // Rebuilds the menu from `models`, keeping whatever is currently saved
    // selected — and listing it even when it isn't among the suggestions,
    // so switching provider (or a fetch that doesn't return it) never
    // silently drops the value the user already has stored.
    function setOptions(models: string[]): void {
      const current = modelValue.value.trim();
      const ids = current && !models.includes(current) ? [current, ...models] : models;
      if (ids.length) {
        modelMenu.replaceChildren(...ids.map((id) => {
          const item = document.createElement('div');
          item.className = 'item';
          item.setAttribute('data-value', id);
          item.textContent = id;
          return item;
        }));
      } else {
        // The gateway's list starts empty (its model IDs are only known
        // after a lookup). Fomantic suppresses its own "no results" text
        // whenever allowAdditions is on, so without this the menu opens as
        // a blank box that looks broken — say what to do instead. A
        // ".message" is Fomantic's own non-selectable menu element.
        const msg = document.createElement('div');
        msg.className = 'message';
        msg.textContent = modelsEmptyText;
        modelMenu.replaceChildren(msg);
      }
      fomanticQuery(modelDropdown).dropdown('refresh');
      if (current) fomanticQuery(modelDropdown).dropdown('set selected', current);
    }

    function applyProvider(): void {
      const isAnthropic = providerInput.value === 'anthropic';
      fetchButton.classList.toggle('tw-hidden', isAnthropic);
      setOptions(isAnthropic ? ANTHROPIC_MODEL_SUGGESTIONS : []);
      modelHelp.textContent = '';
    }

    // Fomantic's dropdown module fires a native "change" on its own hidden
    // input whenever the selected value changes — no separate API needed.
    providerInput.addEventListener('change', applyProvider);
    applyProvider();

    fetchButton.addEventListener('click', async () => {
      fetchButton.disabled = true;
      modelHelp.textContent = '조회 중…';
      try {
        const resp = await POST(modelsUrl, {data: {api_key: keyInput.value.trim()}});
        if (!resp.ok) {
          const text = await resp.text();
          modelHelp.textContent = text || '모델 목록을 가져오지 못했습니다.';
          return;
        }
        const {models} = await resp.json() as {models: string[]};
        setOptions(models);
        modelHelp.textContent = models.length ? `${models.length}개 모델을 찾았습니다 — 입력창에서 선택하세요.` : '조회된 모델이 없습니다.';
      } catch {
        modelHelp.textContent = '모델 목록을 가져오지 못했습니다.';
      } finally {
        fetchButton.disabled = false;
      }
    });
  });
}

function initHeaderRows(field: HTMLElement): void {
  const template = field.querySelector<HTMLTemplateElement>('.company-settings-ai-header-template')!;
  const addButton = field.querySelector<HTMLButtonElement>('.company-settings-ai-header-add')!;
  const addRow = () => {
    addButton.before(template.content.cloneNode(true));
    return addButton.previousElementSibling!.querySelector<HTMLInputElement>('input[name="header_name"]')!;
  };
  addButton.addEventListener('click', () => addRow().focus());
  field.addEventListener('click', (e) => {
    (e.target as Element).closest('.company-settings-ai-header-remove')?.closest('.company-settings-ai-header-row')!.remove();
  });
  if (!field.querySelector('.company-settings-ai-header-row')) addRow();
}

// /-/admin/company-ai (company/admin_ai.go): the model is picked from what the
// provider lists for the form's current URL, key and headers — never typed.
function initPlatformAIModels(form: HTMLFormElement): void {
  const field = form.querySelector<HTMLElement>('.company-platform-ai-model-field')!;
  const dropdown = field.querySelector<HTMLElement>('.company-platform-ai-model')!;
  const value = dropdown.querySelector<HTMLInputElement>('input[name="model"]')!;
  const menu = dropdown.querySelector<HTMLElement>('.menu')!;
  const help = field.querySelector<HTMLElement>('.company-platform-ai-model-help')!;
  const fetchButton = field.querySelector<HTMLButtonElement>('.company-platform-ai-fetch')!;
  fomanticQuery(dropdown).dropdown();

  initPriceHint(form, value);

  const setModels = (models: string[]) => {
    menu.replaceChildren(...models.map((id) => {
      const item = document.createElement('div');
      item.className = 'item';
      item.setAttribute('data-value', id);
      item.textContent = id;
      return item;
    }));
    const keep = models.includes(value.value) ? value.value : '';
    fomanticQuery(dropdown).dropdown('refresh');
    if (keep) {
      fomanticQuery(dropdown).dropdown('set selected', keep);
    } else {
      fomanticQuery(dropdown).dropdown('clear');
    }
  };

  // Another provider lists other models; what was picked for the old one no longer applies.
  const provider = form.querySelector<HTMLSelectElement>('select[name="provider"]')!;
  provider.addEventListener('change', () => {
    for (const el of form.querySelectorAll('.company-platform-ai-openai-only')) {
      el.classList.toggle('tw-hidden', provider.value === 'anthropic');
    }
    setModels([]);
    help.textContent = '';
  });

  fetchButton.addEventListener('click', async () => {
    fetchButton.classList.add('is-loading');
    help.textContent = field.getAttribute('data-i18n-loading')!;
    try {
      const resp = await POST(field.getAttribute('data-models-url')!, {data: new FormData(form)});
      if (!resp.ok) {
        help.textContent = await resp.text();
        return;
      }
      const {models} = await resp.json() as {models: string[]};
      setModels(models);
      help.textContent = models.length ? field.getAttribute('data-i18n-found')!.replace('%d', () => String(models.length)) : field.getAttribute('data-i18n-none')!;
    } finally {
      fetchButton.classList.remove('is-loading');
    }
  });
}

// The price line under the model: the known list price the moment a model is
// picked; any other model is priced by the platform AI once it is saved.
function initPriceHint(form: HTMLFormElement, model: HTMLInputElement): void {
  const line = form.querySelector<HTMLElement>('.company-platform-ai-price');
  if (!line) return;
  const known = JSON.parse(line.getAttribute('data-known-prices')!) as Record<string, [number, number]>;
  model.addEventListener('change', () => {
    const id = model.value.trim().toLowerCase();
    const match = Object.keys(known).filter((k) => id === k || id.startsWith(`${k}-`)).sort((a, b) => b.length - a.length)[0];
    line.textContent = match ?
      line.getAttribute('data-i18n-known')!.replace('%s', () => String(known[match][0])).replace('%s', () => String(known[match][1])) :
      line.getAttribute('data-i18n-unsaved')!;
  });
}
