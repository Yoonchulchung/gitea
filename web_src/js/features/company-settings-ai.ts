import {registerGlobalInitFunc} from '../modules/observer.ts';
import {POST} from '../modules/fetch.ts';

// /user/settings/ai (company/settings_ai.go, custom/templates/company/settings_ai.tmpl).
// Model ID is a free-text input with a <datalist> of suggestions rather
// than a locked-down dropdown, since an internal OpenAI-compatible
// gateway's model names are deployment-specific — not a fixed set this
// codebase could ever hardcode correctly.
//
// Claude's model family is small and effectively fixed, so its suggestions
// are just listed here directly, no request needed. These are pulled from
// this assistant's own known-current model IDs (see AGENTS.md/CLAUDE.md
// context) rather than guessed — Anthropic doesn't publish a public,
// unauthenticated "list models" endpoint. This list will drift as new
// models ship; the field stays free text either way, so an outdated
// suggestion never blocks entering a newer ID by hand.
const ANTHROPIC_MODEL_SUGGESTIONS = [
  'claude-opus-5',
  'claude-sonnet-5',
  'claude-fable-5',
  'claude-haiku-4-5',
];

export function initCompanySettingsAI(): void {
  registerGlobalInitFunc('initCompanySettingsAI', (form: HTMLFormElement) => {
    const modelsUrl = form.getAttribute('data-models-url')!;
    const providerInput = form.querySelector<HTMLInputElement>('.company-settings-ai-provider input[name="provider"]')!;
    const keyInput = form.querySelector<HTMLInputElement>('.company-settings-ai-key')!;
    const modelOptions = form.querySelector<HTMLDataListElement>('#company-settings-ai-model-options')!;
    const fetchButton = form.querySelector<HTMLButtonElement>('.company-settings-ai-fetch-models')!;
    const modelHelp = form.querySelector<HTMLElement>('.company-settings-ai-model-help')!;

    function setOptions(models: string[]): void {
      modelOptions.replaceChildren(...models.map((id) => {
        const opt = document.createElement('option');
        opt.value = id;
        return opt;
      }));
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
